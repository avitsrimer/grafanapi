package config_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
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

// boundFor returns the Keychain payload binding cookie to server through the production helper.
func boundFor(t *testing.T, cookie, server string) string {
	t.Helper()

	secret, err := config.EncodeSessionCredential(cookie, server)
	require.NoError(t, err)

	return secret
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

	require.NoError(t, config.ResolveSessionCookie(store, config.ResolveOptions{})(cfg))

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

	err := config.ResolveSessionCookie(store, config.ResolveOptions{})(cfg)
	require.ErrorIs(t, err, keychain.ErrMalformedCredential)
	require.Contains(t, err.Error(), `"default"`)
	require.NotContains(t, err.Error(), "s3cr3t-cookie-value")
	require.Empty(t, cfg.Contexts["default"].Grafana.SessionCookie)
	require.Nil(t, cfg.Contexts["default"].Grafana.Session)
}

func Test_ResolveSessionCookie_populatesCurrentContext(t *testing.T) {
	store := newFakeKeychainStore()
	store.cookies[keychain.Account("default")] = boundFor(t, "cookie-value", "http://localhost:3000/")

	cfg := &config.Config{
		CurrentContext: "default",
		Contexts: map[string]*config.Context{
			"default": {Name: "default", Grafana: &config.GrafanaConfig{Server: "http://localhost:3000/"}},
		},
	}

	override := config.ResolveSessionCookie(store, config.ResolveOptions{})
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

	override := config.ResolveSessionCookie(store, config.ResolveOptions{})
	require.NoError(t, override(cfg))

	require.Empty(t, cfg.Contexts["default"].Grafana.SessionCookie)
	require.Empty(t, cfg.Contexts["other"].Grafana.SessionCookie)
}

func Test_ResolveSessionCookie_noCurrentContext(t *testing.T) {
	store := newFakeKeychainStore()

	cfg := &config.Config{}

	override := config.ResolveSessionCookie(store, config.ResolveOptions{})
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

	override := config.ResolveSessionCookie(store, config.ResolveOptions{})
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

	override := config.ResolveSessionCookie(store, config.ResolveOptions{})
	require.ErrorContains(t, override(cfg), "keychain: boom")
}

func Test_ResolveContextSessionCookie_nilContextOrGrafana(t *testing.T) {
	store := newFakeKeychainStore()

	binding, err := config.ResolveContextSessionCookie(store, nil, config.ResolveOptions{})
	require.NoError(t, err)
	require.Equal(t, config.SessionBinding{State: config.BindingNone}, binding)

	_, err = config.ResolveContextSessionCookie(store, &config.Context{Name: "default"}, config.ResolveOptions{})
	require.NoError(t, err)
}

func Test_ResolveContextSessionCookie_populatesSessionSource(t *testing.T) {
	store := newFakeKeychainStore()
	store.cookies[keychain.Account("default")] = boundFor(t, "cookie-value", "http://localhost:3000/")

	gCtx := &config.Context{Name: "default", Grafana: &config.GrafanaConfig{Server: "http://localhost:3000/"}}

	binding, err := config.ResolveContextSessionCookie(store, gCtx, config.ResolveOptions{})
	require.NoError(t, err)
	require.Equal(t, config.BindingBound, binding.State)

	require.Equal(t, "cookie-value", gCtx.Grafana.SessionCookie)
	require.NotNil(t, gCtx.Grafana.Session)

	cookie, gen := gCtx.Grafana.Session.Current()
	require.Equal(t, "cookie-value", cookie)
	require.Equal(t, uint64(0), gen)
}

func Test_ResolveContextSessionCookie_notFoundLeavesSessionNil(t *testing.T) {
	store := newFakeKeychainStore()

	gCtx := &config.Context{Name: "default", Grafana: &config.GrafanaConfig{Server: "http://localhost:3000/"}}

	_, err := config.ResolveContextSessionCookie(store, gCtx, config.ResolveOptions{})
	require.NoError(t, err)

	require.Empty(t, gCtx.Grafana.SessionCookie)
	require.Nil(t, gCtx.Grafana.Session)
}

func Test_ResolveContextSessionCookie_storeErrorLeavesSessionNil(t *testing.T) {
	store := newFakeKeychainStore()
	store.errOnGet = errors.New("keychain: boom")

	gCtx := &config.Context{Name: "default", Grafana: &config.GrafanaConfig{Server: "http://localhost:3000/"}}

	_, err := config.ResolveContextSessionCookie(store, gCtx, config.ResolveOptions{})
	require.ErrorContains(t, err, "keychain: boom")

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

const bindingCookie = "s3cr3t-cookie-value"

// failingSetStore is a fakeKeychainStore whose Set fails, to exercise the best-effort write-back.
type failingSetStore struct {
	*fakeKeychainStore
}

func (f failingSetStore) Set(string, string) error {
	return errors.New("keychain: write refused")
}

// resolveFromFile loads a one-context configuration file whose server is fileServer through
// config.Load with the credential Override, optionally rewriting the effective server first
// (what GRAFANA_SERVER does), and returns the loaded context, the binding and the error.
func resolveFromFile(
	t *testing.T,
	store keychain.Store,
	fileServer, overrideServer string,
	opts config.ResolveOptions,
) (*config.Context, config.SessionBinding, error) {
	t.Helper()

	path := filepath.Join(t.TempDir(), "config.yaml")
	body := "current-context: default\ncontexts:\n  default:\n    grafana:\n      server: " + fileServer + "\n"
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))

	var (
		binding config.SessionBinding
		gCtx    *config.Context
	)

	override := func(cfg *config.Config) error {
		gCtx = cfg.Contexts["default"]
		if overrideServer != "" {
			gCtx.Grafana.Server = overrideServer
		}

		var err error

		binding, err = config.ResolveContextSessionCookie(store, gCtx, opts)

		return err
	}

	_, err := config.Load(context.Background(), config.ExplicitConfigFile(path), override)

	return gCtx, binding, err
}

func requireNothingLoaded(t *testing.T, gCtx *config.Context) {
	t.Helper()

	require.NotNil(t, gCtx)
	require.Empty(t, gCtx.Grafana.SessionCookie)
	require.Nil(t, gCtx.Grafana.Session)
}

func Test_ResolveContextSessionCookie_boundOrigin(t *testing.T) {
	canonical, err := config.ServerOrigin("https://grafana.example.com")
	require.NoError(t, err)

	nonCanonical, err := keychain.EncodeCredential(keychain.Credential{Cookie: bindingCookie, Origin: "https://grafana.example.com"})
	require.NoError(t, err)

	tests := []struct {
		name       string
		stored     string
		fileServer string
		wantLoaded bool
	}{
		{name: "same origin spelled differently", stored: boundFor(t, bindingCookie, canonical), fileServer: "https://GRAFANA.example.com:443/sub/", wantLoaded: true},
		{name: "http vs https on the same host", stored: boundFor(t, bindingCookie, canonical), fileServer: "http://grafana.example.com"},
		{name: "another port", stored: boundFor(t, bindingCookie, canonical), fileServer: "https://grafana.example.com:8443"},
		{name: "another host", stored: boundFor(t, bindingCookie, canonical), fileServer: "https://evil.example"},
		{name: "subdomain", stored: boundFor(t, bindingCookie, canonical), fileServer: "https://grafana.example.com.evil.example"},
		{name: "stored origin not canonical fails closed", stored: nonCanonical, fileServer: "https://grafana.example.com"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store := newFakeKeychainStore()
			store.cookies[keychain.Account("default")] = tc.stored

			gCtx, binding, err := resolveFromFile(t, store, tc.fileServer, "", config.ResolveOptions{})

			if tc.wantLoaded {
				require.NoError(t, err)
				require.Equal(t, config.BindingBound, binding.State)
				require.Equal(t, canonical, binding.Origin)
				require.Equal(t, bindingCookie, gCtx.Grafana.SessionCookie)
				require.NotNil(t, gCtx.Grafana.Session)

				return
			}

			require.ErrorIs(t, err, config.ErrSessionOriginMismatch)

			var bindErr *config.SessionBindingError
			require.ErrorAs(t, err, &bindErr)
			require.Equal(t, "default", bindErr.Context)
			require.NotContains(t, err.Error(), bindingCookie)
			require.Equal(t, config.SessionBinding{}, binding)
			requireNothingLoaded(t, gCtx)
			require.Equal(t, tc.stored, store.cookies[keychain.Account("default")], "the item must not be rewritten")
		})
	}
}

func Test_ResolveContextSessionCookie_effectiveServerOverrideMismatch(t *testing.T) {
	store := newFakeKeychainStore()
	store.cookies[keychain.Account("default")] = boundFor(t, bindingCookie, "https://grafana.example.com")

	gCtx, _, err := resolveFromFile(t, store, "https://grafana.example.com", "https://evil.example", config.ResolveOptions{AllowLegacyBind: true})

	require.ErrorIs(t, err, config.ErrSessionOriginMismatch)
	require.NotContains(t, err.Error(), bindingCookie)
	requireNothingLoaded(t, gCtx)
}

func Test_ResolveContextSessionCookie_legacyRefusedWithoutAllowLegacyBind(t *testing.T) {
	tests := []struct {
		name           string
		overrideServer string
	}{
		{name: "server from the file"},
		{name: "server overridden like GRAFANA_SERVER", overrideServer: "https://evil.example"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store := newFakeKeychainStore()
			store.cookies[keychain.Account("default")] = bindingCookie

			var notice bytes.Buffer

			gCtx, _, err := resolveFromFile(t, store, "https://grafana.example.com", tc.overrideServer,
				config.ResolveOptions{AllowLegacyBind: false, Notice: &notice})

			require.ErrorIs(t, err, config.ErrSessionUnbound)
			require.NotContains(t, err.Error(), bindingCookie)
			require.Empty(t, notice.String())
			requireNothingLoaded(t, gCtx)

			// Nothing may rewrite the item: a rotation would otherwise bind it silently.
			require.Equal(t, bindingCookie, store.cookies[keychain.Account("default")])
		})
	}
}

func Test_ResolveContextSessionCookie_legacyMigrates(t *testing.T) {
	store := newFakeKeychainStore()
	store.cookies[keychain.Account("default")] = bindingCookie

	var notice bytes.Buffer

	gCtx, binding, err := resolveFromFile(t, store, "https://Grafana.example.com/sub", "",
		config.ResolveOptions{AllowLegacyBind: true, Notice: &notice})
	require.NoError(t, err)

	wantOrigin, err := config.ServerOrigin("https://grafana.example.com")
	require.NoError(t, err)

	require.Equal(t, config.SessionBinding{State: config.BindingMigrated, Origin: wantOrigin}, binding)
	require.Equal(t, bindingCookie, gCtx.Grafana.SessionCookie)
	require.NotNil(t, gCtx.Grafana.Session)

	cred, err := keychain.DecodeCredential(store.cookies[keychain.Account("default")])
	require.NoError(t, err)
	require.Equal(t, keychain.Credential{Cookie: bindingCookie, Origin: wantOrigin}, cred)

	require.Contains(t, notice.String(), `"default"`)
	require.Contains(t, notice.String(), wantOrigin)
	require.NotContains(t, notice.String(), bindingCookie)
}

func Test_ResolveContextSessionCookie_legacyMigrationWriteFailureStillLoads(t *testing.T) {
	store := failingSetStore{newFakeKeychainStore()}
	store.cookies[keychain.Account("default")] = bindingCookie

	var notice bytes.Buffer

	gCtx, binding, err := resolveFromFile(t, store, "https://grafana.example.com", "",
		config.ResolveOptions{AllowLegacyBind: true, Notice: &notice})
	require.NoError(t, err)

	require.Equal(t, config.BindingMigrated, binding.State)
	require.Equal(t, bindingCookie, gCtx.Grafana.SessionCookie)
	require.Contains(t, notice.String(), "warning")
	require.Contains(t, notice.String(), "write refused")
	require.NotContains(t, notice.String(), bindingCookie)
	require.Equal(t, bindingCookie, store.cookies[keychain.Account("default")])
}

func Test_ResolveContextSessionCookie_emptyServerLoadsNothing(t *testing.T) {
	store := newFakeKeychainStore()
	store.cookies[keychain.Account("default")] = boundFor(t, bindingCookie, "https://grafana.example.com")

	gCtx := &config.Context{Name: "default", Grafana: &config.GrafanaConfig{}}

	binding, err := config.ResolveContextSessionCookie(store, gCtx, config.ResolveOptions{AllowLegacyBind: true})
	require.NoError(t, err)
	require.Equal(t, config.SessionBinding{State: config.BindingNone}, binding)
	requireNothingLoaded(t, gCtx)
}

func Test_ResolveContextSessionCookie_invalidServerWrapsServerURLError(t *testing.T) {
	for name, stored := range map[string]string{
		"bound":  boundFor(t, bindingCookie, "https://grafana.example.com"),
		"legacy": bindingCookie,
	} {
		t.Run(name, func(t *testing.T) {
			store := newFakeKeychainStore()
			store.cookies[keychain.Account("default")] = stored

			const server = "internal-host.test:3000"

			gCtx := &config.Context{Name: "default", Grafana: &config.GrafanaConfig{Server: server}}

			_, err := config.ResolveContextSessionCookie(store, gCtx, config.ResolveOptions{AllowLegacyBind: true})
			require.ErrorIs(t, err, config.ErrInvalidServerURL)
			require.Contains(t, err.Error(), "the session cookie was not sent")
			require.NotContains(t, err.Error(), server)
			require.NotContains(t, err.Error(), bindingCookie)
			requireNothingLoaded(t, gCtx)
			require.Equal(t, stored, store.cookies[keychain.Account("default")])
		})
	}
}
