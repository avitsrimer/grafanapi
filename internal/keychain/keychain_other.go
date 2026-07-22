//go:build !darwin

package keychain

import (
	"fmt"
	"runtime"
)

// newStore returns a stub Store that fails every operation on non-darwin platforms. It keeps
// go vet and cross-builds green without dragging cgo onto other platforms.
//
// We have to return an interface here.
//
//nolint:ireturn
func newStore() Store {
	return stubStore{}
}

// stubStore is the non-darwin Store: the macOS Keychain is unavailable off darwin, so every
// operation reports the unsupported platform.
type stubStore struct{}

// Set is unsupported off darwin.
func (stubStore) Set(account, _ string) error {
	return fmt.Errorf("keychain set for account %q: unsupported platform %s", account, runtime.GOOS)
}

// Get is unsupported off darwin. It reports ErrNotFound (rather than a bare "unsupported
// platform" error) so callers that treat a missing item as "not logged in yet" — e.g.
// config.ResolveContextSessionCookie — degrade gracefully instead of failing every command
// outright on non-darwin builds. Set and Delete still hard-fail: there is no equivalent "absent
// value" to report for a write.
func (stubStore) Get(account string) (string, error) {
	return "", fmt.Errorf("keychain get for account %q: unsupported platform %s: %w", account, runtime.GOOS, ErrNotFound)
}

// Delete is unsupported off darwin.
func (stubStore) Delete(account string) error {
	return fmt.Errorf("keychain delete for account %q: unsupported platform %s", account, runtime.GOOS)
}
