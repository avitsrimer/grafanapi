## grafanapi alerts rules delete

Delete alert rules

### Synopsis

Delete the named alert rules.

Every rule is fetched before it is deleted, so a UID no rule carries is reported as "not found"
rather than as the 500 Grafana answers a DELETE for an unknown UID with. The other named rules are
still deleted.

A rule another provisioner owns - file provisioning, imported Prometheus rules, ... - is refused
rather than deleted: its owner would re-create it on its next run.

Rule UIDs are the ones `alerts rules list` prints in its UID column. There is no --force, since every
rule this command deletes is named on the command line; run --dry-run first to see the list.

Deleting needs the Editor role.

```
grafanapi alerts rules delete UID [UID...] [flags]
```

### Examples

```

	grafanapi alerts rules delete rule-uid-1

	grafanapi alerts rules delete rule-uid-1 rule-uid-2 --dry-run
```

### Options

```
      --dry-run   If set, the operation is planned and printed, without changing anything in Grafana
  -h, --help      help for delete
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

