package server_test

import (
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/grafana/grafanapi/internal/config"
	"github.com/grafana/grafanapi/internal/httputils"
	"github.com/grafana/grafanapi/internal/keychain"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeStore is an in-memory keychain.Store for tests local to this package (mirrors the fakeStore
// helpers used in internal/config and internal/grafana test suites for the same purpose - see
// docs/plans/20260722-auto-rotate-session-on-401.md, Task 5).
type fakeStore struct {
	mu     sync.Mutex
	values map[string]string
}

func newFakeStore() *fakeStore {
	return &fakeStore{values: map[string]string{}}
}

func (f *fakeStore) Set(account, secret string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.values[account] = secret

	return nil
}

func (f *fakeStore) Get(account string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	v, ok := f.values[account]
	if !ok {
		return "", keychain.ErrNotFound
	}

	return v, nil
}

func (f *fakeStore) Delete(account string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	delete(f.values, account)

	return nil
}

func (f *fakeStore) value(account string) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()

	v, ok := f.values[account]

	return v, ok
}

// newReverseProxy builds a ReverseProxy the same way Server.Start wires the "s.proxy" field
// (internal/server/server.go): Transport goes through GrafanaConfig.WrapWithSession so the proxy
// carries (and, when a SessionSource is present, rotates) the session cookie exactly like the
// production reverse proxy.
func newReverseProxy(t *testing.T, gCtx *config.Context, backendURL string) *httputil.ReverseProxy {
	t.Helper()

	target, err := url.Parse(backendURL)
	require.NoError(t, err)

	return &httputil.ReverseProxy{
		Transport: gCtx.Grafana.WrapWithSession(httputils.NewTransport(gCtx)),
		Rewrite: func(r *httputil.ProxyRequest) {
			r.SetURL(target)
		},
	}
}

func TestReverseProxyTransport_CarriesStaticCookie(t *testing.T) {
	var gotCookie string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotCookie = r.Header.Get("Cookie")
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()

	gCtx := &config.Context{Grafana: &config.GrafanaConfig{Server: backend.URL, SessionCookie: "abc123"}}
	proxy := newReverseProxy(t, gCtx, backend.URL)

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/dashboards", nil)
	rec := httptest.NewRecorder()
	proxy.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "grafana_session=abc123", gotCookie)
}

func TestReverseProxyTransport_NoCookieOrSessionMeansNoCookieHeaderAndNoRotation(t *testing.T) {
	var gotCookie string
	var calls atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/api/user/auth-tokens/rotate", func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/api/dashboards", func(w http.ResponseWriter, r *http.Request) {
		gotCookie = r.Header.Get("Cookie")
		w.WriteHeader(http.StatusOK)
	})
	backend := httptest.NewServer(mux)
	defer backend.Close()

	gCtx := &config.Context{Grafana: &config.GrafanaConfig{Server: backend.URL}}
	proxy := newReverseProxy(t, gCtx, backend.URL)

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/dashboards", nil)
	rec := httptest.NewRecorder()
	proxy.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Empty(t, gotCookie)
	assert.Zero(t, calls.Load(), "no rotation should be attempted when no cookie/session is configured")
}

func TestReverseProxyTransport_RotatesSessionOn401(t *testing.T) {
	const (
		oldCookie = "stale-cookie"
		newCookie = "fresh-cookie"
		account   = "grafanapi:test-context"
	)

	var rotateCalls atomic.Int32

	mux := http.NewServeMux()
	mux.HandleFunc("/api/user/auth-tokens/rotate", func(w http.ResponseWriter, r *http.Request) {
		rotateCalls.Add(1)
		assert.Equal(t, "grafana_session="+oldCookie, r.Header.Get("Cookie"))
		http.SetCookie(w, &http.Cookie{Name: "grafana_session", Value: newCookie, Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode})
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/api/dashboards", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Cookie") != "grafana_session="+newCookie {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusOK)
	})

	backend := httptest.NewServer(mux)
	defer backend.Close()

	store := newFakeStore()
	gCtx := &config.Context{
		Grafana: &config.GrafanaConfig{
			Server:  backend.URL,
			Session: config.NewSessionSource(oldCookie, backend.URL, nil, store, account),
		},
	}
	proxy := newReverseProxy(t, gCtx, backend.URL)

	// A GET request built by httptest.NewRequest carries a nil Body, so it is trivially
	// rewindable and the proxy retries transparently after rotation.
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/dashboards", nil)
	rec := httptest.NewRecorder()
	proxy.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, int32(1), rotateCalls.Load())

	stored, ok := store.value(account)
	assert.True(t, ok)
	assert.Equal(t, newCookie, stored)
}

func TestReverseProxyTransport_NonRewindableBodyIsNotRetried(t *testing.T) {
	const (
		oldCookie = "stale-cookie"
		newCookie = "fresh-cookie"
		account   = "grafanapi:test-context"
	)

	var rotateCalls, proxiedCalls atomic.Int32

	mux := http.NewServeMux()
	mux.HandleFunc("/api/user/auth-tokens/rotate", func(w http.ResponseWriter, _ *http.Request) {
		rotateCalls.Add(1)
		// Rotation itself succeeds - the point of this test is that a non-rewindable body still
		// blocks the retry, not that rotation failed.
		http.SetCookie(w, &http.Cookie{Name: "grafana_session", Value: newCookie, Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode})
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/api/dashboards", func(w http.ResponseWriter, _ *http.Request) {
		proxiedCalls.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
	})

	backend := httptest.NewServer(mux)
	defer backend.Close()

	store := newFakeStore()
	gCtx := &config.Context{
		Grafana: &config.GrafanaConfig{
			Server:  backend.URL,
			Session: config.NewSessionSource(oldCookie, backend.URL, nil, store, account),
		},
	}
	proxy := newReverseProxy(t, gCtx, backend.URL)

	// httptest.NewRequest never populates GetBody (it builds the request as http.ReadRequest
	// would for a real incoming connection), so a non-nil body here is not rewindable - exactly
	// the shape of a real proxied POST/PUT from a browser client.
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/dashboards", strings.NewReader("payload"))
	rec := httptest.NewRecorder()
	proxy.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusUnauthorized, rec.Code, "original 401 must surface unretried")
	assert.Equal(t, int32(1), proxiedCalls.Load(), "no retry expected for a non-rewindable body")
	assert.Equal(t, int32(1), rotateCalls.Load(), "rotation still happens so the next request benefits")

	stored, ok := store.value(account)
	assert.True(t, ok, "rotation succeeded and should still be persisted even though this request wasn't retried")
	assert.Equal(t, newCookie, stored)
}
