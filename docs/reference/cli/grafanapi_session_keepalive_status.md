## grafanapi session keepalive status

Show the keep-alive LaunchAgent's status

### Synopsis

Show whether the keep-alive LaunchAgent is installed and loaded, its interval and target binary, and a tail of its log (which never contains a session cookie).

```
grafanapi session keepalive status [flags]
```

### Examples

```

	grafanapi session keepalive status
	grafanapi session keepalive status -o json
```

### Options

```
  -h, --help            help for status
  -o, --output string   Output format. One of: json, text, yaml (default "text")
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

