//go:build darwin

// White-box (package keychain, not keychain_test) so these can reach the unexported withTimeout /
// runWithTimeout helpers directly with an injected slow function, instead of depending on a real
// ACL-blocked Keychain item (which would require deliberately breaking a stored item's ACL and is
// not reproducible in CI).
//
//nolint:testpackage // intentional white-box test, see comment above
package keychain

import (
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestWithTimeout_ReturnsResultWhenFastEnough(t *testing.T) {
	val, err := withTimeout(50*time.Millisecond, func() (string, error) {
		return "ok", nil
	})
	require.NoError(t, err)
	require.Equal(t, "ok", val)
}

func TestWithTimeout_PropagatesUnderlyingError(t *testing.T) {
	wantErr := errors.New("boom")

	_, err := withTimeout(50*time.Millisecond, func() (string, error) {
		return "", wantErr
	})
	require.ErrorIs(t, err, wantErr)
}

func TestWithTimeout_TimesOutOnSlowFunc(t *testing.T) {
	// The injected fn keeps running past the timeout, simulating a Get call parked in securityd
	// waiting on an authorization dialog that will never come. withTimeout must give up and return
	// a descriptive error instead of blocking the caller forever; the goroutine running fn is
	// intentionally abandoned (see withTimeout's doc comment on the leak tradeoff). We still drain
	// `done` in cleanup purely so this test doesn't itself leak past its own execution window.
	done := make(chan struct{})
	t.Cleanup(func() { <-done })

	_, err := withTimeout(20*time.Millisecond, func() (string, error) {
		defer close(done)
		time.Sleep(200 * time.Millisecond)
		return "too-late", nil
	})

	require.Error(t, err)
	require.Contains(t, err.Error(), "keychain access timed out after 20ms")
	require.Contains(t, err.Error(), "Always Allow")
	require.Contains(t, err.Error(), `service "grafanapi"`)
}

func TestRunWithTimeout_TimesOutOnSlowFunc(t *testing.T) {
	done := make(chan struct{})
	t.Cleanup(func() { <-done })

	err := runWithTimeout(20*time.Millisecond, func() error {
		defer close(done)
		time.Sleep(200 * time.Millisecond)
		return nil
	})

	require.Error(t, err)
	require.Contains(t, err.Error(), "keychain access timed out after 20ms")
}

func TestRunWithTimeout_ReturnsNoErrorWhenFastEnough(t *testing.T) {
	err := runWithTimeout(50*time.Millisecond, func() error {
		return nil
	})
	require.NoError(t, err)
}
