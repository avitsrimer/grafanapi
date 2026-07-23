## grafanapi session refresh

Force a Grafana session rotation now

### Synopsis

Force a Grafana session rotation now, reusing the same rotation and Keychain-persist
path as the automatic 401-triggered rotation.

By default this targets a single context (--context, or the current context). --all targets
every context that has a stored session cookie, unconditionally. --due targets only the contexts
whose "live-window" configuration field has elapsed since their last rotation - this is the
scheduler entry point used by "session keepalive". --all and --due are mutually exclusive with
each other and with --context.

Contexts with no stored session cookie (never logged in) are skipped silently under --all/--due,
and reported as a clear error when targeted explicitly.

```
grafanapi session refresh [flags]
```

### Examples

```

	grafanapi session refresh
	grafanapi session refresh --context staging
	grafanapi session refresh --all
	grafanapi session refresh --due
```

### Options

```
      --all    Refresh every context that has a stored session cookie, unconditionally
      --due    Refresh only contexts whose live-window has elapsed since their last rotation (the scheduler entry point)
  -h, --help   help for refresh
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

