// Package explore (whitebox, not explore_test) is intentional here: these tests drive the
// unexported formatCell/formatTimeCell/truncate helpers directly to cover their defensive
// fallback branches, which are not reachable through RenderTable with realistic fixtures.
package explore

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestFormatCell_StringTypeFallsBackToTrimmedTextOnUnmarshalFailure(t *testing.T) {
	// A "string"-typed cell whose raw JSON is not actually a JSON string (defensive: schema type
	// and wire value disagree) falls back to the trimmed raw text instead of panicking or erroring.
	got := formatCell(json.RawMessage("42"), "string")
	assert.Equal(t, "42", got)
}

func TestFormatTimeCell_NonNumericFallsBackToTrimmedText(t *testing.T) {
	// A "time"-typed cell whose raw JSON is not a number (defensive: malformed/unexpected wire
	// value) falls back to the trimmed raw text rather than erroring.
	got := formatTimeCell(json.RawMessage(`"not-a-number"`), `"not-a-number"`)
	assert.Equal(t, `"not-a-number"`, got)
}

func TestTruncate_MaxWidthBoundary(t *testing.T) {
	tests := []struct {
		name     string
		s        string
		maxWidth int
		want     string
	}{
		{name: "maxWidth == 1 returns a single rune, no ellipsis", s: "hello", maxWidth: 1, want: "h"},
		{name: "maxWidth == 2 returns one rune plus ellipsis", s: "hello", maxWidth: 2, want: "h…"},
		{name: "no truncation when s fits exactly", s: "hi", maxWidth: 2, want: "hi"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, truncate(tt.s, tt.maxWidth))
		})
	}
}
