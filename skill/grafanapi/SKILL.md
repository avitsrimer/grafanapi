---
name: grafanapi
description: >-
  Query Grafana datasources and manage Grafana resources as code from the terminal with the
  grafanapi CLI. Use when the user wants to run a PromQL, LogQL, SQL, or ad-hoc query against a
  Grafana datasource, sanity-check a metric or log query, list Grafana datasources, pull or push
  dashboards/folders as code, back up or migrate Grafana resources between environments, or manage
  Grafana login sessions and contexts (multi-environment) from the command line.
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
(`datasources`, `explore`, `resources ...`) shares one exit-code contract:

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

## 5. Dashboards as code (`resources`)

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
  feature toggle on the server).

## 6. Contexts (multi-environment)

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

## 7. Exit codes & troubleshooting

- **Exit 2** = stale/unauthorized session → user runs `grafanapi login update` (or
  `pbpaste | grafanapi login update --cookie-stdin`). Distinct from generic exit 1.
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
grafanapi resources pull -p ./backup --context prod                       # back up an environment
grafanapi resources push -p ./resources --dry-run                         # preview a push
```
