//go:build darwin

package keychain_test

import (
	"testing"

	"github.com/grafana/grafanapi/internal/keychain"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	testCookie = "s3cr3t-cookie-value"
	testOrigin = "https://grafana.example.com:443"
)

func TestCredential_RoundTrip(t *testing.T) {
	raw, err := keychain.EncodeCredential(keychain.Credential{Cookie: testCookie, Origin: testOrigin})
	require.NoError(t, err)

	got, err := keychain.DecodeCredential(raw)
	require.NoError(t, err)
	assert.Equal(t, testCookie, got.Cookie)
	assert.Equal(t, testOrigin, got.Origin)
	assert.False(t, got.Legacy())
}

func TestCredential_EncodeShape(t *testing.T) {
	raw, err := keychain.EncodeCredential(keychain.Credential{Cookie: "c", Origin: testOrigin})
	require.NoError(t, err)
	assert.JSONEq(t, `{"v":1,"cookie":"c","origin":"`+testOrigin+`"}`, raw)
}

func TestDecodeCredential_Legacy(t *testing.T) {
	got, err := keychain.DecodeCredential(testCookie)
	require.NoError(t, err)
	assert.True(t, got.Legacy())
	assert.Equal(t, testCookie, got.Cookie)
	assert.Empty(t, got.Origin)
}

func TestDecodeCredential_Errors(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		wantErr error
	}{
		{"garbage after brace", `{not json` + testCookie, keychain.ErrMalformedCredential},
		{"unsupported version", `{"v":2,"cookie":"` + testCookie + `","origin":"` + testOrigin + `"}`, keychain.ErrUnsupportedCredential},
		{"missing version", `{"cookie":"` + testCookie + `","origin":"` + testOrigin + `"}`, keychain.ErrUnsupportedCredential},
		{"unknown field", `{"v":1,"cookie":"` + testCookie + `","origin":"` + testOrigin + `","x":1}`, keychain.ErrMalformedCredential},
		{"empty cookie", `{"v":1,"cookie":"","origin":"` + testOrigin + `"}`, keychain.ErrMalformedCredential},
		{"empty origin", `{"v":1,"cookie":"` + testCookie + `","origin":""}`, keychain.ErrMalformedCredential},
		{"trailing data", `{"v":1,"cookie":"` + testCookie + `","origin":"` + testOrigin + `"} x`, keychain.ErrMalformedCredential},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := keychain.DecodeCredential(tc.raw)
			require.ErrorIs(t, err, tc.wantErr)
			assert.NotContains(t, err.Error(), testCookie)
			assert.NotContains(t, err.Error(), tc.raw)
		})
	}
}

func TestEncodeCredential_Errors(t *testing.T) {
	tests := []struct {
		name string
		in   keychain.Credential
	}{
		{"empty cookie", keychain.Credential{Origin: testOrigin}},
		{"empty origin", keychain.Credential{Cookie: testCookie}},
		{"both empty", keychain.Credential{}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := keychain.EncodeCredential(tc.in)
			require.Error(t, err)
			assert.NotContains(t, err.Error(), testCookie)
		})
	}
}
