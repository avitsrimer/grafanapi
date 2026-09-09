## grafanapi alerts

Inspect Grafana alert rules and contact points

### Synopsis

Inspect Grafana Unified Alerting.

Alert rules are listed with both their definition and their live evaluation state; contact points
are listed as the server reports them, with their declared secure fields already redacted
server-side (a credential embedded in a URL or a custom header is not).

Every command in this group is read-only.

### Options

```
      --config string    Path to the configuration file to use
      --context string   Name of the context to use
  -h, --help             help for alerts
```

### Options inherited from parent commands

```
      --no-color        Disable color output
  -v, --verbose count   Verbose mode. Multiple -v options increase the verbosity (maximum: 3).
```

### SEE ALSO

* [grafanapi](grafanapi.md)	 - 
* [grafanapi alerts contact-points](grafanapi_alerts_contact-points.md)	 - Inspect notification contact points
* [grafanapi alerts rules](grafanapi_alerts_rules.md)	 - Inspect alert rules

