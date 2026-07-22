## grafanapi datasources

List the datasources configured on the current context

### Synopsis

List every datasource configured on the current context's Grafana instance.

```
grafanapi datasources [flags]
```

### Examples

```

	grafanapi datasources

	grafanapi datasources -o json | jq '.[].uid'
```

### Options

```
      --config string    Path to the configuration file to use
      --context string   Name of the context to use
  -h, --help             help for datasources
  -o, --output string    Output format. One of: json, table, yaml (default "table")
```

### Options inherited from parent commands

```
      --no-color        Disable color output
  -v, --verbose count   Verbose mode. Multiple -v options increase the verbosity (maximum: 3).
```

### SEE ALSO

* [grafanapi](grafanapi.md)	 - 

