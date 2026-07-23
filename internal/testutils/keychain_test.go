package testutils_test

import (
	"testing"
	"time"

	"github.com/grafana/grafanapi/internal/keychain"
	"github.com/grafana/grafanapi/internal/testutils"
	"github.com/stretchr/testify/require"
)

// TestFakeKeychainStore_ModifiedAt_NotFound ensures a fresh store (and an account that was never
// Set nor SetModified) reports keychain.ErrNotFound, matching the real Keychain's behavior for a
// missing item.
func TestFakeKeychainStore_ModifiedAt_NotFound(t *testing.T) {
	req := require.New(t)

	store := testutils.NewFakeKeychainStore()

	_, err := store.ModifiedAt("account")
	req.ErrorIs(err, keychain.ErrNotFound)
}

// TestFakeKeychainStore_ModifiedAt_TracksSet ensures Set records a recent mtime, mirroring
// securityd updating kSecAttrModificationDate on every write.
func TestFakeKeychainStore_ModifiedAt_TracksSet(t *testing.T) {
	req := require.New(t)

	store := testutils.NewFakeKeychainStore()
	before := time.Now()

	req.NoError(store.Set("account", "secret"))

	mtime, err := store.ModifiedAt("account")
	req.NoError(err)
	req.False(mtime.Before(before))
	req.False(mtime.After(time.Now()))
}

// TestFakeKeychainStore_ModifiedAt_SetModified ensures SetModified injects an arbitrary mtime,
// letting tests simulate a stale/fresh last-rotation time independently of when Set ran.
func TestFakeKeychainStore_ModifiedAt_SetModified(t *testing.T) {
	req := require.New(t)

	store := testutils.NewFakeKeychainStore()
	injected := time.Date(2020, time.January, 1, 0, 0, 0, 0, time.UTC)

	store.SetModified("account", injected)

	mtime, err := store.ModifiedAt("account")
	req.NoError(err)
	req.True(mtime.Equal(injected))
}

// TestFakeKeychainStore_ModifiedAt_DeleteClears ensures Delete removes the recorded mtime along
// with the secret, so a deleted-then-queried account reports ErrNotFound again.
func TestFakeKeychainStore_ModifiedAt_DeleteClears(t *testing.T) {
	req := require.New(t)

	store := testutils.NewFakeKeychainStore()
	req.NoError(store.Set("account", "secret"))
	req.NoError(store.Delete("account"))

	_, err := store.ModifiedAt("account")
	req.ErrorIs(err, keychain.ErrNotFound)
}
