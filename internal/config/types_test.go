package config_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

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

// TestGrafanaConfig_IsEmpty_IgnoresSessionCookie guards against a regression where an
// otherwise-empty "grafana: {}" block with a stale/orphaned Keychain entry (populated by
// ResolveSessionCookie independently of the file contents) would report IsEmpty() == false,
// producing the more confusing "server is required" validation error instead of "grafana config
// is required".
func TestGrafanaConfig_IsEmpty_IgnoresSessionCookie(t *testing.T) {
	req := require.New(t)

	req.True(config.GrafanaConfig{SessionCookie: "stale-cookie-value"}.IsEmpty())
	req.False(config.GrafanaConfig{Server: "value", SessionCookie: "stale-cookie-value"}.IsEmpty())
}

// TestGrafanaConfig_IsEmpty_IgnoresSession guards against the same class of regression as
// TestGrafanaConfig_IsEmpty_IgnoresSessionCookie, but for the Session *SessionSource field: a
// resolved SessionSource attached to an otherwise-empty "grafana: {}" block must not affect
// emptiness either.
func TestGrafanaConfig_IsEmpty_IgnoresSession(t *testing.T) {
	req := require.New(t)

	src := config.NewSessionSource("cookie-value", "https://grafana.example.com", nil, nil, "acct")

	req.True(config.GrafanaConfig{Session: src}.IsEmpty())
	req.False(config.GrafanaConfig{Server: "value", Session: src}.IsEmpty())
}

// TestGrafanaConfig_IsEmpty_LiveWindow guards the plan's IsEmpty() requirement: unlike
// SessionCookie/Session, LiveWindow is a real persisted field and must participate in the
// emptiness comparison normally (empty => still empty; set => non-empty).
func TestGrafanaConfig_IsEmpty_LiveWindow(t *testing.T) {
	req := require.New(t)

	req.True(config.GrafanaConfig{}.IsEmpty())
	req.False(config.GrafanaConfig{LiveWindow: "12h"}.IsEmpty())
}

func TestGrafanaConfig_ParsedLiveWindow(t *testing.T) {
	testCases := []struct {
		name           string
		liveWindow     string
		expectedWindow time.Duration
		expectedSet    bool
		expectErr      bool
	}{
		{name: "unset", liveWindow: "", expectedWindow: 0, expectedSet: false},
		{name: "valid", liveWindow: "12h", expectedWindow: 12 * time.Hour, expectedSet: true},
		{name: "minimum boundary", liveWindow: "1m", expectedWindow: time.Minute, expectedSet: true},
		{name: "maximum boundary", liveWindow: "6d", expectedWindow: 6 * 24 * time.Hour, expectedSet: true},
		{name: "unparseable", liveWindow: "not-a-duration", expectErr: true},
		{name: "below minimum", liveWindow: "30s", expectErr: true},
		{name: "above maximum", liveWindow: "7d", expectErr: true},
		{name: "non-finite day count", liveWindow: "Infd", expectErr: true},
		{name: "day count overflowing int64 nanoseconds", liveWindow: "1e300d", expectErr: true},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			req := require.New(t)

			window, set, err := config.GrafanaConfig{LiveWindow: testCase.liveWindow}.ParsedLiveWindow()

			if testCase.expectErr {
				req.Error(err)
				return
			}

			req.NoError(err)
			req.Equal(testCase.expectedSet, set)
			req.Equal(testCase.expectedWindow, window)
		})
	}
}

// TestGrafanaConfig_Validate_LiveWindow ensures Validate rejects a set-but-invalid live-window
// (unparseable or out of [1m, 6d]) while accepting the boundaries and an unset value.
func TestGrafanaConfig_Validate_LiveWindow(t *testing.T) {
	testCases := []struct {
		name       string
		liveWindow string
		expectErr  bool
	}{
		{name: "unset", liveWindow: "", expectErr: false},
		{name: "minimum boundary", liveWindow: "1m", expectErr: false},
		{name: "maximum boundary", liveWindow: "6d", expectErr: false},
		{name: "unparseable", liveWindow: "not-a-duration", expectErr: true},
		{name: "below minimum", liveWindow: "30s", expectErr: true},
		{name: "above maximum", liveWindow: "7d", expectErr: true},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			req := require.New(t)

			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_ = json.NewEncoder(w).Encode(map[string]any{
					"settings": map[string]any{
						"namespace": "stacks-12345",
					},
				})
			}))
			defer server.Close()

			cfg := config.GrafanaConfig{Server: server.URL, LiveWindow: testCase.liveWindow}
			err := cfg.Validate("ctx")

			if testCase.expectErr {
				req.Error(err)
				req.ErrorContains(err, "live-window")
				return
			}

			req.NoError(err)
		})
	}
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
