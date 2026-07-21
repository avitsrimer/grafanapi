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

The cookie is validated against the target server (`GET /api/user`) before
anything is persisted. On success:

* The non-secret context data (server URL, org-id/stack-id, TLS settings) is
  written to the [configuration file](#configuration-file).
* The cookie itself is stored in the macOS Keychain — **never** in the
  plaintext configuration file, and **never** accepted as a command-line flag
  or environment variable.

If neither `--org-id` nor `--stack-id` is given, `grafanapi login` attempts to
discover the Grafana Cloud stack ID automatically using the entered cookie.

Once a session cookie eventually goes stale (for example, after signing out in
the browser, or once the underlying session expires), commands fail with a
"session is stale" error. Refresh the stored cookie without touching any other
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
separately in the macOS Keychain, keyed by context name.

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
