## grafanapi explore

Run an ad-hoc query against a Grafana datasource

### Synopsis

Run a single ad-hoc query against a Grafana datasource and print the result, mirroring
Grafana's Explore UI.

DATASOURCE is resolved first as a UID, then as a name. QUERY is mapped onto the
datasource-type-appropriate request field: "expr" for Prometheus/Loki, "rawSql" for SQL
datasources, "target" for Graphite, "query" for Elasticsearch/InfluxDB (override with --field).

This runs a single query (fixed refId "A"); there is no multi-query or query-history support.

```
grafanapi explore DATASOURCE QUERY [flags]
```

### Examples

```

	# Prometheus/Loki:
	grafanapi explore my-prometheus "up"

	# SQL (rawSql + format:"table" are set automatically):
	grafanapi explore my-postgres "select 1 as n"

	# Pipe JSON output to jq:
	grafanapi explore my-prometheus "up" -o json | jq '.results.A.frames[0].schema'
```

### Options

```
      --config string         Path to the configuration file to use
      --context string        Name of the context to use
      --field string          Query field to use instead of the type-inferred one (e.g. "expr", "rawSql"); required for unrecognized datasource types
      --from string           Start of the query time range, passed through to Grafana verbatim (default "now-1h")
  -h, --help                  help for explore
      --instant               Run an instant query instead of a range query
      --interval string       Minimum query interval, e.g. "15s" (default: let Grafana choose)
      --max-data-points int   Maximum number of data points to request (default 1000)
  -o, --output string         Output format. One of: json, table, yaml (default "table")
      --param stringArray     Additional query parameter as key=value (repeatable); values are parsed as JSON when possible, otherwise kept as a string
      --to string             End of the query time range, passed through to Grafana verbatim (default "now")
```

### Options inherited from parent commands

```
      --no-color        Disable color output
  -v, --verbose count   Verbose mode. Multiple -v options increase the verbosity (maximum: 3).
```

### SEE ALSO

* [grafanapi](grafanapi.md)	 - 

