## grafanapi alerts rules

Inspect and manage alert rules

### Synopsis

Inspect and manage Grafana alert rules.

`list` reports each rule's definition joined with its live evaluation state, and `get` shows one
rule in full, queries and thresholds included. `pause`, `resume` and
`delete` change the named rules in place, addressing them by the UID that listing prints.

Pausing a rule rewrites the whole rule group it lives in, since Grafana has no endpoint for the
flag alone; the group's other rules are sent back untouched. Rules another provisioner owns are
refused rather than overwritten, and all three write commands accept --dry-run.

Listing and getting need the Viewer role; pausing, resuming and deleting need Editor.

### Options

```
  -h, --help   help for rules
```

### Options inherited from parent commands

```
      --config string    Path to the configuration file to use
      --context string   Name of the context to use
      --no-color         Disable color output
  -v, --verbose count    Verbose mode. Multiple -v options increase the verbosity (maximum: 3).
```

### SEE ALSO

* [grafanapi alerts](grafanapi_alerts.md)	 - Inspect and manage Grafana alert rules, contact points and notification policies
* [grafanapi alerts rules delete](grafanapi_alerts_rules_delete.md)	 - Delete alert rules
* [grafanapi alerts rules get](grafanapi_alerts_rules_get.md)	 - Show one alert rule's full definition and evaluation state
* [grafanapi alerts rules list](grafanapi_alerts_rules_list.md)	 - List alert rules and their evaluation state
* [grafanapi alerts rules pause](grafanapi_alerts_rules_pause.md)	 - Pause alert rules
* [grafanapi alerts rules resume](grafanapi_alerts_rules_resume.md)	 - Resume alert rules

