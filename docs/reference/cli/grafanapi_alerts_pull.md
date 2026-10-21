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

With --rule-uid, only the rule group holding each named rule is pulled, and its file holds the
WHOLE group: the file is replaced with the group as Grafana holds it now, so a local edit to a
named rule is discarded - commit or push it first. A file that differs from Grafana in other rules
(local edits, or changes made in Grafana since the last pull) is refused unless --overwrite, which
discards those edits. A file for the group in the other format (-o), or another group's file whose
name clashes, is refused. Editing a sibling rule there is not pushed by push --rule-uid. Contact
points and policies are not pulled, no other file is written or removed (nothing is pruned), and
an unknown uid stops the pull before anything is written.

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

	grafanapi alerts pull --rule-uid rule-uid-1
```

### Options

```
      --folder-uid stringArray   Only pull rule groups from this folder, matched against its UID (repeatable). A UID no folder carries is an error; contact points and the notification policy tree are org-wide and are pulled whatever this is set to
  -h, --help                     help for pull
      --max-concurrent int       Maximum number of concurrent operations (default 10)
  -o, --output string            Output format. One of: json, yaml (default "yaml")
      --overwrite                With --rule-uid, replace a group file even if it differs from Grafana in rules you did not name (those local edits are discarded)
  -p, --path string              Path on disk in which the alerting bundle will be written (default "./alerting")
      --rule-uid stringArray     Only pull the rule group holding this rule, matched against its UID (repeatable). The group's file is replaced with the group as Grafana holds it now, including any local edit to the named rule; contact points and the notification policy tree are not pulled, and no other file is written or removed
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

