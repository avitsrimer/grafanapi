//go:build !darwin

package config_test

import (
	"testing"

	"github.com/grafana/grafanapi/internal/config"
	"github.com/grafana/grafanapi/internal/keychain"
	"github.com/stretchr/testify/require"
)

// Test_ResolveContextSessionCookie_realNonDarwinStore exercises the actual non-darwin
// keychain.Store implementation (rather than a fake), proving that a context which never went
// through `grafanapi login` resolves cleanly on non-darwin platforms instead of failing every
// command with "unsupported platform" (see keychain_other.go's Get, which must wrap
// keychain.ErrNotFound for this to hold).
func Test_ResolveContextSessionCookie_realNonDarwinStore(t *testing.T) {
	store := keychain.NewStore()

	gCtx := &config.Context{Name: "default", Grafana: &config.GrafanaConfig{Server: "http://localhost:3000/"}}

	require.NoError(t, config.ResolveContextSessionCookie(store, gCtx))
	require.Empty(t, gCtx.Grafana.SessionCookie)
}
