## grafanapi resources serve

Serve Grafana resources locally

### Synopsis

Serve Grafana resources locally.

The server started by this command makes it easy to explore and review resources
locally.

While resources are loaded from disk, the server will use the Grafana instance
described in the current context to access some data (example: to run queries
when previewing dashboards).

Security: the server listens on 127.0.0.1 by default and prints a URL carrying
a per-run access token. Open that URL once: the token is exchanged for a
session cookie and changes on every run. Requests with a foreign Host header
are rejected (421), requests without the token (401) or from another origin
(403) too. A non-loopback --address prints a warning: the server acts with your
Grafana session and becomes reachable from other machines.

Note on NFS/SMB and watch mode: fsnotify requires support from underlying
OS to work. The current NFS and SMB protocols does not provide network level
support for file notifications.


```
grafanapi resources serve [RESOURCE_DIR]... [flags]
```

### Examples

```

	# Serve resources from a directory:
	grafanapi resources serve ./resources

	# Serve resources from a directory but don't watch for changes:
	grafanapi resources serve ./resources --no-watch

	# Expose the server to the network (it acts with your Grafana session, so
	# anyone who obtains the token can use that session):
	grafanapi resources serve ./resources --address 0.0.0.0

	# Serve resources from a script that outputs a YAML resource and watch for changes:
	# Note: the Grafana Foundation SDK can be used to generate dashboards (https://grafana.github.io/grafana-foundation-sdk/)
	grafanapi resources serve --script 'go run dashboard-generator/*.go' --watch ./dashboard-generator --script-format yaml

```

### Options

```
      --address string         Address to bind (loopback by default; any other address exposes the server, which acts with your Grafana session, to the network) (default "127.0.0.1")
  -h, --help                   help for serve
      --max-concurrent int     Maximum number of concurrent operations (default 10)
      --no-watch               Do not watch for changes
      --port int               Port on which the server will listen (default 8080)
  -S, --script string          Script to execute to generate a resource
  -f, --script-format string   Format of the data returned by the script (default "json")
  -w, --watch stringArray      Paths to watch for changes
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

