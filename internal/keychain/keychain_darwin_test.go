//go:build darwin

package keychain_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/grafana/grafanapi/internal/keychain"
	"github.com/stretchr/testify/require"
)

// testAccount returns a unique, throwaway account under the real "grafanapi" service so tests
// never collide with a developer's actual stored session cookie, and cleans up the item
// regardless of test outcome.
func testAccount(t *testing.T) string {
	t.Helper()
	account := fmt.Sprintf("grafanapi-test:%s-%d", t.Name(), time.Now().UnixNano())
	t.Cleanup(func() {
		_ = keychain.NewStore().Delete(account)
	})
	return account
}

func TestDarwinStore_SetGetRoundTrip(t *testing.T) {
	store := keychain.NewStore()
	account := testAccount(t)

	require.NoError(t, store.Set(account, "grafana-session-value"))

	got, err := store.Get(account)
	require.NoError(t, err)
	require.Equal(t, "grafana-session-value", got)
}

func TestDarwinStore_SetOverwrites(t *testing.T) {
	store := keychain.NewStore()
	account := testAccount(t)

	require.NoError(t, store.Set(account, "first-value"))
	require.NoError(t, store.Set(account, "second-value"))

	got, err := store.Get(account)
	require.NoError(t, err)
	require.Equal(t, "second-value", got)
}

func TestDarwinStore_DeleteRemoves(t *testing.T) {
	store := keychain.NewStore()
	account := testAccount(t)

	require.NoError(t, store.Set(account, "to-be-deleted"))
	require.NoError(t, store.Delete(account))

	_, err := store.Get(account)
	require.Error(t, err)
	require.ErrorIs(t, err, keychain.ErrNotFound)
}

func TestDarwinStore_GetAfterDelete(t *testing.T) {
	store := keychain.NewStore()
	account := testAccount(t)

	require.NoError(t, store.Set(account, "value"))
	require.NoError(t, store.Delete(account))
	require.NoError(t, store.Delete(account)) // deleting again is not an error

	_, err := store.Get(account)
	require.Error(t, err)
	require.ErrorIs(t, err, keychain.ErrNotFound)
}

func TestDarwinStore_GetNeverSetAccount(t *testing.T) {
	store := keychain.NewStore()
	account := testAccount(t)

	_, err := store.Get(account)
	require.Error(t, err)
	require.ErrorIs(t, err, keychain.ErrNotFound)
}

func TestNewStore_Darwin(t *testing.T) {
	store := keychain.NewStore()
	require.NotNil(t, store)
}

func TestAccount(t *testing.T) {
	require.Equal(t, "grafanapi:prod", keychain.Account("prod"))
}
