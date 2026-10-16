## grafanapi alerts push

Push an alerting configuration from disk to Grafana

### Synopsis

Push the alerting bundle in a directory on disk to the current context.

The bundle is the layout `alerts pull` writes: rule groups under rules/<folder UID>/, the
contact points and the notification policy tree beside them. The folderUid and title recorded
inside each file - not its path - are what is sent to Grafana.

A rule group is written whole: Grafana replaces the group with what the file holds, so a rule
missing from the file is DELETED from that group. Run --dry-run first; it prints exactly that as a
"-" line. Groups, contact points and policies that exist only in Grafana are left alone: this
command never prunes.

Folders are never created. A group naming a folder that does not exist fails; push the folders
first with `grafanapi resources push`.

Objects another provisioner owns (file provisioning, imported Prometheus rules, ...) are refused
rather than overwritten. New objects are claimed for the API unless --ui-editable says otherwise.

Secure contact-point settings must be left as the "[REDACTED]" placeholder a pull wrote: pushing
it back keeps the stored secret, while replacing it sets a new one. A contact point being CREATED
is the exception - there is no stored secret to keep - and is refused until the file carries the
real value, which is what pushing a bundle into a different instance runs into.

Grafana has no optimistic concurrency here: the last writer wins. Pushing needs the Editor role.

```
grafanapi alerts push [flags]
```

### Examples

```

	grafanapi alerts push --dry-run

	grafanapi alerts push

	grafanapi alerts push -p ./alerting --on-error abort
```

### Options

```
      --dry-run              If set, the push is planned and printed, without creating or updating anything in Grafana
  -h, --help                 help for push
      --max-concurrent int   Maximum number of concurrent operations (default 10)
      --on-error string      How to handle errors while processing items:
                               ignore — continue processing all items and exit 0
                               fail   — continue processing all items and exit 1 if any failed (default)
                               abort  — stop on the first error and exit 1 (default "fail")
  -p, --path string          Path on disk from which the alerting bundle will be read (default "./alerting")
      --ui-editable          If set, objects this push CREATES are left editable in the Grafana UI. It has no effect on an update, which always mirrors the provenance the object already carries
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

