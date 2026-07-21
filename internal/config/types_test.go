package config_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/grafana/grafanapi/internal/config"
	"github.com/grafana/grafanapi/internal/format"
	"github.com/stretchr/testify/require"
)

func TestConfig_HasContext(t *testing.T) {
	req := require.New(t)

	cfg := config.Config{
		Contexts: map[string]*config.Context{
			"dev": {
				Grafana: &config.GrafanaConfig{Server: "dev-server"},
			},
		},
		CurrentContext: "dev",
	}

	req.True(cfg.HasContext("dev"))
	req.False(cfg.HasContext("prod"))
}

// TestGrafanaConfig_ParsesWithoutLegacyAuthFields ensures a config containing
// only the still-supported fields (server, org-id, stack-id, tls) parses
// cleanly now that User/Password/APIToken have been removed.
func TestGrafanaConfig_ParsesWithoutLegacyAuthFields(t *testing.T) {
	req := require.New(t)

	yamlDoc := `
server: https://grafana.example.com
org-id: 1
stack-id: 2
tls:
  insecure-skip-verify: true
`

	var grafana config.GrafanaConfig
	req.NoError(format.NewYAMLCodec().Decode(bytes.NewBufferString(yamlDoc), &grafana))

	req.Equal("https://grafana.example.com", grafana.Server)
	req.Equal(int64(1), grafana.OrgID)
	req.Equal(int64(2), grafana.StackID)
	req.NotNil(grafana.TLS)
	req.True(grafana.TLS.Insecure)
	req.Empty(grafana.SessionCookie)
}

// TestGrafanaConfig_SessionCookieNeverSerialized asserts that SessionCookie
// (the in-memory-only field resolved from the Keychain) never appears in
// encoded output, matching its `json:"-" yaml:"-"` tags.
func TestGrafanaConfig_SessionCookieNeverSerialized(t *testing.T) {
	req := require.New(t)

	grafana := config.GrafanaConfig{
		Server:        "https://grafana.example.com",
		SessionCookie: "super-secret-cookie-value",
	}

	var buf bytes.Buffer
	req.NoError(format.NewYAMLCodec().Encode(&buf, grafana))

	req.NotContains(buf.String(), "super-secret-cookie-value")
	req.NotContains(buf.String(), "SessionCookie")
	req.NotContains(buf.String(), "session-cookie")
}

func TestGrafanaConfig_IsEmpty(t *testing.T) {
	req := require.New(t)

	req.True(config.GrafanaConfig{}.IsEmpty())
	req.False(config.GrafanaConfig{TLS: &config.TLS{Insecure: true}}.IsEmpty())
	req.False(config.GrafanaConfig{Server: "value"}.IsEmpty())
}

func TestGrafanaConfig_Validate_AllowsDiscoveredStackID(t *testing.T) {
	req := require.New(t)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"settings": map[string]any{
				"namespace": "stacks-12345",
			},
		})
	}))
	defer server.Close()

	cfg := config.GrafanaConfig{Server: server.URL}

	req.NoError(cfg.Validate("ctx"))
}

func TestGrafanaConfig_Validate_AllowsDiscoveredStackIDAndSuppliedStackID(t *testing.T) {
	req := require.New(t)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"settings": map[string]any{
				"namespace": "stacks-12345",
			},
		})
	}))
	defer server.Close()

	cfg := config.GrafanaConfig{
		Server:  server.URL,
		StackID: 12345,
	}
	req.NoError(cfg.Validate("ctx"))
}

func TestGrafanaConfig_Validate_AllowsOrgId(t *testing.T) {
	req := require.New(t)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"settings": map[string]any{
				"namespace": "stacks-12345",
			},
		})
	}))
	defer server.Close()

	cfg := config.GrafanaConfig{
		Server: server.URL,
		OrgID:  1,
	}
	req.NoError(cfg.Validate("ctx"))
}

func TestGrafanaConfig_Validate_AllowsOrgIdWhenDiscoveryFails(t *testing.T) {
	req := require.New(t)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	cfg := config.GrafanaConfig{
		Server: server.URL,
		OrgID:  1,
	}
	req.NoError(cfg.Validate("ctx"))
}

func TestGrafanaConfig_Validate_MismatchedStackID(t *testing.T) {
	req := require.New(t)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"settings": map[string]any{
				"namespace": "stacks-12345",
			},
		})
	}))
	defer server.Close()

	cfg := config.GrafanaConfig{
		Server:  server.URL,
		StackID: 54321,
	}

	err := cfg.Validate("ctx")
	req.Error(err)
	req.ErrorContains(err, "mismatched")
}

func TestGrafanaConfig_Validate_MissingStackWhenBootdataUnavailable(t *testing.T) {
	req := require.New(t)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	cfg := config.GrafanaConfig{Server: server.URL}

	err := cfg.Validate("ctx")
	req.Error(err)
	req.ErrorContains(err, "missing")
}

func TestGrafanaConfig_Validate_BootdataUnavailableAndSuppliedStackId(t *testing.T) {
	req := require.New(t)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	cfg := config.GrafanaConfig{Server: server.URL, StackID: 5431}

	req.NoError(cfg.Validate("ctx"))
}

func TestMinify(t *testing.T) {
	req := require.New(t)

	cfg := config.Config{
		Contexts: map[string]*config.Context{
			"dev": {
				Grafana: &config.GrafanaConfig{
					Server: "dev-server",
				},
			},
			"prod": {
				Grafana: &config.GrafanaConfig{
					Server: "prod-server",
				},
			},
		},
		CurrentContext: "dev",
	}

	minified, err := config.Minify(cfg)
	req.NoError(err)

	req.Equal(config.Config{
		Contexts: map[string]*config.Context{
			"dev": {
				Grafana: &config.GrafanaConfig{
					Server: "dev-server",
				},
			},
		},
		CurrentContext: "dev",
	}, minified)
}

func TestMinify_withNoCurrentContext(t *testing.T) {
	req := require.New(t)

	cfg := config.Config{
		Contexts: map[string]*config.Context{
			"dev": {
				Grafana: &config.GrafanaConfig{
					Server: "dev-server",
				},
			},
			"prod": {
				Grafana: &config.GrafanaConfig{
					Server: "prod-server",
				},
			},
		},
		CurrentContext: "",
	}

	_, err := config.Minify(cfg)
	req.Error(err)
	req.ErrorContains(err, "current-context must be defined")
}
