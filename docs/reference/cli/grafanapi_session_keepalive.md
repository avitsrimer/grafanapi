## grafanapi session keepalive

Schedule session refresh through a macOS LaunchAgent

### Synopsis

Schedule "session refresh --due" through a macOS launchd LaunchAgent, so contexts that
opt in via the "live-window" configuration field stay warm without any manual action.

### Options

```
  -h, --help   help for keepalive
```

### Options inherited from parent commands

```
      --config string    Path to the configuration file to use
      --context string   Name of the context to operate on (defaults to the current context)
      --no-color         Disable color output
  -v, --verbose count    Verbose mode. Multiple -v options increase the verbosity (maximum: 3).
```

### SEE ALSO

* [grafanapi session](grafanapi_session.md)	 - Manage Grafana session keep-alive
* [grafanapi session keepalive install](grafanapi_session_keepalive_install.md)	 - Install the keep-alive LaunchAgent
* [grafanapi session keepalive status](grafanapi_session_keepalive_status.md)	 - Show the keep-alive LaunchAgent's status
* [grafanapi session keepalive uninstall](grafanapi_session_keepalive_uninstall.md)	 - Remove the keep-alive LaunchAgent

