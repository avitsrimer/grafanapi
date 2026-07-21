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
