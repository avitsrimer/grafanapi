## grafanapi session keepalive

Rotate the session cookie of every logged-in context

### Synopsis

Rotate the Grafana session cookie of every logged-in context and persist the rotated
cookies to the Keychain.

Grafana expires sessions that go unused for its inactive-lifetime window (7 days by default).
Running this command periodically keeps every logged-in session inside that window, so a single
"grafanapi login" keeps working until the server's maximum session lifetime (30 days by default)
or a real logout forces a fresh login.

Contexts that never completed "grafanapi login" are reported and skipped.

With --install-agent, instead of rotating now, install a launchd agent that runs
"grafanapi session keepalive" daily (--at, default 09:00) so sessions stay alive hands-off.

```
grafanapi session keepalive [flags]
```

### Examples

```

	grafanapi session keepalive
	grafanapi session keepalive --context staging
	grafanapi session keepalive --install-agent --at 07:30
```

### Options

```
      --at string                         Time of day (HH:MM, 24h) the launchd agent runs at; only valid with --install-agent (default "09:00")
      --config string                     Path to the configuration file to use
      --context string                    Name of the context to use
  -h, --help                              help for keepalive
      --install-agent session keepalive   Install a launchd agent that runs session keepalive daily, instead of running it now
      --to string                         Folder to write the launchd agent plist to (default ~/Library/LaunchAgents); only valid with --install-agent
```

### Options inherited from parent commands

```
      --no-color        Disable color output
  -v, --verbose count   Verbose mode. Multiple -v options increase the verbosity (maximum: 3).
```

### SEE ALSO

* [grafanapi session](grafanapi_session.md)	 - Manage Grafana login sessions across contexts

