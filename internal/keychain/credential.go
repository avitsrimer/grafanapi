//go:build darwin

package keychain

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"unicode/utf8"
)

// CredentialVersion is the payload version written by EncodeCredential.
const CredentialVersion = 1

// ErrMalformedCredential is returned by DecodeCredential for a payload that is not valid
// versioned JSON, has a missing or non-positive version, or lacks a cookie or an origin. Its
// message never includes the stored item.
var ErrMalformedCredential = errors.New("keychain: stored session item is malformed")

// ErrUnsupportedCredential is returned by DecodeCredential for a payload whose version is greater
// than CredentialVersion. Its message never includes the stored item.
var ErrUnsupportedCredential = errors.New("keychain: stored session item was written by a newer grafanapi")

// EncodeCredential serialises c as the versioned payload stored as the Keychain secret. It
// requires a non-empty Cookie and a non-empty Origin, both valid UTF-8 (encoding/json would
// silently rewrite invalid bytes to U+FFFD, so the stored value would differ from the one the
// caller verified); it does not judge whether the origin is canonical, because normalising is
// the caller's job (this package cannot import internal/config).
func EncodeCredential(c Credential) (string, error) {
	if c.Cookie == "" {
		return "", errors.New("keychain: cannot encode a session without a cookie")
	}
	if c.Origin == "" {
		return "", errors.New("keychain: cannot encode a session without an origin")
	}
	if !utf8.ValidString(c.Cookie) || !utf8.ValidString(c.Origin) {
		return "", errors.New("keychain: cannot encode a session that is not valid UTF-8")
	}

	raw, err := json.Marshal(credentialPayload{Version: CredentialVersion, Cookie: c.Cookie, Origin: c.Origin})
	if err != nil {
		return "", errors.New("keychain: cannot encode the session payload")
	}

	return string(raw), nil
}

// DecodeCredential parses a stored secret. A value that does not start with "{" is a legacy
// item holding the bare cookie and decodes to a Credential with no origin (Legacy reports
// true). Anything starting with "{" must be a strict versioned payload with a cookie and an
// origin. The JSON error is deliberately not wrapped, so no part of the item reaches a message.
func DecodeCredential(raw string) (Credential, error) {
	if !strings.HasPrefix(raw, "{") {
		return Credential{Cookie: raw}, nil
	}

	var payload credentialPayload
	dec := json.NewDecoder(bytes.NewReader([]byte(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&payload); err != nil {
		return Credential{}, ErrMalformedCredential
	}
	// Reject trailing data after the object.
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return Credential{}, ErrMalformedCredential
	}

	if payload.Version < CredentialVersion {
		return Credential{}, ErrMalformedCredential
	}
	if payload.Version > CredentialVersion {
		return Credential{}, ErrUnsupportedCredential
	}
	if payload.Cookie == "" || payload.Origin == "" {
		return Credential{}, ErrMalformedCredential
	}

	return Credential{Cookie: payload.Cookie, Origin: payload.Origin}, nil
}

// Credential is a session cookie together with the origin string it was bound to when stored.
// Origin is empty for a legacy item written before binding existed.
type Credential struct {
	Cookie string
	Origin string
}

// Legacy reports whether the credential came from an item that carries no origin.
func (c Credential) Legacy() bool {
	return c.Origin == ""
}

type credentialPayload struct {
	Version int    `json:"v"`
	Cookie  string `json:"cookie"`
	Origin  string `json:"origin"`
}
