package testutils

import (
	"sync"
	"time"

	"github.com/grafana/grafanapi/internal/keychain"
)

// FakeKeychainStore is an in-memory keychain.Store for tests, shared by the internal/grafana,
// internal/server, and internal/server/grafana test suites (they all needed the same thing: an
// account->secret map that observes whether/what a rotating transport persisted after a
// successful rotation).
type FakeKeychainStore struct {
	mu     sync.Mutex
	values map[string]string
	mtimes map[string]time.Time

	// GetErr, when non-nil, is returned by every Get call instead of the usual map lookup /
	// keychain.ErrNotFound - it lets a test simulate a genuine Keychain read failure (locked
	// keychain, timeout, ...) as distinct from "no item stored".
	GetErr error
	// ModifiedAtErr, when non-nil, is returned by every ModifiedAt call instead of the usual map
	// lookup / keychain.ErrNotFound - same rationale as GetErr, for callers (like `session refresh
	// --due`'s dueContexts) that read the last-rotation time independently of the cookie itself.
	ModifiedAtErr error
}

// NewFakeKeychainStore returns an empty FakeKeychainStore.
func NewFakeKeychainStore() *FakeKeychainStore {
	return &FakeKeychainStore{values: map[string]string{}, mtimes: map[string]time.Time{}}
}

// Set stores secret under account and records the current time as its modification time, mirroring
// the real Keychain's securityd-managed kSecAttrModificationDate being updated on every write.
func (f *FakeKeychainStore) Set(account, secret string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.values[account] = secret
	f.mtimes[account] = time.Now()

	return nil
}

// Get returns the secret stored under account, or keychain.ErrNotFound if none was set. If GetErr
// is set, it is returned instead for every account, regardless of what was stored.
func (f *FakeKeychainStore) Get(account string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.GetErr != nil {
		return "", f.GetErr
	}

	v, ok := f.values[account]
	if !ok {
		return "", keychain.ErrNotFound
	}

	return v, nil
}

// Delete removes the secret stored under account.
func (f *FakeKeychainStore) Delete(account string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	delete(f.values, account)
	delete(f.mtimes, account)

	return nil
}

// ModifiedAt returns the mtime recorded for account (either injected via SetModified or set by the
// most recent Set), or keychain.ErrNotFound if no item exists. If ModifiedAtErr is set, it is
// returned instead for every account, regardless of what was recorded.
func (f *FakeKeychainStore) ModifiedAt(account string) (time.Time, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.ModifiedAtErr != nil {
		return time.Time{}, f.ModifiedAtErr
	}

	mtime, ok := f.mtimes[account]
	if !ok {
		return time.Time{}, keychain.ErrNotFound
	}

	return mtime, nil
}

// SetModified injects mtime as the modification time for account, without affecting its stored
// secret. It lets tests control "last rotation age" precisely (fresh vs. stale) independently of
// when Set was actually called.
func (f *FakeKeychainStore) SetModified(account string, mtime time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.mtimes == nil {
		f.mtimes = map[string]time.Time{}
	}

	f.mtimes[account] = mtime
}

// Value returns the secret currently stored under account, for test assertions.
func (f *FakeKeychainStore) Value(account string) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()

	v, ok := f.values[account]

	return v, ok
}
