package config_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	authlib "github.com/grafana/authlib/types"
	"github.com/grafana/grafanapi/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewNamespacedRESTConfig_UsesBootdataStack(t *testing.T) {
	bootdataServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"settings": map[string]any{
				"namespace": "stacks-98765",
			},
		})
	}))
	defer bootdataServer.Close()

	ctx := config.Context{
		Grafana: &config.GrafanaConfig{
			Server:  bootdataServer.URL + "/grafana",
			StackID: 12345,
		},
	}

	restCfg := config.NewNamespacedRESTConfig(t.Context(), ctx)

	if got, want := restCfg.Namespace, authlib.CloudNamespaceFormatter(98765); got != want {
		t.Fatalf("expected namespace %s, got %s", want, got)
	}

	if ctx.Grafana.StackID != 12345 {
		t.Fatalf("expected original stack ID to remain unchanged, got %d", ctx.Grafana.StackID)
	}
}

func TestNewNamespacedRESTConfig_FallsBackOnBootdataError(t *testing.T) {
	bootdataServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer bootdataServer.Close()

	ctx := config.Context{
		Grafana: &config.GrafanaConfig{
			Server:  bootdataServer.URL,
			StackID: 555,
		},
	}

	restCfg := config.NewNamespacedRESTConfig(t.Context(), ctx)

	if got, want := restCfg.Namespace, authlib.CloudNamespaceFormatter(555); got != want {
		t.Fatalf("expected namespace %s, got %s", want, got)
	}
}

func TestNewNamespacedRESTConfig_FallsBackWhenBootdataNotStack(t *testing.T) {
	bootdataServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"settings": map[string]any{
				"namespace": "grafana",
			},
		})
	}))
	defer bootdataServer.Close()

	ctx := config.Context{
		Grafana: &config.GrafanaConfig{
			Server:  bootdataServer.URL,
			StackID: 42,
		},
	}

	restCfg := config.NewNamespacedRESTConfig(t.Context(), ctx)

	if got, want := restCfg.Namespace, authlib.CloudNamespaceFormatter(42); got != want {
		t.Fatalf("expected namespace %s, got %s", want, got)
	}
}

func TestNewNamespacedRESTConfig_InjectsSessionCookie(t *testing.T) {
	bootdataServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer bootdataServer.Close()

	ctx := config.Context{
		Grafana: &config.GrafanaConfig{
			Server:        bootdataServer.URL,
			StackID:       42,
			SessionCookie: "abc123",
		},
	}

	restCfg := config.NewNamespacedRESTConfig(t.Context(), ctx)
	require.NotNil(t, restCfg.WrapTransport)

	recorder := &recordingRoundTripper{}
	wrapped := restCfg.WrapTransport(recorder)

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, bootdataServer.URL+"/apis", nil)
	require.NoError(t, err)

	resp, err := wrapped.RoundTrip(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.NotNil(t, recorder.req)
	assert.Equal(t, "grafana_session=abc123", recorder.req.Header.Get("Cookie"))
}

func TestNewNamespacedRESTConfig_NoCookieMeansNoWrapTransport(t *testing.T) {
	bootdataServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer bootdataServer.Close()

	ctx := config.Context{
		Grafana: &config.GrafanaConfig{
			Server:  bootdataServer.URL,
			StackID: 42,
		},
	}

	restCfg := config.NewNamespacedRESTConfig(t.Context(), ctx)
	assert.Nil(t, restCfg.WrapTransport)
}

// recordingRoundTripper captures the last request it saw and returns a canned 200 response,
// used to assert headers added by a wrapped transport without hitting the network.
type recordingRoundTripper struct {
	req *http.Request
}

func (rt *recordingRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	rt.req = req

	return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody}, nil
}
