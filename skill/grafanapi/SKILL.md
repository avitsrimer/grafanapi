---
name: grafanapi
description: >-
  Query Grafana datasources and manage Grafana resources as code from the terminal with the
  grafanapi CLI. Use when the user wants to run a PromQL, LogQL, SQL, or ad-hoc query against a
  Grafana datasource, sanity-check a metric or log query, list Grafana datasources, check which
  Grafana alert rules are defined or firing, list notification contact points, pull or push Grafana
  alerting configuration (alert rules, contact points, notification policies) as code, pause,
  resume or delete alert rules, pull or push dashboards/folders as code, back up or migrate Grafana
  resources between environments, or manage Grafana login sessions and contexts
  (multi-environment) from the command line.
---

# grafanapi

`grafanapi` is a kubectl-style CLI for querying Grafana datasources and managing Grafana resources
(dashboards, folders, etc.) as code. Grafana 12+, macOS only. Auth is a browser `grafana_session`
cookie stored in the macOS Keychain — never a token, never plaintext.

## 1. Prerequisites & auth

Start with a human-readable status check:

```shell
grafanapi config check
```

This always exits 0 — it's a diagnostic report (config file, current context, session cookie,
connectivity, Grafana version), not a pass/fail gate. Read its output for a warning/error line
(e.g. `Session cookie: ⚠ ...`, `Connectivity: ✘ ...`) before trusting the context.

The actual pass/fail signal comes from running real work: every authenticated command
(`datasources`, `explore`, `alerts ...`, `resources ...`) shares one exit-code contract:

- Exit 0 → command succeeded, session was valid.
- Exit 2 → session is stale/unauthorized (a `401` from Grafana). Tell the user to refresh it
  themselves:
  - Interactive TTY: `grafanapi login update`
  - Non-interactive: `pbpaste | grafanapi login update --cookie-stdin`

Never ask the user to paste a cookie into the chat, and never accept it as a flag or env var — the
cookie is only read from a no-echo prompt or stdin. Sessions auto-rotate on every `401`, so a single
`login` usually lasts the whole browser session; `login update` is needed only after a real logout,
expiry, or shared-cookie conflict. First run of a freshly built binary triggers a macOS Keychain
"Allow / Always Allow" dialog — expected; choose "Always Allow".

## 2. Discover datasources

Before querying, list what exists on the current context:

```shell
grafanapi datasources
# NAME                UID              TYPE        DEFAULT
# example-postgres    postgres-uid     postgres    false
# example-prometheus  prometheus-uid   prometheus  true

grafanapi datasources -o json | jq '.[].uid'
```

Use either the UID or the NAME as the datasource argument in `explore`.

## 3. Run queries (`explore`)

Single ad-hoc query, fixed `refId` "A" — no multi-query, history, or streaming.

```shell
grafanapi explore <ds-uid-or-name> "<query>" [--from now-1h] [--to now]
```

The datasource arg resolves as UID first, then name; an unknown one errors and lists every
datasource as `name (uid, type)`.

### Query field mapping (type → request field, set automatically)

| Datasource type                                        | Field    | Extra defaults    |
| ------------------------------------------------------ | -------- | ----------------- |
| `prometheus`, `loki`                                   | `expr`   | —                 |
| `mysql`, `postgres`, `grafana-postgresql-datasource`, `mssql` | `rawSql` | `format: "table"` |
| `elasticsearch`, `influxdb`                            | `query`  | —                 |
| `graphite`                                             | `target` | —                 |

**CRITICAL — plugin-suffixed / unlisted types are NOT auto-mapped.** Types like
`grafana-amazonprometheus-datasource`, `grafana-sentry-datasource`, `redis-datasource`, and `tempo`
error out ("unsupported datasource type") because they are not in the table. Supply the field
explicitly with `--field`:

- Prometheus-compatible backends (Amazon Managed Prometheus, Cortex, Mimir, Thanos) → `--field expr`
- Others → `--field query` (or whatever field that backend expects)

```shell
# Prometheus / Loki
grafanapi explore example-prometheus "up"
grafanapi explore example-loki '{app="api"} |= "error"'

# SQL — rawSql + format:table applied automatically
grafanapi explore example-postgres "select 1 as n"

# Amazon Managed Prometheus (plugin type, not auto-mapped)
grafanapi explore example-amp "up" --field expr

# Instant (point-in-time) PromQL over a wider window
grafanapi explore example-prometheus "rate(http_requests_total[5m])" --from now-6h --instant
```

Key flags:
- `--from` / `--to` — time range, passed to Grafana **verbatim** (`now-6h`, `now`; not parsed
  client-side). Defaults `now-1h` / `now`.
- `--instant` — instant query instead of range.
- `--field` — override the query field; required for unlisted/plugin types.
- `--param key=value` — repeatable; add or override any request field. Values parse as JSON when
  possible (`5`, `true`, `{"a":1}`), else kept as string. `--param` wins over everything, including
  generated fields like `format`.
- `--interval 15s`, `--max-data-points 1000` — resolution controls.
- `-o json|yaml|table` — output format (`table` default).

## 4. Read results

**Table (default):** one block per frame, headed by `# A` (the refId), with column headers, RFC3339
timestamps, and label sets shown per series. Good for eyeballing.

**JSON (`-o json`)** — machine-readable, pipe to `jq`:

```shell
grafanapi explore example-prometheus "up" -o json | jq '.results.A.frames[0].schema'
grafanapi explore example-prometheus "up" -o json | jq '.results.A.frames[0].data'
```

Structure: `.results.<refId>.frames[]`, each frame having `.schema` (field names/types) and `.data`
(column-oriented values). refId is always `A`.

## 5. Alerting (`alerts`)

Reading needs Viewer, writing needs Editor. No command here performs an org-admin operation.

```shell
# read
grafanapi alerts rules list [--folder UID|PATH|LAST-SEGMENT] [--state STATE] [-o table|json|yaml]
grafanapi alerts contact-points list [-o table|json|yaml]

# write (see §5.1 — always --dry-run first)
grafanapi alerts pull [-p ./alerting] [--folder-uid UID]... [-o yaml|json]
grafanapi alerts push [-p ./alerting] [--dry-run] [--ui-editable] [--on-error ignore|fail|abort]
grafanapi alerts rules pause|resume|delete UID [UID...] [--dry-run]
```

`rules list` joins rule **definitions** (provisioning API) with **live evaluation state**
(Grafana's prometheus-compatible rules endpoint) on the rule UID, so one call answers both "what is
defined" and "what is firing":

```
FOLDER        GROUP        TITLE          UID         TYPE      STATE     HEALTH  FOR  PAUSED  ALERTS
Probe Folder  probe-group  probe-rule     rule-uid-1  alerting  firing    ok      1m   false   1
Probe Folder  probe-group  probe-rule-ui  rule-uid-2  alerting  inactive  ok      1m   false   0
```

- `STATE` is one of `firing`, `pending`, `recovering`, `inactive`, `recording`, `unknown`,
  `unavailable`. `inactive` is what the UI calls "Normal". `recording` is assigned to every
  recording rule (they have no evaluation state), `unknown` to an alerting rule Grafana has not
  evaluated yet, and `unavailable` when the state view could not be read at all — so a recording
  rule is never reported as either. `unavailable` says nothing about the rule: it means the state
  half of **every** row is missing (see degraded mode below).
- `ALERTS` counts the rule's alert instances that are **not** `Normal`; the response includes
  `Normal` instances, so it is not the length of the instance list. An instance Grafana reports as
  `Normal (Error)`, `Normal (KeepLast)` etc. is still `Normal` and does not count.
- `FOR` is human-formatted (`1m`, not `1m0s`; `0s` when unset). `FOLDER` is the folder's **full
  path** — a nested folder reads `Parent/Child`, not `Child`.
- `--state` is trimmed/lowercased (`--state Firing` works) and accepts `normal` as an alias of
  `inactive`; an unknown value errors before any network call. `--folder` matches the folder UID,
  the full path, **or** the path's last segment — exactly and case-sensitively. Naming a parent
  folder does not match rules in its children; naming a last segment matches the folder *title*, so
  it spans every folder with that title (`Reports` selects both `A/Reports` and `B/Reports`).

`contact-points list` shows `NAME TYPE UID PROVENANCE`. Contact points are **org-wide**, not
folder-scoped. Empty `PROVENANCE` = UI-editable, `api` = created through the provisioning API.
Grafana redacts each type's **declared secure fields** (a Slack token, …) **server-side** as
`"[REDACTED]"`, but a credential embedded in a webhook URL or a custom header is *not* a secure
field and comes back verbatim — check `settings` (json/yaml only) before pasting it anywhere.

```shell
grafanapi alerts rules list --folder "Probe Folder" --state firing
grafanapi alerts rules list -o json | jq -r '.[] | select(.state == "firing") | .title'
grafanapi alerts contact-points list -o json | jq '.[] | {name, type}'
```

- `json`/`yaml` use the same camelCase keys (`folderUID`, `isPaused`, `activeAlerts`,
  `lastEvaluation`, `disableResolveMessage`) in field-declaration order (neither format sorts
  keys). Empty lists encode as `[]`, never `null`; empty optional keys are omitted.
- **Degraded mode:** if the state endpoint fails with anything but a `401`, an **unfiltered** list
  still succeeds — it warns **on stderr** (so `-o json` stays pipeable) and reports every alerting
  rule with `STATE` `unavailable` and an empty `FOLDER`. **Exit 0 with `unavailable` states means
  the run was degraded**: stderr is invisible to a pipe, so check for that state before trusting an
  empty `select(.state == "firing")` — e.g.
  `jq -e 'any(.[]; .state == "unavailable") | not'`. A **filtered** list fails (exit 1) instead of
  printing `[]`: `--folder` in any form, and every `--state` value except
  `unavailable`/`recording`, would be filtering on data that was never fetched — `--state unknown`
  included, since "no entry for this rule" is a claim only a readable state view can support. Use
  `--state unavailable`, or filter the unfiltered `-o json` on `folderUID`, during an outage. A
  `401` from either endpoint is fatal (exit 2, see §9).
- A **Viewer** session suffices for both commands on a default-roles Grafana; under fine-grained
  RBAC they need alert-rule read and notification read permissions. Alert rules in folders the
  user cannot read are filtered out server-side.

### 5.1 Alerting as code — writes

**SAFETY FIRST. These commands change a live Grafana.**

- **Always run `--dry-run` first** and show the user the plan. Every write command takes it. On
  `alerts push` the dry run also runs the read-only checks, so an entry the apply would refuse is
  printed below the plan as `✘ would fail: …` — read those lines out too; it still exits 0.
- **Never run a non-dry-run write against a real instance without the user reviewing the plan**
  and saying to go ahead. Do not "just apply" because the dry run looked fine.
- Exit codes: **0** success (a `--dry-run` is always 0 when it completes), **1** refused or not
  found (403, validation error, unknown rule UID, missing folder, foreign provenance), **2** stale
  session → `grafanapi login update` (§9). A `403` needs a pending change to surface: a push whose
  plan is entirely `Unchanged` writes nothing and exits **0** even without the Editor role.
- Nothing here prints or decrypts a secret. Do not ask the user for one.

```shell
grafanapi alerts pull -p ./alerting             # Grafana → disk
grafanapi alerts push -p ./alerting --dry-run   # review
grafanapi alerts push -p ./alerting             # disk → Grafana
```

Bundle layout (`-o json` writes the same keys as `.json`):

```
alerting/rules/<folder UID>/<group title>.yaml   # one models.AlertRuleGroup per file
alerting/contact-points.yaml                     # list
alerting/policies.yaml                           # notification policy tree
```

What to tell the user, and what to check before pushing:

- **A rule group is written whole.** A rule missing from a group file is **DELETED** from that
  group; the dry run prints it as a `- title (uid)` line. That line is the one to read carefully.
- **Pull again after a push that added a rule.** Rules are matched by UID, and a rule added by hand
  has none until Grafana assigns one on the first push — so pushing the same file twice plans the
  rule as both a `+` and a `-` for the same title and replaces it with a fresh copy (losing its UID,
  alert state and silence history). The plan flags it with a `! <title> (<uid>) would be replaced,
  not updated; run alerts pull first` line. Re-pull (or write the assigned `uid` into the file)
  before editing further.
- **Nothing remote-only is pruned.** Groups/contact points/policies that exist only in Grafana are
  left alone, so a partial bundle is safe to push.
- **Directory names are not authoritative** — the `folderUid` and `title` *inside* each file are
  what gets sent. Note the two spellings in one file: `folderUid` on the group, `folderUID` on each
  rule. Both are correct; edit the group-level one (a push overwrites each rule's from it).
- **Do not hand-edit `orgID`** — a push overwrites it from the remote group or the context.
- **Secrets:** each contact-point type's **declared** secure fields arrive as `"[REDACTED]"`. Leave
  the placeholder exactly as-is; pushing it back over an **existing** contact point keeps the stored
  secret, replacing it sets a new one. That is what makes editing a channel name next to a secret
  safe. **Creating** a contact point is the exception — nothing is stored yet, so a create carrying
  the marker is refused until the file holds the real value, which is what pushing a bundle into a
  *different* instance hits. Anything Grafana does not classify as a secure field (a token in a
  webhook URL or a custom header) is written to `contact-points.yaml` in **plain text**: treat the
  bundle as sensitive and tell the user before they commit it.
- **Folders are never created.** A group naming a missing folder fails with "push folders first
  (`grafanapi resources push`)"; push folders/dashboards first.
- **Provenance:** an object owned by another provisioner (`file`, `converted_prometheus`, …) is
  **refused**, not overwritten. `--ui-editable` applies to objects the push **creates** only; an
  update always mirrors the stored provenance.
- **No concurrency control** — last writer wins. A UI edit between pull and push is silently
  overwritten.
- `--on-error ignore|fail|abort` (default `fail`) behaves as on `resources push`. Output is **one
  line per change** plus `N change(s) applied, M error(s)` — not `resources push`'s single line.
- A pull **prunes stale files** under `rules/` (restricted to `--folder-uid` when given), so a
  group deleted in Grafana is not resurrected by the next push. `--folder-uid` takes a UID only
  (repeatable); unlike `rules list --folder`, no paths.

Individual rules, by UID (the `UID` column of `rules list`):

```shell
grafanapi alerts rules pause  rule-uid-1 rule-uid-2
grafanapi alerts rules resume rule-uid-1
grafanapi alerts rules delete rule-uid-3 --dry-run
```

- Pausing has no dedicated Grafana endpoint: it **rewrites the rule's whole group**, sending the
  group's other rules back untouched. Several rules of one group = one write.
- A rule already in the requested state is reported (`already paused` / `not paused`) and nothing
  is written. Under `--dry-run` every outcome reads `would be …`.
- `delete` looks each rule up first, so an unknown UID is a clean "not found" (exit 1) and the
  other named rules are still deleted. There is no `--force` — every rule is named on the command
  line.
- Degraded/`unavailable` state (above) applies to `rules list` only; it has no bearing on writes.

## 6. Dashboards as code (`resources`)

```shell
grafanapi resources list                 # available API resource kinds on this Grafana
grafanapi resources get dashboards       # get all dashboards (text; -o json/yaml/wide)
grafanapi resources get dashboards/foo   # a single dashboard by UID
grafanapi resources pull -p ./resources  # Grafana → disk (default ./resources)
grafanapi resources push -p ./resources  # disk → Grafana
```

Selectors: `dashboards`, `dashboards/foo`, `dashboards/foo,bar`, or long form
`dashboards.v1alpha1.dashboard.grafana.app/foo`. Multiple kinds space-separated.

Caveats:
- **Manager metadata:** `grafanapi` only touches resources it manages. Resources created in the UI
  are skipped unless you pass `--include-managed` (on `pull` and `push`).
- **Folders before dashboards:** push orders folders first automatically (dependency ordering) —
  keep both in the pushed set.
- Use `--dry-run` on `push`/`delete` to simulate. `push`/`delete`/`edit` mutate Grafana — confirm
  intent before running; prefer `--dry-run` first.
- `resources validate` checks local manifests against the live Grafana instance.
- `resources serve` previews dashboards locally with live reload (needs the `kubernetesDashboards`
  feature toggle on the server). It listens on loopback only by default: open the tokenized URL it
  prints (the token changes every run).

## 7. Contexts (multi-environment)

```shell
grafanapi config list-contexts
grafanapi config use-context staging
grafanapi login --context staging --server https://grafana.example.com   # create/auth a context
```

- `--context NAME` runs any command against a specific context without switching.
- On-prem org-id is auto-detected via `GET /api/org`; Grafana Cloud stack-id is auto-discovered.
  Override with `--org-id` / `--stack-id`.
- `config set contexts.<name>.grafana.<field> <value>` edits non-secret fields (server, org-id,
  TLS). The cookie is managed only through `login` / `login update`.

## 8. Keep sessions alive (`session`)

By default a session only rotates reactively (on a `401`), so an unused context can eventually
expire. `session` adds proactive, scheduled rotation, opt-in per context via `live-window`:

```shell
grafanapi config set contexts.<name>.grafana.live-window 12h   # opt this context in

grafanapi session refresh                 # force-rotate the current context now (ignores live-window)
grafanapi session refresh --all           # force-rotate every context with a stored cookie
grafanapi session refresh --due           # rotate only contexts whose live-window has elapsed (scheduler entry point)

grafanapi session keepalive install       # schedule "session refresh --due" via a macOS LaunchAgent
grafanapi session keepalive status        # installed? loaded? interval, log tail
grafanapi session keepalive uninstall     # remove the LaunchAgent
```

- `session refresh` exit codes follow the same contract as everything else: exit 2 = the session
  was rejected (dead) → tell the user to run `grafanapi login update`; exit 0 = rotated.
- Contexts with no `live-window` set are never touched by `--due` or `keepalive` — opting in is
  explicit, per context.

**Caveats:**
- The **30-day `login_maximum_lifetime` hard cap** still applies: keepalive extends inactivity
  indefinitely, but a session must be re-established roughly monthly via `grafanapi login update`.
- **Shared-cookie chains can race a browser** using the same `grafana_session`; recommend logging
  in from a private/incognito window so the CLI owns a dedicated cookie chain.
- keepalive **requires the user to be logged into macOS** (GUI-domain LaunchAgent + Keychain
  availability); it does not run while logged out.
- The keepalive **log never contains a cookie value** (only ✔/✘ status lines).

## 9. Exit codes & troubleshooting

- **Exit 2** = stale/unauthorized session → user runs `grafanapi login update` (or
  `pbpaste | grafanapi login update --cookie-stdin`). Distinct from generic exit 1.
- **Exit 1 with "Grafana denied the request: your user lacks the required permission"** = a `403`.
  Re-authenticating does **not** help — the user needs the Editor role for alerting writes, or is
  pointed at the wrong instance (`grafanapi config current-context`).
- "datasource not found" → the error lists every datasource; copy the right name/uid.
- "unsupported datasource type" → add `--field expr` (Prometheus-like) or `--field query`.
- macOS Keychain "Allow" dialog after a binary upgrade → expected, choose "Always Allow".
- macOS only.

## Common one-liners

```shell
grafanapi config check                                                   # diagnostic status report (always exits 0)
grafanapi datasources -o json | jq -r '.[] | "\(.name)\t\(.type)"'        # list ds name+type
grafanapi explore example-prometheus "up" --instant                       # quick point-in-time check
grafanapi explore example-amp "up" --field expr -o json | jq '.results.A.frames[0].data'
grafanapi explore example-postgres "select count(*) from users"           # SQL (auto rawSql)
grafanapi alerts rules list --state firing                                # what is firing right now
grafanapi alerts rules list -o json | jq -r '.[] | "\(.state)\t\(.title)"' # rule state + title
grafanapi alerts contact-points list -o json | jq '.[] | {name, type}'    # who gets notified
grafanapi alerts pull -p ./alerting                                       # alerting config → disk
grafanapi alerts push -p ./alerting --dry-run                             # preview an alerting push
grafanapi alerts rules pause rule-uid-1 --dry-run                         # what pausing would touch
grafanapi resources pull -p ./backup --context prod                       # back up an environment
grafanapi resources push -p ./resources --dry-run                         # preview a push
```
