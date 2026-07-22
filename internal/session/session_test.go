package session_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/grafana/grafanapi/internal/config"
	"github.com/grafana/grafanapi/internal/session"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestVerifyCookie_Success(t *testing.T) {
	var (
		pathCheck   string
		cookieCheck string
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pathCheck = r.URL.Path
		cookieCheck = r.Header.Get("Cookie")
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	gCtx := &config.Context{
		Grafana: &config.GrafanaConfig{
			Server:        server.URL,
			SessionCookie: "abc123",
		},
	}

	err := session.VerifyCookie(t.Context(), gCtx)
	require.NoError(t, err)
	assert.Equal(t, "/api/user", pathCheck)
	assert.Equal(t, config.CookieHeaderValue("abc123"), cookieCheck)
}

func TestVerifyCookie_Unauthorized(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()

	gCtx := &config.Context{
		Grafana: &config.GrafanaConfig{
			Server:        server.URL,
			SessionCookie: "stale-cookie",
		},
	}

	err := session.VerifyCookie(t.Context(), gCtx)
	require.Error(t, err)
	assert.ErrorIs(t, err, session.ErrUnauthorized)
}

func TestVerifyCookie_UnexpectedStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	gCtx := &config.Context{
		Grafana: &config.GrafanaConfig{
			Server:        server.URL,
			SessionCookie: "abc123",
		},
	}

	err := session.VerifyCookie(t.Context(), gCtx)
	require.Error(t, err)
	assert.NotErrorIs(t, err, session.ErrUnauthorized)
}

func TestVerifyCookie_TLSSkipVerify(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	gCtx := &config.Context{
		Grafana: &config.GrafanaConfig{
			Server:        server.URL,
			SessionCookie: "abc123",
			TLS: &config.TLS{
				Insecure: true,
			},
		},
	}

	err := session.VerifyCookie(t.Context(), gCtx)
	require.NoError(t, err)
}

func TestVerifyCookie_NoGrafanaContext(t *testing.T) {
	err := session.VerifyCookie(t.Context(), &config.Context{})
	require.Error(t, err)
}

func TestVerifyCookie_InvalidServerAddress(t *testing.T) {
	gCtx := &config.Context{
		Grafana: &config.GrafanaConfig{
			Server:        "://not-a-valid-url",
			SessionCookie: "abc123",
		},
	}

	err := session.VerifyCookie(t.Context(), gCtx)
	require.Error(t, err)
}

// TestVerifyCookie_MalformedTLSConfigSurfacesError ties finding 4 (TLS.ToStdTLSConfig propagating
// PEM errors) to the login/login-update verification path: malformed CAData must surface as an
// error from VerifyCookie rather than silently falling back to a plain system-trust TLS config.
func TestVerifyCookie_MalformedTLSConfigSurfacesError(t *testing.T) {
	gCtx := &config.Context{
		Grafana: &config.GrafanaConfig{
			Server:        "https://example.invalid",
			SessionCookie: "abc123",
			TLS:           &config.TLS{CAData: []byte("not a certificate")},
		},
	}

	err := session.VerifyCookie(t.Context(), gCtx)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ca-data")
}

func TestCurrentOrgID_Success(t *testing.T) {
	var (
		pathCheck   string
		cookieCheck string
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pathCheck = r.URL.Path
		cookieCheck = r.Header.Get("Cookie")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":5}`))
	}))
	defer server.Close()

	gCtx := &config.Context{
		Grafana: &config.GrafanaConfig{
			Server:        server.URL,
			SessionCookie: "abc123",
		},
	}

	orgID, err := session.CurrentOrgID(t.Context(), gCtx)
	require.NoError(t, err)
	assert.Equal(t, int64(5), orgID)
	assert.Equal(t, "/api/org", pathCheck)
	assert.Equal(t, config.CookieHeaderValue("abc123"), cookieCheck)
}

func TestCurrentOrgID_Unauthorized(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()

	gCtx := &config.Context{
		Grafana: &config.GrafanaConfig{
			Server:        server.URL,
			SessionCookie: "stale-cookie",
		},
	}

	_, err := session.CurrentOrgID(t.Context(), gCtx)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "401")
}

func TestCurrentOrgID_MalformedBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("not json"))
	}))
	defer server.Close()

	gCtx := &config.Context{
		Grafana: &config.GrafanaConfig{
			Server:        server.URL,
			SessionCookie: "abc123",
		},
	}

	_, err := session.CurrentOrgID(t.Context(), gCtx)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "decoding organization id response")
}

func TestCurrentOrgID_ZeroIDIsError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":0}`))
	}))
	defer server.Close()

	gCtx := &config.Context{
		Grafana: &config.GrafanaConfig{
			Server:        server.URL,
			SessionCookie: "abc123",
		},
	}

	_, err := session.CurrentOrgID(t.Context(), gCtx)
	require.Error(t, err)
}

func TestCurrentOrgID_NoGrafanaContext(t *testing.T) {
	_, err := session.CurrentOrgID(t.Context(), &config.Context{})
	require.Error(t, err)
}

func TestCurrentOrgID_InvalidServerAddress(t *testing.T) {
	gCtx := &config.Context{
		Grafana: &config.GrafanaConfig{
			Server:        "://not-a-valid-url",
			SessionCookie: "abc123",
		},
	}

	_, err := session.CurrentOrgID(t.Context(), gCtx)
	require.Error(t, err)
}
