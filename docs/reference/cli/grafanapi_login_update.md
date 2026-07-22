## grafanapi login update

Refresh a stale Grafana session cookie

### Synopsis

Refresh the Grafana session cookie stored for an existing context.

Unlike "grafanapi login", "login update" never re-asks for the server: it loads the current (or
--context-selected) context's server from the configuration file, then reads a new session cookie
— from an interactive, no-echo prompt, or from stdin with --cookie-stdin — validates it against
that stored server (GET /api/user), and — only on success — overwrites the cookie in the macOS
Keychain. The configuration file itself is never modified.

```
grafanapi login update [flags]
```

### Examples

```

	grafanapi login update
	grafanapi login update --context staging
	pbpaste | grafanapi login update --cookie-stdin
```

### Options

```
      --config string    Path to the configuration file to use
      --context string   Name of the context to update (defaults to the current context)
      --cookie-stdin     Read the session cookie from stdin instead of the interactive prompt
  -h, --help             help for update
```

### Options inherited from parent commands

```
      --no-color        Disable color output
  -v, --verbose count   Verbose mode. Multiple -v options increase the verbosity (maximum: 3).
```

### SEE ALSO

* [grafanapi login](grafanapi_login.md)	 - Authenticate to a Grafana instance using a session cookie

