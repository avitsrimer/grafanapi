package grafana_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/grafana/grafanapi/internal/config"
	"github.com/grafana/grafanapi/internal/httputils"
	"github.com/grafana/grafanapi/internal/server/grafana"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAuthenticateRequest_SetsCookieHeader(t *testing.T) {
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://example.invalid/api/dashboards", nil)
	require.NoError(t, err)

	grafana.AuthenticateRequest(&config.GrafanaConfig{SessionCookie: "abc123"}, req)

	assert.Equal(t, "grafana_session=abc123", req.Header.Get("Cookie"))
}

func TestAuthenticateRequest_NoCookieMeansNoCookieHeader(t *testing.T) {
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://example.invalid/api/dashboards", nil)
	require.NoError(t, err)

	grafana.AuthenticateRequest(&config.GrafanaConfig{}, req)

	assert.Empty(t, req.Header.Get("Cookie"))
}

func TestAuthenticateRequest_NilConfigDoesNotPanic(t *testing.T) {
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://example.invalid/api/dashboards", nil)
	require.NoError(t, err)

	assert.NotPanics(t, func() {
		grafana.AuthenticateRequest(nil, req)
	})
	assert.Empty(t, req.Header.Get("Cookie"))
}

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
