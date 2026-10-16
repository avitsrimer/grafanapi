## grafanapi alerts contact-points list

List notification contact points

### Synopsis

List every notification contact point visible to the current context's session.

Contact points are org-wide, not folder-scoped.

Grafana replaces the secure fields of a contact point's settings - the fields its type declares as
secret, such as a Slack token - with "[REDACTED]" server-side. A credential embedded somewhere
else, for example inside a webhook URL or a custom header value, is NOT a secure field and is
returned verbatim, so check the settings before pasting them anywhere. Settings are shown by the
json and yaml output formats only; the table shows NAME, TYPE, UID and PROVENANCE.

An empty PROVENANCE means the contact point is editable in the UI; "api" means it was created
through the provisioning API.

```
grafanapi alerts contact-points list [flags]
```

### Examples

```

	grafanapi alerts contact-points list

	grafanapi alerts contact-points list -o json | jq '.[] | {name, type}'
```

### Options

```
  -h, --help            help for list
  -o, --output string   Output format. One of: json, table, yaml (default "table")
```

### Options inherited from parent commands

```
      --config string    Path to the configuration file to use
      --context string   Name of the context to use
      --no-color         Disable color output
  -v, --verbose count    Verbose mode. Multiple -v options increase the verbosity (maximum: 3).
```

### SEE ALSO

* [grafanapi alerts contact-points](grafanapi_alerts_contact-points.md)	 - Inspect notification contact points

