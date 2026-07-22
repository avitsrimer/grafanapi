//go:build !darwin

package keychain_test

import (
	"testing"

	"github.com/grafana/grafanapi/internal/keychain"
	"github.com/stretchr/testify/require"
)

func TestStubStore_Set(t *testing.T) {
	store := keychain.NewStore()
	err := store.Set("grafanapi:prod", "value")
	require.ErrorContains(t, err, "unsupported platform")
}

func TestStubStore_Get(t *testing.T) {
	store := keychain.NewStore()
	_, err := store.Get("grafanapi:prod")
	require.ErrorContains(t, err, "unsupported platform")

	// Get must wrap keychain.ErrNotFound: callers such as config.ResolveContextSessionCookie
	// treat ErrNotFound as "no cookie stored yet" and continue, while any other error is fatal.
	// Without this, every command on a non-darwin build would fail outright, even for a context
	// that never went through `grafanapi login`.
	require.ErrorIs(t, err, keychain.ErrNotFound)
}

func TestStubStore_Delete(t *testing.T) {
	store := keychain.NewStore()
	err := store.Delete("grafanapi:prod")
	require.ErrorContains(t, err, "unsupported platform")
}

func TestNewStore_Other(t *testing.T) {
	store := keychain.NewStore()
	require.NotNil(t, store)
}

func TestAccount(t *testing.T) {
	require.Equal(t, "grafanapi:prod", keychain.Account("prod"))
}
