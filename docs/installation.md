---
title: Installation
weight: -1
---

# Installation

!!! note
    This fork is distributed for **macOS (Apple Silicon / arm64) only**. The
    session cookie is stored in the macOS Keychain via cgo bindings to
    `Security.framework`, which cannot be cross-compiled for other platforms.

## Homebrew (recommended)

```shell
brew install avitsrimer/apps/grafanapi
```

This installs the darwin/arm64 binary from the
[Homebrew cask](https://github.com/avitsrimer/homebrew-apps) published by the
release pipeline, and automatically clears the Gatekeeper quarantine
attribute so the (unsigned, ad-hoc) binary runs without extra steps.

## Claude Code skill

`grafanapi` bundles a [Claude Code](https://claude.com/claude-code) skill that
teaches agents how to use it (auth, datasources, `explore`, `resources`).
After installing the binary, run:

```shell
grafanapi install-skill
```

This writes the skill to `~/.claude/skills/grafanapi` (or `--to <path>` for a
project-local `.claude` folder), replacing anything already installed there.
No Grafana configuration or session is required for this command.

## Prebuilt tarball

Prebuilt tarballs are attached to each
[release](https://github.com/avitsrimer/grafanapi/releases/latest).

* Download the `grafanapi_Darwin_arm64.tar.gz` archive from the Assets section
* Extract the archive
* Move the `grafanapi` executable to a directory on your `PATH`
* Ensure you have execute permission on the file

Because the binary is not notarized/code-signed, macOS Gatekeeper quarantines
files downloaded outside Homebrew. Clear the quarantine attribute once after
extracting:

```shell
xattr -d com.apple.quarantine ./grafanapi
```

!!! tip
    The first time `grafanapi` reads a session cookie from the Keychain after
    a (re)build, macOS shows an "Allow / Always Allow" dialog for that binary.
    Choose "Always Allow" so subsequent commands don't prompt again.

## Build from source

To build `grafanapi` from source you must:

* have [`git`](https://git-scm.com/) installed
* have [`go`](https://go.dev/) v1.24 (or greater) installed
* be building on macOS/arm64, and have Xcode Command Line Tools installed (for
  cgo/`Security.framework`)

```shell
go install github.com/grafana/grafanapi/cmd/grafanapi@latest
```
