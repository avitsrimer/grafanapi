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

## Documentation

See [the documentation](https://grafana.github.io/grafanapi/) to learn how to
install, configure and use the Grafana CLI.

## Maturity

> [!WARNING]
> **This repository is currently *in public preview*, which means that it is still under active development.**
> Bugs and issues are handled solely by Engineering teams. On-call support or SLAs are not available.

This project should be considered as "public preview". While it is used by Grafana Labs, it is still under active development.

Additional information can be found in [Release life cycle for Grafana Labs](https://grafana.com/docs/release-life-cycle/).

## Contributing

See our [contributing guide](CONTRIBUTING.md).
