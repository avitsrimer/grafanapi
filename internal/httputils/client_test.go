package httputils_test

import (
	"testing"

	"github.com/grafana/grafanapi/internal/config"
	"github.com/grafana/grafanapi/internal/httputils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewTransport_NoTLSConfigUsesSystemTrust(t *testing.T) {
	gCtx := &config.Context{Grafana: &config.GrafanaConfig{Server: "https://example.invalid"}}

	transport, err := httputils.NewTransport(gCtx)
	require.NoError(t, err)
	require.NotNil(t, transport)
	require.NotNil(t, transport.TLSClientConfig)
	assert.False(t, transport.TLSClientConfig.InsecureSkipVerify)
}

// TestNewTransport_MalformedTLSConfigSurfacesError ties finding 4 (TLS.ToStdTLSConfig propagating
// PEM errors) to the serve/dashboard-proxy transport: malformed CAData must surface as an error
// from NewTransport rather than silently falling back to a plain system-trust TLS config.
func TestNewTransport_MalformedTLSConfigSurfacesError(t *testing.T) {
	gCtx := &config.Context{
		Grafana: &config.GrafanaConfig{
			Server: "https://example.invalid",
			TLS:    &config.TLS{CAData: []byte("not a certificate")},
		},
	}

	_, err := httputils.NewTransport(gCtx)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ca-data")
}
