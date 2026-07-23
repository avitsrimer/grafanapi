package config_test

import (
	"errors"
	"testing"
	"time"

	"github.com/grafana/grafanapi/internal/config"
	"github.com/grafana/grafanapi/internal/keychain"
	"github.com/stretchr/testify/require"
)

// fakeKeychainStore is an in-memory keychain.Store for tests. errOnGet, when set, is returned by
// Get regardless of the requested account (used to simulate a real Keychain failure that is not
// keychain.ErrNotFound).
type fakeKeychainStore struct {
	cookies  map[string]string
	errOnGet error
}

func newFakeKeychainStore() *fakeKeychainStore {
	return &fakeKeychainStore{cookies: map[string]string{}}
}

func (f *fakeKeychainStore) Set(account, secret string) error {
	f.cookies[account] = secret
	return nil
}

func (f *fakeKeychainStore) Get(account string) (string, error) {
	if f.errOnGet != nil {
		return "", f.errOnGet
	}

	cookie, ok := f.cookies[account]
	if !ok {
		return "", keychain.ErrNotFound
	}

	return cookie, nil
}

func (f *fakeKeychainStore) Delete(account string) error {
	delete(f.cookies, account)
	return nil
}

// ModifiedAt is a trivial stub satisfying the grown keychain.Store interface: these tests never
// exercise last-rotation-time lookups.
func (f *fakeKeychainStore) ModifiedAt(string) (time.Time, error) {
	return time.Time{}, keychain.ErrNotFound
}

func Test_ResolveSessionCookie_populatesCurrentContext(t *testing.T) {
	store := newFakeKeychainStore()
	store.cookies[keychain.Account("default")] = "cookie-value"

	cfg := &config.Config{
		CurrentContext: "default",
		Contexts: map[string]*config.Context{
			"default": {Name: "default", Grafana: &config.GrafanaConfig{Server: "http://localhost:3000/"}},
		},
	}

	override := config.ResolveSessionCookie(store)
	require.NoError(t, override(cfg))

	require.Equal(t, "cookie-value", cfg.Contexts["default"].Grafana.SessionCookie)
}

func Test_ResolveSessionCookie_ignoresOtherContexts(t *testing.T) {
	store := newFakeKeychainStore()
	store.cookies[keychain.Account("other")] = "cookie-value"

	cfg := &config.Config{
		CurrentContext: "default",
		Contexts: map[string]*config.Context{
			"default": {Name: "default", Grafana: &config.GrafanaConfig{Server: "http://localhost:3000/"}},
			"other":   {Name: "other", Grafana: &config.GrafanaConfig{Server: "http://other:3000/"}},
		},
	}

	override := config.ResolveSessionCookie(store)
	require.NoError(t, override(cfg))

	require.Empty(t, cfg.Contexts["default"].Grafana.SessionCookie)
	require.Empty(t, cfg.Contexts["other"].Grafana.SessionCookie)
}

func Test_ResolveSessionCookie_noCurrentContext(t *testing.T) {
	store := newFakeKeychainStore()

	cfg := &config.Config{}

	override := config.ResolveSessionCookie(store)
	require.NoError(t, override(cfg))
}

func Test_ResolveSessionCookie_notFoundLeavesCookieEmpty(t *testing.T) {
	store := newFakeKeychainStore()

	cfg := &config.Config{
		CurrentContext: "default",
		Contexts: map[string]*config.Context{
			"default": {Name: "default", Grafana: &config.GrafanaConfig{Server: "http://localhost:3000/"}},
		},
	}

	override := config.ResolveSessionCookie(store)
	require.NoError(t, override(cfg))

	require.Empty(t, cfg.Contexts["default"].Grafana.SessionCookie)
}

func Test_ResolveSessionCookie_storeErrorSurfaces(t *testing.T) {
	store := newFakeKeychainStore()
	store.errOnGet = errors.New("keychain: boom")

	cfg := &config.Config{
		CurrentContext: "default",
		Contexts: map[string]*config.Context{
			"default": {Name: "default", Grafana: &config.GrafanaConfig{Server: "http://localhost:3000/"}},
		},
	}

	override := config.ResolveSessionCookie(store)
	require.ErrorContains(t, override(cfg), "keychain: boom")
}

func Test_ResolveContextSessionCookie_nilContextOrGrafana(t *testing.T) {
	store := newFakeKeychainStore()

	require.NoError(t, config.ResolveContextSessionCookie(store, nil))
	require.NoError(t, config.ResolveContextSessionCookie(store, &config.Context{Name: "default"}))
}

func Test_ResolveContextSessionCookie_populatesSessionSource(t *testing.T) {
	store := newFakeKeychainStore()
	store.cookies[keychain.Account("default")] = "cookie-value"

	gCtx := &config.Context{Name: "default", Grafana: &config.GrafanaConfig{Server: "http://localhost:3000/"}}

	require.NoError(t, config.ResolveContextSessionCookie(store, gCtx))

	require.Equal(t, "cookie-value", gCtx.Grafana.SessionCookie)
	require.NotNil(t, gCtx.Grafana.Session)

	cookie, gen := gCtx.Grafana.Session.Current()
	require.Equal(t, "cookie-value", cookie)
	require.Equal(t, uint64(0), gen)
}

func Test_ResolveContextSessionCookie_notFoundLeavesSessionNil(t *testing.T) {
	store := newFakeKeychainStore()

	gCtx := &config.Context{Name: "default", Grafana: &config.GrafanaConfig{Server: "http://localhost:3000/"}}

	require.NoError(t, config.ResolveContextSessionCookie(store, gCtx))

	require.Empty(t, gCtx.Grafana.SessionCookie)
	require.Nil(t, gCtx.Grafana.Session)
}

func Test_ResolveContextSessionCookie_storeErrorLeavesSessionNil(t *testing.T) {
	store := newFakeKeychainStore()
	store.errOnGet = errors.New("keychain: boom")

	gCtx := &config.Context{Name: "default", Grafana: &config.GrafanaConfig{Server: "http://localhost:3000/"}}

	require.ErrorContains(t, config.ResolveContextSessionCookie(store, gCtx), "keychain: boom")

	require.Empty(t, gCtx.Grafana.SessionCookie)
	require.Nil(t, gCtx.Grafana.Session)
}
