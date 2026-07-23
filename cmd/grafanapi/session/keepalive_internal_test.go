// Package session (whitebox, not session_test) is intentional here, mirroring refresh_test.go's
// rationale: deriveInterval and minLiveWindowAcrossContexts are pure, unexported helpers behind
// "session keepalive install", and are exercised directly rather than only indirectly through a
// full command run. No //nolint:testpackage is needed here: refresh_test.go, already in this same
// package, already accounts for the testpackage linter's one package-level finding.
package session

import (
	"testing"
	"time"

	"github.com/grafana/grafanapi/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestDeriveInterval covers both clamp branches (the 15m floor and the 12h ceiling) as well as the
// unclamped middle of the range, so "session keepalive install"'s default-interval derivation
// (minWindow/2) is verified at every boundary, not just by incidental coverage through the
// install command's integration tests.
func TestDeriveInterval(t *testing.T) {
	testCases := []struct {
		name      string
		minWindow time.Duration
		expected  time.Duration
	}{
		{
			name:      "floor clamp: half of the smallest live-window is below 15m",
			minWindow: time.Minute, // half = 30s, clamped up to derivedIntervalMin (15m)
			expected:  derivedIntervalMin,
		},
		{
			name:      "ceiling clamp: half of the live-window is above 12h",
			minWindow: 30 * time.Hour, // half = 15h, clamped down to derivedIntervalMax (12h)
			expected:  derivedIntervalMax,
		},
		{
			name:      "unclamped: half falls within [15m, 12h]",
			minWindow: time.Hour, // half = 30m, no clamping
			expected:  30 * time.Minute,
		},
		{
			name:      "floor boundary: half exactly 15m",
			minWindow: 30 * time.Minute,
			expected:  derivedIntervalMin,
		},
		{
			name:      "ceiling boundary: half exactly 12h",
			minWindow: 24 * time.Hour,
			expected:  derivedIntervalMax,
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			assert.Equal(t, testCase.expected, deriveInterval(testCase.minWindow))
		})
	}
}

// TestMinLiveWindowAcrossContexts_InvalidAndMultiContext covers the two branches finding 10 called
// out: a set-but-invalid live-window records a warning and is excluded (rather than aborting
// derivation for every other context), and across multiple opted-in contexts the smallest
// live-window wins.
func TestMinLiveWindowAcrossContexts_InvalidAndMultiContext(t *testing.T) {
	t.Run("invalid window warns and is excluded, valid ones still considered", func(t *testing.T) {
		cfg := &config.Config{
			Contexts: map[string]*config.Context{
				"valid-large": {Name: "valid-large", Grafana: &config.GrafanaConfig{Server: "https://a.example", LiveWindow: "2h"}},
				"invalid":     {Name: "invalid", Grafana: &config.GrafanaConfig{Server: "https://b.example", LiveWindow: "not-a-duration"}},
				"valid-small": {Name: "valid-small", Grafana: &config.GrafanaConfig{Server: "https://c.example", LiveWindow: "30m"}},
			},
		}

		minWindow, ok, warnings := minLiveWindowAcrossContexts(cfg)

		require.True(t, ok)
		assert.Equal(t, 30*time.Minute, minWindow)
		require.Len(t, warnings, 1)
		assert.Contains(t, warnings[0], `"invalid"`)
	})

	t.Run("only an invalid window opted in yields not-found", func(t *testing.T) {
		cfg := &config.Config{
			Contexts: map[string]*config.Context{
				"invalid": {Name: "invalid", Grafana: &config.GrafanaConfig{Server: "https://a.example", LiveWindow: "not-a-duration"}},
			},
		}

		_, ok, warnings := minLiveWindowAcrossContexts(cfg)

		assert.False(t, ok)
		require.Len(t, warnings, 1)
	})

	t.Run("multiple valid opted-in contexts: the smallest wins", func(t *testing.T) {
		cfg := &config.Config{
			Contexts: map[string]*config.Context{
				"two-hours":     {Name: "two-hours", Grafana: &config.GrafanaConfig{Server: "https://a.example", LiveWindow: "2h"}},
				"thirty-min":    {Name: "thirty-min", Grafana: &config.GrafanaConfig{Server: "https://b.example", LiveWindow: "30m"}},
				"one-hour":      {Name: "one-hour", Grafana: &config.GrafanaConfig{Server: "https://c.example", LiveWindow: "1h"}},
				"unset-context": {Name: "unset-context", Grafana: &config.GrafanaConfig{Server: "https://d.example"}},
			},
		}

		minWindow, ok, warnings := minLiveWindowAcrossContexts(cfg)

		require.True(t, ok)
		assert.Equal(t, 30*time.Minute, minWindow)
		assert.Empty(t, warnings)
	})
}
