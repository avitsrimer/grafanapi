# Automatic `grafana_session` rotation on 401

## Overview

**What:** Make `grafanapi` transparently refresh a stale Grafana session cookie. When any outbound
request receives a `401`, the tool calls `POST /api/user/auth-tokens/rotate` against the context's
server with the *current* cookie, reads the new `grafana_session` value from the response's
`Set-Cookie` header, updates the in-memory cookie, persists it to the macOS Keychain, and retries the
original request **once** with the fresh cookie. All of this is invisible to the user: commands that
would previously have failed with "session is stale — run `grafanapi login update`" now succeed for
as long as the token is still *rotatable* (Grafana keeps accepting a token at the rotate endpoint for
longer than it accepts it on ordinary API calls).

**Why (verified against a real Grafana 12.x):** Grafana ≥10.2 does not rotate session tokens on
ordinary API responses — the browser frontend keeps a session alive by periodically calling
`POST /api/user/auth-tokens/rotate`, whose `Set-Cookie` carries the new `grafana_session` (plus
`grafana_session_expiry`). A token stops authenticating ordinary requests roughly
`token_rotation_interval_minutes` (default 10) after its last rotation, **but the rotate endpoint
still accepts it for longer**: empirically `GET /api/user` returned `401` while
`POST /api/user/auth-tokens/rotate` with the *same* cookie returned `200`. So a "stale" cookie is
usually exchangeable for a fresh one. The session only truly dies at
`login_maximum_inactive_lifetime` / `login_maximum_lifetime_duration` / logout / revocation — then
rotate also returns `401`/`403`, and we fall back to the existing stale-session error path. Without
this feature the operator has to re-run `grafanapi login update` (paste a fresh cookie from the
browser) roughly every 10 minutes of activity, which is untenable for pull/push workflows.

**Integration:** Builds on the completed session-cookie auth rework (see
`docs/plans/completed/20260721-session-cookie-auth-and-release.md`). That work established four
cookie-injection points, an in-memory `GrafanaConfig.SessionCookie`, the `internal/keychain` store,
and centralized `401` rendering in `cmd/grafanapi/fail/convert.go`. This feature adds one shared,
mutable **`SessionSource`** (mutex + generation counter + `Rotate()` method) created at
credential-resolution time and threaded into every transport via a single `WrapWithSession` helper,
replacing the per-path static cookie plumbing. The existing stale-session error path is preserved
untouched as the fallback for a truly-dead session.

## Context (from discovery)

### Cookie-injection points today (all resolved against HEAD of `session-cookie-auth-and-release`)

1. **k8s dynamic REST path** — `internal/config/rest.go`, `NewNamespacedRESTConfig`. When
   `SessionCookie != ""` it sets `rcfg.WrapTransport` to a closure returning a
   `sessionCookieRoundTripper{cookie, next}` (already defined in this file) that clones each request
   and sets `Cookie: grafana_session=<v>` via `config.CookieHeaderValue`. This wrapper sits at the
   transport level, so it already sees raw `*http.Response` 401s before client-go decodes them into
   `k8sapi.StatusError`. Consumed by `internal/resources/{dynamic,discovery,remote/*}` — pusher/puller
   run up to the default 10 concurrent operations via `golang.org/x/sync/errgroup`
   (`internal/resources/remote/puller.go:107`).
2. **openapi client path** — `internal/grafana/client.go`, `ClientFromContext`. Currently sets
   `cfg.HTTPHeaders = {"Cookie": ...}` on `goapi.TransportConfig` — a **static map** applied by the
   vendored `RetryableTransport`. This map cannot see a 401 or use a rotated value, so it must be
   reworked to a transport-level wrapper. `TransportConfig` (vendored
   `grafana-openapi-client-go/client/grafana_http_api_client.go`) also exposes `Client *http.Client`,
   but `newTransportWithConfig` unconditionally overrides `runtime.Transport = retryableTransport`
   (verified), so passing `Client` does **not** control the RoundTripper. `go-openapi` `Runtime.Submit`
   builds `&http.Client{Transport: r.Transport}` (verified at
   `vendor/github.com/go-openapi/runtime/client/runtime.go:445`), so the effective RoundTripper is the
   `*httptransport.Runtime.Transport` field. The clean hook is therefore: build the client with
   `NewHTTPClientWithConfig` (keeps retry, org-id auth, and the custom JSON consumer), then wrap
   `client.Transport.(*httptransport.Runtime).Transport` with our rotating RoundTripper. `401` is not
   in the default `RetryStatusCodes` (`[429, 5xx]`), so it passes straight through to our wrapper.
   Used by `grafana.GetVersion` (config check).
3. **serve reverse-proxy path** — `internal/server/server.go:67-78` (`httputil.ReverseProxy` with
   `Transport: httputils.NewTransport(s.context)` and a `Rewrite` that calls
   `grafana.AuthenticateRequest`) and `internal/server/grafana/requests.go`
   (`AuthenticateAndProxyHandler` builds its own `http.Client{Transport: httputils.NewTransport(cfg)}`
   and calls `AuthenticateRequest`). Both attach the cookie via `AuthenticateRequest`.
4. **stack-id discovery / bootdata** — `internal/config/stack_id.go`, `DiscoverStackID` (GET
   `/bootdata`, 5 s timeout, own `http.Client`, attaches the cookie directly). Takes `GrafanaConfig`
   **by value**, so mutations would not persist. Pre-auth discovery.

### Config / credential resolution

- `internal/config/types.go` — `GrafanaConfig{Server, OrgID, StackID, TLS, SessionCookie}`.
  `SessionCookie` is `json:"-" yaml:"-"` (never serialized, no env tag). `IsEmpty()` uses struct
  equality (`grafana == GrafanaConfig{}`) after zeroing `SessionCookie`, so the struct must stay
  comparable and any new field must be zeroed in `IsEmpty()` too.
- `internal/config/credentials.go` — `ResolveSessionCookie(store) Override` and
  `ResolveContextSessionCookie(store, gCtx)` populate `gCtx.Grafana.SessionCookie` from the Keychain
  (`keychain.ErrNotFound` → leave empty). This is the single credential-resolution seam, wired into
  `Options.LoadConfig`/`LoadRESTConfig` and `config check` (`cmd/grafanapi/config/command.go`).
- `internal/keychain` — `Store{Set,Get,Delete}`; `Set` is documented **atomic** (never delete-then-add)
  and, on darwin, guarded by a 15 s timeout (`keychain_darwin.go:136`). `Account(name)` = `"grafanapi:"+name`.

### Error handling (fallback path, must remain intact)

- `cmd/grafanapi/fail/convert.go` — `convertAPIErrors` maps `k8sapi.IsUnauthorized` and
  `convertSessionErrors` maps a `runtime.APIError` with `Code == 401` (openapi) and
  `session.ErrUnauthorized`, both to `staleSessionError` (summary "Grafana session is stale or
  unauthorized", suggestion `Run: grafanapi login update`, exit code 2). Because our transport returns
  the **original 401 response** when rotation gives up, these existing branches fire unchanged for a
  truly-dead session.

### Import-cycle constraints (hard-won from the previous feature)

- `internal/session` imports `internal/config` (`VerifyCookie` takes `*config.Context`). Therefore
  package `config` must **NOT** import `internal/session`.
- `internal/httputils` imports `internal/config`. Therefore package `config` must **NOT** import
  `internal/httputils` either. `stack_id.go` already demonstrates the workaround: build the rotate
  `http.Transport`/`http.Client` inline from `cfg.TLS.ToStdTLSConfig()`, not via `httputils.NewTransport`.
- `config` already imports `internal/keychain` (in `credentials.go`), so placing the `SessionSource`
  (which persists via `keychain.Store`) in package `config` introduces **no new import edge**.

### Dependencies

`golang.org/x/sync` is already a direct dependency (v0.19.0) — `singleflight` is available. No new
dependency is required for the chosen mutex+generation design (see Solution Overview).

## Development Approach

- **Testing approach: Regular** — write the implementation for each task first, then its tests,
  matching repo conventions (`testify` `assert`/`require`, table-driven cases, `httptest.Server`
  scripting, fake `keychain.Store`, `testdata/` fixtures where relevant).
- Complete each task **fully** before starting the next. Every task MUST include new/updated tests.
- **All tests must pass** (`make tests`, race-enabled) before moving to the next task.
- If scope changes mid-implementation, **update this plan file** (add ➕ tasks, mark ⚠️ blockers)
  before continuing.
- Keep changes minimal and elegant: one `SessionSource`, one `WrapWithSession` helper, reuse the
  existing credential-resolution seam and the existing centralized `401` rendering. No proactive
  timer-based rotation, no rotation for login flows, no new config knobs. Cross-compilation to linux
  must stay green (`GOOS=linux CGO_ENABLED=0`) — nothing added here is platform-specific except that
  the real `keychain.Store` remains darwin-only (already handled by the build-tagged stub).

## Testing Strategy

- **Unit (platform-neutral), package `config`:** `SessionSource` and the rotating RoundTripper are
  exercised entirely against `httptest.Server`s and a fake `keychain.Store` (no cgo, no live Grafana):
  - **Happy path:** first request → `401`; assert the transport then issues
    `POST /api/user/auth-tokens/rotate` carrying the *old* cookie; respond `Set-Cookie:
    grafana_session=<new>`; assert the original request is retried carrying the *new* cookie and
    returns `200`; assert the fake `keychain.Store` received `<new>`.
  - **Single-flight:** N goroutines each drive a request that `401`s at the same time (an atomic
    counter on the httptest handler); assert exactly **one** rotate call happened and every request
    ultimately succeeded with the rotated cookie.
  - **Rotate rejected:** rotate endpoint returns `401` (and, as a separate case, `403`); assert the
    transport gives up and returns the **original** `401` response (no retry), and that the fake
    keychain was **not** written.
  - **Non-rewindable body:** a request whose `Body != nil` and `GetBody == nil` `401`s; assert a
    rotate happened (so the next request would succeed) but the request was **not** retried and the
    original `401` surfaced.
  - **Keychain persist failure after rotation:** fake store's `Set` returns an error; assert a warning
    is emitted to the injected writer (stderr in production) and the request still succeeds with the
    in-memory rotated cookie.
  - **Cookie never logged:** assert no logging round-tripper is installed and the rotate client is
    built without any request-dumping transport (structural assertion / no cookie in any captured
    output).
- **Wiring tests:** k8s path (`rest_test.go`) — drive a dynamic/discovery call or the wrapped
  RoundTripper directly against an `httptest.Server` and assert the inbound `Cookie` header, then the
  rotate-and-retry sequence. openapi path (`internal/grafana`) — `grafana.GetVersion` against an
  `httptest.Server` scripted `401 → rotate → 200`, asserting one rotate and success. serve paths —
  assert the proxy transport is wrapped (cookie present; optional rotate-on-401).
- **Fallback rendering (`cmd/grafanapi/fail/convert_test.go`):** unchanged behavior — a k8s `401`
  `StatusError` and an openapi `401` still render as the stale-session `DetailedError` with exit code
  2 (this is what surfaces when rotation gives up).
- **No live Grafana in unit tests.** Live end-to-end verification is a generic Post-Completion manual
  step. No real hostname or cookie value is ever recorded anywhere.

## Progress Tracking

- Mark `- [x]` immediately upon completing each checkbox.
- Prefix discovered-mid-work tasks with ➕ and append them in-place.
- Prefix blockers with ⚠️ and stop to re-plan when one is hit.

## Solution Overview

**Architecture chosen:**

- **One shared `SessionSource`** (`internal/config/session_source.go`, package `config`): a small
  struct holding a `sync.Mutex`, the current cookie, a `generation uint64`, the immutable
  `server`/`*TLS`/`account`/`keychain.Store`, and an injectable warn writer. It exposes
  `Current() (cookie string, gen uint64)` and `Rotate(ctx, usedGen uint64) (string, error)`. Created
  during credential resolution (`credentials.go`) — exactly when and only when a cookie is loaded from
  the Keychain — and referenced from a new `GrafanaConfig.Session *SessionSource` field
  (`json:"-" yaml:"-"`, no env tag; zeroed in `IsEmpty()`).

- **One `WrapWithSession` helper** on `*GrafanaConfig` (in `session_source.go`): returns a
  `*rotatingRoundTripper{src, next}` when `Session != nil`, the existing `*sessionCookieRoundTripper`
  when only `SessionCookie != ""` (backward-compatible static path, e.g. tests), and `next` unchanged
  otherwise. Every transport path calls this single helper, so cookie injection **and** rotation live
  in one place:
  - k8s: `rest.go` `WrapTransport` → `cfg.Grafana.WrapWithSession(rt)`.
  - openapi: `grafana/client.go` wraps `client.Transport.(*httptransport.Runtime).Transport`.
  - serve: `server.go` and `requests.go` wrap `httputils.NewTransport(...)`.

- **`rotatingRoundTripper`** (`session_source.go`): on each `RoundTrip` it clones the request, sets
  `Cookie: grafana_session=<Current()>`, and delegates to `next`. If the response is `401` and the
  request body is rewindable (`GetBody != nil` or no body), it calls `src.Rotate(ctx, gen)`; on
  success it drains/closes the first response and retries **once** with the new cookie; on rotate
  failure it returns the **original** `401` response so the centralized stale-session rendering fires.
  For a non-rewindable body it still calls `Rotate` (so the *next* request succeeds) but does not
  retry, returning the original `401`.

- **Rotation is single-flight per process via the mutex + generation counter.** `Rotate(ctx, usedGen)`
  takes the lock; if `s.gen > usedGen` another goroutine already rotated, so it returns the current
  cookie with **no network call**; otherwise it performs the rotate POST, and on success updates
  `cookie`/`gen++` and best-effort persists to the Keychain. Concurrent 401ing requests serialize on
  the lock: the first rotates, the rest observe the advanced generation and immediately retry with the
  fresh cookie. The rotate POST uses its **own** `http.Client` (TLS from the context, bounded timeout,
  **not** the rotating transport — no recursion), and never logs the cookie.

**Key design decisions & rationale:**

1. **Mutex + generation counter over `singleflight`.** `singleflight.Group` collapses only
   *temporally overlapping* calls; it does not help the very common sequential case where request A
   rotates, A's flight completes, and request B — which had already passed the injection point holding
   the *old* cookie — then `401`s and would start a *second* redundant rotate (rotating a
   just-rotated token, which competes with itself and can shorten session life). The generation
   counter dedups both concurrent **and** sequential post-rotation 401s: B compares the generation it
   used against the current one, sees it advanced, and simply retries with the current cookie. It is
   also fewer moving parts (no closure/keying) and makes "wait, then retry with the rotated cookie"
   fall out for free. Holding the lock across the rotate network call is intentional and bounded (one
   rotate per ~10 min of session life, capped by the rotate client timeout); this is the same pattern
   `golang.org/x/oauth2`'s `reuseTokenSource` uses for token refresh.

2. **`SessionSource` in package `config`.** It must persist via `keychain.Store` (already imported by
   `config`) and must not import `internal/session` (cycle) or `internal/httputils` (cycle). Placing
   it in `config` and building the rotate transport inline from `cfg.TLS` (exactly as `stack_id.go`
   already does for `/bootdata`) satisfies every constraint with **no new import edge**. Imports used:
   `context`, `crypto/tls`, `crypto/x509`, `errors`, `fmt`, `io`, `net/http`, `net/url`, `os`,
   `strings`, `sync`, `time`, and `internal/keychain` (the TLS-material fix in `types.go` adds
   `crypto/x509` there too).

3. **openapi path reworked to transport-level, dropping `HTTPHeaders`.** The static `HTTPHeaders` map
   is removed; the cookie is injected by the same `rotatingRoundTripper` that wraps the runtime's
   `Transport`, so the openapi path sees 401s and uses the rotated value identically to the k8s path.
   `NewHTTPClientWithConfig` is retained (keeps retry, org-id auth, custom JSON consumer); only the
   runtime's `Transport` field is wrapped. The `*httptransport.Runtime` type assertion is guarded — a
   failure returns an explicit error rather than silently sending unauthenticated requests.

4. **bootdata (`stack_id.go`) is explicitly OUT OF SCOPE for rotation.** It is a pre-auth discovery
   probe called *during* config load/validation (before a coherent `SessionSource` is guaranteed),
   takes `GrafanaConfig` **by value** (mutations could not persist), and is a single non-retryable GET
   whose `401` is already handled gracefully (discovery fails soft; validation falls back to configured
   `OrgID`/`StackID` or surfaces a namespace error). The real resource operations all flow through the
   rotating transport and will rotate on `401`, so bootdata staleness never blocks a command. Wiring
   rotation here would add complexity for no user-visible benefit. Left as the existing static-cookie
   attachment.

5. **Truly-dead session preserves the existing UX.** When rotate returns `401`/`403` (or any error),
   the transport returns the original `401`, which the unchanged `fail/convert.go` renders as
   "Grafana session is stale — run `grafanapi login update`" with exit code 2. No change to the
   error-rendering pipeline is required; a test locks this in.

6. **login / login update untouched.** They store a freshly pasted cookie and validate it via
   `session.VerifyCookie` (its own client), never constructing a rotating transport. No `SessionSource`
   is created for them; nothing to change.

## Technical Details

### Rotate endpoint & cookie extraction

- Request: `POST <server>/api/user/auth-tokens/rotate`, header `Cookie: grafana_session=<current>`,
  empty body. `<server>` is `GrafanaConfig.Server` with any path preserved and
  `/api/user/auth-tokens/rotate` appended (mirror `buildBootdataURL`/`buildUserURL` in
  `stack_id.go`/`session.go`).
- Client: dedicated `http.Client{Timeout: rotateTimeout, Transport: &http.Transport{Proxy:
  http.ProxyFromEnvironment, TLSClientConfig: tlsFrom(cfg.TLS)}}` — never the rotating transport, no
  debug/logging round-tripper. `rotateTimeout` const = `10 * time.Second` (matches
  `session.verifyTimeout`).
- **TLS material (must be complete):** `tlsFrom(cfg.TLS)` must produce a `tls.Config` carrying the
  **full** context TLS material — `RootCAs` built from `CAData` and a client certificate built from
  `CertData`/`KeyData` — in addition to `InsecureSkipVerify`/`ServerName`/`NextProtos`. The existing
  `(*TLS).ToStdTLSConfig` has a `// TODO: CertData, KeyData, CAData` and drops exactly those three, so
  reusing it verbatim would let ordinary k8s requests succeed on an mTLS/custom-CA context (the k8s
  path honors `CertData`/`KeyData`/`CAData`, `rest.go:38-42`) while the rotate handshake silently
  fails and the command degrades to the stale-session error. **Decision (option b):** complete the
  `ToStdTLSConfig` TODO — populate `RootCAs` (`x509.NewCertPool` + `AppendCertsFromPEM(CAData)`) and
  `Certificates` (`tls.X509KeyPair(CertData, KeyData)` when both are set) — so both the rotate client
  and any future direct client get correct TLS. This lives in package `config` (only `crypto/tls`,
  `crypto/x509`), no new import edge. `stack_id.go`'s bootdata client has the same latent gap; fold in
  the fix there **only if trivial** (it already calls `ToStdTLSConfig`, so completing the helper fixes
  it for free) — but do not expand bootdata's scope beyond inheriting the corrected helper.
- **Rotate context:** derive the rotate request's context via
  `context.WithoutCancel(req.Context())` (Go ≥1.21; module is on Go 1.26) wrapped in
  `context.WithTimeout(..., rotateTimeout)`. Trade-off: this deliberately decouples the rotate POST
  from a short per-request deadline (so a request whose own context is about to expire can still
  complete the rotation that will unblock the retry and every subsequent request) while still bounding
  the rotate itself at `rotateTimeout`; it does forgo propagating an outright *cancellation* of the
  parent (e.g. Ctrl-C), which is acceptable for a ≤10 s bounded call.
- Response handling:
  - `200`: parse new cookie via `(&http.Response{Header: resp.Header}).Cookies()`, find the one named
    `config.SessionCookieName` (`grafana_session`), take its `.Value`. Missing/empty → error.
    (`grafana_session_expiry` is ignored — we only track the session value.)
  - `401` or `403`: return `ErrRotateUnauthorized` (sentinel `var` in package `config`).
  - anything else: `fmt.Errorf("session rotation: unexpected status %d", code)`.
- The `Rotate` method returns any of these errors; the `rotatingRoundTripper` treats **all** of them
  identically (give up, return the original `401`). The sentinel exists mainly for testing/clarity.

### `SessionSource` (sketch, package `config`; follows repo symbol-ordering)

```go
const rotateTimeout = 10 * time.Second

var ErrRotateUnauthorized = errors.New("session rotation: unauthorized")

type SessionSource struct {
    mu      sync.Mutex
    cookie  string
    gen     uint64

    server  string
    tls     *TLS
    account string
    store   keychain.Store
    warn    io.Writer // defaults to os.Stderr; injectable for tests
}

func NewSessionSource(cookie, server string, tls *TLS, store keychain.Store, account string) *SessionSource

func (s *SessionSource) Current() (string, uint64)            // brief lock
func (s *SessionSource) Rotate(ctx context.Context, usedGen uint64) (string, error) // lock held across rotate
func (s *SessionSource) doRotate(ctx context.Context, cookie string) (string, error) // network; unexported
```

- `Rotate` first checks `s.gen > usedGen` and short-circuits. On successful `doRotate` it sets
  `s.cookie`, `s.gen++`, then `if err := s.store.Set(s.account, newCookie); err != nil { warn(...) }`
  (persist failure is non-fatal — requirement 4). `keychain.Store.Set` is atomic, so a failure leaves
  the prior stored value intact.

### `rotatingRoundTripper` (sketch)

```go
type rotatingRoundTripper struct {
    src  *SessionSource
    next http.RoundTripper
}

func (rt *rotatingRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
    cookie, gen := rt.src.Current()
    first := req.Clone(req.Context())
    first.Header.Set("Cookie", CookieHeaderValue(cookie))

    resp, err := rt.next.RoundTrip(first)
    if err != nil || resp.StatusCode != http.StatusUnauthorized {
        return resp, err
    }

    rewindable := req.Body == nil || req.GetBody != nil

    newCookie, rerr := rt.src.Rotate(req.Context(), gen)
    if rerr != nil || !rewindable {
        return resp, nil // original 401 surfaces (rotation failed, or body can't be replayed)
    }

    // Obtain the replay body BEFORE touching the first response, so a GetBody failure
    // leaves the original 401 fully intact to return.
    var replay io.ReadCloser
    if req.GetBody != nil {
        body, gberr := req.GetBody()
        if gberr != nil {
            return resp, nil // original 401 surfaces; do not retry with a nil body
        }
        replay = body
    }

    // Drain a bounded prefix + close the 401 body so the connection can be reused.
    _, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
    _ = resp.Body.Close()

    retry := req.Clone(req.Context())
    retry.Body = replay
    retry.Header.Set("Cookie", CookieHeaderValue(newCookie))
    return rt.next.RoundTrip(retry) // single retry; return as-is even if it 401s again
}
```

- Note: in the non-rewindable branch (and on a `GetBody()` error) we still call `Rotate` (so the next
  request benefits) but return the original `401`. `req.Clone` on the retry re-derives headers from the
  original request, avoiding any leakage of the first attempt's mutated state.
- **k8s bodies genuinely retry:** client-go serializes create/update payloads to a `bytes.Reader`, so
  `http.NewRequest` auto-populates `GetBody` (client-go `request.go` ~L987-994). The primary push
  use-case is therefore rewindable and does retry after rotation — the non-rewindable branch is an
  edge case (e.g. a streamed body), not the dominant path.

### `WrapWithSession` (sketch)

```go
func (cfg *GrafanaConfig) WrapWithSession(next http.RoundTripper) http.RoundTripper {
    switch {
    case cfg.Session != nil:
        return &rotatingRoundTripper{src: cfg.Session, next: next}
    case cfg.SessionCookie != "":
        return &sessionCookieRoundTripper{cookie: cfg.SessionCookie, next: next}
    default:
        return next
    }
}
```

### `GrafanaConfig` change

Add `Session *SessionSource` with `json:"-" yaml:"-"` (no env tag). In `IsEmpty()`, zero it
alongside `SessionCookie` before the struct-equality comparison so a resolved source never affects
emptiness (mirror the existing `grafana.SessionCookie = ""` line). Pointer fields keep the struct
comparable.

### Credential resolution change

In `ResolveContextSessionCookie` (`credentials.go`), after `gCtx.Grafana.SessionCookie = cookie`, also
set `gCtx.Grafana.Session = NewSessionSource(cookie, gCtx.Grafana.Server, gCtx.Grafana.TLS, store,
keychain.Account(gCtx.Name))`. This runs only when a cookie was actually loaded (`ErrNotFound` leaves
both nil), so unauthenticated contexts and login flows get no source. `ResolveSessionCookie` (the
Override for the current context) already delegates here, so both `LoadConfig`/`LoadRESTConfig` and
`config check` get the source for free.

### Request flow (rotate-on-401, k8s path)

```
LoadRESTConfig → resolve cookie + SessionSource from Keychain → NewNamespacedRESTConfig
  → rcfg.WrapTransport = cfg.Grafana.WrapWithSession(rt)  (rotatingRoundTripper)
  → dynamic client request → 401 seen at transport level
     → Rotate(ctx, gen): POST /api/user/auth-tokens/rotate with old cookie
        → 200 + Set-Cookie: new value → persist to Keychain → retry request once with new cookie → 200
        → 401/403 → return original 401 → k8sapi.StatusError → fail.convertAPIErrors
           → "session is stale — run grafanapi login update", exit code 2
```

## Implementation Steps

### Task 1 — `SessionSource`, rotate logic, `rotatingRoundTripper`, and the `Session` field (package `config`)

**Files:**
- Create: `internal/config/session_source.go` (`SessionSource`, `NewSessionSource`, `Current`,
  `Rotate`, `doRotate`, `rotatingRoundTripper`, `WrapWithSession`, `ErrRotateUnauthorized`,
  `rotateTimeout`)
- Modify: `internal/config/types.go` (add `Session *SessionSource` field to `GrafanaConfig`; zero it
  in `IsEmpty()` alongside `SessionCookie`) — added here, not in Task 2, because `WrapWithSession`
  switches on `cfg.Session` and would not compile without the field, making this task's own build/test
  gate unsatisfiable otherwise.
- Create: `internal/config/session_source_test.go` (package `config`, so it can drive the unexported
  round-tripper and a fake `keychain.Store`)
- Modify: `internal/config/types_test.go` (`IsEmpty()` stays true for an empty grafana block even with
  a stray source attached)

- [x] Add `Session *SessionSource` to `GrafanaConfig` (`json:"-" yaml:"-"`, no env tag); zero it in
      `IsEmpty()` alongside `SessionCookie` so a resolved source never affects emptiness (pointer field
      keeps the struct comparable).
- [x] Implement `SessionSource` with mutex + generation counter, `NewSessionSource`, `Current`,
      `Rotate` (short-circuit on `gen > usedGen`; lock held across `doRotate`; non-fatal keychain
      `Set` with warn), and `doRotate` (POST to `/api/user/auth-tokens/rotate` on a dedicated
      TLS-aware, bounded-timeout client that is NOT the rotating transport; the rotate context is
      derived via `context.WithoutCancel(req.Context())` + `rotateTimeout`; extract `grafana_session`
      from `Set-Cookie`; map `401`/`403` → `ErrRotateUnauthorized`; never log the cookie). The rotate
      client's `tls.Config` must carry the full context TLS material (see Task 1's TLS checkbox below).
- [x] Build the rotate client's `tls.Config` from a TLS helper that carries `RootCAs` (from `CAData`)
      and the client certificate (from `CertData`/`KeyData`) in addition to
      `Insecure`/`ServerName`/`NextProtos` — do **not** use the existing `ToStdTLSConfig` as-is (it has
      a TODO and drops `CertData`/`KeyData`/`CAData`, so mTLS/custom-CA contexts would fail the rotate
      handshake while ordinary k8s requests succeed). Complete the `ToStdTLSConfig` TODO in
      `types.go` (or add a sibling `ToRotateTLSConfig`) so it populates `RootCAs`/`Certificates`.
- [x] Implement `rotatingRoundTripper.RoundTrip` (inject cookie, detect `401`, rewindable check; on the
      retry path call `req.GetBody()` **first** and, if it errors, return the original `401` without
      retrying; only after `GetBody()` succeeds drain a small bounded prefix of the first response
      (`io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))`) and `Close` it for connection reuse,
      then retry once with the rotated cookie; return original `401` on rotate failure / non-rewindable)
      and `(*GrafanaConfig).WrapWithSession`. (Note: `WrapWithSession` ended up with a **value**
      receiver `(cfg GrafanaConfig)`, not a pointer receiver, to satisfy the `recvcheck` linter — every
      other `GrafanaConfig` method (`IsEmpty`, `Validate`, `validateNamespace`) already uses a value
      receiver, and `Session`/`TLS` are pointer fields so the value copy still shares the same
      `SessionSource`/`TLS`. Behavior is identical; only the receiver style changed from the sketch.)
- [x] Follow the repo Go symbol-ordering convention (consts, vars, funcs, exported types, exported
      methods, unexported methods/funcs). Keep the existing `sessionCookieRoundTripper` in `rest.go`
      as the static fallback branch of `WrapWithSession`.
- [x] Write tests for success cases: `401 → rotate (old cookie sent) → Set-Cookie new → retry (new
      cookie sent) → 200`, and the fake keychain received the new value; single-flight (N concurrent
      401s → exactly one rotate via an atomic counter, all succeed); a custom-CA case using
      `httptest.NewTLSServer` whose CA is supplied via `CAData`, proving the rotate client trusts it.
- [x] Write tests for error cases: rotate returns `401` and `403` → original `401` returned, no retry,
      keychain not written; non-rewindable body → rotate happens but no retry, original `401`;
      `GetBody()` error → original `401`, no retry; keychain `Set` failure after rotation → warning to
      the injected writer, request still `200`; `IsEmpty()` true for an empty grafana block.
- [x] Run tests — must pass before next task (also verify `GOOS=linux CGO_ENABLED=0 go build ./...`).

### Task 2 — Build the `SessionSource` during credential resolution

**Files:**
- Modify: `internal/config/credentials.go` (`ResolveContextSessionCookie` also builds the source)
- Modify: `internal/config/credentials_test.go` (source populated iff cookie loaded)

- [x] In `ResolveContextSessionCookie`, after populating `SessionCookie`, construct and assign
      `gCtx.Grafana.Session` via `NewSessionSource(...)` using the same `store` and
      `keychain.Account(gCtx.Name)`; leave both nil on `keychain.ErrNotFound`.
- [x] Confirm both `LoadConfig`/`LoadRESTConfig` (via the `ResolveSessionCookie` Override) and
      `config check` (via the direct per-context call) get the source for free — no change needed in
      `cmd/grafanapi/config/command.go`.
- [x] Confirm `config view` / reference docs still never see the field (json/yaml `-`).
- [x] Write tests for success cases: fake store returns a cookie → `SessionCookie` and `Session` both
      populated; `Session.Current()` returns the loaded cookie at generation 0.
- [x] Write tests for error cases: fake store returns `ErrNotFound` → both nil, load succeeds; a real
      store error still surfaces as a load error.
- [x] Run tests — must pass before next task.

### Task 3 — k8s REST path via `WrapWithSession` + fallback-rendering test

**Files:**
- Modify: `internal/config/rest.go` (`NewNamespacedRESTConfig` uses `cfg.Grafana.WrapWithSession`)
- Modify: `internal/config/rest_test.go`
- Modify: `cmd/grafanapi/fail/convert_test.go` (lock in stale-session rendering when rotation gives up)

- [x] Replace the inline `WrapTransport` closure in `NewNamespacedRESTConfig` with
      `rcfg.WrapTransport = func(rt http.RoundTripper) http.RoundTripper { return
      cfg.Grafana.WrapWithSession(rt) }` (covers both the rotating and static-fallback cases; leave the
      no-cookie case producing no wrapper). Implemented as `if cfg.Grafana.Session != nil ||
      cfg.Grafana.SessionCookie != "" { ... }` guarding the closure, matching the "no-cookie means no
      wrapper" requirement (verified by the pre-existing
      `TestNewNamespacedRESTConfig_NoCookieMeansNoWrapTransport`).
- [x] Ensure TLS config is still applied independently of the wrapper (unchanged block above it).
- [x] Write tests for success cases: with a `SessionSource`, the wrapped RoundTripper sends the cookie
      and performs rotate-and-retry against an `httptest.Server`; with only `SessionCookie`, the static
      header is sent; with neither, no `Cookie` header is added. Added
      `TestNewNamespacedRESTConfig_RotatesSessionOn401` (rotate-and-retry, keychain persisted); the
      static-cookie and no-cookie cases were already covered by
      `TestNewNamespacedRESTConfig_InjectsSessionCookie` /
      `TestNewNamespacedRESTConfig_NoCookieMeansNoWrapTransport`.
- [x] Write tests for error cases: rotate rejected at the k8s transport → original `401` returned →
      `convert_test.go` asserts a k8s `401` `StatusError` still renders as stale-session (exit code 2,
      `login update` suggestion). Added `TestNewNamespacedRESTConfig_RotateRejectedSurfacesOriginal401`
      (no retry, keychain not written) and cross-referenced it from a doc comment on the pre-existing
      `TestErrorToDetailedError_StaleSession` in `cmd/grafanapi/fail/convert_test.go`, which already
      locks in the k8s-401-renders-as-stale-session assertion this fallback path depends on.
- [x] Run tests — must pass before next task. `go test -race ./...` passes; `golangci-lint run -c
      .golangci.yaml ./...` reports exactly the pre-existing 14 findings (5 gosec, 3 govet, 1
      nolintlint, 5 staticcheck), no new ones; `GOOS=linux CGO_ENABLED=0 go build ./... && go vet
      ./...` passes.

### Task 4 — openapi path reworked to transport-level rotation

**Calibration:** the only production openapi consumer today is `grafana.GetVersion` →
`GET /api/health`, which is typically **unauthenticated**, so in practice this wrapper is exercised
mostly by tests. It is reworked anyway for path consistency with the k8s transport (single
`WrapWithSession` mechanism, cookie injected identically) and to keep any future authenticated openapi
usage correct — not because a live openapi 401 is common today.

**Files:**
- Modify: `internal/grafana/client.go` (drop `HTTPHeaders`; wrap the runtime's `Transport`)
- Create/Modify: `internal/grafana/client_test.go`

- [ ] In `ClientFromContext`, remove the `cfg.HTTPHeaders` cookie map; keep `OrgID` and TLS passthrough
      and `NewHTTPClientWithConfig`. After construction, type-assert
      `client.Transport.(*httptransport.Runtime)`; on `ok`, set `rt.Transport =
      ctx.Grafana.WrapWithSession(rt.Transport)`; on `!ok`, return an explicit error rather than
      sending unauthenticated requests.
- [ ] Add the `github.com/go-openapi/runtime/client` import for the `*httptransport.Runtime` type
      (vendored; no import cycle). `go mod tidy`/`vendor` if the direct-dependency set changes.
- [ ] Write tests for success cases: `grafana.GetVersion` (or a `Health` call) against an
      `httptest.Server` scripted `401 → rotate → 200`, asserting exactly one rotate call and success;
      the inbound `Cookie` header carries the current then rotated value.
- [ ] Write tests for error cases: rotate rejected → the openapi `401` surfaces as a
      `runtime.APIError` (Code 401) → `convertSessionErrors` renders stale-session (asserted in
      `convert_test.go` from Task 3, or a focused case here); the `!ok` transport-assertion guard
      returns its explicit error.
- [ ] Run tests — must pass before next task.

### Task 5 — serve reverse-proxy and dashboard-proxy paths

**Files:**
- Modify: `internal/server/server.go` (wrap the `ReverseProxy` `Transport`)
- Modify: `internal/server/grafana/requests.go` (wrap the dashboard-proxy client `Transport`)
- Modify: `internal/server/grafana/requests_test.go`

- [ ] In `server.go`, set the `ReverseProxy.Transport` to
      `s.context.Grafana.WrapWithSession(httputils.NewTransport(s.context))`; keep the existing
      `AuthenticateRequest` call in `Rewrite` (harmless — the transport re-sets the cookie to the live
      value, which wins after a rotation).
- [ ] In `requests.go` (`AuthenticateAndProxyHandler`), set the dashboard-proxy client's `Transport`
      to `cfg.Grafana.WrapWithSession(httputils.NewTransport(cfg))`; keep `AuthenticateRequest`.
      Keep both handlers free of any cookie logging (existing comments already note this).
- [ ] Confirm no import cycle (both files already import `internal/config` and `internal/httputils`).
- [ ] Write tests for success cases: the proxy transport carries the cookie; when a `SessionSource` is
      present, a scripted `401 → rotate → 200` on an `httptest.Server` is served transparently for a
      rewindable (GET) proxied request.
- [ ] Write tests for error cases: no cookie/source → no `Cookie` header and no rotation attempt; a
      non-rewindable proxied body → no retry, original `401` passed back to the caller.
- [ ] Run tests — must pass before next task.

### Task 6 — Verify acceptance criteria

**Files:**
- Modify: none (verification only; fix regressions in-place if found)

- [ ] `make tests` (race) passes across all packages (fall back to `go test -race ./...` if `devbox`
      is unavailable on the machine, as the prior plan did).
- [ ] `make build` produces `./bin/grafanapi` on darwin/arm64 with `CGO_ENABLED=1`; `--version` runs.
- [ ] `make lint` passes: no **new** findings versus the pre-existing 14-finding baseline (5 gosec, 3
      govet, 1 nolintlint, 5 staticcheck) recorded in the completed plan's Task 11 — verify by diffing
      `golangci-lint run` output against that baseline (or a `main`/base-branch worktree run); a lock
      held across a network call in `Rotate` may draw attention, so annotate with a justified
      `//nolint` + comment only if a linter flags it, and record it as an intentional addition to the
      baseline.
- [ ] `GOOS=linux CGO_ENABLED=0 go build ./... && go vet ./...` passes (the `!darwin` keychain stub
      keeps cross-build green; nothing in this feature is platform-specific).
- [ ] `goreleaser check` passes; `make reference-drift` passes (no command-help changes are expected —
      no new commands/flags — so the CLI/env/config reference docs should be drift-free; regenerate and
      confirm).
- [ ] Run the full suite once more — must pass before the documentation task.

### Task 7 — Update documentation and complete the plan

**Files:**
- Modify: `docs/configuration.md` (automatic rotation; login-once-per-browser-session-lifetime;
  shared-chain caveat + private-window recommendation)
- Modify: `AGENTS.md` (auth/rotation notes; `internal/config` `SessionSource`; `internal/grafana`
  transport-level cookie injection)
- Regenerate: `docs/reference/**` via `make reference` if any command help changed (it should not)
- Move: this plan → `docs/plans/completed/20260722-auto-rotate-session-on-401.md`

- [ ] In `docs/configuration.md`, document that session rotation is **automatic**: after a successful
      `grafanapi login`, the CLI transparently refreshes the `grafana_session` cookie on `401` and
      re-persists it to the Keychain, so a normal browser-session lifetime typically needs only one
      `login`. Explain that `grafanapi login update` is still needed only after the session truly dies
      (logout / max-lifetime / revocation).
- [ ] Document the **shared-chain caveat**: a cookie copied from a still-open browser tab is shared
      with that tab — the browser and the CLI then compete to rotate the same session, and each
      rotation invalidates the other party's copy. Recommend logging into Grafana in a **private/
      incognito window**, copying that window's `grafana_session`, and closing the window so only the
      CLI drives rotation for that token.
- [ ] Update `AGENTS.md`: note the shared, mutable `SessionSource` (mutex + generation counter) in
      `internal/config`, the single `WrapWithSession` transport wrapper across the k8s/openapi/serve
      paths, that the openapi path now injects the cookie at the transport level (no more static
      `HTTPHeaders`), and that bootdata rotation is intentionally out of scope.
- [ ] Run `make reference` / `make reference-drift`; confirm no drift (no command-surface change).
- [ ] Add a short review section to this plan (what changed, deviations), then move it to
      `docs/plans/completed/` (via `git mv`).
- [ ] Run `make all` (lint, tests, build, docs) — must pass (fall back to the underlying commands if
      `devbox`/`mkdocs` are unavailable, matching the prior plan's approach).

## Post-Completion

These require the user and cannot be safely automated by the implementation agents:

- **Live end-to-end verification (real Grafana, generic — never record the real hostname or cookie):**
  1. `grafanapi login --server "$GRAFANA_TEST_SERVER"` → paste the current browser `grafana_session`
     at the no-echo prompt (ideally from a private/incognito window, per the docs caveat) → expect
     success and a Keychain item under service `grafanapi`.
  2. Run a resource command (`grafanapi resources list dashboards`) → succeeds.
  3. Wait past the token-rotation interval (~10 min of inactivity) so the cookie is stale for ordinary
     requests but still rotatable, then rerun the resource command → expect it to **succeed silently**
     (the CLI rotated and retried); confirm the Keychain item's value changed.
  4. Truly kill the session (log out in the browser / revoke the token), rerun a resource command →
     expect the "session is stale — run `grafanapi login update`" error with exit code 2.
  5. `grafanapi login update` → paste a fresh cookie → subsequent commands succeed again.
  6. Concurrency smoke: `grafanapi resources pull` (or push) of many resources across a stale-token
     boundary → completes without a burst of rotate calls (single-flight holds) and without errors.
- **Keychain "Allow" prompt:** the first Keychain access after each rebuild triggers the macOS
  "Allow / Always Allow" dialog (ACL bound to the binary's cdhash); rotation's `keychain.Set` writes
  are covered by the same grant — choose "Always Allow" for the installed binary.
