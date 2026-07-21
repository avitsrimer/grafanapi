package grafana_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/grafana/grafanapi/internal/config"
	"github.com/grafana/grafanapi/internal/grafana"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGetVersion_AttachesSessionCookie(t *testing.T) {
	var gotCookie string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotCookie = r.Header.Get("Cookie")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"version": "12.0.0"})
	}))
	defer server.Close()

	gCtx := &config.Context{
		Grafana: &config.GrafanaConfig{
			Server:        server.URL,
			SessionCookie: "abc123",
		},
	}

	version, err := grafana.GetVersion(gCtx)
	require.NoError(t, err)
	assert.Equal(t, "12.0.0", version.String())
	assert.Equal(t, "grafana_session=abc123", gotCookie)
}

func TestGetVersion_NoCookieMeansNoCookieHeader(t *testing.T) {
	var gotCookie string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotCookie = r.Header.Get("Cookie")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"version": "12.0.0"})
	}))
	defer server.Close()

	gCtx := &config.Context{
		Grafana: &config.GrafanaConfig{
			Server: server.URL,
		},
	}

	_, err := grafana.GetVersion(gCtx)
	require.NoError(t, err)
	assert.Empty(t, gotCookie)
}
