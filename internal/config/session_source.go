package config

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/grafana/grafanapi/internal/keychain"
)

// rotateTimeout bounds the POST /api/user/auth-tokens/rotate request issued by
// (*SessionSource).doRotate. Matches internal/session.verifyTimeout.
const rotateTimeout = 10 * time.Second

// ErrRotateUnauthorized is returned by (*SessionSource).Rotate when the rotate endpoint itself
// rejects the current cookie (401 or 403): the session is truly dead, not merely stale.
var ErrRotateUnauthorized = errors.New("session rotation: unauthorized")

// NewSessionSource creates a SessionSource seeded with cookie for the Grafana instance at server,
// using tlsCfg for the dedicated rotate HTTP client and persisting rotated values to store under
// account. tlsCfg may be nil.
func NewSessionSource(cookie, server string, tlsCfg *TLS, store keychain.Store, account string) *SessionSource {
	return &SessionSource{
		cookie:  cookie,
		server:  server,
		tls:     tlsCfg,
		store:   store,
		account: account,
		warn:    os.Stderr,
	}
}

// SessionSource is a shared, mutable holder for a Grafana session cookie that rotates itself on
// demand (see Rotate) and persists the result to the Keychain. One instance is created per
// resolved context and shared by every transport for that context (via
// GrafanaConfig.WrapWithSession), so all outbound requests observe the same rotation.
//
// The mutex + generation counter dedups concurrent/sequential post-rotation 401s: a caller that
// saw generation gen before its request 401'd calls Rotate(ctx, gen), which no-ops (no network
// call) if another goroutine already rotated past gen.
//
// mu guards only the in-memory (cookie, gen, inflight) state, never the rotate network call or the
// Keychain write, so Current() never blocks on a rotation in flight - see Rotate for the
// single-flight mechanics that make this safe.
type SessionSource struct {
	mu       sync.Mutex
	cookie   string
	gen      uint64
	inflight *rotateCall

	server  string
	tls     *TLS
	account string
	store   keychain.Store
	warn    io.Writer // defaults to os.Stderr; injectable for tests

	// rotateFunc, when set, replaces doRotate as the function runRotate calls. It exists solely so
	// tests can simulate doRotate panicking (a real panic is otherwise impractical to trigger from
	// the outside); production code always leaves it nil, in which case runRotate calls s.doRotate.
	rotateFunc func(ctx context.Context, cookie string) (string, error)
}

// Current returns the in-memory cookie and the generation it was set at. It only ever takes mu
// for the duration of a map-like field read, never for a rotation in progress, so it never blocks
// on network I/O or the Keychain - see the SessionSource doc comment.
func (s *SessionSource) Current() (string, uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.cookie, s.gen
}

// rotateCall represents a single in-flight (or completed) rotation, shared by every caller that
// asks Rotate to rotate the same generation while it is running. cookie/err are only written once,
// by the goroutine that performs doRotate, strictly before done is closed; every other field access
// happens after receiving from done, so no further synchronization is needed (channel close
// establishes the happens-before relationship).
type rotateCall struct {
	done   chan struct{}
	cookie string
	err    error
}

// Rotate exchanges the current session cookie for a fresh one via
// POST /api/user/auth-tokens/rotate and returns it. usedGen is the generation the caller observed
// via Current before its request 401'd: if the source has already advanced past usedGen (another
// goroutine rotated first), Rotate short-circuits and returns the current cookie with no network
// call.
//
// If a rotation for the current generation is already in flight, Rotate waits on it instead of
// starting a second one (single-flight) - and, critically, does so without holding mu, so Current()
// keeps serving the OLD cookie to unrelated concurrent requests while the rotation is running (their
// requests may themselves 401 against the old cookie and then join the same in-flight rotation).
//
// On a successful rotation the new cookie is stored in memory and the generation is incremented
// atomically with publishing the result to every waiter (see runRotate); the Keychain persist
// happens after releasing mu and is best-effort (a persist failure - or even a panic from a
// misbehaving Store - is logged to warn but does not fail the rotation - the in-memory cookie is
// still authoritative).
func (s *SessionSource) Rotate(ctx context.Context, usedGen uint64) (string, error) {
	s.mu.Lock()

	if s.gen > usedGen {
		// Another goroutine already rotated past the generation the caller observed.
		cookie := s.cookie
		s.mu.Unlock()

		return cookie, nil
	}

	if call := s.inflight; call != nil {
		// A rotation for this generation is already running: wait for it without holding mu.
		s.mu.Unlock()
		<-call.done

		return call.cookie, call.err
	}

	call := &rotateCall{done: make(chan struct{})}
	s.inflight = call
	cookieToRotate := s.cookie
	s.mu.Unlock()

	newCookie, err := s.runRotate(ctx, cookieToRotate, call)
	if err != nil {
		return "", err
	}

	s.persist(newCookie)

	return newCookie, nil
}

// Refresh forces a rotation now, regardless of staleness, reusing the same single-flight rotate +
// Keychain-persist path as the automatic 401-triggered rotation (see Rotate). It is safe to call
// concurrently with an in-flight 401 rotation: it joins that rotation rather than starting a second
// one.
//
// Refresh cannot be "missed": Rotate's only short-circuit fires when s.gen has already advanced
// past the generation observed here, which can only happen if a peer rotation completed in the tiny
// window between Current and Rotate re-acquiring mu - in which case returning that just-completed
// rotation's cookie is correct, not a skipped refresh. In the common case (no concurrent rotation)
// s.gen is unchanged and Rotate always performs a real network rotation.
func (s *SessionSource) Refresh(ctx context.Context) (string, error) {
	_, gen := s.Current()

	return s.Rotate(ctx, gen)
}

// WrapWithSession returns the RoundTripper used to inject (and, when possible, rotate) the
// Grafana session cookie for every outbound request built from cfg. It is the single mechanism
// shared by every transport path (k8s dynamic client, openapi client, serve reverse proxy):
//   - a rotatingRoundTripper when cfg.Session is set (the normal, credential-resolved path: every
//     production assignment of SessionCookie that flows through this method also sets Session in
//     the same place - ResolveContextSessionCookie in credentials.go - so this case always wins
//     whenever there is a cookie to inject at all in production),
//   - the static sessionCookieRoundTripper when only cfg.SessionCookie is set with no Session:
//     this never happens on a production path that reaches WrapWithSession today (the login/login
//     update flows set SessionCookie without a Session, but they verify/persist the cookie via
//     internal/session.VerifyCookie directly, never via a transport wrapped here) - the case is
//     kept as a deliberate, tested fallback for any caller that constructs a GrafanaConfig with a
//     bare cookie (unit tests do this routinely) rather than as production-reachable behavior,
//   - next unchanged when neither is set.
func (cfg GrafanaConfig) WrapWithSession(next http.RoundTripper) http.RoundTripper {
	switch {
	case cfg.Session != nil:
		return &rotatingRoundTripper{src: cfg.Session, next: next}
	case cfg.SessionCookie != "":
		return &sessionCookieRoundTripper{cookie: cfg.SessionCookie, next: next}
	default:
		return next
	}
}

// runRotate performs the rotate network call for call - the in-flight marker already published to
// s.inflight - and atomically publishes the outcome under ONE acquisition of mu: call.cookie/err
// are set, s.inflight is cleared, and (on success) s.cookie/s.gen are advanced; call.done closes
// only after mu is released. That ordering is what makes single-flight dedup correct: no goroutine
// can see s.inflight == nil and s.gen unchanged - and start a redundant rotation - before this
// rotation's result is published via call.done.
//
// The same defer recovers a panic from rotate into an error result, so it can't leave s.inflight
// permanently set and call.done never closed (which would deadlock every future Rotate call here).
// Because the panic path skips any "return" statement, only a NAMED return lets recover() substitute
// an error for the result - an unnamed return would still report zero values.
//
// rotate defaults to s.doRotate; s.rotateFunc overrides it only for tests simulating a panic.
//
//nolint:nonamedreturns // see comment above
func (s *SessionSource) runRotate(ctx context.Context, cookie string, call *rotateCall) (newCookie string, err error) {
	rotate := s.doRotate
	if s.rotateFunc != nil {
		rotate = s.rotateFunc
	}

	defer func() {
		if r := recover(); r != nil {
			newCookie = ""
			err = fmt.Errorf("session rotation: panicked: %v", r)
		}

		s.mu.Lock()
		call.cookie = newCookie
		call.err = err
		s.inflight = nil
		if err == nil {
			s.cookie = newCookie
			s.gen++
		}
		s.mu.Unlock()

		close(call.done)
	}()

	return rotate(ctx, cookie)
}

// persist best-effort saves cookie to the Keychain: a returned error, or - defensively - a panic
// from a misbehaving Store.Set, is logged to s.warn rather than failing the rotation or crashing the
// process. By the time persist runs, the rotation has already been published to every waiter via
// runRotate, so there is nothing left that a panic here could deadlock; recovering it merely keeps a
// single bad Store implementation from taking down the whole process for what is documented as a
// best-effort, non-fatal step.
func (s *SessionSource) persist(cookie string) {
	defer func() {
		if r := recover(); r != nil {
			fmt.Fprintf(s.warn, "warning: failed to persist rotated Grafana session to the keychain: panic: %v\n", r)
		}
	}()

	if setErr := s.store.Set(s.account, cookie); setErr != nil {
		fmt.Fprintf(s.warn, "warning: failed to persist rotated Grafana session to the keychain: %v\n", setErr)
	}
}

// doRotate performs the actual rotate HTTP call. It never logs cookie and never uses the
// rotating RoundTripper (it builds its own dedicated client), so there is no recursion.
func (s *SessionSource) doRotate(ctx context.Context, cookie string) (string, error) {
	rotateURL, err := buildRotateURL(s.server)
	if err != nil {
		return "", fmt.Errorf("session rotation: invalid server address: %w", err)
	}

	// Deliberately decouple the rotate call from the caller's own (possibly short) deadline: a
	// request whose context is about to expire should still be able to complete the rotation
	// that will unblock the retry and every subsequent request. The rotate call itself remains
	// bounded by rotateTimeout.
	rotateCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), rotateTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(rotateCtx, http.MethodPost, rotateURL.String(), nil)
	if err != nil {
		return "", fmt.Errorf("session rotation: building request: %w", err)
	}
	req.Header.Set("Cookie", CookieHeaderValue(cookie))

	client, err := s.rotateClient()
	if err != nil {
		return "", err
	}

	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("session rotation: %w", err)
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
		return extractSessionCookie(resp)
	case http.StatusUnauthorized, http.StatusForbidden:
		return "", ErrRotateUnauthorized
	default:
		return "", fmt.Errorf("session rotation: unexpected status %d", resp.StatusCode)
	}
}

// rotateClient builds the dedicated HTTP client used for the rotate request: TLS-aware, bounded
// timeout, and never the rotating transport (no recursion) nor any request-dumping/logging
// round-tripper (the cookie must never be logged). A malformed CAData/CertData/KeyData in s.tls is
// reported as an error rather than silently degrading to a client that trusts only the system pool
// or carries no client certificate (see TLS.ToStdTLSConfig).
func (s *SessionSource) rotateClient() (*http.Client, error) {
	transport := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
	}

	if s.tls != nil {
		tlsConfig, err := s.tls.ToStdTLSConfig()
		if err != nil {
			return nil, fmt.Errorf("session rotation: %w", err)
		}

		transport.TLSClientConfig = tlsConfig
	}

	return &http.Client{
		Timeout:   rotateTimeout,
		Transport: transport,
	}, nil
}

// rotatingRoundTripper injects the current session cookie into every outbound request and, on a
// 401 response, attempts a single rotate-and-retry before surfacing the failure. See the
// RoundTrip doc comment for the exact decision tree.
type rotatingRoundTripper struct {
	src  *SessionSource
	next http.RoundTripper
}

// RoundTrip injects the current session cookie and delegates to next. If the response is not a
// 401, it is returned unchanged. On a 401:
//   - it calls src.Rotate, which is a no-op network-wise if another goroutine already rotated
//     past the generation used for this request;
//   - if rotation fails, the original 401 response is returned unchanged so the centralized
//     stale-session error rendering (cmd/grafanapi/fail/convert.go) fires;
//   - if rotation succeeds but the request body cannot be replayed (Body != nil and
//     GetBody == nil, or GetBody() itself errors), the original 401 is returned unchanged - no
//     retry is attempted, though the rotation still benefits the next request;
//   - otherwise the request is retried exactly once with the rotated cookie, and whatever that
//     retry returns (success or another 401) is returned as-is.
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
		// Deliberately not propagating rerr: RoundTrip's contract is to return the HTTP
		// response as-is; the caller-visible outcome of a failed/skipped rotation is the
		// original 401 response, not a Go error (which centralized fail/convert.go rendering
		// does not expect here). rerr itself is not observable by callers, by design.
		//nolint:nilerr // intentional: original 401 surfaces as (resp, nil), see comment above
		return resp, nil
	}

	// Obtain the replay body BEFORE touching the first response, so a GetBody failure leaves the
	// original 401 fully intact to return.
	var replay io.ReadCloser
	if req.GetBody != nil {
		body, gberr := req.GetBody()
		if gberr != nil {
			//nolint:nilerr // intentional: original 401 surfaces unretried, see comment above
			return resp, nil
		}
		replay = body
	}

	// Drain a bounded prefix of the 401 body before closing it: for a keep-alive connection the
	// transport can only reuse it once the body has been read to EOF (or close forces it to give
	// up and not reuse the connection at all). This is a best-effort nicety, not a guarantee - a
	// body longer than 4096 bytes still causes the connection to be discarded rather than reused;
	// resp.Body.Close() below is what actually matters for correctness (releasing the response).
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	_ = resp.Body.Close()

	// Retried with the caller's own request context (not a decoupled one): this retry is the
	// caller's request, so it must honor the caller's own deadline/cancellation like any other
	// attempt at it would - if the caller's context expires mid-retry, a context-deadline error is
	// the honest outcome. Only the rotate call itself (doRotate, via context.WithoutCancel) is
	// deliberately decoupled from the caller's deadline, because it must finish and publish the
	// new cookie for every other waiter even if this particular caller has since given up.
	retry := req.Clone(req.Context())
	retry.Body = replay
	retry.Header.Set("Cookie", CookieHeaderValue(newCookie))

	return rt.next.RoundTrip(retry)
}

// buildRotateURL appends /api/user/auth-tokens/rotate to server, preserving any existing path
// (mirrors buildBootdataURL in stack_id.go and buildUserURL in internal/session/session.go).
func buildRotateURL(server string) (*url.URL, error) {
	parsed, err := url.Parse(server)
	if err != nil {
		return nil, err
	}

	trimmedPath := strings.TrimSuffix(parsed.Path, "/")
	parsed.Path = trimmedPath + "/api/user/auth-tokens/rotate"
	parsed.RawQuery = ""
	parsed.Fragment = ""

	return parsed, nil
}

// extractSessionCookie reads the grafana_session cookie value from resp's Set-Cookie headers.
// grafana_session_expiry is ignored - only the session value is tracked.
func extractSessionCookie(resp *http.Response) (string, error) {
	for _, cookie := range resp.Cookies() {
		if cookie.Name == SessionCookieName && cookie.Value != "" {
			return cookie.Value, nil
		}
	}

	return "", fmt.Errorf("session rotation: no %s cookie in response", SessionCookieName)
}
