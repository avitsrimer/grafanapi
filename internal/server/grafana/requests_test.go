package grafana_test

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/grafana/grafana-app-sdk/logging"
	"github.com/grafana/grafanapi/internal/config"
	"github.com/grafana/grafanapi/internal/httputils"
	"github.com/grafana/grafanapi/internal/keychain"
	"github.com/grafana/grafanapi/internal/server/grafana"
	"github.com/grafana/grafanapi/internal/testutils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAuthenticateAndProxyHandler_NoGrafanaURLConfigured(t *testing.T) {
	handler := grafana.AuthenticateAndProxyHandler(&config.Context{Grafana: &config.GrafanaConfig{}})

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/dashboards", nil)
	rec := httptest.NewRecorder()

	handler(rec, req)

	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "No Grafana URL configured")
}

func TestAuthenticateAndProxyHandler_ProxiesSuccessfulResponseWithCookieAndUserAgent(t *testing.T) {
	var gotCookie, gotUserAgent, gotPath string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotCookie = r.Header.Get("Cookie")
		gotUserAgent = r.Header.Get("User-Agent")
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("hello from grafana"))
	}))
	t.Cleanup(backend.Close)

	cfg := &config.Context{Grafana: &config.GrafanaConfig{Server: backend.URL, SessionCookie: "abc123"}}
	handler := grafana.AuthenticateAndProxyHandler(cfg)

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/dashboards", nil)
	rec := httptest.NewRecorder()

	handler(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "hello from grafana", rec.Body.String())
	assert.Equal(t, "/api/dashboards", gotPath)
	assert.Equal(t, "grafana_session=abc123", gotCookie)
	assert.Equal(t, httputils.UserAgent, gotUserAgent)
}

func TestAuthenticateAndProxyHandler_NonOKStatusIsForwardedAsIs(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte("not found upstream"))
	}))
	t.Cleanup(backend.Close)

	cfg := &config.Context{Grafana: &config.GrafanaConfig{Server: backend.URL}}
	handler := grafana.AuthenticateAndProxyHandler(cfg)

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/dashboards", nil)
	rec := httptest.NewRecorder()

	handler(rec, req)

	assert.Equal(t, http.StatusNotFound, rec.Code)
	assert.Equal(t, "not found upstream", rec.Body.String())
}

func TestAuthenticateAndProxyHandler_RedirectToLoginRendersAuthenticationError(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/login", http.StatusFound)
	}))
	t.Cleanup(backend.Close)

	cfg := &config.Context{Grafana: &config.GrafanaConfig{Server: backend.URL}}
	handler := grafana.AuthenticateAndProxyHandler(cfg)

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/dashboards", nil)
	rec := httptest.NewRecorder()

	handler(rec, req)

	assert.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.Contains(t, rec.Body.String(), "Authentication error")
	assert.Contains(t, rec.Body.String(), "missing or incorrect")
}

func TestAuthenticateAndProxyHandler_RedirectElsewhereIsFollowed(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/redirected" {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("final destination"))
			return
		}
		http.Redirect(w, r, "/redirected", http.StatusFound)
	}))
	t.Cleanup(backend.Close)

	cfg := &config.Context{Grafana: &config.GrafanaConfig{Server: backend.URL}}
	handler := grafana.AuthenticateAndProxyHandler(cfg)

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/dashboards", nil)
	rec := httptest.NewRecorder()

	handler(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "final destination", rec.Body.String())
}

// TestAuthenticateAndProxyHandler_RotatesSessionOn401 verifies Task 5's dashboard-proxy wiring:
// when cfg has a resolved SessionSource, the client built inside AuthenticateAndProxyHandler goes
// through GrafanaConfig.WrapWithSession, so a 401 from the backend triggers a rotate-and-retry
// against the same server, with the fresh cookie persisted to the keychain and forwarded to the
// caller transparently (status 200, no visible failure).
func TestAuthenticateAndProxyHandler_RotatesSessionOn401(t *testing.T) {
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
		_, _ = w.Write([]byte("hello from grafana"))
	})

	backend := httptest.NewServer(mux)
	t.Cleanup(backend.Close)

	store := testutils.NewFakeKeychainStore()
	cfg := &config.Context{
		Grafana: &config.GrafanaConfig{
			Server:  backend.URL,
			Session: config.NewSessionSource(oldCookie, backend.URL, nil, store, account),
		},
	}
	handler := grafana.AuthenticateAndProxyHandler(cfg)

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/dashboards", nil)
	rec := httptest.NewRecorder()

	handler(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "hello from grafana", rec.Body.String())
	assert.Equal(t, int32(1), rotateCalls.Load())

	stored, ok := store.Value(account)
	assert.True(t, ok)
	assert.Equal(t, newCookie, stored)
}

// TestAuthenticateAndProxyHandler_RotateRejectedSurfacesOriginal401 is the fallback side of Task
// 5: when the rotate endpoint itself rejects the current cookie, the wrapped transport gives up
// (no retry) and the handler forwards the original 401 to the caller unchanged, exactly as it
// already forwards any other non-redirect status code.
func TestAuthenticateAndProxyHandler_RotateRejectedSurfacesOriginal401(t *testing.T) {
	const (
		oldCookie = "stale-cookie"
		account   = "grafanapi:test-context"
	)

	var proxiedCalls atomic.Int32

	mux := http.NewServeMux()
	mux.HandleFunc("/api/user/auth-tokens/rotate", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	})
	mux.HandleFunc("/api/dashboards", func(w http.ResponseWriter, _ *http.Request) {
		proxiedCalls.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte("unauthorized upstream"))
	})

	backend := httptest.NewServer(mux)
	t.Cleanup(backend.Close)

	store := testutils.NewFakeKeychainStore()
	cfg := &config.Context{
		Grafana: &config.GrafanaConfig{
			Server:  backend.URL,
			Session: config.NewSessionSource(oldCookie, backend.URL, nil, store, account),
		},
	}
	handler := grafana.AuthenticateAndProxyHandler(cfg)

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/dashboards", nil)
	rec := httptest.NewRecorder()

	handler(rec, req)

	assert.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.Equal(t, "unauthorized upstream", rec.Body.String())
	assert.Equal(t, int32(1), proxiedCalls.Load(), "no retry should be attempted when rotation is rejected")

	_, getErr := store.Get(account)
	assert.ErrorIs(t, getErr, keychain.ErrNotFound, "keychain must not be written when rotation is rejected")
}

func TestAuthenticateAndProxyHandler_RefusesCrossOriginRedirect(t *testing.T) {
	// The cookieless case is the one the proxy's own CheckRedirect has to carry: with a cookie the
	// origin-bound round-tripper already refuses the hop.
	testCases := []struct {
		name   string
		cookie string
	}{
		{name: "with session cookie", cookie: "abc123"},
		{name: "without session cookie", cookie: ""},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			trap := testutils.NewCookieTrap(t)
			backend := testutils.NewRedirectServer(t, trap.URL+"/cb?token=s3cret", http.StatusFound)

			cfg := &config.Context{Grafana: &config.GrafanaConfig{Server: backend.URL, SessionCookie: testCase.cookie}}
			handler := grafana.AuthenticateAndProxyHandler(cfg)

			req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/dashboards", nil)
			rec := httptest.NewRecorder()

			handler(rec, req)

			assert.Equal(t, http.StatusInternalServerError, rec.Code)
			assert.Equal(t, "Internal Server Error", strings.TrimSpace(rec.Body.String()))
			assert.NotContains(t, rec.Body.String(), "s3cret")
			assert.Zero(t, trap.Requests(), "the redirect target must never be contacted")
		})
	}
}

func TestAuthenticateAndProxyHandler_RefusedRedirectLogsOriginsOnly(t *testing.T) {
	trap := testutils.NewCookieTrap(t)
	backend := testutils.NewRedirectServer(t, trap.URL+"/cb?token=s3cret", http.StatusFound)

	cfg := &config.Context{Grafana: &config.GrafanaConfig{Server: backend.URL, SessionCookie: "abc123"}}
	handler := grafana.AuthenticateAndProxyHandler(cfg)

	var logs bytes.Buffer
	ctx := logging.Context(t.Context(), logging.NewSLogLogger(slog.NewTextHandler(&logs, nil)))
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/api/dashboards", nil)
	rec := httptest.NewRecorder()

	handler(rec, req)

	from, err := config.ServerOrigin(backend.URL)
	require.NoError(t, err)
	to, err := config.ServerOrigin(trap.URL)
	require.NoError(t, err)

	logged := logs.String()
	assert.Contains(t, logged, from)
	assert.Contains(t, logged, to)
	assert.NotContains(t, logged, "s3cret", "the Location query must not reach the log")
	assert.NotContains(t, logged, "/cb", "the Location path must not reach the log")
}

func TestAuthenticateAndProxyHandler_RedirectToLoginOnOtherOriginStillRendersAuthenticationError(t *testing.T) {
	trap := testutils.NewCookieTrap(t)
	backend := testutils.NewRedirectServer(t, trap.URL+"/login", http.StatusFound)

	cfg := &config.Context{Grafana: &config.GrafanaConfig{Server: backend.URL, SessionCookie: "abc123"}}
	handler := grafana.AuthenticateAndProxyHandler(cfg)

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/dashboards", nil)
	rec := httptest.NewRecorder()

	handler(rec, req)

	assert.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.Contains(t, rec.Body.String(), "Authentication error")
	assert.Zero(t, trap.Requests(), "no request is sent for a login redirect")
}

func TestAuthenticateAndProxyHandler_SameOriginRedirectKeepsCookie(t *testing.T) {
	var cookies []string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cookies = append(cookies, r.Header.Get("Cookie"))
		if r.URL.Path == "/redirected" {
			_, _ = w.Write([]byte("final destination"))
			return
		}
		http.Redirect(w, r, "/redirected", http.StatusFound)
	}))
	t.Cleanup(backend.Close)

	cfg := &config.Context{Grafana: &config.GrafanaConfig{Server: backend.URL, SessionCookie: "abc123"}}
	handler := grafana.AuthenticateAndProxyHandler(cfg)

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/dashboards", nil)
	rec := httptest.NewRecorder()

	handler(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "final destination", rec.Body.String())
	assert.Equal(t, []string{config.CookieHeaderValue("abc123"), config.CookieHeaderValue("abc123")}, cookies)
}
