## grafanapi session keepalive uninstall

Remove the keep-alive LaunchAgent

### Synopsis

Remove the keep-alive LaunchAgent: boot it out of launchd and delete its plist. Idempotent - it succeeds even when no LaunchAgent is currently installed.

```
grafanapi session keepalive uninstall [flags]
```

### Examples

```

	grafanapi session keepalive uninstall
```

### Options

```
  -h, --help   help for uninstall
```

### Options inherited from parent commands

```
      --config string    Path to the configuration file to use
      --context string   Name of the context to operate on (defaults to the current context)
      --no-color         Disable color output
  -v, --verbose count    Verbose mode. Multiple -v options increase the verbosity (maximum: 3).
```

### SEE ALSO

* [grafanapi session keepalive](grafanapi_session_keepalive.md)	 - Schedule session refresh through a macOS LaunchAgent

