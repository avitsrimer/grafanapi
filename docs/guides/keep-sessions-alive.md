---
title: Keep sessions alive
---

`grafanapi` authenticates with a Grafana **session cookie**. Today that cookie rotates
automatically whenever it is used (see [`login`](../reference/cli/grafanapi_login.md)), but it is
never touched otherwise: a context that goes unused for `login_maximum_inactive_lifetime`
(Grafana's default is 7 days) simply expires, even though a single rotation a day would have kept
it alive indefinitely (up to the separate `login_maximum_lifetime` hard cap, default 30 days).

The `session` command group closes that gap: it lets `grafanapi` rotate a context's session
proactively, on a schedule, instead of relying solely on reactive rotation.

## Opt a context in with `live-window`

Keep-alive is **opt-in per context**. Set `live-window` to the maximum age you want that context's
session to reach before it is rotated again:

```shell
grafanapi config set contexts.<name>.grafana.live-window 12h
```

`live-window` accepts any Go duration between `1m` and `6d`. A context with no `live-window` set is
never touched by keep-alive — `session refresh --due` and `session keepalive` both skip it
silently.

To check a context's current `live-window` and how long ago its session was last rotated:

```shell
grafanapi config check
```

## Force a rotation with `session refresh`

```shell
grafanapi session refresh                 # rotate the current context, unconditionally
grafanapi session refresh --context prod  # rotate a specific context, unconditionally
grafanapi session refresh --all           # rotate every context that has a stored cookie
grafanapi session refresh --due           # rotate only contexts whose live-window has elapsed
```

- Manual `refresh` (no `--all`/`--due`) and `--all` **ignore `live-window` entirely** — they force
  a rotation regardless of how fresh the session already is. Use these when you want an immediate,
  unconditional rotation.
- `--due` is the scheduler entry point: it rotates only the contexts whose `live-window` has been
  set **and** whose last rotation is older than that window. This is what `session keepalive`
  installs on a schedule; you can also run it by hand.
- A rejected rotation (dead session) exits with code `2` — refresh it with
  [`grafanapi login update`](../reference/cli/grafanapi_login_update.md).

## Schedule it with `session keepalive`

```shell
grafanapi session keepalive install            # install a LaunchAgent running "session refresh --due"
grafanapi session keepalive status              # is it installed / loaded? interval, log tail
grafanapi session keepalive uninstall            # remove the LaunchAgent
```

`keepalive install` requires at least one context to have `live-window` set — otherwise it errors
with a message telling you to set one first. By default it derives its wake interval from the
smallest `live-window` across every opted-in context (`min/2`, clamped to `[15m, 12h]`); pass
`--interval` to override that derivation directly (validated against `[1m, 6d]`).

Installing is idempotent — running it again (for example, after opting another context in)
replaces the previous LaunchAgent definition and reloads it.

## Caveats

- **The 30-day hard cap still applies.** Keep-alive extends Grafana's inactivity timeout
  indefinitely, but the separate `login_maximum_lifetime` cap (default 30 days) is not affected by
  rotation — a session must still be re-established roughly monthly with
  `grafanapi login update`.
- **A shared cookie can race a browser.** If the same `grafana_session` cookie is also used in a
  browser tab, both sides rotating it independently can conflict. Log in to `grafanapi` from a
  private/incognito browser window so the CLI owns a dedicated cookie chain.
- **Keep-alive requires an active macOS login session.** The LaunchAgent runs in the GUI domain and
  needs Keychain access, so it does not run while the machine is logged out.
- **The log never contains a cookie value.** `session keepalive status` and
  `~/Library/Logs/grafanapi/keepalive.log` only ever show `✔`/`✘` status lines per context.
