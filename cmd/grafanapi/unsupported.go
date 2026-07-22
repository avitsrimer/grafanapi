//go:build !darwin

package main

// This file makes a non-darwin build fail loudly and clearly instead of failing deep inside
// internal/keychain with a confusing "undefined: NewStore" (or, worse, succeeding and only
// failing at runtime). grafanapi is macOS-only: the Keychain-backed credential store
// (internal/keychain/keychain_darwin.go) is cgo against Security.framework and has no
// implementation for any other platform, so there is nothing sensible to build here.
//
// Referencing this undeclared identifier forces a compile-time error whose message names the
// reason directly, e.g.:
//
//	./unsupported.go:14:6: undefined: grafanapi_requires_macOS_this_platform_is_not_supported
var _ = grafanapi_requires_macOS_this_platform_is_not_supported
