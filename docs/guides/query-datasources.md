---
title: Query datasources
---

This section describes how to use the Grafana CLI to run ad-hoc queries against a Grafana
datasource directly from your terminal, mirroring Grafana's Explore UI.

This is useful for sanity-checking a PromQL/LogQL/SQL query, spot-checking a datasource's health,
or wiring a quick check into a CI job — without opening the Grafana UI.

```shell
grafanapi explore <datasource-uid-or-name> "<query>" [flags]
```

## Datasource resolution

`DATASOURCE` is resolved first as a UID, then as a name. If neither matches, the error lists every
configured datasource as `name (uid, type)` so you can copy the right identifier.

```shell
grafanapi explore does-not-exist "up"
```

```
Error: datasource "does-not-exist" not found; available datasources:
  example-postgres (postgres-uid, postgres)
  example-prometheus (prometheus-uid, prometheus)
```

## Query field mapping

The positional `QUERY` argument is mapped onto the request field that the datasource's type
expects:

| Datasource type                                              | Query field | Extra defaults      |
| ------------------------------------------------------------ | ----------- | -------------------- |
| `prometheus`                                                  | `expr`      | —                     |
| `loki`                                                        | `expr`      | —                     |
| `mysql`, `postgres`, `grafana-postgresql-datasource`, `mssql` | `rawSql`    | `format: "table"`     |
| `elasticsearch`                                               | `query`     | —                     |
| `influxdb`                                                    | `query`     | —                     |
| `graphite`                                                    | `target`    | —                     |

An unrecognized datasource type errors out and names the type; use `--field` to specify the
request field explicitly (for example, a datasource type added to Grafana after this table was
written).

```shell
# Prometheus/Loki
grafanapi explore example-prometheus "up"

# SQL — rawSql + format:"table" are set automatically
grafanapi explore example-postgres "select 1 as n"

# Unrecognized type: name the field yourself
grafanapi explore example-custom-ds "some query" --field customField
```

## Flags

- `--field string` — override the type-inferred query field (e.g. `expr`, `rawSql`); required for
  datasource types not in the table above.
- `--param key=value` — repeatable; add or override any request field. Values are parsed as JSON
  when possible (so `5`, `true`, `{"a":1}` become typed), otherwise kept as a plain string. `--param`
  entries win over everything else, so they can override a generated field such as `format`.
- `--from` / `--to` — start/end of the query time range, passed through to Grafana **verbatim**
  (relative strings like `now-6h` or `now` are not parsed client-side). Defaults: `now-1h` / `now`.
- `--interval` — minimum query interval (e.g. `15s`), converted to `intervalMs`. Left unset,
  Grafana chooses the interval itself.
- `--instant` — run an instant query (`instant: true, range: false`) instead of a range query.
- `--max-data-points` — requested query resolution (default `1000`).
- `-o, --output` — `table` (default, human-readable), `json`, or `yaml` for piping to tools like
  `jq`.

```shell
grafanapi explore example-prometheus "rate(http_requests_total[5m])" --from now-6h --instant

grafanapi explore example-prometheus "up" -o json | jq '.results.A.frames[0].schema'
```

## Scope

This runs a **single** query with a fixed `refId` of `"A"`: there is no multi-query support, no
query history, and no streaming. CSV output isn't implemented either — `-o json` covers piping to
other tools today, and is the recommended way to extract specific fields.

## Stale sessions

If your session cookie has expired, `explore` fails the same way every other authenticated command
does: a "Grafana session is stale or unauthorized" error suggesting `grafanapi login update`. See
[`grafanapi login`](../reference/cli/grafanapi_login.md) and
[`grafanapi login update`](../reference/cli/grafanapi_login_update.md) for authenticating, or the
[configuration guide](../configuration.md) for how the session cookie is resolved.
