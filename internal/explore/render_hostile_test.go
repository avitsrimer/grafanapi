package explore_test

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"

	"github.com/grafana/grafanapi/internal/explore"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func renderHostile(t *testing.T) string {
	t.Helper()

	resp := decodeRenderFixture(t, "testdata/hostile_frame.json")

	var buf bytes.Buffer
	require.NoError(t, explore.RenderTable(&buf, resp, explore.RenderOptions{}))

	return buf.String()
}

// singleCellResponse builds a one-column string frame holding value.
func singleCellResponse(t *testing.T, value string) *explore.QueryResponse {
	t.Helper()

	raw, err := json.Marshal(value)
	require.NoError(t, err)

	return &explore.QueryResponse{
		Results: map[string]explore.FrameResult{
			"A": {Frames: []explore.Frame{{
				Schema: explore.FrameSchema{Fields: []explore.FieldSchema{{Name: "v", Type: "string"}}},
				Data:   explore.FrameData{Values: [][]json.RawMessage{{raw}}},
			}}},
		},
	}
}

func renderSingleCell(t *testing.T, value string, maxWidth int) string {
	t.Helper()

	var buf bytes.Buffer
	require.NoError(t, explore.RenderTable(&buf, singleCellResponse(t, value), explore.RenderOptions{MaxCellWidth: maxWidth}))

	lines := strings.Split(strings.TrimSuffix(buf.String(), "\n"), "\n")
	require.Len(t, lines, 3, "header, column header and exactly one row")

	return lines[2]
}

func TestRenderTable_HostileFrameHasNoTerminalControl(t *testing.T) {
	out := renderHostile(t)

	assert.NotContains(t, out, "\x9b")

	for _, r := range out {
		if r == '\n' {
			continue
		}

		assert.False(t, unicode.IsControl(r) || unicode.Is(unicode.Cf, r), "unsafe rune %U in output", r)
	}

	assert.True(t, utf8.ValidString(out))
}

func TestRenderTable_HostileFrameKeepsEveryRowOnOneLine(t *testing.T) {
	out := renderHostile(t)

	// "# A evil", the column header, eight rows, a blank separator and "B: no data".
	lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
	assert.Len(t, lines, 12, "output:\n%s", out)
}

func TestRenderTable_HostileFrameKeepsVisibleTextAndDropsPayloads(t *testing.T) {
	out := renderHostile(t)

	assert.Contains(t, out, "red")
	assert.NotContains(t, out, "[31m")
	assert.Contains(t, out, "after")
	assert.Contains(t, out, "tail")
	assert.Contains(t, out, "c1")
	assert.Contains(t, out, "pre")

	assert.NotContains(t, out, "]52;c;")
	assert.NotContains(t, out, "aGk=")
	assert.NotContains(t, out, "unterminated")
	assert.NotContains(t, out, "0;title")

	// A newline, a tab and a carriage return each become a space.
	assert.Contains(t, out, "line1 line2")
	assert.Contains(t, out, "tab here")
	assert.Contains(t, out, "cr here")
}

func TestRenderTable_HostileFrameSanitizesHeaders(t *testing.T) {
	lines := strings.Split(renderHostile(t), "\n")

	// refID "A\x1b[2J" and schema name "ev<RLO>il".
	assert.Equal(t, "# A ev il", lines[0])
	// Field name, and a label key whose CSI sequence is dropped whole.
	assert.Contains(t, lines[1], `txt {k="v"}`)
	// The refID of the frame-less result.
	assert.Equal(t, "B : no data", lines[len(lines)-2])
}

func TestRenderTable_TruncationMeasuresWidthAfterSanitizing(t *testing.T) {
	assert.Equal(t, "redg…", renderSingleCell(t, "\x1b[31mredgreen", 5))
	assert.Equal(t, "red", renderSingleCell(t, "\x1b[31mred", 5))
}

func TestRenderTable_TruncatesToRunesNotBytes(t *testing.T) {
	got := renderSingleCell(t, strings.Repeat("é", 70)+"\x1b", 50)

	assert.Equal(t, 50, utf8.RuneCountInString(got))
	assert.Equal(t, strings.Repeat("é", 49)+"…", got)
}
