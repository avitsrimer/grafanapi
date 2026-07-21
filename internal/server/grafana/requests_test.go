package grafana_test

import (
	"net/http"
	"testing"

	"github.com/grafana/grafanapi/internal/config"
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
