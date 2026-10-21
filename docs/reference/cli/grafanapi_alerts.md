## grafanapi alerts

Inspect and manage Grafana alert rules, contact points and notification policies

### Synopsis

Inspect and manage Grafana Unified Alerting.

Alert rules are listed with both their definition and their live evaluation state; contact points
are listed as the server reports them, with their declared secure fields already redacted
server-side (a credential embedded in a URL or a custom header is not).

The configuration is also managed as code: `alerts pull` writes it to a directory of files and
`alerts push` sends it back. Individual rules are changed in place with
`alerts rules pause`, `resume` and `delete`, and edited one at a time with
`alerts rules get`, `alerts pull --rule-uid` and `alerts push --rule-uid`.

A rule group is the unit of every rule write: pushing or pausing rewrites the whole group, so a
rule missing from a pushed group file is deleted from it. Objects another provisioner owns (file
provisioning, imported Prometheus rules, ...) are refused rather than overwritten, and Grafana
offers no optimistic concurrency here - the last writer wins. Every write command takes --dry-run;
run it first.

Reading needs the Viewer role and writing needs Editor. No command in this group performs an
org-admin operation.

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
* [grafanapi alerts pull](grafanapi_alerts_pull.md)	 - Pull alerting configuration from Grafana to disk
* [grafanapi alerts push](grafanapi_alerts_push.md)	 - Push an alerting configuration from disk to Grafana
* [grafanapi alerts rules](grafanapi_alerts_rules.md)	 - Inspect and manage alert rules

