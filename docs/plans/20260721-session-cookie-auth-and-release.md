# Session-cookie authentication, `login` command, and macOS release pipeline

## Overview

**What:** Replace every existing authentication method in `grafanapi` (API token / Bearer,
basic-auth user+password, and their env vars) with a single supported mechanism: the Grafana
**session cookie** (`Cookie: grafana_session=<value>`) attached to every outbound HTTP request on
both transport paths (the `grafana-openapi-client-go` client and the `k8s.io/client-go` dynamic
REST client). Add a `login` command (and `login update` subcommand) that prompts for the cookie
no-echo, validates it against a live Grafana instance, stores non-secret context data in the config
file, and stores the cookie in the macOS Keychain via hand-rolled cgo (no daemon). Add a clear
"session is stale — run `grafanapi login update`" error path on `401`. Finally, rewrite the release
pipeline (GoReleaser + GitHub Actions) to produce a signed-off darwin/arm64 Homebrew cask, modeled
on the sibling `jenkins-cli` project.

**Why:** The fork targets a single operator's macOS workflow against Grafana instances that
authenticate browser sessions via the `grafana_session` cookie. Service-account tokens and basic
auth are not available/desired in this environment. Secrets must never live in the plaintext config
file; the Keychain provides OS-level protection with silent same-binary reads (ACL-only design).

**Integration:** Reuses the existing kubectl-style context machinery in `internal/config`
(server URL, org-id/stack-id, TLS stay in the config file). Only the secret moves out to the
Keychain. The cookie is resolved into an in-memory-only field at config-load time and consumed by
the existing client constructors. No parallel "profile" system is introduced.

## Context (from discovery)

### Auth injection points (all must switch to the cookie)

1. **k8s dynamic REST path** — `internal/config/rest.go`, `NewNamespacedRESTConfig`. Currently sets
   `rcfg.BearerToken` (from `APIToken`) or `rcfg.Username`/`rcfg.Password`. `rest.Config` exposes
   `WrapTransport transport.WrapperFunc` (confirmed at `k8s.io/client-go/rest/config.go`) — the
   idiomatic way to inject a header on every request. Consumed by `internal/resources/dynamic`,
   `internal/resources/discovery`, `internal/resources/remote/{pusher,puller,deleter}.go`.
2. **openapi client path** — `internal/grafana/client.go`, `ClientFromContext`. Currently sets
   `cfg.BasicAuth` / `cfg.APIKey`. `goapi.TransportConfig` (module cache:
   `grafana-openapi-client-go@.../client/grafana_http_api_client.go:173`) exposes
   `HTTPHeaders map[string]string` — set `{"Cookie": "grafana_session=<value>"}` there. Used by
   `grafana.GetVersion` (config check).
3. **serve reverse-proxy path** — `internal/server/grafana/requests.go`, `AuthenticateRequest`.
   Currently `SetBasicAuth` / `Authorization: Bearer`. Set the `Cookie` header instead.
4. **stack-id discovery** — `internal/config/stack_id.go`, `DiscoverStackID` (GET `/bootdata`).
   Must attach the cookie so discovery works when the endpoint requires an authenticated session.

### Config

- `internal/config/types.go` — `GrafanaConfig` holds `Server`, `User`, `Password`, `APIToken`
  (all env-tagged), `OrgID`, `StackID`, `TLS`. `User`/`Password`/`APIToken` and their
  `GRAFANA_USER`/`GRAFANA_PASSWORD`/`GRAFANA_TOKEN` env tags must be removed. `IsEmpty()` uses
  struct equality (`grafana == GrafanaConfig{}`) — must stay comparable.
- `internal/config/loader.go` — `Load`/`Write`, XDG location `grafanapi/config.yaml`, env var
  `GRAFANAPI_CONFIG`, `0600` perms. Length-preserving secret redaction via
  `secrets.RedactYAMLSecrets` keyed off `datapolicy:"secret"` tags.
- `cmd/grafanapi/config/command.go` — `Options.loadConfigTolerant` applies env overrides via
  `caarlos0/env`; `LoadConfig`/`LoadRESTConfig` add validation. `config check` builds a
  `NamespacedRESTConfig` per context and calls discovery + `grafana.GetVersion`.
- `internal/config/editor.go` — `SetValue`/`UnsetValue` (used by `config set`/`unset`); dot-path
  setters must no longer accept the removed secret fields.

### Error handling

- `cmd/grafanapi/fail/convert.go` — `convertAPIErrors` already switches on
  `k8sapi.IsUnauthorized(statusErr)`; this is the central hook for the k8s path 401. `DetailedError`
  (`cmd/grafanapi/fail/detailed.go`) carries `Summary`, `Suggestions`, `ExitCode *int`, `Parent`.
  `main.handleError` renders it and exits with `*ExitCode` (default 1).

### Docs / reference generation (auto-generated, drift-checked by CI)

- `scripts/env-vars-reference/main.go` reflects over `config.Config` `env` tags → removing the three
  env tags automatically drops them from `docs/reference/environment-variables/index.md`.
- `scripts/config-reference/main.go` → `docs/reference/configuration/index.md`.
- `scripts/cmd-reference/main.go` → CLI reference (will pick up the new `login` command).
- Hand-written: `docs/configuration.md`, `README.md`, `AGENTS.md` (symlinked as `CLAUDE.md`).
- `make reference-drift` fails CI if generated docs are stale — regeneration is mandatory.

### Release / CI (baseline to rewrite)

- `.goreleaser.yaml` — multi-OS (linux/windows/darwin), `CGO_ENABLED=0`, tar.gz/zip, no brew.
- `.github/workflows/release.yaml` — `ubuntu-latest`, devbox + `goreleaser release`, plus docs
  build/publish jobs. `publish-docs.yaml` handles docs on `workflow_dispatch` independently.
- `.github/workflows/ci.yaml` — lint (`make lint`), tests (`make tests`), docs drift (`make docs`
  / `make cli-reference`) on `ubuntu-latest`.
- `Makefile` — builds to `./bin/grafanapi`; `ldflags` inject `main.version/commit/date`.

### GitHub remote (for homepage URLs)

`git remote -v` → `origin git@github.com:avitsrimer/grafanapi.git`. So:
owner **`avitsrimer`**, repo **`grafanapi`**, homepage **`https://github.com/avitsrimer/grafanapi`**,
Homebrew tap repo **`avitsrimer/homebrew-apps`** (directory `Casks`, branch `main`).

Note: the Go **module path stays `github.com/grafana/grafanapi`** (upstream fork identity) while the
repo/homepage/tap use the `avitsrimer` owner. This mismatch is **intentional** — do not "fix" the
homepage or ldflags package paths to match the module path.

### Dependencies

`go.mod` already lists `golang.org/x/term` and `golang.org/x/sys` as indirect and
`golang.org/x/sync` direct. `go mod tidy` will promote term/sys to direct once imported. No
third-party keyring library is added (hand-rolled cgo, matching the sibling project).

## Development Approach

- **Testing approach: Regular** — write the implementation for each task first, then its tests,
  matching repo conventions (`testify` assert/require, table-driven cases, `testdata/` fixtures).
- Complete each task **fully** before starting the next. Every task MUST include new/updated tests.
- **All tests must pass** (`make tests`, race-enabled) before moving to the next task.
- If scope changes mid-implementation, **update this plan file** (add ➕ tasks, mark ⚠️ blockers)
  before continuing.
- Keep changes minimal and elegant: reuse the context/config machinery; do not invent a parallel
  profile store. Cross-compilation to linux must stay green via a `//go:build !darwin` stub.

## Testing Strategy

- **Unit (platform-neutral):** cookie header injection into `rest.Config` (via `WrapTransport`
  round-trip against an `httptest.Server`), openapi `HTTPHeaders`, serve-proxy header, bootdata
  cookie attachment, config parse/validate without secret fields, `SetValue`/`UnsetValue` rejecting
  removed fields, `login` prompting/validation flow (injected `prompter` + `httptest.Server`
  returning 200/401), stale-session error translation, keychain account/service naming.
- **Keychain cgo (`keychain_darwin.go`):** exercised only on darwin via a build-tagged test that
  writes/reads/deletes a throwaway item under a test-only service name and cleans up; skipped
  automatically on non-darwin. The non-darwin stub has a test asserting it returns
  "unsupported platform".
- **Prompter / TTY:** production `ttyPrompter` is not unit-tested directly (needs a TTY); logic is
  tested through the injectable `prompter` interface with fakes.
- **Fixtures:** update `internal/config/testdata/*.yaml` and `cmd/grafanapi/config/testdata/*.yaml`
  to drop `token`/`user`/`password` keys; add fixtures asserting those keys are now rejected/ignored.
- **No live Grafana in unit tests** — all validation calls hit an `httptest.Server`. Live E2E is a
  Post-Completion manual step using `$GRAFANA_TEST_SERVER` and a keychain-stored test session.

## Progress Tracking

- Mark `- [x]` immediately upon completing each checkbox.
- Prefix discovered-mid-work tasks with ➕ and append them in-place.
- Prefix blockers with ⚠️ and stop to re-plan when one is hit.

## Solution Overview

**Architecture chosen:**

- **Secret storage:** macOS Keychain generic-password items, one per context. Service
  `"grafanapi"`, account `"grafanapi:<context-name>"`, value = raw cookie bytes. ACL-only design
  (no `kSecAttrAccessControl` / `kSecAttrAccessible*`), so the login keychain authorizes reads by
  the creating binary's ad-hoc cdhash — same-binary reads are silent, a rebuilt binary re-prompts
  "Allow". **No credential-agent daemon** (a per-invocation direct call is simple and sufficient;
  the TTL/daemon complexity from `jcli` is not justified without a re-read-heavy workload).
- **Config = non-secret only:** `GrafanaConfig` keeps `Server`, `OrgID`, `StackID`, `TLS`. A new
  in-memory-only field `SessionCookie string` (`json:"-" yaml:"-"`) carries the resolved cookie; it
  is never serialized. A credential-resolution step loads it from the Keychain into the current
  context at config-load time.
- **Transport injection:** k8s path uses `rest.Config.WrapTransport`; openapi path uses
  `TransportConfig.HTTPHeaders`; serve proxy and bootdata set the header directly. A single helper
  formats the header value (`grafana_session=<value>`).
- **Login:** cobra command with an injectable `prompter` (production impl reads `/dev/tty` +
  `golang.org/x/term.ReadPassword`). Validate-before-persist: GET `/api/user` with the entered
  cookie; on non-200 (esp. 401) fail without touching config or Keychain.
- **Stale session:** `internal/session` provides `IsUnauthorized`, a `StaleSessionError` type, and
  `VerifyCookie`. Remote command error paths translate a 401 into a re-verified stale-session error;
  `fail/convert.go` renders it (and any raw k8s 401) with the suggestion to run `login update` and a
  dedicated exit code.

**Key design decisions & rationale:**

1. **`WrapTransport` over a custom `rest.Config` field** — idiomatic k8s, avoids forking transport
   construction, composes with existing TLS config.
2. **In-memory `SessionCookie` field on `GrafanaConfig`** — reuses the struct the client
   constructors already receive; `json:"-" yaml:"-"` guarantees it never hits disk and never appears
   in `config view` / reference docs. No env tag ⇒ never settable via env (cookie must never be a
   flag or env var).
3. **No daemon** — deliberate simplification vs. `jcli`; documented in this plan.
4. **Central 401 rendering in `fail/convert.go` + command-level re-verification** — guarantees the
   stale-session message appears for *any* 401 while still performing the required GET `/api/user`
   staleness check where a live context is available.

## Technical Details

### Cookie header

**Import-cycle constraint:** `internal/session` imports `internal/config` (its `VerifyCookie` takes
`*config.Context`). Therefore package `config` — which includes `rest.go` and `stack_id.go` — must
**NOT** import `internal/session`. The cookie name constant and header formatter live in **package
`config`** (leaf, no session dependency); `internal/session`, `internal/grafana`, and
`internal/server/grafana` import them from `config`.

- Constant `config.SessionCookieName = "grafana_session"`.
- Helper `config.CookieHeaderValue(cookie string) string` → `"grafana_session=" + cookie`.
- Attached as HTTP header `Cookie`.
- `internal/session` keeps only the config-dependent pieces (`VerifyCookie`, `StaleSessionError`,
  `IsUnauthorized`) and re-uses `config.CookieHeaderValue`.

### Keychain item spec

| Attribute            | Value                                             |
|----------------------|---------------------------------------------------|
| Class                | `kSecClassGenericPassword`                        |
| `kSecAttrService`    | `"grafanapi"`                                     |
| `kSecAttrAccount`    | `"grafanapi:" + contextName`                      |
| `kSecValueData`      | raw cookie bytes (no JSON wrapper)                |
| Access control       | none (ACL-only; login keychain)                   |

Interface (`internal/keychain/keychain.go`):

```go
type Store interface {
    Set(account, secret string) error
    Get(account string) (string, error) // returns ErrNotFound sentinel when absent
    Delete(account string) error
}
```

- `Account(contextName string) string` → `"grafanapi:" + contextName`.
- `const Service = "grafanapi"`.
- `var ErrNotFound = errors.New("keychain: item not found")` (maps from `errSecItemNotFound` -25300).
- `NewStore() Store` returns the darwin cgo impl or the stub depending on build tag.

### Credential resolution

- `internal/config` gains an override/helper `ResolveSessionCookie(store keychain.Store)` that, for
  the **current** context, calls `store.Get(keychain.Account(name))` and populates
  `ctx.Grafana.SessionCookie`, ignoring `ErrNotFound` (leaves it empty). Wired into
  `Options.LoadConfig`/`LoadRESTConfig`. `config check` resolves per-context as it iterates.

### Request flow (get/list/pull/push/delete)

```
LoadRESTConfig → resolve cookie from Keychain → NewNamespacedRESTConfig
  → rest.Config.WrapTransport adds "Cookie: grafana_session=<v>"
  → dynamic client call → on 401 the raw k8s StatusError bubbles to Cobra
  → main.handleError → fail.ErrorToDetailedError (convertAPIErrors maps IsUnauthorized)
  → "Grafana session is stale — run grafanapi login update", exit code 2
```

### Error types (`internal/session/errors.go`)

```go
var ErrUnauthorized = errors.New("unauthorized")            // wraps low-level 401s
type StaleSessionError struct{ Context string; Parent error } // Error() mentions login update
func IsUnauthorized(err error) bool  // true for k8s IsUnauthorized, openapi 401, ErrUnauthorized
func VerifyCookie(ctx context.Context, gCtx *config.Context) error // GET /api/user; 200→nil, 401→ErrUnauthorized
```

`VerifyCookie` builds its request cookie via `config.CookieHeaderValue` (no `session`→`config`→`session`
cycle: only `session` imports `config`). There is **no** `TranslateUnauthorized` and **no** per-command
re-verification — commands return raw errors and `fail/convert.go` does the mapping centrally.

`fail/convert.go`: the **single** place 401s become user-facing. `convertAPIErrors` already switches
on `k8sapi.IsUnauthorized(statusErr)` — extend that branch (and the openapi-path 401) to emit
`DetailedError{Summary:"Grafana session is stale or unauthorized", Suggestions:["Run: grafanapi
login update"], ExitCode: ptr(2), Parent: err}`. Errors bubble from each command straight to Cobra →
`main.handleError` → `fail.ErrorToDetailedError`, so no per-command wiring and no extra GET
`/api/user` round-trip are needed (matches the jcli reference: 401 maps directly to an auth error).
`StaleSessionError` is only produced by `login`/`login update`'s own validation path and is likewise
rendered here.

### Login command surface

```
grafanapi login [--server URL] [--org-id N | --stack-id N] [--context NAME]
grafanapi login update [--context NAME]
```

- `login`: prompt server (echoed) unless `--server`; prompt cookie (no-echo). Optional `--org-id`
  (on-prem) — if neither `--org-id` nor `--stack-id` given, attempt `DiscoverStackID` using the
  entered cookie. Validate GET `/api/user`. Persist context (server/org-id/stack-id/TLS untouched
  if pre-existing) via `config.Write`; store cookie in Keychain. Make the context current if none.
- `login update`: load current (or `--context`) context; **do not** re-ask server; prompt cookie
  only; validate against the stored server; overwrite the Keychain item.
- The cookie is **never** accepted via flag or env var.

## Implementation Steps

### Task 1 — Keychain package (cgo + stub + interface)

**Files:**
- Create: `internal/keychain/keychain.go` (interface, `Service`, `Account`, `ErrNotFound`, `NewStore`)
- Create: `internal/keychain/keychain_darwin.go` (`//go:build darwin`, cgo Security.framework)
- Create: `internal/keychain/keychain_other.go` (`//go:build !darwin`, unsupported-platform stub)
- Create: `internal/keychain/keychain_darwin_test.go` (`//go:build darwin`)
- Create: `internal/keychain/keychain_other_test.go` (`//go:build !darwin`)

- [x] Define `Store` interface, `Service` const, `Account(name)`, `ErrNotFound`, `NewStore()`.
- [x] Implement darwin cgo `Set`/`Get`/`Delete` via `SecItemAdd`/`SecItemCopyMatching`/`SecItemDelete`
      (delete-then-add on `Set`; ACL-only, no accessibility attrs; map -25300 → `ErrNotFound`).
- [x] Implement `!darwin` stub returning `errors.New("keychain: unsupported platform")` for all ops.
- [x] Follow the repo Go symbol-ordering convention (consts, vars, funcs, types, methods).
- [x] Write tests for darwin success cases: Set→Get round-trips, Set overwrites, Delete removes,
      Get-after-Delete → `ErrNotFound`; use a test-only service/account and clean up in `t.Cleanup`.
- [x] Write tests for error cases: `!darwin` stub returns unsupported-platform for Set/Get/Delete;
      darwin Get on a never-set account returns `ErrNotFound`.
- [x] Run tests — must pass before next task (also verify `GOOS=linux CGO_ENABLED=0 go build ./...`).

### Task 2 — Cookie header helper (package `config`) + `internal/session` (verify, error types)

**Files:**
- Create: `internal/config/cookie.go` (`SessionCookieName`, `CookieHeaderValue`) — leaf, no `session` import
- Create: `internal/config/cookie_test.go`
- Create: `internal/session/session.go` (`VerifyCookie`)
- Create: `internal/session/errors.go` (`ErrUnauthorized`, `StaleSessionError`, `IsUnauthorized`)
- Create: `internal/session/session_test.go`
- Create: `internal/session/errors_test.go`

- [x] In package `config`, add `SessionCookieName = "grafana_session"` and
      `CookieHeaderValue(cookie string) string` → `"grafana_session=<cookie>"`. **Do not** import
      `internal/session` from `config` (would create `config`→`session`→`config`).
- [x] Implement `session.VerifyCookie` doing GET `/api/user` against `gCtx.Grafana.Server` (honoring
      TLS) with the cookie header built via `config.CookieHeaderValue`; 200→nil, 401→`ErrUnauthorized`,
      other→wrapped error.
- [x] Implement `StaleSessionError` (with `Context`, `Parent`, and `Unwrap`) and `IsUnauthorized`
      (covers `k8sapi.IsUnauthorized`, openapi 401, and `ErrUnauthorized`). **No**
      `TranslateUnauthorized` and **no** per-command re-verification (handled centrally in Task 8).
- [x] Reuse `internal/httputils` transport for the verify client (TLS-aware); no secrets logged.
- [x] Write tests for success cases: `config.CookieHeaderValue` formatting; `VerifyCookie` against an
      `httptest.Server` returning 200 → nil; `IsUnauthorized` true/false table.
- [x] Write tests for error cases: `VerifyCookie` 401 → `ErrUnauthorized`; 500 → wrapped non-nil;
      `StaleSessionError.Error()`/`Unwrap()` behavior.
- [x] Run tests — must pass before next task.
- [x] ➕ `VerifyCookie(ctx, gCtx *config.Context)` reads `gCtx.Grafana.SessionCookie`, which did not
      exist yet on `GrafanaConfig` (it was scheduled for Task 3). Added the field
      (`SessionCookie string` with `json:"-" yaml:"-"`, no env tag) to `internal/config/types.go`
      now as a minimal prerequisite so Task 2 compiles; the legacy `User`/`Password`/`APIToken`
      field removal is left for Task 3 as planned — that task's "add SessionCookie" checkbox will
      find the field already present.

### Task 3 — Remove decommissioned auth from config types & validation

**Files:**
- Modify: `internal/config/types.go` (drop `User`, `Password`, `APIToken` + env tags; add `SessionCookie string` json/yaml `-`)
- Modify: `internal/config/types_test.go`
- Modify: `internal/config/editor_test.go` (add rejection test only)

- [x] Delete `User`/`Password`/`APIToken` fields and their `env`/`datapolicy` tags from
      `GrafanaConfig`; add in-memory `SessionCookie string` with `json:"-" yaml:"-"`.
      (`SessionCookie` was already present from Task 2's ➕ prerequisite; only the three
      legacy fields were removed here.)
- [x] Keep `IsEmpty()` correct (struct stays comparable); ensure `SessionCookie` does not break
      equality semantics used by validation (only ever set alongside `Server`).
- [x] **Verify (no code change):** `editor.go` `SetValue`/`UnsetValue` are reflection/yaml-tag
      driven, so removing the struct fields makes `config set ...grafana.token` fail with
      "unable to locate path" automatically — no edit to `editor.go` needed; just confirm the behavior.
      Confirmed via `Test_SetValue_rejectsRemovedSecretFields` / `Test_UnsetValue_rejectsRemovedSecretFields`.
- [x] Write tests for success cases: parsing a config with only `server`/`org-id`/`stack-id`/`tls`;
      `SessionCookie` never serialized by `config view`.
- [x] Write tests for error cases: `config set ...grafana.token X` (and user/password) is rejected
      with the reflection-driven "unable to locate path" error.
- [x] Run tests — must pass before next task.
- [x] ➕ Removing the three fields broke compilation/tests in packages outside this task's file
      list, as anticipated by the task brief. Minimal fixes applied (Task 4/9 will properly finish
      these):
      - `internal/config/rest.go` (`NewNamespacedRESTConfig`): removed the dead
        `BearerToken`/`Username`/`Password` switch (Task 4 replaces it with `WrapTransport` cookie
        injection).
      - `internal/grafana/client.go` (`ClientFromContext`): removed the dead `BasicAuth`/`APIKey`
        assignment (Task 4 replaces it with `HTTPHeaders` cookie injection).
      - `internal/server/grafana/requests.go` (`AuthenticateRequest`): emptied the dead
        basic/bearer branches into a no-op (Task 4 sets the `Cookie` header here).
      - `internal/config/loader_test.go`: fixed a compile error from the removed `APIToken` field
        and adjusted `TestLoad_DoesNotLeakSecretsOnError` (a pre-existing secret-redaction
        regression suite whose denylist is built from `datapolicy:"secret"`-tagged fields):
        removed the now-moot "bad-token-separator"/"bad-password-indent" cases (they tested
        redaction of the now-deleted `token`/`password` fields — Task 9 deletes/repurposes
        those fixtures), repurposed `valid-config.yaml`/`validation-error.yaml` around the one
        remaining secret field (TLS `key-data`), and dropped the equivalent lines from
        `internal/config/testdata/config.yaml`.
      - `cmd/grafanapi/config/testdata/config.yaml` and `partial-config.yaml`: dropped `token:`
        keys (unknown-field strict-decode failures otherwise).
      - `cmd/grafanapi/config/command_test.go`: dropped `token`/`"**REDACTED**"` expectations from
        `view` output assertions; replaced the `Test_UnsetCommand` scenario (previously unsetting
        `grafana.user`) with `grafana.org-id`; replaced the two `GRAFANA_TOKEN` env-var tests with
        `GRAFANA_ORG_ID` equivalents (the removed field's env tag no longer overrides anything).
      Full fixture/redaction redesign (friendly migration-message for legacy keys, deleting/
      repurposing `bad-*.yaml`) remains Task 9's job per this plan.

### Task 4 — Cookie injection on both transport paths + serve proxy + bootdata

**Files:**
- Modify: `internal/config/rest.go` (replace Bearer/basic-auth with `WrapTransport` cookie header)
- Modify: `internal/config/rest_test.go`
- Modify: `internal/grafana/client.go` (replace `BasicAuth`/`APIKey` with `HTTPHeaders` cookie)
- Modify: `internal/config/stack_id.go` (attach cookie to `/bootdata` request)
- Modify: `internal/config/stack_id_test.go`
- Modify: `internal/server/grafana/requests.go` (`AuthenticateRequest` sets `Cookie` header)

- [ ] In `NewNamespacedRESTConfig`, delete the `BearerToken`/`Username`/`Password` switch; when
      `SessionCookie != ""`, set `rcfg.WrapTransport` to a `transport.WrapperFunc` adding
      `Cookie: grafana_session=<v>` to each request.
- [ ] In `ClientFromContext`, delete `BasicAuth`/`APIKey` assignment; set
      `cfg.HTTPHeaders = map[string]string{"Cookie": config.CookieHeaderValue(...)}` when present
      (keep `OrgID` passthrough). `rest.go`/`stack_id.go` (in package `config`) call
      `CookieHeaderValue` directly — no `session` import from `config`.
- [ ] Attach the cookie header to the `/bootdata` request in `DiscoverStackID`; update
      `AuthenticateRequest` to set the `Cookie` header (remove basic/bearer branches).
- [ ] Write tests for success cases: `httptest.Server` asserts the inbound `Cookie` header on the
      k8s path (build a dynamic/discovery call or exercise the wrapped RoundTripper directly) and on
      the openapi path via `grafana.GetVersion`; bootdata test asserts the cookie is sent.
- [ ] Write tests for error cases: no cookie present → no `Cookie` header added (unauthenticated
      request still well-formed); TLS config still applied alongside the wrapper.
- [ ] Run tests — must pass before next task.

### Task 5 — Credential resolution wired into config loading

**Files:**
- Create: `internal/config/credentials.go` (`ResolveSessionCookie(store) Override`)
- Create: `internal/config/credentials_test.go`
- Modify: `cmd/grafanapi/config/command.go` (`LoadConfig`/`LoadRESTConfig` resolve current context; `config check` resolves per-context)
- Modify: `cmd/grafanapi/config/command_test.go`

- [ ] Add `ResolveSessionCookie` that populates the current context's `SessionCookie` from the
      Keychain (`ErrNotFound` → leave empty, no error).
- [ ] **Ordering (critical):** `loader.go` applies overrides in slice order. The
      `ResolveSessionCookie` override MUST run **before** the validator override in `LoadConfig`
      (because `Validate` → `validateNamespace` → `DiscoverStackID` needs the cookie), and in
      `LoadRESTConfig` the cookie must be resolved **before** `cfg.GetCurrentContext().ToRESTConfig(ctx)`.
- [ ] Wire resolution into `LoadConfig`/`LoadRESTConfig`; in `config check`, resolve each context's
      cookie before its connectivity/version probes.
- [ ] Inject the `keychain.Store` (use `keychain.NewStore()`; allow a test seam via a package-level
      var or parameter so tests pass a fake store).
- [ ] Write tests for success cases: fake store returns a cookie → `SessionCookie` populated and the
      resulting REST/openapi clients carry the header.
- [ ] Write tests for error cases: fake store returns `ErrNotFound` → load succeeds with empty
      cookie; a real store error surfaces as a load error.
- [ ] Run tests — must pass before next task.

### Task 6 — `login` command with prompter and validate-before-persist

**Files:**
- Create: `cmd/grafanapi/login/command.go` (`Command()`, `login` RunE)
- Create: `cmd/grafanapi/login/prompt.go` (`prompter` interface, `ttyPrompter`)
- Create: `cmd/grafanapi/login/command_test.go`
- Create: `cmd/grafanapi/login/testdata/` (fixtures as needed)
- Modify: `cmd/grafanapi/root/command.go` (register `login`)

- [ ] Implement `prompter` (`promptLine`, `promptSecret`) with `ttyPrompter` opening `/dev/tty` and
      `term.ReadPassword` for no-echo; injectable for tests.
- [ ] Implement `login`: resolve context name; prompt server unless `--server`; prompt cookie
      no-echo; optional `--org-id`/`--stack-id` (else attempt `DiscoverStackID` with the cookie);
      validate via `session.VerifyCookie`; on success `config.Write` the context (make current if
      none) then `keychain.Set(Account(name), cookie)`; on validation failure persist nothing.
- [ ] Register the command in `root.Command`; ensure the cookie is never a flag/env var.
- [ ] Write tests for success cases: fake prompter + `httptest.Server` 200 → context written
      (no secret in file) and fake keychain received the cookie; `--server` skips the URL prompt.
- [ ] Write tests for error cases: server returns 401 → error, config file and keychain untouched;
      empty cookie input → error; missing server with no `--server` and empty prompt → error.
- [ ] Run tests — must pass before next task.

### Task 7 — `login update` subcommand

**Files:**
- Create: `cmd/grafanapi/login/update.go` (`updateCmd`)
- Modify: `cmd/grafanapi/login/command.go` (attach `update` subcommand)
- Modify: `cmd/grafanapi/login/command_test.go` (add update cases)

- [ ] Implement `login update`: load current (or `--context`) context; error if the context/server
      is missing; prompt cookie only (no server prompt); validate against the stored server; on
      success overwrite the Keychain item; do not modify the config file.
- [ ] Reuse the shared `prompter` and validation helpers from Task 6.
- [ ] Ensure `--context` selects a non-current context correctly.
- [ ] Write tests for success cases: fake prompter + 200 → keychain overwritten, config unchanged,
      server not re-prompted.
- [ ] Write tests for error cases: unknown/empty context → error; 401 on validation → keychain not
      overwritten.
- [ ] Run tests — must pass before next task.

### Task 8 — Centralized stale-session 401 rendering (fail/convert.go only)

**Files:**
- Modify: `cmd/grafanapi/fail/convert.go` (map k8s 401 + openapi 401 + `*session.StaleSessionError` to the stale-session message + exit code 2)
- Modify/Create: `cmd/grafanapi/fail/convert_test.go`

No per-command wiring: `cmd/grafanapi/resources/onerror.go` is the per-resource `--on-error` mode
(unrelated) and there is **no** shared choke point — each command returns its error straight to
Cobra, which bubbles to `main.handleError` → `fail.ErrorToDetailedError`. So all 401 mapping lives in
`convertAPIErrors`. `serve.go` keeps its existing proxy `401`→HTML handling untouched. Matches the
jcli reference: 401 maps directly to an auth error with no re-verification round-trip.

- [ ] In `convertAPIErrors`, extend the existing `k8sapi.IsUnauthorized(statusErr)` branch to emit
      `Summary:"Grafana session is stale or unauthorized"`, `Suggestions:["Run: grafanapi login
      update"]`, and `ExitCode: ptr(2)` (keep `IsForbidden` as its own permission message).
- [ ] Add handling for the openapi-path 401 (config check / `grafana.GetVersion`) — detect the
      runtime `*runtime.APIError`/401 shape via `session.IsUnauthorized` and render the same
      stale-session `DetailedError`.
- [ ] Add a `*session.StaleSessionError` branch (produced only by `login`/`login update` validation)
      rendering the same message + exit code 2.
- [ ] Write tests for success cases: non-401 errors (404/forbidden/network) pass through with their
      existing messages unchanged.
- [ ] Write tests for error cases: a simulated k8s 401 `StatusError` → `DetailedError` with the
      `login update` suggestion and exit code 2; an openapi 401 and a `*StaleSessionError` render the same.
- [ ] Run tests — must pass before next task.

### Task 9 — Config/test fixture & redaction cleanup

**Legacy-config decision (resolves the strict-decode contradiction):** `internal/format/codec.go:82`
decodes YAML with `yaml.Strict()`, so a config still containing `token:`/`user:`/`password:` keys
produces a **hard `UnmarshalError`** (unknown fields) once the struct fields are removed — silently
ignoring them is impossible. Chosen path **(a)**: accept the hard error and add a **friendly
migration message** in `convertConfigErrors` (`cmd/grafanapi/fail/convert.go`) so that an
`UnmarshalError` whose underlying cause references the removed keys tells the user those auth fields
are gone and to run `grafanapi login`. This keeps `GrafanaConfig` clean (no deprecated no-op fields)
and reuses the existing error-rendering pipeline.

**Redactor note (no code change):** `internal/secrets/redactor.go` and `internal/secrets/yaml.go`
are entirely `datapolicy:"secret"`-tag driven. Removing the token/password fields (whose tags are
gone) needs **zero** change to the redactor code itself — only its *tests* change. The exec agent
should not hunt for redactor logic to edit.

**Files:**
- Modify: `internal/config/testdata/valid-config.yaml`, `config.yaml`, `validation-error.yaml`
- Delete: `internal/config/testdata/bad-password-indent.yaml`, `internal/config/testdata/bad-token-separator.yaml` (repurpose to a non-secret parse-error fixture)
- Modify: `internal/config/loader_test.go`
- Modify: `cmd/grafanapi/fail/convert.go` (`convertConfigErrors`: legacy-auth-key migration message)
- Modify: `cmd/grafanapi/fail/convert_test.go`
- Modify: `internal/secrets/yaml_test.go`, `internal/secrets/redactor_test.go` (drop token/password cases; keep TLS `key-data` secret coverage) — tests only, no redactor code change
- Modify: `cmd/grafanapi/config/testdata/config.yaml`, `partial-config.yaml`; `cmd/grafanapi/config/command_test.go`

- [ ] Remove `token`/`user`/`password` keys from all config fixtures; retain `server`/`org-id`/
      `stack-id`/`tls`.
- [ ] Replace the password/token parse-error fixtures with an equivalent non-secret parse-error
      fixture so `loader_test.go` still covers annotated parse errors.
- [ ] In `convertConfigErrors`, add a friendly message for the `UnmarshalError` caused by legacy
      `token:`/`user:`/`password:` keys: explain those auth fields were removed and suggest
      `Run: grafanapi login` (best-effort match on the underlying strict-decode "unknown field" text).
- [ ] Update `secrets` **tests** only: `datapolicy:"secret"` now covers just TLS `key-data`; the
      in-memory cookie is never serialized, so never redacted from files. (No redactor code change.)
- [ ] Write tests for success cases: redaction still masks TLS `key-data`; `config view` output
      contains no secret keys.
- [ ] Write tests for error cases: a legacy config containing `token:`/`password:` fails strict
      decode and renders the migration message pointing to `grafanapi login`.
- [ ] Run tests — must pass before next task.

### Task 10 — GoReleaser + release workflow (macOS/arm64 + Homebrew cask)

**Files:**
- Modify: `.goreleaser.yaml` (darwin/arm64 only, `CGO_ENABLED=1`, tar.gz, `homebrew_casks`)
- Modify: `.github/workflows/release.yaml` (macos-latest + goreleaser; keep/relocate docs publishing)
- Modify: `.github/workflows/ci.yaml` (add a linux `CGO_ENABLED=0` cross-build stub-sanity step)
- Modify: `Makefile` (add a `cross-build` target if absent; ensure darwin cgo build works)

- [ ] Rewrite `.goreleaser.yaml`: single build `goos:[darwin] goarch:[arm64]`, `CGO_ENABLED=1`,
      existing `-X main.version/commit/date` ldflags plus `-s -w`; tar.gz archive; add
      `homebrew_casks` → repository `owner: avitsrimer / name: homebrew-apps / branch: main /
      token: "{{ .Env.HOMEBREW_TAP_TOKEN }}"`, `directory: Casks`,
      `homepage: "https://github.com/avitsrimer/grafanapi"`, description, and a post-install hook
      running `xattr -dr com.apple.quarantine` on the staged `grafanapi` binary (unsigned).
- [ ] Rewrite `release.yaml`: trigger on `v*`, `runs-on: macos-latest`, `contents: write`, checkout
      (fetch-depth 0, persist-credentials false), setup-go from `go.mod`, run
      `goreleaser release --clean` with `GITHUB_TOKEN` + `HOMEBREW_TAP_TOKEN` env. Remove the
      release-embedded docs jobs (docs continue via `publish-docs.yaml`); confirm `publish-docs.yaml`
      still deploys docs (add a `release: published` or tag trigger there if docs must ship on
      release).
- [ ] Add a linux stub-sanity CI step (`GOOS=linux CGO_ENABLED=0 go build ./... && go vet ./...`) so
      the `!darwin` keychain stub keeps cross-build green.
- [ ] Write/adjust tests where applicable (workflow/goreleaser are config — validate with
      `goreleaser check` locally; add a Make target or CI step invoking it).
- [ ] Verify `goreleaser check` passes and `goreleaser release --snapshot --clean` builds locally on
      darwin/arm64 (cgo).
- [ ] Run `make tests` — must pass before next task.

### Task 11 — Verify acceptance criteria

**Files:**
- Modify: none (verification only; fix regressions in-place if found)

- [ ] `make tests` (race) passes across all packages.
- [ ] `make build` produces `./bin/grafanapi` on darwin/arm64 with `CGO_ENABLED=1`.
- [ ] `make lint` passes (golangci-lint clean, including cgo files and build-tagged stubs).
- [ ] `GOOS=linux CGO_ENABLED=0 go build ./... && go vet ./...` passes (stub cross-build).
- [ ] `make reference-drift` passes after regenerating (`make docs` / `make reference`) — no
      uncommitted drift in generated reference docs.
- [ ] `goreleaser check` passes.
- [ ] Run full suite once more — must pass before documentation task.

### Task 12 — Update documentation

**Files:**
- Modify: `README.md`, `docs/configuration.md`, `docs/installation.md`, `docs/guides/*` (auth/login flow, Homebrew install)
- Modify: `docs/reference/**` via `make reference` (env-vars, config, CLI reference regenerate)
- Modify: `AGENTS.md` (= `CLAUDE.md` symlink) — auth section, new `login`/`login update`, keychain notes, macOS-only release
- Move: this plan → `docs/plans/completed/20260721-session-cookie-auth-and-release.md`

- [ ] Rewrite auth documentation: session-cookie only; document `login` / `login update`; remove all
      references to API tokens, basic auth, and `GRAFANA_TOKEN`/`GRAFANA_USER`/`GRAFANA_PASSWORD`.
- [ ] Document Homebrew install (`brew install avitsrimer/apps/grafanapi`) and the tarball fallback
      (`xattr -d com.apple.quarantine`); note macOS/arm64-only distribution and Keychain "Allow"
      prompt on first read after a rebuild.
- [ ] Regenerate all reference docs (`make reference`) and confirm no drift.
- [ ] Update `AGENTS.md`/`CLAUDE.md` overview, auth notes, and remove stale token/basic-auth guidance.
- [ ] Add a short review section to this plan (what changed, deviations), then move it to
      `docs/plans/completed/`.
- [ ] Run `make all` (lint, tests, build, docs) — must pass.

## Post-Completion

These require the user and cannot be safely automated by the implementation agents:

- **`HOMEBREW_TAP_TOKEN` GitHub secret:** the release workflow needs a PAT with write access to
  `avitsrimer/homebrew-apps`. GitHub secrets are write-only — the value cannot be read back from the
  sibling `jenkins-cli` repo's Actions config, so it must be provided fresh. The implementation
  agent will *attempt* `gh secret set HOMEBREW_TAP_TOKEN --repo avitsrimer/grafanapi` **only if** a
  suitable PAT is already available locally (e.g. exported in an env var the user points to) and
  `gh` is authenticated for `github.com/avitsrimer`; otherwise the user must run:
  `gh secret set HOMEBREW_TAP_TOKEN --repo avitsrimer/grafanapi` (or set it in repo Settings →
  Secrets → Actions) with a PAT scoped to write the tap repo. **Do not** put the token in any file.
- **Homebrew tap repo:** ensure `avitsrimer/homebrew-apps` exists with a `Casks/` directory on
  `main` (GoReleaser pushes the generated cask there).
- **Cut a release:** push a `vX.Y.Z` tag to trigger the release workflow on `macos-latest`. Verify
  the cask lands in the tap and `brew install avitsrimer/apps/grafanapi` works on a clean machine.
- **Live end-to-end verification (real Grafana):** with `GRAFANA_TEST_SERVER` set to a reachable
  Grafana 12+ instance and a valid browser `grafana_session` cookie to hand:
  1. `grafanapi login --server "$GRAFANA_TEST_SERVER"` → paste the cookie at the no-echo prompt →
     expect success and a Keychain item under service `grafanapi`.
  2. `grafanapi config check` → connectivity online, Grafana version ≥ 12.
  3. `grafanapi resources list dashboards` (or a get/pull) → succeeds using the stored cookie.
  4. Invalidate the session (log out in the browser), rerun a resources command → expect the
     "session is stale — run `grafanapi login update`" error with exit code 2.
  5. `grafanapi login update` → paste a fresh cookie → subsequent commands succeed again.
  (Never record the real hostname or cookie value anywhere in the repo.)
- **Keychain "Allow" prompt:** the first Keychain read after each rebuild triggers a macOS
  "Allow / Always Allow" dialog (ACL bound to the binary's cdhash) — choose "Always Allow" for the
  installed binary.
