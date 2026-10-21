## grafanapi alerts rules get

Show one alert rule's full definition and evaluation state

### Synopsis

Show one alert rule: its full definition (queries, condition, thresholds) and its live
evaluation state.

The output has two keys. "rule" is the same summary `alerts rules list` prints, state included.
"definition" is the rule as the provisioning API holds it, through the projection `alerts pull`
writes into a group file, so it reads exactly as the rule does in a pulled bundle (without the
read-only id, updated and provenance keys). `alerts rules list -o json` carries no queries, which
is the gap this command closes.

Like an unfiltered list, a failure of the state endpoint is not fatal: the command warns on stderr
and reports state "unavailable". An unknown UID fails with "Alert rule not found".

Needs the Viewer role. Output is yaml by default; json is the other format.

```
grafanapi alerts rules get UID [flags]
```

### Examples

```

	grafanapi alerts rules get rule-uid-1

	grafanapi alerts rules get rule-uid-1 -o json | jq '.definition.data'
```

### Options

```
  -h, --help            help for get
  -o, --output string   Output format. One of: json, yaml (default "yaml")
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

