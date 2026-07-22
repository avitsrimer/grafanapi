## grafanapi resources get

Get resources from Grafana

### Synopsis

Get resources from Grafana using a specific format. See examples below for more details.

```
grafanapi resources get [RESOURCE_SELECTOR]... [flags]
```

### Examples

```

	# Everything:

	grafanapi resources get dashboards/foo

	# All instances for a given kind(s):

	grafanapi resources get dashboards
	grafanapi resources get dashboards folders

	# Single resource kind, one or more resource instances:

	grafanapi resources get dashboards/foo
	grafanapi resources get dashboards/foo,bar

	# Single resource kind, long kind format:

	grafanapi resources get dashboard.dashboards/foo
	grafanapi resources get dashboard.dashboards/foo,bar

	# Single resource kind, long kind format with version:

	grafanapi resources get dashboards.v1alpha1.dashboard.grafana.app/foo
	grafanapi resources get dashboards.v1alpha1.dashboard.grafana.app/foo,bar

	# Multiple resource kinds, one or more resource instances:

	grafanapi resources get dashboards/foo folders/qux
	grafanapi resources get dashboards/foo,bar folders/qux,quux

	# Multiple resource kinds, long kind format:

	grafanapi resources get dashboard.dashboards/foo folder.folders/qux
	grafanapi resources get dashboard.dashboards/foo,bar folder.folders/qux,quux

	# Multiple resource kinds, long kind format with version:

	grafanapi resources get dashboards.v1alpha1.dashboard.grafana.app/foo folders.v1alpha1.folder.grafana.app/qux
```

### Options

```
  -h, --help              help for get
      --on-error string   How to handle errors during resource operations:
                            ignore — continue processing all resources and exit 0
                            fail   — continue processing all resources and exit 1 if any failed (default)
                            abort  — stop on the first error and exit 1 (default "fail")
  -o, --output string     Output format. One of: json, text, wide, yaml (default "text")
```

### Options inherited from parent commands

```
      --config string    Path to the configuration file to use
      --context string   Name of the context to use
      --no-color         Disable color output
  -v, --verbose count    Verbose mode. Multiple -v options increase the verbosity (maximum: 3).
```

### SEE ALSO

* [grafanapi resources](grafanapi_resources.md)	 - Manipulate Grafana resources

