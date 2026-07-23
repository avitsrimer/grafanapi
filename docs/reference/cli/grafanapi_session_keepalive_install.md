## grafanapi session keepalive install

Install the keep-alive LaunchAgent

### Synopsis

Install the keep-alive LaunchAgent: a macOS launchd LaunchAgent that periodically runs
"session refresh --due", so every context whose "live-window" is set stays warm without any manual
action.

At least one context must have "live-window" set (grafanapi config set contexts.<name>.grafana.
live-window 12h) before this command will install anything.

By default the agent's wake interval is derived from the minimum live-window across every opted-in
context (min/2, clamped to [15m, 12h]) - "session refresh --due" re-checks each context's own window
on every wake, so a modest, derived cadence is sufficient. --interval overrides that derivation
(validated against [1m, 6d], never clamped to the narrower derived range).

Installing is idempotent: running it again (e.g. after adding another opted-in context) replaces
the previous plist and reloads it.

```
grafanapi session keepalive install [flags]
```

### Examples

```

	grafanapi session keepalive install
	grafanapi session keepalive install --interval 6h
```

### Options

```
  -h, --help                help for install
      --interval duration   How often the LaunchAgent wakes to check for due contexts (default: derived from the minimum live-window, min/2 clamped to [15m, 12h])
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

