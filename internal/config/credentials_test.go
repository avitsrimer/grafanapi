package config_test

import (
	"errors"
	"fmt"
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

func Test_EncodeSessionCredential(t *testing.T) {
	const cookie = "s3cr3t-cookie-value"

	const server = "https://Grafana.Example.com/sub/"

	secret, err := config.EncodeSessionCredential(cookie, server)
	require.NoError(t, err)

	wantOrigin, err := config.ServerOrigin(server)
	require.NoError(t, err)

	cred, err := keychain.DecodeCredential(secret)
	require.NoError(t, err)
	require.Equal(t, keychain.Credential{Cookie: cookie, Origin: wantOrigin}, cred)
}

func Test_EncodeSessionCredential_errors(t *testing.T) {
	const cookie = "s3cr3t-cookie-value"

	tests := []struct {
		name   string
		cookie string
		server string
		is     error
	}{
		{name: "invalid server", cookie: cookie, server: "bare-host.example", is: config.ErrInvalidServerURL},
		{name: "empty server", cookie: cookie, server: "", is: config.ErrInvalidServerURL},
		{name: "empty cookie", cookie: "", server: "https://grafana.example.com"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			secret, err := config.EncodeSessionCredential(tc.cookie, tc.server)
			require.Error(t, err)
			require.Empty(t, secret)

			if tc.is != nil {
				require.ErrorIs(t, err, tc.is)
			}

			require.NotContains(t, err.Error(), cookie)

			if tc.server != "" {
				require.NotContains(t, err.Error(), tc.server)
			}
		})
	}
}

func Test_ResolveSessionCookie_boundPayloadLoadsCookieNotJSON(t *testing.T) {
	const cookie = "s3cr3t-cookie-value"

	const server = "https://grafana.example.com"

	secret, err := config.EncodeSessionCredential(cookie, server)
	require.NoError(t, err)

	store := newFakeKeychainStore()
	store.cookies[keychain.Account("default")] = secret

	cfg := &config.Config{
		CurrentContext: "default",
		Contexts: map[string]*config.Context{
			"default": {Name: "default", Grafana: &config.GrafanaConfig{Server: server}},
		},
	}

	require.NoError(t, config.ResolveSessionCookie(store)(cfg))

	require.Equal(t, cookie, cfg.Contexts["default"].Grafana.SessionCookie)
	require.NotNil(t, cfg.Contexts["default"].Grafana.Session)

	current, _ := cfg.Contexts["default"].Grafana.Session.Current()
	require.Equal(t, cookie, current)
}

func Test_ResolveSessionCookie_malformedPayloadErrorsWithoutRawValue(t *testing.T) {
	const raw = `{"v":1,"cookie":"s3cr3t-cookie-value","origin":"https://grafana.example.com:443","extra":true}`

	store := newFakeKeychainStore()
	store.cookies[keychain.Account("default")] = raw

	cfg := &config.Config{
		CurrentContext: "default",
		Contexts: map[string]*config.Context{
			"default": {Name: "default", Grafana: &config.GrafanaConfig{Server: "https://grafana.example.com"}},
		},
	}

	err := config.ResolveSessionCookie(store)(cfg)
	require.ErrorIs(t, err, keychain.ErrMalformedCredential)
	require.Contains(t, err.Error(), `"default"`)
	require.NotContains(t, err.Error(), "s3cr3t-cookie-value")
	require.Empty(t, cfg.Contexts["default"].Grafana.SessionCookie)
	require.Nil(t, cfg.Contexts["default"].Grafana.Session)
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

func TestSessionBindingError(t *testing.T) {
	const cookie = "s3cr3t-cookie-value"

	bound, err := config.ServerOrigin("https://grafana.example.com")
	require.NoError(t, err)

	target, err := config.ServerOrigin("https://evil.example")
	require.NoError(t, err)

	t.Run("mismatch", func(t *testing.T) {
		var err error = &config.SessionBindingError{Context: "dev", BoundOrigin: bound, ServerOrigin: target}

		require.ErrorIs(t, err, config.ErrSessionOriginMismatch)
		require.NotErrorIs(t, err, config.ErrSessionUnbound)
		require.Contains(t, err.Error(), `"dev"`)
		require.Contains(t, err.Error(), bound)
		require.Contains(t, err.Error(), target)
		require.NotContains(t, err.Error(), cookie)
	})

	t.Run("unbound", func(t *testing.T) {
		var err error = &config.SessionBindingError{Context: "dev", ServerOrigin: target}

		require.ErrorIs(t, err, config.ErrSessionUnbound)
		require.NotErrorIs(t, err, config.ErrSessionOriginMismatch)
		require.Contains(t, err.Error(), `"dev"`)
		require.Contains(t, err.Error(), target)
		require.NotContains(t, err.Error(), bound)

		for _, override := range []string{"--config", "GRAFANAPI_CONFIG", "GRAFANA_SERVER", "XDG_CONFIG_*", "$HOME"} {
			require.Contains(t, err.Error(), override)
		}

		require.NotContains(t, err.Error(), cookie)
	})

	t.Run("wrapped", func(t *testing.T) {
		wrapped := fmt.Errorf("load: %w", &config.SessionBindingError{Context: "dev", BoundOrigin: bound, ServerOrigin: target})

		var be *config.SessionBindingError
		require.ErrorAs(t, wrapped, &be)
		require.ErrorIs(t, wrapped, config.ErrSessionOriginMismatch)
	})
}
