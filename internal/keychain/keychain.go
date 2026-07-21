// Package keychain provides platform secret storage for the Grafana session cookie. The
// darwin implementation (keychain_darwin.go) stores items in the macOS Keychain via hand-rolled
// cgo bindings to Security.framework; the non-darwin stub (keychain_other.go) fails every
// operation so cross-compilation to other platforms stays green without dragging cgo along.
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
	// Set stores secret under account, creating or replacing the item without prompting.
	Set(account, secret string) error
	// Get returns the secret for account, or ErrNotFound if no item exists.
	Get(account string) (string, error)
	// Delete removes the item for account; a missing item is not an error.
	Delete(account string) error
}
