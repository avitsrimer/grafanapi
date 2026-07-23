## grafanapi session

Manage Grafana session keep-alive

### Synopsis

Manage Grafana session keep-alive.

grafanapi authenticates to Grafana using a session cookie (see "grafanapi login"), which Grafana
rotates automatically on use and otherwise expires after a period of inactivity. The "session"
command group lets grafanapi keep a context's session alive proactively instead of relying solely
on that reactive, request-triggered rotation: "session refresh" forces a rotation now, and
"session keepalive" schedules that refresh through a macOS LaunchAgent for contexts that opt in
via the "live-window" configuration field.

### Options

```
      --config string    Path to the configuration file to use
      --context string   Name of the context to operate on (defaults to the current context)
  -h, --help             help for session
```

### Options inherited from parent commands

```
      --no-color        Disable color output
  -v, --verbose count   Verbose mode. Multiple -v options increase the verbosity (maximum: 3).
```

### SEE ALSO

* [grafanapi](grafanapi.md)	 - 
* [grafanapi session keepalive](grafanapi_session_keepalive.md)	 - Schedule session refresh through a macOS LaunchAgent
* [grafanapi session refresh](grafanapi_session_refresh.md)	 - Force a Grafana session rotation now

