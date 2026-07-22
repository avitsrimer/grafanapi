package config_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	authlib "github.com/grafana/authlib/types"
	"github.com/grafana/grafanapi/internal/config"
	"github.com/grafana/grafanapi/internal/keychain"
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
	require.NotNil(t, restCfg.WrapTransport)

	// WrapTransport is always installed now (NewNamespacedRESTConfig delegates the Session/
	// SessionCookie decision entirely to WrapWithSession's default case), but with neither set it
	// must pass every request through unchanged - no Cookie header added.
	recorder := &recordingRoundTripper{}
	wrapped := restCfg.WrapTransport(recorder)

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, bootdataServer.URL+"/apis", nil)
	require.NoError(t, err)

	resp, err := wrapped.RoundTrip(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.NotNil(t, recorder.req)
	assert.Empty(t, recorder.req.Header.Get("Cookie"))
}

// TestNewNamespacedRESTConfig_RotatesSessionOn401 verifies Task 3's wiring: when the context has
// a resolved SessionSource (as opposed to a bare static SessionCookie), NewNamespacedRESTConfig's
// WrapTransport goes through GrafanaConfig.WrapWithSession, so a 401 from the wrapped transport
// triggers a rotate-and-retry against the rotate server, with the fresh cookie persisted to the
// keychain.
func TestNewNamespacedRESTConfig_RotatesSessionOn401(t *testing.T) {
	rotateServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The same test server also fields the bootdata discovery probe NewNamespacedRESTConfig
		// issues against cfg.Grafana.Server; only the rotate path is scripted, bootdata is left
		// to fail soft (as in TestNewNamespacedRESTConfig_FallsBackOnBootdataError).
		if r.URL.Path != "/api/user/auth-tokens/rotate" {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}

		assert.Equal(t, "grafana_session=old-cookie", r.Header.Get("Cookie"))
		http.SetCookie(w, &http.Cookie{Name: "grafana_session", Value: "new-cookie", Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode})
		w.WriteHeader(http.StatusOK)
	}))
	defer rotateServer.Close()

	store := newFakeKeychainStore()
	session := config.NewSessionSource("old-cookie", rotateServer.URL, nil, store, "acct")

	ctx := config.Context{
		Grafana: &config.GrafanaConfig{
			Server:  rotateServer.URL,
			StackID: 42,
			Session: session,
		},
	}

	restCfg := config.NewNamespacedRESTConfig(t.Context(), ctx)
	require.NotNil(t, restCfg.WrapTransport)

	next := &scriptedSessionRoundTripper{newCookie: "new-cookie"}
	wrapped := restCfg.WrapTransport(next)

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, rotateServer.URL+"/apis/dashboards", nil)
	require.NoError(t, err)

	resp, err := wrapped.RoundTrip(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	require.Len(t, next.seen, 2)
	assert.Equal(t, "grafana_session=old-cookie", next.seen[0])
	assert.Equal(t, "grafana_session=new-cookie", next.seen[1])

	stored, getErr := store.Get("acct")
	require.NoError(t, getErr)
	assert.Equal(t, "new-cookie", stored)
}

// TestNewNamespacedRESTConfig_RotateRejectedSurfacesOriginal401 is the fallback side of Task 3:
// when the rotate endpoint itself rejects the current cookie, the wrapped transport must give up
// (no retry) and return the original 401 unchanged, so the existing centralized stale-session
// rendering (cmd/grafanapi/fail/convert.go, locked in by
// TestErrorToDetailedError_StaleSession/"k8s unauthorized StatusError") still fires once client-go
// turns that 401 into a k8sapi.StatusError.
func TestNewNamespacedRESTConfig_RotateRejectedSurfacesOriginal401(t *testing.T) {
	rotateServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer rotateServer.Close()

	store := newFakeKeychainStore()
	session := config.NewSessionSource("old-cookie", rotateServer.URL, nil, store, "acct")

	ctx := config.Context{
		Grafana: &config.GrafanaConfig{
			Server:  rotateServer.URL,
			StackID: 42,
			Session: session,
		},
	}

	restCfg := config.NewNamespacedRESTConfig(t.Context(), ctx)
	require.NotNil(t, restCfg.WrapTransport)

	next := &scriptedSessionRoundTripper{newCookie: "new-cookie"}
	wrapped := restCfg.WrapTransport(next)

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, rotateServer.URL+"/apis/dashboards", nil)
	require.NoError(t, err)

	resp, err := wrapped.RoundTrip(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	assert.Len(t, next.seen, 1, "no retry should be attempted when rotation is rejected")

	_, getErr := store.Get("acct")
	assert.ErrorIs(t, getErr, keychain.ErrNotFound, "keychain must not be written when rotation is rejected")
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

// scriptedSessionRoundTripper is the "next" transport wrapped by the rotating RoundTripper under
// test: it records every Cookie header it saw and returns 200 only once the inbound cookie
// matches newCookie, 401 otherwise - simulating a session that has gone stale and been rotated.
type scriptedSessionRoundTripper struct {
	newCookie string
	seen      []string
}

func (rt *scriptedSessionRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	cookie := req.Header.Get("Cookie")
	rt.seen = append(rt.seen, cookie)

	if cookie == "grafana_session="+rt.newCookie {
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: http.NoBody}, nil
	}

	return &http.Response{StatusCode: http.StatusUnauthorized, Header: http.Header{}, Body: http.NoBody}, nil
}
