# `grafanapi session` — scheduled session keep-alive

## Overview

**What:** A new top-level `session` command group that lets `grafanapi` keep its own Grafana
sessions alive proactively, on a schedule, instead of relying solely on the reactive `401`-triggered
rotation that already exists. Keep-alive is **opt-in per context** via a new `live-window`
configuration field.

```
grafanapi session refresh [--context X | --all]        # force a rotation NOW (unconditional)
grafanapi session refresh --due                         # scheduler entry point: only contexts due per live-window
grafanapi session keepalive install [--interval 12h]    # schedule --due via a launchd LaunchAgent
grafanapi session keepalive status                      # is it installed / loaded? interval, log tail
grafanapi session keepalive uninstall                   # remove the LaunchAgent

grafanapi config set contexts.<name>.grafana.live-window 12h   # opt a context into keep-alive
```

**Why:** Today a session rotates **only** when a request receives a `401`
(`rotatingRoundTripper` → `SessionSource.Rotate` → `POST /api/user/auth-tokens/rotate` → Keychain
persist, in `internal/config/session_source.go`). There is no proactive rotation, so a context that
goes unused simply dies after Grafana's `login_maximum_inactive_lifetime` (default 7 days) elapses —
even though a single `POST .../rotate` a day would have kept it alive indefinitely (up to the
separate hard `login_maximum_lifetime` cap, default 30 days). `session refresh` exposes the rotation
as a first-class command; `live-window` lets an operator declare "keep this context's session no
older than 12h"; and `session keepalive` schedules `session refresh --due` through a macOS **launchd
LaunchAgent** (the project is macOS-only — launchd, never cron) so warm contexts stay warm without
any manual action, and untagged contexts are left untouched.

**Integration:** Builds directly on the completed session-cookie auth and automatic-rotation work
(`docs/plans/completed/20260721-session-cookie-auth-and-release.md`,
`docs/plans/completed/20260722-auto-rotate-session-on-401.md`). `session refresh` reuses the exact
`SessionSource` rotation + Keychain-persist plumbing already shipped; it adds one small public entry
point (`SessionSource.Refresh`) that force-rotates while remaining single-flight-safe. Per-context
resolution follows the pattern `config check` already uses (`internal/config`.
`ResolveContextSessionCookie` per context — see `cmd/grafanapi/config/command.go` `checkContext`).
`live-window` is a new optional, non-secret field on `GrafanaConfig`, settable through the existing
reflection-driven `config set`. Last-rotation time is read from the Keychain item's modification date
(a new `keychain.Store.ModifiedAt` method) — **no new persisted state**. `session keepalive` adds a
new, self-contained `internal/launchd` package (plist generation + a `launchctl` seam) with no new
third-party dependency, and `config check` gains a keep-alive status section.

## Context (from discovery)

### Rotation plumbing (`internal/config/session_source.go`) — verified

- `SessionSource` holds `cookie`, a `gen uint64` generation counter, and an `inflight *rotateCall`
  single-flight marker, all guarded by `mu`. It is constructed **per resolved context** by
  `NewSessionSource(cookie, server, tls, store, account)` and shared by every transport for that
  context.
- `Current() (string, uint64)` returns the in-memory cookie and its generation without blocking on
  any rotation in flight.
- `Rotate(ctx, usedGen uint64) (string, error)` is the sole rotation entry point:
  - **Short-circuit:** if `s.gen > usedGen` (another goroutine already rotated past the generation the
    caller observed), it returns the current cookie with **no network call**.
  - **Single-flight:** if a rotation for the current generation is already in flight, it waits on
    `call.done` instead of starting a second one — without holding `mu`.
  - Otherwise it runs `runRotate` → `doRotate` (`POST /api/user/auth-tokens/rotate` with the stored
    cookie via a dedicated, non-logging, TLS-aware client), publishes the new cookie and increments
    `gen` under one `mu` acquisition, then best-effort `persist`s to the Keychain.
  - On a `401`/`403` from the rotate endpoint it returns the sentinel
    **`config.ErrRotateUnauthorized`** ("session rotation: unauthorized").
- `doRotate` / `persist` are unexported; `Refresh` (new) reuses them **through** `Rotate`.

### Config field, validation, and reflection-driven `config set` — verified

- `GrafanaConfig` (`internal/config/types.go`) holds `Server`, `OrgID`, `StackID`, `TLS`, and the
  unserialized `SessionCookie`/`Session`. `Validate(contextName)` checks `server` then
  `validateNamespace`. `IsEmpty()` zeroes `SessionCookie`/`Session` before the `== GrafanaConfig{}`
  comparison. A new **persisted** `string` field (`live-window`) participates in that comparison
  correctly (empty ⇒ still empty; `grafana: {live-window: 12h}` ⇒ non-empty) and must **not** be
  zeroed in `IsEmpty()`.
- `config set` (`internal/config/editor.go` `SetValue` → `updateValue`) is **reflection/yaml-tag
  driven** with a generic `reflect.String` case and **no field allowlist** (verified: `editor_test.go`
  notes it is "reflection/yaml-tag driven"). A plain `string` field tagged `yaml:"live-window"`
  therefore works with `config set contexts.<name>.grafana.live-window 12h` for free, exactly like
  `grafana.server`. **Decision: `live-window` is a `string`** (human-friendly `"12h"` in YAML, parsed
  to a `time.Duration` by a helper) to keep `config set` free and the YAML readable.
- `config reference` docs are generated (`scripts/config-reference/`) and pick up the new field
  automatically; regeneration is covered in Verify.

### Keychain layer & last-rotation time (mtime) — verified feasible

- `internal/keychain` is **darwin-only** (both `keychain.go` and `keychain_darwin.go` carry
  `//go:build darwin`; there is **no** non-darwin stub file). `Store` is a 3-method interface
  (`Set`/`Get`/`Delete`); `darwinStore` implements it via hand-rolled cgo to Security.framework;
  `getItem` already calls `SecItemCopyMatching` with `kSecReturnData` + `kSecMatchLimitOne`.
- **Last-rotation time = the Keychain item's `kSecAttrModificationDate`.** `securityd` updates it on
  every `SecItemUpdate`/`SecItemAdd`, i.e. every `Set` — which is every persist: `login`,
  `login update`, and every rotation. So the modification date is exactly "the last time this
  context's cookie was (re)written" with **zero new state to maintain**.
- **Feasibility (confirmed):** add a cgo `getItemModDate(service, account, *outUnixSeconds)` that runs
  `SecItemCopyMatching` with `kSecReturnAttributes = kCFBooleanTrue`, `kSecMatchLimitOne`, and
  **without** `kSecReturnData`, reads `kSecAttrModificationDate` (a `CFDateRef`) from the returned
  `CFDictionaryRef`, and converts to Unix seconds via `CFDateGetAbsoluteTime()` +
  `kCFAbsoluteTimeIntervalSince1970` (978307200.0). **Because it requests attributes only (never the
  data) it does not decrypt the secret and therefore does NOT trigger the ACL "Allow" prompt** — so
  `config check` and the scheduler read last-rotation age silently. `time` is already imported in
  `keychain_darwin.go`.
- **New `Store` method:** `ModifiedAt(account string) (time.Time, error)` returning `ErrNotFound` when
  no item exists. Implementers: `darwinStore` (real), plus the two test fakes —
  `internal/testutils/keychain.FakeKeychainStore` (add injectable `mtimes` + `SetModified`) and the
  whitebox `fakeStore` in `internal/config/session_source_test.go`. No non-darwin stub exists.
- **Rejected alternative — JSON state file** under `~/Library/Application Support/grafanapi/`: adds a
  persisted artifact that must be written on every rotation, cleaned up on `config unset`, and kept in
  sync with the Keychain; the mtime approach needs none of that and reads without a prompt. Recorded
  as the fallback only if a future macOS change removes attribute-read access.

### Per-context credential resolution & `config check` — verified

- `internal/config.ResolveContextSessionCookie(store, gCtx)` populates `SessionCookie` **and**
  constructs `gCtx.Grafana.Session` (`*SessionSource`) from the Keychain, keyed by
  `keychain.Account(gCtx.Name)`. `keychain.ErrNotFound` leaves both empty and returns `nil`; the
  `SessionSource` is **only** constructed when a cookie was loaded — so `gCtx.Grafana.Session == nil`
  is the "no stored cookie, skip silently" signal `--all`/`--due` need.
- `config check` (`cmd/grafanapi/config/command.go` `checkCmd`/`checkContext`) is the reference for
  iterating **every** context: loads tolerantly (no per-context validation), calls
  `ResolveContextSessionCookie` in a `for _, gCtx := range cfg.Contexts` loop, and renders `✔/⚠/✘`
  lines with `io.Success/Warning/Error`. It **always exits 0** (diagnostic report). The keep-alive
  section is added here.
- `config.Load(ctx, source, overrides...)`, `Config{Contexts, CurrentContext}`, `GetCurrentContext`,
  `HasContext`, `config.ContextNotFound` are exported (used by `login`). The `session` command loads
  via `config.Load` + a `configSource()` helper (like `login`), needing **no new API** on the config
  command package.

### Exit-code-2 (stale session) rendering — verified

- Exit codes: `cmd/grafanapi/main.go` → `fail.ErrorToDetailedError(err).ExitCode`. `fail/convert.go`
  `convertSessionErrors` maps a **`*runtime.APIError{Code:401}`** to `staleSessionError` — "Grafana
  session is stale or unauthorized", suggestion `Run: grafanapi login update`, **exit code 2**. It
  maps the bare `session.ErrUnauthorized` to the *different* `loginRejectedError` (login prompt only).
- **For `refresh`:** `Rotate` returns `config.ErrRotateUnauthorized`, which `fail` does not recognize.
  To get exit-2 rendering **without touching `fail/`**, the refresh command converts a rejection into
  `runtime.NewAPIError("session refresh", nil, http.StatusUnauthorized)` — the exact technique the
  completed `explore` plan used. `github.com/go-openapi/runtime` is already vendored.

### Command / options / output patterns — verified

- **Registration:** `cmd/grafanapi/root/command.go` `rootCmd.AddCommand(session.Command())`, alongside
  `config`, `datasources`, `explore`, `login`, `resources`.
- **Options pattern:** `login.Options` is the model for a **standalone** top-level command owning
  `--config`/`--context` + a `configSource()` helper + a package-level `keychainStore` test seam
  (`SetKeychainStore(store) func()`). `session` follows this because `refresh --all`/`--due` resolve
  **every** context themselves.
- **Output/messages:** `cmd/grafanapi/io` `Success/Warning/Error/Info` render `✔/⚠/✘/🛈`; `io.Options`
  provides `-o/--output` with `json`/`yaml` builtins + `RegisterCustomCodec`/`DefaultFormat` (used by
  `status -o json`).
- **Test seams:** `config.SetKeychainStore`, `testutils.NewFakeKeychainStore`,
  `testutils.CommandTestCase` (+ `CommandSuccess`/`CommandErrorContains`/`CommandOutputContains`),
  `testutils.CreateTempFile`. The `session` package adds its own `keychainStore` + `controller` seams.

### No existing launchd / os.Executable usage

- `grep` across the repo (excluding `vendor/`) finds **no** existing `os.Executable`, `launchctl`,
  `LaunchAgent`, or `StartInterval` usage — all-new surface, owned by `internal/launchd`. No plist
  library is vendored and **none will be added** (hard constraint): the plist is generated with
  `text/template` and read back with `encoding/xml` token scanning (stdlib).

### Reference-docs generation (CI-enforced)

- `make cli-reference` adds `grafanapi_session.md`, `grafanapi_session_refresh.md`,
  `grafanapi_session_keepalive.md`, `grafanapi_session_keepalive_install.md`, `..._status.md`,
  `..._uninstall.md`, and updates the `grafanapi.md` index. `make config-reference` picks up
  `live-window`. `make reference-drift` (CI) fails unless regenerated + committed. **Mandatory.**
- Lint baseline is **14 findings** (5 gosec, 3 govet, 1 nolintlint, 5 staticcheck), all pre-existing.
  `goreleaser check` must pass.

## Development Approach

- **Testing approach: Regular** — implementation first, then tests, matching repo conventions:
  `testify`, table-driven cases, `httptest.Server` for HTTP, golden files for generated artifacts,
  fakes for external seams (Keychain, `launchctl`, clock, filesystem). **No real Grafana, launchctl,
  launchd, or Keychain in unit tests.**
- Complete each task **fully** (including tests) before the next; every task ends with a "run tests —
  must pass before next task" gate.
- **Scope discipline (minimal):** refresh force-rotates already-stored cookies only (never logs in,
  never prompts, never reads a cookie from anywhere but the Keychain). `live-window` is the only new
  config field. keepalive manages exactly one LaunchAgent running `session refresh --due`. No
  Linux/cron path, no arbitrary command scheduling, no multiple agents.
- Keep it elegant: launchd domain logic lives in a fully unit-testable `internal/launchd`; due-
  selection is a **pure function** with injected clock + mtime lookup; the `cmd/grafanapi/session`
  package is thin wiring. Rotation is reused, never reimplemented (one 3-line `SessionSource.Refresh`).
- **The session cookie is never printed or logged** — not to stdout, not to the launchd log, not in
  any error. Assert this in tests.
- If scope changes mid-implementation, **update this plan file** (add ➕ tasks, mark ⚠️ blockers)
  before continuing.

## Testing Strategy

- **`SessionSource.Refresh` (whitebox, `internal/config`):** `httptest` rotate endpoint —
  `200`+`Set-Cookie` → new cookie, `gen` +1, fake store persisted; `401` → `ErrRotateUnauthorized`,
  `gen` unchanged, store untouched; **single-flight**: hold a rotation in flight (block the handler)
  and fire a concurrent `Refresh` → endpoint hit **once**, both callers get the same cookie, `gen`
  +1 (no double-rotate).
- **`live-window` config:** `GrafanaConfig.Validate` rejects an unparseable window and one outside
  `[1m, 6d]`, accepts `1m`/`6d` boundaries and empty (unset); the parse helper returns
  `(0, false, nil)` when unset, `(d, true, nil)` when valid, error when set-but-invalid; a
  reflection-`SetValue` test confirms `contexts.x.grafana.live-window` round-trips (extend
  `editor_test.go`); `IsEmpty()` still true for a bare `grafana: {}` and false once `live-window` set.
- **`keychain.Store.ModifiedAt`:** fake store returns injected mtimes and `ErrNotFound` for unknown
  accounts (the real cgo path is exercised only in Post-Completion live verification, matching the
  repo convention that Keychain cgo has no unit test beyond the darwin build).
- **Due-selection (pure function, table-driven, fake clock + fake mtime):** a config with mixed
  contexts (no `live-window`; window fresh; window stale; window set but no Keychain item; window
  set-but-invalid) and a fixed `now` → exactly the stale-window contexts selected; unset/fresh/no-
  cookie skipped; invalid-window skipped with a recorded warning (never selected, never a hard error).
- **`internal/launchd` plist generation:** golden byte-equality for the default spec; an XML-special
  binary path (`/opt/homebrew/bin/gr&af<a>napi`) is escaped and `encoding/xml`-parseable.
- **`internal/launchd` binary-path resolution:** fake `os.Executable`/stat/symlink seam — plain path
  passthrough; Homebrew Cellar path + matching stable `/opt/homebrew/bin/grafanapi` symlink → symlink;
  Cellar path + no matching symlink → resolved real path; `os.Executable` error propagates.
- **`internal/launchd` Inspect:** round-trips the golden plist (label, interval seconds, args);
  malformed/absent plist → clear error.
- **`launchctl` seam:** `fakeController` records `Bootstrap`/`Bootout`/`Print` and returns scripted
  output/errors; **no real `launchctl`**.
- **`session refresh` command** (`httptest` + fake Keychain + temp config): single current context
  `200` → `✔ refreshed session for context X`, exit 0, **no cookie in stdout**; single `401` → exit 2
  stale-session (assert `fail.ErrorToDetailedError(err).ExitCode == 2` + summary); `--all`
  one-live/one-dead/one-no-cookie → both cookie-bearing **attempted** (server hit twice), exit 2;
  `--all` network-only failure → exit 1; **`--due`** with fresh+stale+unset contexts (fake mtimes) →
  only the stale one is rotated, exit 0; `--due` where a due context is dead → exit 2. Plus a
  **direct** table-driven test of the pure `dueContexts` selector covering every branch.
- **`session keepalive install/status/uninstall`** (fake controller + temp `HOME`): install with at
  least one `live-window` set writes the plist, derives StartInterval from the min window (`min/2`
  clamped to `[15m, 12h]`), calls `Bootout`→`Bootstrap`, is idempotent; **install with NO context
  having `live-window` errors** with the "set contexts.<name>.grafana.live-window first" message;
  explicit `--interval` overrides the derivation and is validated `[1m, 6d]` (`30s`/`7d` rejected,
  `1m`/`6d` accepted); `status` installed+loaded prints interval/binary/loaded=yes + log tail, not-
  installed prints a clear line, `-o json` decodes; `uninstall` `Bootout`+remove, idempotent.
- **`config check` keep-alive section:** fake controller + fake Keychain mtimes + temp config —
  renders LaunchAgent installed/loaded + interval/binary, and per-context `live-window` value (or "not
  set") + last-rotation age when derivable; a controller/inspect error renders "unknown" and the check
  **still exits 0**; extend the existing `config check` tests.

## Progress Tracking

- Mark `- [x]` immediately upon completing each checkbox.
- Prefix discovered-mid-work tasks with ➕ and append them in-place.
- Prefix blockers with ⚠️ and stop to re-plan when one is hit.

## Solution Overview

### `SessionSource.Refresh` vs the generation counter (RESOLVED — unchanged)

**Decision:** add one small public method to `SessionSource`:

```go
// Refresh forces a rotation now, regardless of staleness, reusing the same single-flight rotate +
// Keychain-persist path as the automatic 401-triggered rotation. It is safe to call concurrently
// with an in-flight 401 rotation: it joins that rotation rather than starting a second one.
func (s *SessionSource) Refresh(ctx context.Context) (string, error) {
	_, gen := s.Current()
	return s.Rotate(ctx, gen)
}
```

**Why this is exactly right, and why it does not double-rotate:** `Rotate(ctx, usedGen)`'s only
short-circuit is `if s.gen > usedGen`. Passing the generation observed *at the moment of the call*
means the short-circuit can fire **only** if a peer advances the generation in the tiny window
between `Current()` and `Rotate` re-acquiring `mu` — i.e. only when a rotation *just completed*, in
which case reusing that fresh cookie is correct, not a missed refresh. In the one-shot CLI there is no
such concurrency, so `s.gen == gen` and `Rotate` always performs a **real** network rotation ("force
now"). Single-flight safety is inherited: an already-in-flight rotation is joined via `call.done`,
never duplicated. No duplication of `doRotate`/`persist`, no new locking, no bypass of the counter.

> `Refresh` lives in `internal/config` — no import of `internal/session`, preserving the import-cycle
> rule. The `session` **command** package may import both `config` and `session`; `refresh` needs only
> `config`.

### `live-window` opt-in and `--due` selection (RESOLVED)

- **`live-window`** is an optional `string` field on `GrafanaConfig` (`yaml:"live-window"`, **no env
  tag** — it is a scheduling policy, not a secret and not a per-invocation override). Presence = the
  context **opts into** scheduled keep-alive; the value = how fresh its session must be kept.
  Validated **only when set**: `time.ParseDuration` must succeed and the result must be in `[1m, 6d]`
  (same bounds as `--interval`). A helper on `GrafanaConfig` centralizes parse+bounds so `Validate`,
  `config set` round-trips, `--due`, and `config check` all agree.
- **Last-rotation time** comes from `keychain.Store.ModifiedAt(account)` (the item's
  `kSecAttrModificationDate`; see Context) — no new state, no prompt.
- **`--due` selection** is a **pure function** in the `session` command package:

  ```go
  func dueContexts(cfg *config.Config, now time.Time,
      modAt func(account string) (time.Time, error)) (due []*config.Context, warnings []string)
  ```

  For each context: skip if `live-window` unset; if set-but-invalid, record a warning and skip (never
  fail the scheduled run for one bad context); look up `modAt(keychain.Account(name))` — on
  `ErrNotFound` (no stored cookie) skip; select iff `now.Sub(lastRotation) >= window`. Injected `now`
  + `modAt` make it trivially testable with a fake clock and fake mtimes.
- **`session refresh --due`** (the scheduler entry point) runs `dueContexts`, emits warnings via
  `io.Warning`, then refreshes exactly the selected contexts using the same per-context helper as
  `--all`, with the same exit-code aggregation (auth failure ⇒ exit 2, non-auth-only ⇒ 1, else 0). A
  `--due` run with nothing due is a success (exit 0) that prints a single "nothing due" line.
- **Manual `session refresh [--context|--all]` stays unconditional** — it ignores `live-window`
  entirely (explicit user intent). `--due` is mutually exclusive with `--all` and `--context`.

### Binary-path resolution for the plist (RESOLVED — unchanged)

launchd stores an **absolute** program path re-read on every run, so it must survive upgrades
(`internal/launchd.ResolveBinaryPath`): `os.Executable()` → `filepath.EvalSymlinks`. If the real path
is under `/Cellar/` or `/Caskroom/` (a versioned Homebrew path a `brew upgrade` renames), prefer a
**stable** Homebrew symlink if it exists and resolves to the same file — try `/opt/homebrew/bin/
grafanapi` then `/usr/local/bin/grafanapi`; use the candidate **verbatim** (the symlink is stable
across upgrades). Otherwise fall back to the resolved real path. Filesystem calls sit behind an
injectable seam for the table-driven test. Rationale documented in code: a versioned Cellar path in
the plist breaks on the next `brew upgrade`; the stable symlink does not.

### The `launchctl` seam (RESOLVED — unchanged)

`internal/launchd.Controller` with `Bootstrap(domainTarget, plist)`, `Bootout(serviceTarget)`,
`Print(serviceTarget)`; modern targets `gui/<uid>` and `gui/<uid>/<label>` (uid from `os.Getuid()`).
Real `execController` shells out with **fixed** subcommands (annotate any gosec G204 with a justified
`//nolint:gosec`). Install always `Bootout`s (ignoring "not loaded") before `Bootstrap` for idempotent
reinstall; a `Bootstrap` failure leaves the plist written and prints a manual-`launchctl` fallback
message rather than failing. Command packages hold an overridable `controller` var + `SetController`
test seam.

### Package layout

- **`internal/config/`** (modify): `session_source.go` `Refresh`; `types.go` `live-window` field +
  parse helper + validation.
- **`internal/keychain/`** (modify): `Store.ModifiedAt`; `keychain_darwin.go` `getItemModDate` cgo.
- **`internal/testutils/keychain.go`** (modify): `FakeKeychainStore.ModifiedAt` + `SetModified`.
- **`internal/launchd/`** (new): `spec.go`, `plist.go` (`Generate`+`Inspect`), `path.go`
  (`ResolveBinaryPath`+fs seam), `controller.go` (`Controller`+`execController`+targets), `paths.go`
  (home-based path helpers), tests + `testdata/keepalive.plist.golden`.
- **`cmd/grafanapi/session/`** (new): `command.go` (parent, `Options`, `keychainStore`+`controller`
  seams), `refresh.go` (unconditional + `--due` + `dueContexts`), `keepalive.go` (install/status/
  uninstall), `*_test.go`.
- **`cmd/grafanapi/config/command.go`** (modify): keep-alive status section + a `keepaliveController`
  seam.
- **Register** in `cmd/grafanapi/root/command.go`.

## Technical Details

### `internal/config` — `Refresh`, `live-window`, validation

- `session_source.go`: add `Refresh` (3-line body above) in exported-methods order after `Rotate`,
  with the force-now + single-flight-join GoDoc contract.
- `types.go`: add `LiveWindow string \`json:"live-window,omitempty" yaml:"live-window,omitempty"\`` to
  `GrafanaConfig`, plus a parse helper `ParsedLiveWindow() (time.Duration, bool, error)` (name the
  field and helper to satisfy the linter). Because it is a real persisted `string`, it participates in
  `IsEmpty()`'s comparison correctly and must **not** be zeroed there.
- **The `LiveWindow` field MUST carry an explicit GoDoc comment** — the config-reference generator
  (`scripts/config-reference/`) renders field doc comments as the user-facing field description, and
  the linter (`nolintlint`/godot conventions in the baseline) requires exported fields be documented.
  Write it as user-facing help, e.g. "LiveWindow opts this context into scheduled keep-alive and sets
  how fresh its session must be kept (a Go duration such as \"12h\"; must be between 1m and 6d). Unset
  means the context is not kept alive on a schedule."
- `ParsedLiveWindow()` returns `(0, false, nil)` when empty; else `time.ParseDuration` + bounds-check
  `[1m, 6d]`, returning a descriptive error when invalid.
- `GrafanaConfig.Validate`: after the existing checks, if `live-window` is set, call the helper and
  return a `ValidationError` (path `$.contexts.'%s'.grafana.live-window`) on parse/bounds failure.
  **No special-casing of a `live-window`-only context:** a context with `live-window` set but no
  `server` correctly fails the pre-existing `server is required` validation (the `server` check runs
  first in `Validate`), which is the desired behavior — a context must still be a real, addressable
  Grafana target to be kept alive.
- **Bounds:** define unexported `minLiveWindow = time.Minute` / `maxLiveWindow = 6*24*time.Hour` in
  `internal/config` for validation, mirrored by the interval bounds in `internal/launchd`; documented
  to stay in sync (`[1m, 6d]`). Kept separate to avoid a `config`→`launchd` import edge.

### `internal/keychain` — `ModifiedAt`

- Add to `Store`: `ModifiedAt(account string) (time.Time, error)` (GoDoc: returns the item's last
  modification time — updated on every `Set` — or `ErrNotFound`; attributes-only read, no prompt).
- `keychain_darwin.go`: add cgo `getItemModDate` querying `SecItemCopyMatching` with
  `kSecReturnAttributes=kCFBooleanTrue`, `kSecMatchLimitOne`, **no** `kSecReturnData`; extract
  `kSecAttrModificationDate` (`CFDateRef`) → Unix seconds (`CFDateGetAbsoluteTime()` + 978307200.0).
  `darwinStore.ModifiedAt` wraps it in `withTimeout`, maps `errSecItemNotFound` → `ErrNotFound`, and
  returns `time.Unix(sec, 0)`.
- Fakes: `FakeKeychainStore` gains `mtimes map[string]time.Time` + `SetModified(account, t)`; its
  `Set` records the current mtime so tests can also rely on `Set` updating it; `ModifiedAt` returns the
  stored mtime or `ErrNotFound`. Add a trivial `ModifiedAt` to the whitebox `fakeStore`.

### `session refresh` (`cmd/grafanapi/session/refresh.go`)

- Flags: `--all` (bool), `--due` (bool); `--config`/`--context` from the parent's persistent flags.
  Validate: at most one of `--all`/`--context`/`--due`.
- Load config with `config.Load(cmd.Context(), opts.configSource())` (tolerant — no validation).
- **Target selection:** `--due` → `dueContexts(cfg, time.Now(), keychainStore.ModifiedAt)` (emit
  warnings; if none due, print "nothing due" and exit 0); `--all` → all contexts sorted by name;
  default → `--context` value (or `cfg.CurrentContext`), erroring via `config.ContextNotFound`.
- **Per context** `refreshContext(gCtx) (status, error)`: `ResolveContextSessionCookie`; if
  `Session == nil` (no cookie) → skip (silent under `--all`/`--due`; clear error under an explicit
  single target); else `Session.Refresh(ctx)` → `io.Success("refreshed session for context %s", name)`
  on success (**never** the cookie), classify `ErrRotateUnauthorized` as auth failure, other errors as
  non-auth failures.
- **Exit codes:** single target rejected → `runtime.NewAPIError("session refresh", nil, 401)` (exit
  2); other → the error (exit 1). `--all`/`--due`: attempt every selected context, print per-context
  ✔/✘; after the loop, auth failure ⇒ `*runtime.APIError{401}` (exit 2), else non-auth-only ⇒ joined
  error (exit 1), else `nil`. No-cookie/unset contexts never count as failures.

### `internal/launchd` — spec, paths, plist, inspect, path resolution, controller

- `spec.go`: `const Label = "io.github.avitsrimer.grafanapi.keepalive"` (locked); `AgentSpec{Label,
  BinaryPath, Args []string, IntervalSeconds int, StdoutPath, StderrPath}`; `DefaultAgentSpec(binary,
  interval)` with `Args = []string{"session","refresh","--due"}` (**note: `--due`, not `--all`** — the
  scheduler must honor per-context windows), Stdout/StderrPath = `LogPath()`. Interval bounds
  `minInterval=time.Minute`, `maxInterval=6*24*time.Hour`.
- `paths.go`: `LaunchAgentsDir/PlistPath/LogDir/LogPath` under `os.UserHomeDir()`:
  `~/Library/LaunchAgents/io.github.avitsrimer.grafanapi.keepalive.plist`, log
  `~/Library/Logs/grafanapi/keepalive.log`.
- `plist.go`: `Generate(io.Writer, AgentSpec)` via `text/template` (`RunAtLoad` **false**), an `xml`
  escape func applied to the binary path + log paths; `Inspect(plistPath) (AgentSpec, error)` via
  `encoding/xml` token scanning (Label, StartInterval, ProgramArguments).
- `path.go`: `ResolveBinaryPath()` + `fileSystem` seam (see Solution Overview).
- `controller.go`: `Controller` interface, `execController`, `NewExecController`, `UserDomainTarget`/
  `UserServiceTarget`.

### `session keepalive install` (`keepalive.go`)

- Flag `--interval` (`time.Duration`, default **0** = "derive"). Load config; collect every context's
  parsed `live-window`.
- **If NO context has `live-window` set → error:** "no context opts into keep-alive; set one first,
  e.g. grafanapi config set contexts.<name>.grafana.live-window 12h" (exit 1).
- **StartInterval:** if `--interval` given, validate it against `[1m, 6d]` and use it verbatim; else
  derive from the **minimum** live-window across opted-in contexts as `min/2`, **clamped to
  `[15m, 12h]`** (a 12h window polls ~every 6h; a 1m window clamps up to 15m). **The `[15m, 12h]` clamp
  applies ONLY to the derived interval** — an explicit `--interval` is bounded solely by `[1m, 6d]` and
  is never clamped to the narrower derived range (so a user may set, e.g., `--interval 1m` or
  `--interval 6d`). The agent runs `session refresh --due`, which re-checks each window, so a modest
  fixed cadence suffices and the clamp bounds the wake frequency for the auto-derived case.
- Resolve binary; build spec; `MkdirAll` LaunchAgents + Logs dirs; write plist; `Bootout` (ignore
  not-loaded) → `Bootstrap`; on `Bootstrap` failure print the manual-fallback `io.Warning` and return
  nil; on success `io.Success("installed keepalive LaunchAgent (polling every %s, honoring each
  context's live-window); logs: %s", interval, LogPath())`.

### `session keepalive status` (`keepalive.go`)

- `io.Options` with a `text` default codec + `json`/`yaml` builtins. Gather `statusReport{Installed,
  Loaded bool; IntervalSeconds int; Binary, PlistPath, LogPath string; LogTail []string}` from
  `os.Stat`+`Inspect`, `controller.Print` (loaded bool; errors → Loaded=false, no failure), and the
  last ~10 log lines (best-effort). Log tail echoed verbatim (log never contains a cookie).

### `session keepalive uninstall` (`keepalive.go`)

- `controller.Bootout(UserServiceTarget())` (ignore not-loaded) + `os.Remove(PlistPath())` (ignore
  `os.ErrNotExist`); idempotent ✔ even when absent.

### `config check` keep-alive section (`cmd/grafanapi/config/command.go`)

- Add a `keepaliveController launchd.Controller = launchd.NewExecController()` package var +
  `SetKeepaliveController` test seam (mirrors `keychainStore`). The `internal/config`-command →
  `internal/launchd` import is acyclic.
- After the per-context loop in `checkCmd`, render a **"Keep-alive"** block:
  - LaunchAgent: `os.Stat(launchd.PlistPath())` for installed; `keepaliveController.Print(...)` for
    loaded; `launchd.Inspect` for interval + target binary. **Any error → render the affected line as
    "unknown"** (via `io.Warning`), never abort — `config check` still exits 0.
  - Per context: `live-window` value or "not set"; when the context has a stored cookie, last-rotation
    age from `keychainStore.ModifiedAt(keychain.Account(name))` (e.g. "rotated 3h ago"; skip/`—` on
    `ErrNotFound` or error).
- Extend `command_test.go`: inject a fake controller + fake Keychain mtimes; assert the rendered
  section for installed+loaded vs absent, `live-window` set vs "not set", an age line, and that an
  inspection error yields "unknown" with the command still succeeding (exit 0).

### `session` parent + registration (`command.go`, `root/command.go`)

- `Command()` builds the `session` parent, binds `Options` on `PersistentFlags()`, and adds
  `refreshCmd`/`keepaliveCommand`. Package vars `keychainStore` (+`SetKeychainStore`) and `controller`
  (+`SetController`) with the `//nolint:gochecknoglobals // test seam` annotation. `root/command.go`:
  import + `AddCommand(session.Command())` in alphabetical position.

## Implementation Steps

### Task 1 — `SessionSource.Refresh` (force-rotate, single-flight-safe)

**Files:**
- Modify: `internal/config/session_source.go`
- Modify: `internal/config/session_source_test.go`

- [x] Add `Refresh(ctx) (string, error)` (`Current()` → `Rotate(ctx, gen)`) in the correct
      symbol-ordering position, with the force-now + single-flight-join GoDoc contract.
- [x] Test rotate `200`: new cookie, `gen` +1, fake store persisted.
- [x] Test rotate `401`: `ErrRotateUnauthorized`, `gen` unchanged, store untouched.
- [x] Test single-flight: in-flight rotation held on a channel + concurrent `Refresh` → endpoint hit
      once, same cookie to both, `gen` +1.
- [x] Run tests — must pass before next task.

### Task 2 — `live-window` config field + `keychain.Store.ModifiedAt` (last-rotation time)

**Files:**
- Modify: `internal/config/types.go`, `internal/config/types_test.go`,
  `internal/config/editor_test.go`
- Modify: `internal/keychain/keychain.go`, `internal/keychain/keychain_darwin.go`
- Modify: `internal/testutils/keychain.go`, `internal/config/session_source_test.go` (satisfy the
  grown `Store` interface)
- Modify (unlisted implementers the interface change breaks — all need a trivial `ModifiedAt` stub,
  or the Task-2 test gate will not compile): `internal/explore/run_test.go` (`nopStore`),
  `internal/config/credentials_test.go` (`fakeKeychainStore` — note `panickingStore` embeds
  `*fakeStore`/`*fakeKeychainStore` and inherits `ModifiedAt` for free, no separate stub),
  `cmd/grafanapi/config/command_test.go` (`fakeKeychainStore`),
  `cmd/grafanapi/login/command_test.go` (`fakeKeychainStore`)

- [x] Add the `live-window` `string` field (`yaml:"live-window,omitempty"`, no env tag) with an
      **explicit GoDoc comment** (the config-reference generator renders it as user-facing text and
      the linter requires it) + a parse/bounds helper (`ParsedLiveWindow`), wire it into
      `GrafanaConfig.Validate` (set-but-invalid or out-of-`[1m,6d]` → `ValidationError`), and confirm
      `IsEmpty()` treats it correctly.
- [x] Add `ModifiedAt(account) (time.Time, error)` to `keychain.Store`; implement the darwin cgo
      `getItemModDate` (attributes-only `SecItemCopyMatching`, `kSecAttrModificationDate` → Unix
      seconds, `errSecItemNotFound` → `ErrNotFound`), documenting the no-prompt property.
- [x] Extend `FakeKeychainStore` with injectable mtimes (`SetModified`, `Set` updates mtime) +
      `ModifiedAt`; add a trivial `ModifiedAt` to the whitebox `fakeStore`.
- [x] Add trivial `ModifiedAt` stubs to the four unlisted `Store` implementers above (`nopStore`,
      the two `fakeKeychainStore`s in cmd tests, and `credentials_test.go`'s `fakeKeychainStore`);
      `panickingStore` inherits via its embedded `*fakeStore`, so it needs none. This unblocks every
      package's compilation for the Task-2 gate.
- [x] Tests: validation (invalid, `[1m,6d]` bounds, unset); `ParsedLiveWindow` cases; `config set
      contexts.x.grafana.live-window 12h` round-trip; fake `ModifiedAt` returns set/injected mtime and
      `ErrNotFound` otherwise.
- [x] Run tests (all packages — must compile with the grown interface) — must pass before next task.

### Task 3 — `internal/launchd`: spec, paths, and plist generation

**Files:**
- Create: `internal/launchd/spec.go`, `internal/launchd/paths.go`, `internal/launchd/plist.go`,
  `internal/launchd/plist_test.go`, `internal/launchd/testdata/keepalive.plist.golden`

- [x] Define `Label`, `AgentSpec`, `DefaultAgentSpec` (`Args = session refresh --due`), interval
      bounds, and the home-based path helpers.
- [x] Implement `Generate` (`text/template`, `RunAtLoad` false, `xml`-escaped binary/log paths).
- [x] Add the golden plist (a `12h`→`43200`s default spec, plain `/opt/homebrew/bin/grafanapi`) + a
      byte-equality test.
- [x] Test XML escaping (`&`/`<`/`>` in the binary path escaped and `encoding/xml`-parseable).
- [x] Run tests — must pass before next task.

### Task 4 — `internal/launchd`: binary-path resolution + plist inspection

**Files:**
- Create: `internal/launchd/path.go`, `internal/launchd/path_test.go`
- Modify: `internal/launchd/plist.go`, `internal/launchd/plist_test.go`

- [x] `ResolveBinaryPath()` with `os.Executable`→`EvalSymlinks`→Homebrew-stable-symlink preference
      behind a `fileSystem` seam; document the survives-`brew upgrade` rationale.
- [x] Table test: plain passthrough; Cellar + matching symlink → symlink; Cellar + no symlink →
      resolved path; `os.Executable` error propagates.
- [x] `Inspect(plistPath)` via `encoding/xml` token scanning (Label, StartInterval, ProgramArguments);
      malformed/absent → clear error.
- [x] Test `Inspect` round-trips the golden plist and errors on truncated/missing input.
- [x] Run tests — must pass before next task.

### Task 5 — `internal/launchd`: the `launchctl` Controller seam

**Files:**
- Create: `internal/launchd/controller.go`, `internal/launchd/controller_test.go`

- [ ] `Controller` interface + `UserDomainTarget`/`UserServiceTarget` (uid from `os.Getuid()`) +
      `execController`/`NewExecController` shelling to `launchctl` with fixed subcommands (justified
      `//nolint:gosec` if G204 flags it).
- [ ] Test target formatting against a stubbed uid; test `execController` argv construction **without**
      running real `launchctl`.
- [ ] Provide a reusable `fakeController` (recording calls, scripted returns) for Tasks 7–9.
- [ ] Run tests — must pass before next task.

### Task 6 — `session` parent, `Options`, seams, and root registration

**Files:**
- Create: `cmd/grafanapi/session/command.go`
- Modify: `cmd/grafanapi/root/command.go`

- [ ] Implement `Command()`, `Options{ConfigFile, Context}` + `BindFlags`/`configSource`, and the
      `keychainStore`/`controller` vars + `SetKeychainStore`/`SetController` restorers.
- [ ] Wire `AddCommand(refreshCmd, keepaliveCommand)` (stubs OK) so the package compiles.
- [ ] Register `session.Command()` in `root/command.go` (import + `AddCommand`, alphabetical).
- [ ] `go build ./...` compiles; `grafanapi session --help` lists `refresh` and `keepalive`.
- [ ] Run tests — must pass before next task.

### Task 7 — `session refresh` (unconditional `--all`/`--context` + scheduler `--due`)

**Files:**
- Create: `cmd/grafanapi/session/refresh.go`, `cmd/grafanapi/session/refresh_test.go`

- [ ] Implement `refreshCmd` with `--all`/`--due` (mutually exclusive with each other and
      `--context`), config load via `config.Load`, and the shared per-context `refreshContext` helper.
- [ ] Implement the **pure** `dueContexts(cfg, now, modAt)` selector (unset skip; invalid-window
      warn+skip; `ErrNotFound` skip; select iff `now-lastRotation >= window`) and wire `--due` to it
      (`keychainStore.ModifiedAt` as `modAt`, `time.Now()` as `now`), emitting warnings and a
      "nothing due" success when empty.
- [ ] Implement exit-code mapping: single rejection → `*runtime.APIError{401}` (exit 2); `--all`/
      `--due` aggregate (attempt all; auth ⇒ 2, non-auth-only ⇒ 1, else 0); cookie never in output.
- [ ] Tests (httptest + fake Keychain + temp config): single `200` ✔/exit 0/no-cookie; single `401`
      exit 2; `--all` live+dead+no-cookie → both attempted, exit 2; `--all` network-only → exit 1;
      **`--due` with fake mtimes** (fresh+stale+unset) → only stale rotated (exit 0); `--due` dead
      due-context → exit 2. Plus a **direct** table-driven test of `dueContexts` (fake clock/mtimes)
      covering every branch.
- [ ] Run tests — must pass before next task.

### Task 8 — `session keepalive install / status / uninstall`

**Files:**
- Create: `cmd/grafanapi/session/keepalive.go`, `cmd/grafanapi/session/keepalive_test.go`

- [ ] `install`: **error if no context has `live-window`** (helpful "set ... first" message); else
      StartInterval = `--interval` (validated `[1m,6d]`) or derived `min(live-window)/2` clamped to
      `[15m,12h]`; resolve binary; write plist (`Args = session refresh --due`); `Bootout`→`Bootstrap`
      via the seam; graceful fallback on `Bootstrap` failure; idempotent.
- [ ] `status`: `io.Options` (`text` default + `json`/`yaml`); gather `statusReport` from `os.Stat`+
      `Inspect`+`controller.Print`+log tail; verbatim log tail.
- [ ] `uninstall`: `Bootout`+`os.Remove`, both idempotent; ✔ even when absent.
- [ ] Tests (fake controller + temp `HOME` + fake Keychain + temp config): install-with-window writes
      plist + Bootout→Bootstrap + correct derived interval + idempotent; **install-without-any-window
      errors**; `--interval` override + bounds (`30s`/`7d` rejected, `1m`/`6d` accepted); status
      installed+loaded vs not-installed + `-o json` decode; uninstall removes plist + idempotent.
- [ ] Run tests — must pass before next task.

### Task 9 — `config check` keep-alive status section

**Files:**
- Modify: `cmd/grafanapi/config/command.go`, `cmd/grafanapi/config/command_test.go`

- [ ] Add the `keepaliveController` var + `SetKeepaliveController` seam; render a "Keep-alive" block:
      LaunchAgent installed/loaded + interval/target binary (via `os.Stat`/`Print`/`Inspect`), and
      per-context `live-window` (or "not set") + last-rotation age (via `keychainStore.ModifiedAt`).
- [ ] Ensure **every** launchd/keychain inspection error degrades to "unknown"/`—` and the command
      still returns nil (exit 0).
- [ ] Extend `command_test.go`: fake controller + fake mtimes — installed+loaded rendering, absent
      rendering, `live-window` set vs "not set", an age line, and an inspection-error → "unknown" with
      the check still succeeding.
- [ ] Run tests — must pass before next task.

### Task 10 — Verify acceptance criteria

**Files:**
- Modify: none (verification only; fix regressions in-place if found)

- [ ] `make tests` (race) passes across all packages (fall back to `go test -race ./...` if `devbox`
      is unavailable, as prior plans did).
- [ ] `make build` produces `./bin/grafanapi` on darwin/arm64; `grafanapi session --help`, `session
      refresh --help` (`--all`/`--due`), and `session keepalive install|status|uninstall --help`
      (`--interval`/`-o`) all render with the documented flags.
- [ ] `make lint` passes with **no new findings** vs the 14-finding baseline (5 gosec, 3 govet, 1
      nolintlint, 5 staticcheck); diff `golangci-lint run -c .golangci.yaml ./...` and annotate any
      unavoidable new finding (e.g. gosec G204 on the `launchctl` exec, or the new cgo) with a
      justified `//nolint` + comment recorded here.
- [ ] `goreleaser check` passes.
- [ ] **Reference docs (MANDATORY):** `make cli-reference` (new `session*` pages + `grafanapi.md`
      index), `make config-reference` (picks up `live-window`), `make env-var-reference`; then `make
      reference-drift` passes with the regenerated files staged. If `devbox` is unavailable, run the
      underlying `go run scripts/*-reference/*.go ...` directly.
- [ ] `go clean -testcache && go test -race ./...` once more — must pass before the docs task.

### Task 11 — Update documentation and complete the plan

**Files:**
- Modify: `README.md`, `docs/guides/index.md`, `skill/grafanapi/SKILL.md`, `AGENTS.md`
- Create: `docs/guides/keep-sessions-alive.md`
- Regenerate: `docs/reference/**` (done in Task 10; re-confirm no drift — includes the `live-window`
  config-reference entry)
- Move: this plan → `docs/plans/completed/20260723-session-keepalive.md` (via `git mv`)

- [ ] `README.md`: add `config set ...live-window 12h`, `session refresh --all`, and `session
      keepalive install` examples.
- [ ] New `docs/guides/keep-sessions-alive.md`: why proactive rotation is needed
      (`login_maximum_inactive_lifetime`), opting a context in with `live-window`, `refresh`
      (unconditional vs `--due`), `keepalive install/status/uninstall`, the LaunchAgent + log location,
      and the caveats below.
- [ ] `skill/grafanapi/SKILL.md`: add a "Keep sessions alive" section — `live-window` opt-in,
      `session refresh` (exit 2 = dead ⇒ `login update`) and `--due`, `keepalive install/status/
      uninstall` — plus the **caveats** (locked):
      - The **30-day `login_maximum_lifetime` hard cap** still applies: keepalive extends inactivity
        indefinitely but a session must be re-established roughly monthly via `grafanapi login update`.
      - **Shared-cookie chains can race a browser** using the same `grafana_session`; recommend
        logging in from a private/incognito window so the CLI owns a dedicated cookie chain.
      - keepalive **requires the user to be logged into macOS** (GUI-domain LaunchAgent + Keychain
        availability); it does not run while logged out.
      - The keepalive **log never contains a cookie value** (only ✔/✘ status lines).
- [ ] `docs/guides/index.md`: add a card for the new guide.
- [ ] `AGENTS.md`: add the `session` command group; the `internal/launchd/` package (plist gen,
      binary-path resolution, `launchctl` seam, `Inspect`); the `live-window` field + `--due` scheduler
      model; `SessionSource.Refresh`; and `keychain.Store.ModifiedAt` as the last-rotation source.
- [ ] Confirm the `configuration` reference documents `live-window` (regenerated in Task 10).
- [ ] Run `make reference` / `make reference-drift`; confirm zero drift.
- [ ] Add a short Review section (what changed, deviations), then `git mv` this plan to
      `docs/plans/completed/`.
- [ ] Run `make all` (lint, tests, build, docs) — must pass (fall back to underlying commands if
      `devbox`/`mkdocs` unavailable).

## Verify

Acceptance criteria (verified in Task 10 unless noted):

- `SessionSource.Refresh` force-rotates every call, advances `gen` by exactly 1 per success, joins
  (never duplicates) an in-flight rotation, persists to the Keychain.
- `live-window` is settable via `config set`, validated `[1m, 6d]` when present, and absent means
  "not opted in"; `config check` still exits 0.
- `keychain.Store.ModifiedAt` returns the item's last-write time (updated on every persist) without a
  prompt, or `ErrNotFound`; fakes support injected mtimes.
- `session refresh` (current) prints `✔ refreshed session for context X`, exits 0, prints **no
  cookie**; a rejection exits **2**; `--all` attempts every cookie-bearing context and exits 2 iff any
  was auth-rejected; **`--due`** refreshes only contexts whose `live-window` is set and whose last
  rotation is older than the window, skipping unset/fresh/no-cookie and warning (not failing) on an
  invalid window.
- `session keepalive install` errors when **no** context opts in; otherwise writes a valid,
  XML-escaped plist (byte-equal to golden for the default spec) running `session refresh --due`,
  derives StartInterval from the min window (`min/2` clamped `[15m,12h]`) unless `--interval` overrides
  (validated `[1m,6d]`), targets the stable Homebrew symlink when applicable, is idempotent, and
  degrades gracefully if bootstrap fails. `status` reports installed/loaded/interval/binary + log tail
  (no cookie); `uninstall` removes the plist and is idempotent.
- `config check` renders the keep-alive section (agent installed/loaded, interval/binary, per-context
  `live-window` + last-rotation age), degrading launchd/keychain errors to "unknown" and **always
  exiting 0**.
- **No test invokes real `launchctl`/`launchd` or the real Keychain.** `make tests`/`make build`/`make
  lint` (no new findings vs the 14-finding baseline)/`goreleaser check`/`make reference-drift` all
  pass; new CLI pages + the `live-window` config-reference entry are generated and committed.

## Post-Completion

These require the user (a real macOS machine with a live, authenticated context) and cannot be safely
automated. **Never record a real hostname, cookie value, or organization/company name** — use
`$GRAFANA_TEST_SERVER` and "a test context" throughout.

- **Live end-to-end verification (real launchd + real Keychain mtime, short cadence):**
  1. On an authenticated context, `grafanapi config set contexts.<name>.grafana.live-window 12h`, then
     `grafanapi config check` → expect the Keep-alive section to show `live-window: 12h` and a
     last-rotation age, LaunchAgent "not installed".
  2. `grafanapi session refresh` → `✔ refreshed session for context <name>`, exit 0; re-run `config
     check` → the last-rotation age resets to seconds (proves `ModifiedAt` tracks the persist).
  3. `grafanapi session refresh --due` immediately after → prints "nothing due" (window fresh), exit 0.
  4. `grafanapi session keepalive install --interval 1m` (deliberately short, for observation) →
     ✔ install; plist at `~/Library/LaunchAgents/io.github.avitsrimer.grafanapi.keepalive.plist` with
     `ProgramArguments` ending `session refresh --due`.
  5. `grafanapi session keepalive status` → installed: yes, loaded: yes, interval `1m`, resolved
     binary path. Then temporarily lower `live-window` (e.g. `2m`), wait, and `tail
     ~/Library/Logs/grafanapi/keepalive.log` → expect a fresh `✔ refreshed session ...` line and **no
     cookie value** anywhere in it.
  6. `grafanapi session keepalive uninstall` → ✔; confirm the plist is gone and `launchctl print
     gui/$(id -u)/io.github.avitsrimer.grafanapi.keepalive` reports it unloaded; re-running succeeds
     (idempotent). Restore a sane `live-window`/interval (`keepalive install` with default) if keeping
     it.
- **Homebrew-upgrade survival (optional, Homebrew installs):** confirm `status`'s reported binary path
  is the stable `/opt/homebrew/bin/grafanapi`, not a versioned `.../Cellar/...` path.
- **Keychain "Allow" prompt:** the first *data* read after a rebuild triggers the macOS "Allow / Always
  Allow" dialog (attributes-only `ModifiedAt` reads do **not**); choose "Always Allow" so scheduled
  rotations persist.
- **Release note (ships in the next release):** add a changelog entry — "Added per-context
  `live-window` opt-in and `grafanapi session refresh` / `session keepalive install|status|uninstall`
  for proactive, scheduled session rotation via a macOS LaunchAgent" — when the next version is cut.
