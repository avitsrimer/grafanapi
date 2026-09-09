---
title: Alerting
---

This section describes how to inspect Grafana Unified Alerting from your terminal: which alert
rules are defined, which of them are firing right now, and which contact points they notify.

Every command in the `alerts` group is **read-only** — nothing in it can create, update or delete
an alert rule or a contact point.

```shell
grafanapi alerts rules list [flags]
grafanapi alerts contact-points list [flags]
```

## Permissions

Both commands only read. On a Grafana with the default basic roles a **Viewer** session is enough
(verified on OSS 12.3.2). With fine-grained RBAC the session needs alert-rule read and
notification read permissions; without them Grafana answers `403` and the command fails rather
than returning a shorter list.

What Grafana *does* filter silently is folder access: alert rules in folders your user cannot read
are simply absent from the response and the command still succeeds. Contact points are **not**
folder-scoped — they are org-wide objects gated by notification permissions, so you either see all
of them or none.

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

Both commands accept `-o table` (the default), `-o json` and `-o yaml`.

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

## Stale sessions

If your session cookie has expired, both commands fail the same way every other authenticated
command does: a "Grafana session is stale or unauthorized" error suggesting
`grafanapi login update`, and exit code **2** (distinct from the generic exit code 1). This holds
for a `401` from either endpoint `rules list` calls.

See [`grafanapi login`](../reference/cli/grafanapi_login.md) and
[`grafanapi login update`](../reference/cli/grafanapi_login_update.md) for authenticating, or the
[configuration guide](../configuration.md) for how the session cookie is resolved.

## Scope

This group is read-only and covers alert rules and contact points only. Notification policies,
silences, mute timings, message templates and alert-rule history are not exposed, and there is no
create/update/delete path — use the Grafana UI or the provisioning API for those.
