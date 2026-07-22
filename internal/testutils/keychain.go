package testutils

import (
	"sync"

	"github.com/grafana/grafanapi/internal/keychain"
)

// FakeKeychainStore is an in-memory keychain.Store for tests, shared by the internal/grafana,
// internal/server, and internal/server/grafana test suites (they all needed the same thing: an
// account->secret map that observes whether/what a rotating transport persisted after a
// successful rotation).
type FakeKeychainStore struct {
	mu     sync.Mutex
	values map[string]string
}

// NewFakeKeychainStore returns an empty FakeKeychainStore.
func NewFakeKeychainStore() *FakeKeychainStore {
	return &FakeKeychainStore{values: map[string]string{}}
}

// Set stores secret under account.
func (f *FakeKeychainStore) Set(account, secret string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.values[account] = secret

	return nil
}

// Get returns the secret stored under account, or keychain.ErrNotFound if none was set.
func (f *FakeKeychainStore) Get(account string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

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

	return nil
}

// Value returns the secret currently stored under account, for test assertions.
func (f *FakeKeychainStore) Value(account string) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()

	v, ok := f.values[account]

	return v, ok
}
