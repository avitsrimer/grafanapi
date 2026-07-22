package grafana_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/go-openapi/runtime"
	"github.com/grafana/grafanapi/internal/config"
	"github.com/grafana/grafanapi/internal/grafana"
	"github.com/grafana/grafanapi/internal/keychain"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeStore is a minimal in-memory keychain.Store used only to observe whether/what
// ClientFromContext's rotating transport persists after a successful rotation.
type fakeStore struct {
	mu     sync.Mutex
	values map[string]string
}

func newFakeStore() *fakeStore {
	return &fakeStore{values: map[string]string{}}
}

func (f *fakeStore) Set(account, secret string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.values[account] = secret

	return nil
}

func (f *fakeStore) Get(account string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	v, ok := f.values[account]
	if !ok {
		return "", keychain.ErrNotFound
	}

	return v, nil
}

func (f *fakeStore) Delete(account string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.values, account)

	return nil
}

func (f *fakeStore) value(account string) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.values[account]

	return v, ok
}

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

func TestGetVersion_RotatesSessionOn401(t *testing.T) {
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
	mux.HandleFunc("/api/health", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Cookie") != "grafana_session="+newCookie {
			w.WriteHeader(http.StatusUnauthorized)

			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"version": "12.0.0"})
	})

	server := httptest.NewServer(mux)
	defer server.Close()

	store := newFakeStore()
	gCtx := &config.Context{
		Grafana: &config.GrafanaConfig{
			Server:  server.URL,
			Session: config.NewSessionSource(oldCookie, server.URL, nil, store, account),
		},
	}

	version, err := grafana.GetVersion(gCtx)
	require.NoError(t, err)
	assert.Equal(t, "12.0.0", version.String())
	assert.Equal(t, int32(1), rotateCalls.Load())

	stored, ok := store.value(account)
	assert.True(t, ok)
	assert.Equal(t, newCookie, stored)
}

func TestGetVersion_RotateRejectedSurfacesOriginal401(t *testing.T) {
	const (
		oldCookie = "stale-cookie"
		account   = "grafanapi:test-context"
	)

	mux := http.NewServeMux()
	mux.HandleFunc("/api/user/auth-tokens/rotate", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	})
	mux.HandleFunc("/api/health", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	})

	server := httptest.NewServer(mux)
	defer server.Close()

	store := newFakeStore()
	gCtx := &config.Context{
		Grafana: &config.GrafanaConfig{
			Server:  server.URL,
			Session: config.NewSessionSource(oldCookie, server.URL, nil, store, account),
		},
	}

	_, err := grafana.GetVersion(gCtx)
	require.Error(t, err)

	apiErr := &runtime.APIError{}
	require.ErrorAs(t, err, &apiErr)
	assert.Equal(t, http.StatusUnauthorized, apiErr.Code)

	_, ok := store.value(account)
	assert.False(t, ok)
}
