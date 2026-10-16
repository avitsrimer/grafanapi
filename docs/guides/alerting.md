---
title: Alerting
---

This section describes how to inspect and manage Grafana Unified Alerting from your terminal:
which alert rules are defined, which of them are firing right now, which contact points they
notify — and how to keep the whole configuration in files you can review, diff and promote between
environments.

```shell
grafanapi alerts rules list [flags]
grafanapi alerts contact-points list [flags]

grafanapi alerts pull [-p ./alerting] [--folder-uid UID]... [-o yaml|json]
grafanapi alerts push [-p ./alerting] [--dry-run] [--ui-editable] [--on-error ignore|fail|abort]

grafanapi alerts rules pause  UID [UID...] [--dry-run]
grafanapi alerts rules resume UID [UID...] [--dry-run]
grafanapi alerts rules delete UID [UID...] [--dry-run]
```

Every command that changes Grafana takes `--dry-run`. Run it first.

## Permissions

| Command                        | Role needed | Notes                                                |
| ------------------------------ | ----------- | ---------------------------------------------------- |
| `alerts rules list`            | Viewer      | rules in folders you cannot read are filtered out server-side |
| `alerts contact-points list`   | Viewer      | contact points are org-wide, so it is all or nothing |
| `alerts pull`                  | Viewer      | writes only to disk                                  |
| `alerts push`                  | **Editor**  | creates and updates rule groups, contact points, the policy tree |
| `alerts rules pause`/`resume`  | **Editor**  | rewrites the rule's whole group                      |
| `alerts rules delete`          | **Editor**  | deletes the named rules                              |

Verified on Grafana OSS 12.3.2 with the default basic roles. With fine-grained RBAC a read needs
alert-rule read and notification read permissions, and a write needs `alert.provisioning:write`;
without them Grafana answers `403`, which the CLI renders as "Grafana denied the request: your
user lacks the required permission" and exit code **1** (not the stale-session exit 2 — see
[Stale sessions](#stale-sessions)). A `403` needs a pending change to surface, though: a `push`
whose plan is entirely `Unchanged` attempts no write at all, so it exits **0** even for a Viewer.

No command in this group performs an **org-admin** operation. Resetting the Alertmanager
configuration, exporting decrypted secrets, deleting contact points and editing data sources are
all outside it, deliberately.

What Grafana *does* filter silently is folder access on reads: alert rules in folders your user
cannot read are simply absent from the response and the command still succeeds. Contact points are
**not** folder-scoped — they are org-wide objects gated by notification permissions, so you either
see all of them or none. On a `push`, a folder you may not read is reported per folder rather than
failing the whole run — see [Folders must exist first](#folders-must-exist-first).

## Listing alert rules

[`grafanapi alerts rules list`](../reference/cli/grafanapi_alerts_rules_list.md) answers "what is
defined" and "what is firing" in one call. It joins two sources on the rule UID:

| Source                                        | Provides                                                     |
| --------------------------------------------- | ------------------------------------------------------------ |
| `GET /api/v1/provisioning/alert-rules`        | definitions: title, folder UID, group, `for`, paused, labels, annotations, provenance |
| `GET /api/prometheus/grafana/api/v1/rules`    | live state: folder **path**, state, health, last evaluation, active-instance counts |

```shell
grafanapi alerts rules list
```

```
FOLDER        GROUP        TITLE          UID         TYPE      STATE     HEALTH  FOR  PAUSED  ALERTS
Probe Folder  probe-group  probe-rule     rule-uid-1  alerting  firing    ok      1m   false   1
Probe Folder  probe-group  probe-rule-ui  rule-uid-2  alerting  inactive  ok      1m   false   0
```

Rows are sorted by folder path, then group, then rule title (the UID breaks ties, so the order is
stable between runs).

Definitions are the primary data and state is enrichment: a rule that exists but has no state
entry — one Grafana has not evaluated yet — is still listed, with `STATE` `unknown` and its
`FOLDER` still filled in (see [Filtering](#filtering)).

### Columns

- `FOLDER` — the folder's **full path**, which only the state endpoint reports (see
  [Degraded mode](#degraded-mode)). A nested folder reads `Parent Folder/Child Folder`, not
  `Child Folder`; a `/` inside a folder title itself arrives escaped as `\/`. Use `-o json` for
  `folderUID`.
- `TYPE` — `alerting` or `recording`. Recording rules are returned by the provisioning API, so
  they appear in this list too.
- `STATE` — see [Rule states](#rule-states).
- `HEALTH` — the rule's evaluation health as Grafana reports it (`ok`, `error`, `nodata`,
  `unknown`).
- `FOR` — the pending period, formatted for humans: `1m` rather than `1m0s`, `0s` when unset.
- `PAUSED` — whether evaluation is paused for the rule.
- `ALERTS` — how many of the rule's alert instances are **not** `Normal` (that is: `Alerting`,
  `Pending`, `Recovering`, `NoData` or `Error`). It is an exact count of the rule's instances, not
  a total instance count: an instance that evaluated to `Normal` for some *reason* — Grafana
  reports those as `Normal (Error)`, `Normal (KeepLast)` and so on — is still `Normal` and does
  not count.

Labels, annotations, `folderUID`, `provenance`, `updated` and `lastEvaluation` are omitted from the
table on purpose; `-o json` and `-o yaml` carry the full rule.

### Rule states

`STATE` takes one of seven values. Four come from Grafana's evaluation engine verbatim, and three
are assigned by the join:

| State         | Meaning                                                                    |
| ------------- | -------------------------------------------------------------------------- |
| `firing`      | the rule is alerting                                                       |
| `pending`     | the condition is met but the pending period (`FOR`) has not elapsed yet    |
| `recovering`  | the rule has stopped alerting and is waiting out its keep-firing period    |
| `inactive`    | the rule is evaluating normally (Grafana's UI labels this "Normal")        |
| `recording`   | assigned to every recording rule, which has no evaluation state by nature  |
| `unknown`     | the state view was read, but has no entry for this rule yet — Grafana has not evaluated it |
| `unavailable` | the state view could **not** be read at all, so no rule's state is known — see [Degraded mode](#degraded-mode) |

A recording rule is therefore never reported as `unknown` or `unavailable`, and neither of those
ever hides a recording rule.

`unavailable` is the one value that says nothing about the rule itself: it means the state half of
every row is missing. Automation reading `-o json` should treat it as "no state was fetched" rather
than as a rule state — it is the signal that a `select(.state == "firing")` result of `[]` cannot be
trusted.

### Filtering

`--state` takes a single value. It is trimmed and lowercased before matching, so `--state Firing`
works, and `normal` is accepted as an alias of `inactive` to match the wording of the Grafana UI.
Anything else is rejected with an error listing the valid values, before any network call is made.

```shell
grafanapi alerts rules list --state firing
grafanapi alerts rules list --state normal       # same as --state inactive
grafanapi alerts rules list --state recording    # only recording rules
grafanapi alerts rules list --state unknown      # rules Grafana has not evaluated yet
grafanapi alerts rules list --state unavailable  # only ever matches during a state outage
```

`--folder` matches a rule's folder **UID**, its **full path**, or that path's **last segment** —
exactly and case-sensitively in all three cases (no prefix or substring matching, no globbing):

```shell
grafanapi alerts rules list --folder probe-folder                      # by UID
grafanapi alerts rules list --folder "Probe Folder"                    # by path (root folder)
grafanapi alerts rules list --folder "Parent Folder/Child Folder"      # by full path
grafanapi alerts rules list --folder "Child Folder"                    # by last segment
```

Naming a parent folder does **not** match the rules in its children: `--folder "Parent Folder"`
only matches rules whose own folder is `Parent Folder`.

The last-segment form matches the folder **title**, not one folder. Grafana allows the same title
under two different parents, so `--folder Reports` lists the rules of both `A/Reports` and
`B/Reports` — the `FOLDER` column says which is which, and naming either path in full selects just
that folder.

The path and last-segment forms need the folder's path, which only the state endpoint reports: per
rule for a rule it has evaluated, and per **folder** for the rest. So a rule that has never been
evaluated is still matched by path or title, as long as *some* rule in its folder has been. The one
gap: if **no** rule in a folder has ever been evaluated, that folder's path appears nowhere in
Grafana's response, so its rules show an empty `FOLDER` and are reachable only with
`--folder <UID>`:

```shell
grafanapi alerts rules list -o json | jq -r '.[] | select(.folder == null) | .folderUID' | sort -u
```

Both filters are applied client-side over the joined list, so they compose freely:

```shell
grafanapi alerts rules list --folder "Probe Folder" --state firing
```

### Degraded mode

If the state endpoint fails with anything other than a `401`, an **unfiltered** list does not fail.
It logs a warning and returns every rule definition with `STATE` set to `unavailable` and `FOLDER`
empty:

```
alert rule state unavailable; STATE shows unavailable and FOLDER is empty for every rule
```

The warning goes to stderr, so `-o json` output stays a valid `jq` pipe in degraded mode.

!!! warning "Check for `unavailable` in automation"

    Because the run succeeds, a degraded list exits **0**. The warning is on stderr, where a pipe
    does not see it, so the `unavailable` state on the rows themselves is what tells automation
    that the state half was never fetched — otherwise
    `alerts rules list -o json | jq 'map(select(.state == "firing"))'` returning `[]` is
    indistinguishable from "nothing is firing":

    ```shell
    grafanapi alerts rules list -o json \
      | jq -e 'any(.[]; .state == "unavailable") | not' > /dev/null \
      || echo "alert rule state was not fetched; the states above mean nothing" >&2
    ```

A **filtered** list fails instead, with the generic exit code **1** and the state-endpoint failure
attached as the cause:

```
alerts: cannot apply --state firing without the alert rule state: alerts: alert rule state:
unexpected status 500: ...
```

Filtering on data that could not be fetched would print `[]` and exit 0 — indistinguishable from
"nothing is firing", and invisible to a `jq` pipe. So:

- `--folder` always fails, in every form. Folder paths come only from the state endpoint, and a UID
  cannot be told apart from a path or a title, so there is no form of the filter that can be
  answered honestly.
- `--state` fails for every value except `unavailable` and `recording`. Those two are decided
  without the state view at all — `unavailable` is exactly what every alerting rule is stamped with
  once reading it failed, and `recording` comes from the rule definition — so they keep working.
- `--state unknown` therefore **fails** in degraded mode, even though the join assigns `unknown`
  too. It is a claim about a state view that *was* read ("no entry for this rule yet"), so
  answering it during an outage would silently redefine it as "every alerting rule" and exit 0.
  `--state unavailable` is the filter for an outage.

To get *something* out of a state-endpoint outage, drop the filters and filter the `-o json` output
on `folderUID` instead, remembering that `state` is `unavailable` for every alerting rule:

```shell
grafanapi alerts rules list -o json | jq '[.[] | select(.folderUID == "probe-folder")]'
```

A `401` is different: it is fatal from either endpoint, so a stale session is never silently
reported as "no state". See [Stale sessions](#stale-sessions).

## Listing contact points

[`grafanapi alerts contact-points list`](../reference/cli/grafanapi_alerts_contact-points_list.md)
lists the notification contact points alert rules deliver to:

```shell
grafanapi alerts contact-points list
```

```
NAME            TYPE   UID       PROVENANCE
email receiver  email
probe-slack     slack  cp-uid-1  api
```

Contact points are sorted by name (the UID breaks ties — Grafana allows two contact points to
share a name).

An empty `PROVENANCE` means the contact point is editable in the UI; `api` means it was created
through the provisioning API and the UI treats it as provisioned. An empty `UID` is normal for
Grafana's built-in default email receiver.

Grafana replaces the **secure fields** of a contact point's `settings` — the fields its type
declares as secret, such as a Slack token or a Pushover key — with the literal string
`"[REDACTED]"` **server-side**, before the CLI ever sees it:

```shell
grafanapi alerts contact-points list -o json | jq '.[] | select(.type == "slack") | .settings'
```

```json
{
  "recipient": "#probe",
  "url": "[REDACTED]"
}
```

!!! warning "Redaction covers declared secure fields only"

    A credential that lives somewhere Grafana does not treat as a secure field is returned
    **verbatim** — a token embedded in a webhook URL path or query string, or in a custom header
    value, is not redacted. Check `settings` before pasting it into a ticket or a chat.

`settings` is a nested object of arbitrary, per-type shape, so it is a json/yaml-only field — the
table shows `NAME`, `TYPE`, `UID` and `PROVENANCE` only.

## Output formats

Both **list** commands accept `-o table` (the default), `-o json` and `-o yaml`. (On
[`alerts pull`](#alerting-as-code) `-o` means something else: the format of the *files* it writes,
`yaml` or `json` only.)

`table` is the human-readable rendering described above. `json` and `yaml` encode the full objects
and use the **same camelCase keys** (`folderUID`, `isPaused`, `activeAlerts`, `lastEvaluation`,
`disableResolveMessage`, …), so a `jq` filter translates directly to a `yq` one.

Neither format sorts keys: both emit them in the order the encoded type declares its fields. For a
rule that is `uid, title, type, folderUID, folder, ruleGroup, for, isPaused, provenance, labels,
annotations, updated, state, health, lastEvaluation, activeAlerts`. Contact points happen to come
out alphabetically only because the generated model declares its fields that way.

Both commands always emit a list, so an empty result is `[]` and never `null` — a `jq '.[]'` pipe
keeps working when there are no rules or contact points at all.

Optional keys are omitted when empty rather than emitted as a zero value, so a rule that has never
been evaluated has no `lastEvaluation` key at all (the same holds for `updated`, `folder`,
`provenance`, `labels`, `annotations` and `health`):

```shell
grafanapi alerts rules list -o json | jq '.[] | select(.lastEvaluation == null) | .title'
```

More examples:

```shell
# Just the firing rules, by title
# (an "unavailable" state anywhere in the output means the state view could not be read, so an
#  empty result here says nothing — see Degraded mode)
grafanapi alerts rules list -o json | jq -r '.[] | select(.state == "firing") | .title'

# Rule title, state and active instance count, tab-separated
grafanapi alerts rules list -o json | jq -r '.[] | "\(.title)\t\(.state)\t\(.activeAlerts)"'

# Which rules are paused?
grafanapi alerts rules list -o json | jq -r '.[] | select(.isPaused) | .title'

# Folder UID → rule count
grafanapi alerts rules list -o json | jq -r 'group_by(.folderUID)[] | "\(.[0].folderUID) \(length)"'

# Contact point name and type
grafanapi alerts contact-points list -o json | jq -r '.[] | "\(.name)\t\(.type)"'
```

## Alerting as code

[`grafanapi alerts pull`](../reference/cli/grafanapi_alerts_pull.md) writes the alerting
configuration of the current context to a directory, and
[`grafanapi alerts push`](../reference/cli/grafanapi_alerts_push.md) sends a directory back — the
same pull → edit → review → push loop `resources pull`/`push` gives dashboards and folders.

```shell
grafanapi alerts pull -p ./alerting            # Grafana → disk
$EDITOR ./alerting/rules/folder-uid-1/probe-group.yaml
grafanapi alerts push -p ./alerting --dry-run  # review the plan
grafanapi alerts push -p ./alerting            # disk → Grafana
```

### The bundle layout

```
alerting/
  rules/
    folder-uid-1/
      probe-group.yaml          # one rule group
    folder-uid-2/
      latency.yaml
  contact-points.yaml           # every contact point, one list
  policies.yaml                 # the notification policy tree
```

`-o json` writes the same layout with `.json` files and the **same keys**; pick one format per
bundle and stay with it. A pull removes the top-level file in every other extension it recognises,
so `pull -o json` after `pull -o yaml` cannot leave two files disagreeing about the same object.

The directory names are **not authoritative**. `<folder UID>` and the file name are sanitised
(`/`, `\`, control characters and a leading `.` become `_`), so what a later push sends to Grafana
is the `folderUid` and `title` recorded *inside* each file. Rename a file freely; change the
`title` inside it and you have renamed the group.

That sanitisation is many-to-one, so two groups in one folder can map to the same file name (`a/b`
and `a_b` both become `a_b.yaml`). A pull that hits such a collision **aborts before writing
anything**, naming both groups — silently overwriting one with the other would look, to the next
push, like every rule of the overwritten group had been deleted. Rename one of the two groups in
Grafana and pull again. The same rule applies to the bundle you hand back: two files declaring the
same `folderUid` and `title` are refused by `alerts push`, naming both files.

Names are compared **case-insensitively**, and after Unicode normalisation, because macOS holds
`Alpha.yaml` and `alpha.yaml` as one file. Two groups in a folder titled `Alpha` and `alpha` are
therefore a collision, and so are two folder UIDs that differ only in case — even though Grafana
itself treats each pair as two distinct objects. Re-titling a group from `Alpha` to `alpha` is
fine: the next pull rewrites the one file that exists and keeps it.

!!! warning "`folderUid` on the group, `folderUID` on each rule"

    Both spellings appear in one group file, because Grafana's own API models spell them
    differently — the group carries `folderUid` and every rule under `rules[]` carries `folderUID`.
    They are not a typo and they must agree. A push overwrites each rule's `folderUID` and
    `ruleGroup` from the group's own values, so the group-level `folderUid` is the one to edit.

Each rule also carries an `orgID`. **Do not hand-edit it** — a push overwrites it from the remote
group when the group already exists, otherwise from the context's `org-id`, and only falls back to
what the file says. It is written out so the file round-trips, not so you can change an org.

Because a push fills all three in, a **hand-written** rule may leave `folderUID`, `ruleGroup`,
`orgID` and `uid` out entirely: the group file is what addresses the group, and Grafana assigns the
uid on create. The fields a rule cannot be written without are `title`, `condition`, `data`,
`execErrState`, `noDataState` and `for`. The one case a push cannot fill in is `orgID` for a group
that does not exist yet on a Grafana Cloud context (which addresses a stack, so its `org-id` is 0);
that group fails by name, telling you to set `org-id` on the context or `orgID` on every rule.
Leaving the three out is not a permanent diff: the plan compares the rule a push would *send*, with
those fields already filled in, so an omitted — or disagreeing — value is never reported as a change
(see [Review the plan first](#review-the-plan-first)).

A `uid`, when present, must be unique across the whole bundle: it addresses one rule in the whole
instance, so two rules claiming it — in one file or in two — are refused, naming both.

An `alerts pull` never writes an object's `id`, `updated` or `provenance`: those are Grafana's to
set, and a file carrying them would be rejected as an unknown key on the way back in. Both formats
are read **strictly** — an unknown or misspelled key is an error naming the file, before any
network call.

### Stale files are pruned on pull

A pull removes files under `rules/` that it did not just write, so a group deleted in Grafana is
not resurrected by the next push. With `--folder-uid` that pruning is restricted to the named
folders, which keeps a partial bundle's other folders intact:

```shell
grafanapi alerts pull -p ./alerting --folder-uid folder-uid-1   # only folder-uid-1/ is reconciled
```

Pruning only touches files the layout owns — `.yaml`, `.yml` and `.json` one level below `rules/` —
so a `README.md`, an editor backup or a directory of your own under `rules/` is left alone, and no
directory is ever removed.

`--folder-uid` takes a folder **UID** only (unlike `rules list --folder`, which also accepts a
path) and is repeatable; a UID no folder carries is an error. Contact points and the policy tree
are org-wide and are pulled whatever it is set to.

`alerts pull` also takes `--max-concurrent` (default 10), which bounds the rule-group fetches the
same way it bounds a push's writes.

### Review the plan first

```shell
grafanapi alerts push -p ./alerting --dry-run
```

```
ACTION     KIND          ID                        DETAIL
Update     ContactPoint  cp-uid-1
Unchanged  PolicyTree    policies
Update     RuleGroup     folder-uid-1/probe-group  ~ interval 60s→120s
                                                     + disk-almost-full (rule-uid-2)
                                                     - old-rule (rule-uid-3)
Create     RuleGroup     folder-uid-2/latency      + p99-latency (rule-uid-4)
1 to create, 2 to update, 1 unchanged
```

A dry run issues **no writes at all** and exits 0. `Unchanged` entries are listed rather than
hidden, because "nothing to do" is the answer a dry run is most often asked for.

Rules are matched by UID, so moving one within a group file is not reported as a change to that
rule — but it *is* a change to the group: Grafana evaluates a group's rules in the order the group
body lists them, so a file whose rules were only re-ordered still needs the group write, and the
plan says so with a single line naming no rule:

```
Update     RuleGroup     folder-uid-1/probe-group  ~ rule order
```

What a rule is compared against is the body the push would **send**, not the rule as the file spells
it: `folderUID`, `ruleGroup` and `orgID` are filled in from the group being pushed first, so a rule
that leaves them out or disagrees about them is `Unchanged` rather than a `~` line no push could
ever close. Every other field is compared exactly as written.

It does run the read-only checks that decide whether a write is *allowed* — the folder lookup, the
provenance classification, the redacted-secret check — so an entry the apply would refuse is
reported below the plan instead of appearing in it:

```
ACTION     KIND          ID                        DETAIL
Update     ContactPoint  cp-uid-1
Unchanged  PolicyTree    policies
0 to create, 1 to update, 1 unchanged
✘ would fail: Update RuleGroup folder-uid-1/probe-group: alerts: push rule group folder-uid-1/probe-group: provenance "file" is managed outside the API
```

A dry run still exits **0** in that case: it changed nothing, and showing the refusal before the
apply hits it is the point. `--on-error` has no effect on a dry run — stopping at the first refusal
would hide the rest of the plan.

### A rule group is written whole

!!! danger "A rule missing from a group file is deleted from that group"

    Grafana replaces a rule group with exactly what the push sends, so removing a rule from the
    file removes it from Grafana. The dry run prints that as the `- old-rule (rule-uid-3)` line
    above — the one line to read carefully before applying.

Run `alerts pull` again after a push that **added** a rule, before editing the bundle further.
Rules are matched by UID, and a rule you typed into the file by hand has none until Grafana assigns
one on the first push; pushing the same file a second time therefore plans it as both a `+` and a
`-` for the same title and replaces the rule with a fresh copy — losing its UID, its alert state
and its silence history. The plan says so on a line of its own, above the pair it explains:

```
Update     RuleGroup     folder-uid-1/probe-group  ! disk-almost-full (rule-uid-2) would be replaced, not updated; run alerts pull first
                                                     + disk-almost-full
                                                     - disk-almost-full (rule-uid-2)
```

Re-pulling (or writing the assigned `uid` into the file yourself) makes the next plan read
`unchanged`. The match stays by UID rather than falling back to the title: a title is not an
identity, and matching on it would silently update the wrong rule the moment one is renamed.

The reverse is not true: groups, contact points and policies that exist **only in Grafana** are
left alone. This command never prunes remote objects, so a bundle covering one folder is safe to
push against an instance that has twenty.

### Folders must exist first

Folders are never created by a push. A group naming a folder that does not exist fails with

```
folder folder-uid-9 does not exist; push folders first (grafanapi resources push)
```

and the other groups are still pushed. Push the folders with
[`grafanapi resources push`](../reference/cli/grafanapi_resources_push.md) first, then the alerting
bundle.

A folder your user may not *read* is treated the same way — one refusal, not the end of the push:

```
folder folder-uid-9: permission denied
```

Grafana grants `folders:read` per folder, so a bundle spanning folders you only partly own is
expected to report this for the ones you do not. The groups in the folders you can read are still
pushed, and `--dry-run` still prints their plan alongside the `✘ would fail` lines — which is how
you find out which half of a bundle you are allowed to write. A `401` is different: the session
itself is gone, so the push stops there and exits 2.

### Who owns an object: provenance

Grafana records how each rule, contact point and policy tree was created, and enforces the rules
around it. `grafanapi` classifies that value before every write, three ways:

| Stored provenance | What it means | What a push does |
| ----------------- | ------------- | ---------------- |
| empty | editable in the Grafana UI | writes it, leaving it UI-editable |
| `api` | created through the provisioning API | writes it, keeping it API-owned |
| anything else (`file`, `converted_prometheus`, `disabled`, …) | owned by another provisioner | **refuses the write** |

A refused object fails on its own — the rest of the push continues (subject to `--on-error`) —
with

```
alerts: push rule group folder-uid-1/probe-group: provenance "file" is managed outside the API
```

This is not caution for its own sake: an object file provisioning owns would simply be re-created
from its file on the next reload, and Grafana answers some of those writes with a 500 or a 409
anyway. A rule group whose rules disagree with each other about provenance is refused the same
way.

For a **new** object, `--ui-editable` picks which side it lands on: by default a push claims what
it creates for the API (`provenance: api`, matching what the UI shows as "provisioned"), and
`--ui-editable` leaves it editable in the UI instead. It applies to **creates only** — an update
always mirrors the provenance the object already carries, whatever the flag says.

### Secrets: what is redacted, and what is not

Grafana replaces the fields a contact-point type **declares secure** with the literal string
`"[REDACTED]"` server-side, so that is what a pull writes to `contact-points.yaml`. **Leave it
there.** Pushing the placeholder back keeps the stored secret; replacing it with a real value sets
a new one. That is what makes pull → edit → push safe for contact points: you can change a channel
name next to a secret you never see.

```yaml
- uid: cp-uid-1
  name: probe-slack
  type: slack
  settings:
    recipient: "#probe-v2"   # edited
    url: "[REDACTED]"        # left alone — the stored secret is kept
```

The plan treats a `"[REDACTED]"` value as unchanged, so it never shows up as a diff, and it never
hides a change to a field next to it — including inside an array, such as a webhook's custom
headers.

!!! warning "Only declared secure fields are redacted — treat the bundle as sensitive"

    A credential Grafana does not treat as a secure field is written to disk **verbatim**: a token
    embedded in a webhook URL path or query string, or in a custom header value, lands in
    `contact-points.yaml` in plain text. The bundle files are created `0600`, but that is the only
    protection they have. Read `contact-points.yaml` before committing a bundle to a repository,
    attaching it to a ticket, or copying it between machines.

#### Creating a contact point needs the real secret

"Push the placeholder back and the stored secret is kept" is a property of an **update**. There is
nothing stored for an object that does not exist yet, so a push that would *create* a contact point
carrying `"[REDACTED]"` is refused rather than sent:

```
alerts: push contact point cp-uid-1: secret field(s) settings.url are redacted; set the real value(s) in the file before creating
```

This is what promoting a bundle from one instance to **another** runs into: over there every contact
point is a create. Fill in the real values for the new contact points first (and keep that copy of
the bundle out of version control), or create them once in the target instance's UI and re-pull.

### Applying, and what the output means

```shell
grafanapi alerts push -p ./alerting
```

```
✔ Update ContactPoint cp-uid-1
✔ Unchanged PolicyTree policies
✔ Update RuleGroup folder-uid-1/probe-group
✔ Create RuleGroup folder-uid-2/latency
✔ 4 change(s) applied, 0 error(s)
```

Unlike [`resources push`](../reference/cli/grafanapi_resources_push.md), which prints a single
summary line, `alerts push` prints **one line per change**. A bundle holds few objects and each one
has a large blast radius — a rule group covers every rule in it — so the intent is that the output
is short enough to read in full.

`--on-error` decides what a failed change does, exactly as on `resources push`:

- `ignore` — attempt every change, exit **0** even if some failed
- `fail` (default) — attempt every change, exit **1** if any failed
- `abort` — stop at the first failure and exit **1**. Under `abort` the rule groups are written one
  at a time, so "stop" means what it says; otherwise they are written concurrently, bounded by
  `--max-concurrent` (default 10).

Failures are printed after the successes, so the thing to act on is the last thing on screen, and
the exit status carries the *first* failure's own error — a `403` still renders as the
permission-denied message and a `401` still exits 2.

!!! warning "No concurrency control: the last writer wins"

    Grafana's provisioning API has no ETag, version or `If-Match` on any of these objects. If
    someone edits a rule in the UI between your `pull` and your `push`, the push silently
    overwrites their edit — and if two pushes race, whichever finishes last is what survives. A
    `--dry-run` immediately before applying narrows the window; it does not close it.

## Pause, resume, delete

Individual rules are changed in place by UID, without a bundle:

```shell
grafanapi alerts rules pause  rule-uid-1 rule-uid-2
grafanapi alerts rules resume rule-uid-1
grafanapi alerts rules delete rule-uid-3 --dry-run
```

The UIDs are the ones the `UID` column of
[`alerts rules list`](../reference/cli/grafanapi_alerts_rules_list.md) prints, and its `PAUSED`
column shows the result.

```
✔ rule-uid-1 (probe-rule in probe-group) paused
✔ rule-uid-2 (disk-almost-full in probe-group) paused
✔ 2 rule(s) paused, 0 unchanged, 0 error(s)
```

A rule already in the state you asked for is reported as `already paused` (or `not paused` for a
resume) and its group is **not** rewritten. Under `--dry-run` every outcome is prefixed
`would be`, in the summary line too.

!!! note "Pausing rewrites the rule's whole group"

    Grafana has no endpoint for pausing a single rule: on 12.3.x a single-rule update that only
    flips `isPaused` returns `200` but does not persist for an Editor. So `pause`/`resume` flip the
    flag and write the whole rule group back, sending every *other* rule in it exactly as Grafana
    reported it. Naming several rules of one group is therefore a single write, and a group owned
    by another provisioner — or whose rules disagree about provenance — is refused rather than
    claimed, as on a push.

`delete` fetches each rule first, so a UID no rule carries is a clean "not found" rather than the
`500` Grafana answers a `DELETE` for an unknown UID with, and the other named rules are still
deleted:

```
✔ rule-uid-1 (probe-rule in probe-group) deleted
✘ rule-uid-9 alerts: rule rule-uid-9 not found
⚠ 1 rule(s) deleted, 0 unchanged, 1 error(s)
```

There is no `--force`: every rule these commands touch is named on the command line, so there is no
"delete everything matching" case to guard. Run `--dry-run` to see the list first.

### Exit codes

| Exit | Meaning |
| ---- | ------- |
| 0 | everything succeeded (also the whole of a `--dry-run`, and of a push run with `--on-error ignore`) |
| 1 | something was refused or not found: a `403`, a validation error, an unknown rule UID, a missing or unreadable folder, foreign provenance |
| 2 | the session cookie is stale or unauthorized — see [Stale sessions](#stale-sessions) |

## Stale sessions

If your session cookie has expired, every command in the group fails the same way every other
authenticated command does: a "Grafana session is stale or unauthorized" error suggesting
`grafanapi login update`, and exit code **2** (distinct from the generic exit code 1). This holds
for a `401` from either endpoint `rules list` calls, and from any call a write makes — a `401`
mid-push ends the push rather than being recorded as one more failed change, since nothing after it
could succeed.

A `403` is different and is *not* exit 2: re-authenticating cannot help a session whose user lacks
the Editor role. It renders as "Grafana denied the request: your user lacks the required
permission" and exits **1**.

See [`grafanapi login`](../reference/cli/grafanapi_login.md) and
[`grafanapi login update`](../reference/cli/grafanapi_login_update.md) for authenticating, or the
[configuration guide](../configuration.md) for how the session cookie is resolved.

## Scope

The group covers alert rules, contact points and the notification policy tree. Silences, mute
timings, message templates and alert-rule history are not exposed — use the Grafana UI or the
provisioning API for those.

Within what it does cover, five limits are worth knowing before you rely on it:

- **No pruning.** A push adds and replaces; it never deletes an object the bundle omits. Delete a
  rule by removing it from its group file (which *is* a deletion, see
  [A rule group is written whole](#a-rule-group-is-written-whole)), or a rule group in the Grafana
  UI.
- **No concurrency control.** The last writer wins, always.
- **Nothing org-admin.** No Alertmanager reset, no decrypted export, no contact-point deletion, no
  data-source or mute-timing management.
- **Objects another provisioner owns are refused**, not overwritten — an instance managed by file
  provisioning cannot be managed with this group.
- **A field the client's model does not know is dropped.** Bundles are decoded into the API models
  the client was generated from, so a rule field a newer Grafana added — `metadata.editor_settings`
  is the one to watch — is not written to the file on pull, and a push of that file therefore sends
  the object without it. Grafana applies exactly the body it is sent, so the field is reset to its
  default on the objects you push. Values *inside* a known field are not affected: a
  `noDataState: KeepLast` or a contact point of a type this client predates round-trips unchanged,
  because the bundle is checked for structure, not against the generated enums. Check the Grafana
  UI after the first push of a rule that relies on such a field.
