# Grafana CLI

> [!NOTE]
> **This project is a fork of [grafana/grafanactl](https://github.com/grafana/grafanactl), renamed to `grafanapi`.** All credit for the original design and implementation goes to the Grafana Labs team and contributors. This fork is maintained independently of the upstream project.

Grafana CLI (_grafanapi_) is a command-line tool designed to simplify interaction with Grafana instances.

It enables users to authenticate, manage multiple environments, and perform administrative tasks through Grafana's REST API — all from the terminal.

Whether you're automating workflows in CI/CD pipelines or switching between staging and production environments, Grafana CLI provides a flexible and scriptable way to manage your Grafana setup efficiently.

This fork authenticates exclusively via a Grafana **session cookie**
(`grafana_session`), stored in the macOS Keychain, and is distributed for
**macOS (arm64) only**:

```shell
brew install avitsrimer/apps/grafanapi
grafanapi login --server https://grafana.example.com
```

Then, optionally, install the bundled Claude Code skill so agents know how to use `grafanapi`:

```shell
grafanapi install-skill
```

## Example: query a datasource

Run a single ad-hoc query against any configured Grafana datasource, mirroring Grafana's Explore UI:

```shell
# List the datasources configured on the current context
grafanapi datasources

# Prometheus/Loki
grafanapi explore example-prometheus "up"

# SQL (rawSql + format:"table" are set automatically)
grafanapi explore example-postgres "select 1 as n"

# Pipe JSON output to jq
grafanapi explore example-prometheus "up" -o json | jq '.results.A.frames[0].schema'
```

## Example: keep a session alive

Opt a context into scheduled keep-alive, then let a macOS LaunchAgent keep it warm:

```shell
# Opt a context in: keep its session no older than 12h
grafanapi config set contexts.<name>.grafana.live-window 12h

# Force a rotation right now for every context with a stored cookie
grafanapi session refresh --all

# Install a LaunchAgent that runs "session refresh --due" on a schedule
grafanapi session keepalive install
```

See the [keep sessions alive guide](https://avitsrimer.github.io/grafanapi/guides/keep-sessions-alive/)
for the full workflow and caveats.

## Documentation

See [the documentation](https://avitsrimer.github.io/grafanapi/) to learn how to
install, configure and use the Grafana CLI.

## Maturity

> [!WARNING]
> **This fork is under active development and maintained independently, on a best-effort basis.**
> There are no on-call support or SLAs. Bugs and issues are tracked and addressed as time allows.

This project should be considered a personal fork, not an officially supported Grafana Labs
project. See the upstream [`grafanactl`](https://github.com/grafana/grafanactl) project for the
Grafana Labs-maintained tool this fork is based on.

## Contributing

See our [contributing guide](CONTRIBUTING.md).
