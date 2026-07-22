## grafanapi login

Authenticate to a Grafana instance using a session cookie

### Synopsis

Authenticate to a Grafana instance using a browser session cookie (grafana_session).

The cookie is validated against the target server (GET /api/user) before anything is persisted.
It is never accepted as a command-line flag value or environment variable: it is read either from
an interactive, no-echo prompt, or — with --cookie-stdin — from stdin, for scripting and CI. On
success, the context (server, org-id/stack-id, TLS) is written to the configuration file and the
cookie itself is stored in the macOS Keychain, never in the plaintext configuration file.

```
grafanapi login [flags]
```

### Examples

```

	grafanapi login --server https://grafana.example.com
	pbpaste | grafanapi login --server https://grafana.example.com --cookie-stdin
```

### Options

```
      --config string    Path to the configuration file to use
      --context string   Name of the context to create or update (defaults to the current context, or "default")
      --cookie-stdin     Read the session cookie from stdin instead of the interactive prompt (requires --server)
  -h, --help             help for login
      --org-id int       Organization ID, for on-prem Grafana; skips Grafana Cloud stack-id discovery
      --server string    Grafana server URL; skips the interactive server prompt
      --stack-id int     Grafana Cloud stack ID; skips stack-id discovery
```

### Options inherited from parent commands

```
      --no-color        Disable color output
  -v, --verbose count   Verbose mode. Multiple -v options increase the verbosity (maximum: 3).
```

### SEE ALSO

* [grafanapi](grafanapi.md)	 - 
* [grafanapi login update](grafanapi_login_update.md)	 - Refresh a stale Grafana session cookie

