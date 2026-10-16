## grafanapi alerts pull

Pull alerting configuration from Grafana to disk

### Synopsis

Pull the alerting configuration of the current context into a directory on disk.

A bundle holds the rule groups, the contact points and the notification policy tree, laid out as:

	<path>/rules/<folder UID>/<group title>.<yaml|json>
	<path>/contact-points.<yaml|json>
	<path>/policies.<yaml|json>

Path components are sanitized and are NOT authoritative: the folderUid and title recorded inside
each group file are what a later push sends to Grafana. Do not hand-edit orgID - push overwrites
it from the remote group or from the context.

Files under rules/ that this pull did not produce are removed, so a group deleted in Grafana is
not resurrected by the next push. With --folder-uid that pruning is restricted to the named
folders, leaving a partial bundle's other folders untouched.

Secure contact-point settings arrive as "[REDACTED]" and must be left that way: pushing the
placeholder back keeps the stored secret, while replacing it sets a new one.

Reading needs the Viewer role. Pushing the result back needs Editor.

```
grafanapi alerts pull [flags]
```

### Examples

```

	grafanapi alerts pull

	grafanapi alerts pull -p ./alerting -o json

	grafanapi alerts pull --folder-uid folder-uid-1 --folder-uid folder-uid-2
```

### Options

```
      --folder-uid stringArray   Only pull rule groups from this folder, matched against its UID (repeatable). A UID no folder carries is an error; contact points and the notification policy tree are org-wide and are pulled whatever this is set to
  -h, --help                     help for pull
      --max-concurrent int       Maximum number of concurrent operations (default 10)
  -o, --output string            Output format. One of: json, yaml (default "yaml")
  -p, --path string              Path on disk in which the alerting bundle will be written (default "./alerting")
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

