## grafanapi resources validate

Validate resources

### Synopsis

Validate resources.

This command validates its inputs against a remote Grafana instance.


```
grafanapi resources validate [RESOURCE_SELECTOR]... [flags]
```

### Examples

```

	# Validate all resources in the default directory
	grafanapi resources validate

	# Validate a single resource kind
	grafanapi resources validate dashboards

	# Validate a multiple resource kinds
	grafanapi resources validate dashboards folders

	# Displaying validation results as YAML
	grafanapi resources validate -o yaml

	# Displaying validation results as JSON
	grafanapi resources validate -o json

```

### Options

```
  -h, --help                 help for validate
      --max-concurrent int   Maximum number of concurrent operations (default 10)
      --on-error string      How to handle errors during resource operations:
                               ignore — continue processing all resources and exit 0
                               fail   — continue processing all resources and exit 1 if any failed (default)
                               abort  — stop on the first error and exit 1 (default "fail")
  -o, --output string        Output format. One of: json, text, yaml (default "text")
  -p, --path strings         Paths on disk from which to read the resources. (default [./resources])
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

