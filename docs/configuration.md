---
title: Configuration
---

# Configuration

Grafana CLI can be configured in two ways: using environment variables or through a configuration file.

Environment variables can only describe a single context, and are best suited to CI environments.

Configuration files can store multiple contexts, providing a convenient way to switch between Grafana instances.

## Authenticating

Grafana CLI authenticates to Grafana using a browser **session cookie**
(`grafana_session`) rather than an API token or basic-auth credentials. Run
`grafanapi login` once per Grafana instance you want to manage:

```shell
grafanapi login --server https://grafana.example.com
```

You will be prompted for the session cookie value at a no-echo prompt. Copy
the value of the `grafana_session` cookie from your browser's developer tools
(Application/Storage → Cookies) after signing in to that Grafana instance, and
paste it in.

For non-interactive shells (CI, scripts) or to avoid retyping the cookie,
pass `--cookie-stdin` to read it from stdin instead — `login` still needs
`--server` in that case, since stdin is no longer available for the server
prompt:

```shell
pbpaste | grafanapi login --server https://grafana.example.com --cookie-stdin
```

`grafanapi login update` accepts the same flag, and never needs `--server`
since it always reuses the context's existing one:

```shell
pbpaste | grafanapi login update --cookie-stdin
```

The cookie is validated against the target server (`GET /api/user`) before
anything is persisted. On success:

* The non-secret context data (server URL, org-id/stack-id, TLS settings) is
  written to the [configuration file](#configuration-file).
* The cookie itself is stored in the macOS Keychain — **never** in the
  plaintext configuration file, and **never** accepted as a command-line flag
  or environment variable.

If neither `--org-id` nor `--stack-id` is given, `grafanapi login` attempts to
discover the Grafana Cloud stack ID automatically using the entered cookie. If
that discovery finds nothing — the on-prem case, since on-prem Grafana has no
`/bootdata` namespace to discover a stack ID from — `login` falls back to
detecting the organization ID instead, once the cookie has been validated:
it calls `GET /api/org` and persists the returned ID, printing "Detected
organization id N". If that call also fails, `login` defaults to org-id `1`
and prints a warning; pass `--org-id` explicitly to override either behavior.

### Server URL requirements

The session cookie only ever travels to the server you configured, so grafanapi
is strict about that URL:

* **`https://` is required.** Plain `http://` is accepted only for a Grafana
  running on your own machine: `localhost`, any `127.x.x.x` address, or `::1`.
  `localhost` is trusted by name, without a DNS lookup. Anything else is rejected,
  including `http://0.0.0.0`, Docker service names such as
  `http://grafana:3000` and `host.docker.internal`. For a local container,
  publish the port and use `http://localhost:<port>`.
* **No credentials in the URL.** `https://user:password@host` is rejected; use
  `grafanapi login` instead.
* **Internationalised domain names must be written in punycode** (`xn--...`),
  for example `https://xn--bcher-kva.example` rather than
  `https://bücher.example`.
* **Set `server` to the URL Grafana really serves, including any sub-path.**
  grafanapi never follows a redirect to another origin (scheme, host or port)
  and never sends the session cookie anywhere but the configured origin. If
  your Grafana sits behind a proxy that redirects, for example from `http://`
  to `https://` or to a canonical host name, the command fails with "Refused a
  redirect away from the configured Grafana server". Fix it by setting the
  server to the final URL:

  ```shell
  grafanapi config set contexts.<name>.grafana.server https://grafana.example.com
  ```

  A refused redirect while the session is being rotated shows up as a stale
  session (exit code 2) plus a warning line; check `server` before logging in
  again.

!!! warning
    `insecure-skip-verify` is still accepted, but it turns off certificate
    verification: an active attacker on the network path can then read the
    session cookie. Never use it for a remote production host.

### Session binding

A stored session is bound to the server it was issued for. At login,
grafanapi records the server's **origin** (scheme, host and port, with the port
always explicit, for example `https://grafana.example.com:443`) together with
the cookie in the same Keychain item. Every time the cookie is loaded, the
origin of the effective server is compared with the stored one. Only an exact
match sends the cookie. Host names are compared in lower case and a path such
as `/grafana` does not take part, so `https://Grafana.Example.com/grafana/` and
`https://grafana.example.com` are the same origin.

If the origins differ, nothing is sent: the cookie is not loaded at all, and
the command fails with "Session cookie not sent: server does not match its
login" (exit code **1**) naming both origins. This is what happens when
`GRAFANA_SERVER`, a `--config` file, `GRAFANAPI_CONFIG` or an `.envrc` points a
context at a different server than the one it was logged in to. In that case,
in this order:

1. Remove the override: unset `GRAFANA_SERVER` and `GRAFANAPI_CONFIG`, drop
   `--config`, and look for an `.envrc` or a repo-local configuration. The
   context then targets its bound origin again.
2. Only if you really moved the context to another server, and from a shell
   without those overrides, run
   `grafanapi login update --context <name> --rebind`. Without `--rebind`,
   `login update` refuses to move a session to another origin before it asks
   for a cookie.
3. Never paste a session cookie while an unexpected server is configured:
   `login update` sends it to that server to validate it.

For a different Grafana instance, prefer a separate context
(`grafanapi login --context <name> --server ...`) over re-binding.

#### Sessions stored by an older version

Sessions stored before binding existed carry no origin. They are bound on first
use, but only when the server provably comes from your own configuration file,
because binding an unbound session to whatever server is configured would
defeat the check. Binding happens automatically only when **all** of these
hold:

* `--config` is not used and `GRAFANAPI_CONFIG` is unset or empty;
* `XDG_CONFIG_HOME` and `XDG_CONFIG_DIRS` are unset or empty;
* `$HOME` is your account's own home directory;
* `GRAFANA_SERVER` is not set at all (even if empty). Commands that never read
  it, `session refresh` and `login update`, ignore this condition.

The first command that meets those conditions binds the session to the server
in your configuration file and prints one line on stderr, for example
`notice: bound the stored session for context "default" to https://grafana.example.com:443 (first use since upgrading)`.
In any other case nothing is sent and the command fails with "Session cookie
not sent: session is not bound to a server yet" (exit code **2**). Run the
command again from a shell without those overrides, or run
`grafanapi login update --context <name>` from one. Under `--config` or
`GRAFANAPI_CONFIG`, `login update` also needs `--rebind` for an unbound session.

`grafanapi config check` prints a `Session binding:` line for every context:
`bound to <origin>`, `bound to <origin> (migrated from an unbound session)`,
`no stored session`, or the error above followed by the same remedies. Reading
it adds no Keychain prompt beyond the one `config check` already causes.

!!! note

    The Keychain item now holds a small JSON payload (the cookie and its
    origin) instead of the bare cookie, so a script that reads it with
    `security find-generic-password -w` sees that payload. An older grafanapi
    sends such a payload as the cookie, so after a downgrade run
    `grafanapi login update` once.

!!! warning

    Origin binding does not cover everything a configuration file controls. A
    hostile `--config` that keeps the bound origin can still set
    `tls.insecure-skip-verify` or an attacker-controlled `ca-data`, which with a
    network position exposes the cookie. Only use configuration files you trust.

### Automatic session rotation

Grafana session cookies stop authenticating ordinary API calls after a
rotation interval (10 minutes by default on the server), well before the
underlying session actually expires. Grafana CLI handles this transparently:
when a request comes back `401`, the CLI itself calls the session-rotation
endpoint with the current cookie, obtains a fresh one, re-persists it to the
Keychain, and retries the original request once — all without any visible
interruption. In practice this means a single `grafanapi login` is typically
enough for the entire lifetime of the underlying browser session; you should
only need `grafanapi login update` once that session truly ends (explicit
logout, the server's maximum session lifetime, or revocation).

!!! warning "Shared-cookie caveat"

    A `grafana_session` cookie copied from a browser tab that is still open
    is *shared* with that tab. The browser and `grafanapi` will then both try
    to rotate the same underlying session, and each side's rotation
    invalidates the other side's copy of the cookie — causing seemingly
    random "session is stale" failures for whichever side rotates second.

    To avoid this, sign in to Grafana in a **private/incognito window**,
    copy the `grafana_session` cookie from that window, and close the window
    afterwards. That leaves `grafanapi` as the sole party rotating the token.

Once a session cookie eventually goes stale (for example, after signing out in
the browser, once the underlying session expires, or after a shared-cookie
conflict as described above), commands fail with a "session is stale" error
and exit with status code **2** — distinct from the generic exit code 1 used
for other errors, so CI/CD scripts can branch on it specifically (for example,
to trigger an alert to re-run `grafanapi login update` rather than treating it
as an ordinary failure). Refresh the stored cookie without touching any other
context settings:

```shell
grafanapi login update
```

See the [`grafanapi login`](./reference/cli/grafanapi_login.md) and
[`grafanapi login update`](./reference/cli/grafanapi_login_update.md)
reference pages for the full flag list.

!!! note

    On the first Keychain read after a rebuild of `grafanapi`, macOS prompts
    with an "Allow / Always Allow" dialog — this is expected: the Keychain
    item's access control is bound to the binary that created it. Choose
    "Always Allow" for the binary you intend to keep using.

## Using environment variables

The minimum requirement is to set the URL of the Grafana instance and the organization ID to use:

```shell
GRAFANA_SERVER='http://localhost:3000' GRAFANA_ORG_ID='1' grafanapi config check
```

Environment variables cover the non-secret parts of a context only
(server, org-id, stack-id, and so on). The session cookie is never settable
via an environment variable — authenticate with [`grafanapi login`](#authenticating)
instead, which stores the cookie in the Keychain.

`GRAFANA_SERVER` must name the same origin as the context's login: a stored
session is only sent to the origin it was bound to (see
[Session binding](#session-binding)), so a different server fails with an error
and sends no cookie.

!!! note

    * Every supported environment variable is listed in our [reference documentation](./reference/environment-variables/index.md).
    * Check the [config file reference documentation](./reference/configuration/index.md) for details on all available config options.

## Defining contexts

Grafana CLI supports multiple contexts, thereby allowing easy switching between instances. By default, Grafana CLI uses the `default` context.

The recommended way to create or update a context is `grafanapi login`, which
prompts for the server and cookie together and validates them before
persisting anything:

```shell
grafanapi login --context staging --server https://staging.grafana.example --org-id 1
```

Non-secret fields can also be adjusted directly with `config set`, for example
to change the org-id of an already-authenticated context:

```shell
grafanapi config set contexts.staging.grafana.org-id 2
```

!!! note

    `config set`/`config unset` only manage non-secret fields (server, org-id,
    stack-id, TLS). The session cookie is managed exclusively through
    `grafanapi login` / `grafanapi login update`.

## Configuration file

Grafana CLI stores its configuration in a YAML file. Its location is determined as follows:

1. If the `--config` flag is set, then that file will be loaded. No other location will be considered.
2. If the `$XDG_CONFIG_HOME` environment variable is set, then it will be used: `$XDG_CONFIG_HOME/grafanapi/config.yaml`
3. If the `$HOME environment` variable is set, then it will be used: `$HOME/.config/grafanapi/config.yaml`
4. If the `$XDG_CONFIG_DIRS` environment variable is set, then it will be used: `$XDG_CONFIG_DIRS/grafanapi/config.yaml`

The configuration file never contains the session cookie: it is stored
separately in the macOS Keychain, keyed by context name. The Keychain item holds
the cookie and the origin it is bound to as a small JSON payload.

!!! tip

    The `grafanapi config check` command will display the configuration file currently in use.

## Useful commands

Check the configuration:

```shell
grafanapi config check
```

List existing contexts:

```shell
grafanapi config list-contexts
```

Switch to a different context:

```shell
grafanapi config use-context staging
```

See the entire configuration:

```shell
grafanapi config view
```
