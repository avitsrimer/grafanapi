package testutils_test

import (
	"errors"
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

// TestFakeKeychainStore_GetErr_Scripted ensures a scripted GetErr is returned for every account
// instead of keychain.ErrNotFound / the stored secret, so callers can simulate a genuine Keychain
// read failure (as opposed to "no item stored").
func TestFakeKeychainStore_GetErr_Scripted(t *testing.T) {
	req := require.New(t)

	store := testutils.NewFakeKeychainStore()
	req.NoError(store.Set("account", "secret"))

	boom := errors.New("keychain locked")
	store.GetErr = boom

	_, err := store.Get("account")
	req.ErrorIs(err, boom)
}

// TestFakeKeychainStore_ModifiedAtErr_Scripted mirrors TestFakeKeychainStore_GetErr_Scripted for
// ModifiedAt.
func TestFakeKeychainStore_ModifiedAtErr_Scripted(t *testing.T) {
	req := require.New(t)

	store := testutils.NewFakeKeychainStore()
	store.SetModified("account", time.Now())

	boom := errors.New("keychain locked")
	store.ModifiedAtErr = boom

	_, err := store.ModifiedAt("account")
	req.ErrorIs(err, boom)
}
