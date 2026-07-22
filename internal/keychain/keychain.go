//go:build darwin

// Package keychain provides macOS secret storage for the Grafana session cookie. The
// implementation (keychain_darwin.go) stores items in the macOS Keychain via hand-rolled cgo
// bindings to Security.framework. The package is darwin-only: grafanapi is distributed for
// macOS only, so there is no non-darwin build to support.
//
// Items are stored as plain generic-password entries with no access-control / accessibility
// attributes set (ACL-only design): the login keychain trusts the ad-hoc code identity of the
// binary that created the item, so that same binary reads it back silently, while a rebuilt
// binary (different ad-hoc cdhash) triggers the standard "Allow / Always Allow" prompt. There is
// no credential-agent daemon — every call goes straight to the Keychain.
package keychain

import "errors"

// Service is the generic-password service string for every item this package manages.
const Service = "grafanapi"

// ErrNotFound is returned by Store.Get when no item exists for the given account. It maps from
// the Security framework's errSecItemNotFound (-25300) on darwin.
var ErrNotFound = errors.New("keychain: item not found")

// Account derives the per-context keychain account string for contextName.
func Account(contextName string) string {
	return Service + ":" + contextName
}

// NewStore returns the platform Store implementation: the cgo-backed macOS Keychain store on
// darwin, or a stub that fails every operation on other platforms.
//
// We have to return an interface here.
//
//nolint:ireturn
func NewStore() Store {
	return newStore()
}

// Store reads and writes the Grafana session cookie in the platform secret store, keyed by
// account (see Account). The interface is deliberately platform-neutral so callers and tests
// never need cgo types.
type Store interface {
	// Set stores secret under account, creating or replacing the item without prompting. Set
	// must be atomic: if it returns an error, any secret previously stored under account must
	// be left intact (implementations must not implement this as a delete followed by an add,
	// since a failure between those two steps would destroy the prior value with no recovery).
	Set(account, secret string) error
	// Get returns the secret for account, or ErrNotFound if no item exists.
	Get(account string) (string, error)
	// Delete removes the item for account; a missing item is not an error.
	Delete(account string) error
}
