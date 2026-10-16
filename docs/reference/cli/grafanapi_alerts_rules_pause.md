## grafanapi alerts rules pause

Pause alert rules

### Synopsis

Pause the named alert rules, so Grafana stops evaluating them.

Grafana has no endpoint for this: a rule's isPaused flag is flipped by rewriting the whole rule
group it lives in. Naming several rules of one group is therefore a single write, and that group's
other rules are sent back exactly as Grafana reported them.

A rule already in the requested state is reported as such and its group is not rewritten. A group
whose rules are owned by another provisioner - file provisioning, imported Prometheus rules, ... -
or that disagree about their provenance is refused rather than claimed.

Rule UIDs are the ones `alerts rules list` prints in its UID column, and the PAUSED column shows
the result. Run --dry-run first to see what a group write would cover.

Pausing needs the Editor role.

```
grafanapi alerts rules pause UID [UID...] [flags]
```

### Examples

```

	grafanapi alerts rules pause rule-uid-1

	grafanapi alerts rules pause rule-uid-1 rule-uid-2 --dry-run
```

### Options

```
      --dry-run   If set, the operation is planned and printed, without changing anything in Grafana
  -h, --help      help for pause
```

### Options inherited from parent commands

```
      --config string    Path to the configuration file to use
      --context string   Name of the context to use
      --no-color         Disable color output
  -v, --verbose count    Verbose mode. Multiple -v options increase the verbosity (maximum: 3).
```

### SEE ALSO

* [grafanapi alerts rules](grafanapi_alerts_rules.md)	 - Inspect and manage alert rules

