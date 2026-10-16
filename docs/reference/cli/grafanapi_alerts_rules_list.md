## grafanapi alerts rules list

List alert rules and their evaluation state

### Synopsis

List every alert rule visible to the current context's session.

Each rule's definition (from the provisioning API) is joined with its live evaluation state (from
Grafana's prometheus-compatible rules endpoint) on the rule UID, so a single command answers both
"what is defined" and "what is firing".

STATE reads "recording" for recording rules, which have no evaluation state by nature, "unknown"
for an alerting rule Grafana has not evaluated yet, and "unavailable" when the rule's state could
not be fetched at all. ALERTS counts the rule's alert instances that are not Normal.

FOLDER is the folder's full path, so a nested folder reads "Parent/Child". --folder accepts the
folder UID, that full path, or the path's last segment - which matches every folder with that
title, so two folders sharing a leaf title are selected together.

Folder paths and evaluation states both come from the state endpoint. When it is unavailable an
unfiltered list still succeeds: it warns on stderr and reports every alerting rule with STATE
"unavailable" and an empty FOLDER, so automation reading the output can detect the degraded run
rather than mistaking it for an empty result. A --folder value, or a --state value other than
"unavailable" or "recording", fails instead - filtering on data that could not be fetched would
report an empty list as a success.

```
grafanapi alerts rules list [flags]
```

### Examples

```

	grafanapi alerts rules list

	grafanapi alerts rules list --folder "Probe Folder" --state firing

	grafanapi alerts rules list -o json | jq '.[] | select(.state == "firing") | .title'
```

### Options

```
      --folder string   Only list rules in this folder, matched against its UID, its full path ("Parent/Child") or that path's last segment, which matches every folder with that title (exact, case-sensitive)
  -h, --help            help for list
  -o, --output string   Output format. One of: json, table, yaml (default "table")
      --state string    Only list rules in this evaluation state. One of: firing, pending, recovering, inactive, unknown, recording, unavailable ("normal" is accepted as an alias of "inactive")
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

