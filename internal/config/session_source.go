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

// SessionSource is a shared, mutable holder for a Grafana session cookie that can rotate itself
// on demand (see Rotate) and persist the rotated value to the platform Keychain. A single
// SessionSource is created per resolved context (see credentials.go) and referenced from every
// transport that needs the cookie, via GrafanaConfig.WrapWithSession, so all outbound requests
// for a context observe the same rotation.
//
// The mutex + generation counter dedups both concurrent and sequential post-rotation 401s: a
// caller that observed generation gen before a request 401s calls Rotate(ctx, gen); if another
// goroutine already rotated past gen, Rotate returns the current cookie immediately with no
// network call.
type SessionSource struct {
	mu     sync.Mutex
	cookie string
	gen    uint64

	server  string
	tls     *TLS
	account string
	store   keychain.Store
	warn    io.Writer // defaults to os.Stderr; injectable for tests
}

// Current returns the in-memory cookie and the generation it was set at.
func (s *SessionSource) Current() (string, uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.cookie, s.gen
}

// Rotate exchanges the current session cookie for a fresh one via
// POST /api/user/auth-tokens/rotate and returns it. usedGen is the generation the caller observed
// via Current before its request 401'd: if the source has already advanced past usedGen (another
// goroutine rotated first), Rotate short-circuits and returns the current cookie with no network
// call. On a successful rotation the new cookie is stored in memory, the generation is
// incremented, and the value is best-effort persisted to the Keychain (a persist failure is
// logged to warn but does not fail the rotation - the in-memory cookie is still authoritative).
func (s *SessionSource) Rotate(ctx context.Context, usedGen uint64) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.gen > usedGen {
		// Another goroutine already rotated past the generation the caller observed.
		return s.cookie, nil
	}

	newCookie, err := s.doRotate(ctx, s.cookie)
	if err != nil {
		return "", err
	}

	s.cookie = newCookie
	s.gen++

	if err := s.store.Set(s.account, newCookie); err != nil {
		fmt.Fprintf(s.warn, "warning: failed to persist rotated Grafana session to the keychain: %v\n", err)
	}

	return newCookie, nil
}

// WrapWithSession returns the RoundTripper used to inject (and, when possible, rotate) the
// Grafana session cookie for every outbound request built from cfg. It is the single mechanism
// shared by every transport path (k8s dynamic client, openapi client, serve reverse proxy):
//   - a rotatingRoundTripper when cfg.Session is set (the normal, credential-resolved path),
//   - the existing static sessionCookieRoundTripper when only cfg.SessionCookie is set (kept for
//     backward compatibility, e.g. tests that set the cookie directly without a SessionSource),
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

	resp, err := s.rotateClient().Do(req)
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
// round-tripper (the cookie must never be logged).
func (s *SessionSource) rotateClient() *http.Client {
	transport := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
	}

	if s.tls != nil {
		transport.TLSClientConfig = s.tls.ToStdTLSConfig()
	}

	return &http.Client{
		Timeout:   rotateTimeout,
		Transport: transport,
	}
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

	// Drain a bounded prefix + close the 401 body so the underlying connection can be reused.
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	_ = resp.Body.Close()

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
