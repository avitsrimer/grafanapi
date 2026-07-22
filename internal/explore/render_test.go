package explore_test

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/grafana/grafanapi/internal/explore"
	"github.com/grafana/grafanapi/internal/format"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRenderTable_PrometheusFrame(t *testing.T) {
	resp := decodeRenderFixture(t, "testdata/prometheus_range.json")

	var buf bytes.Buffer
	require.NoError(t, explore.RenderTable(&buf, resp, explore.RenderOptions{}))

	out := buf.String()

	assert.Contains(t, out, "# A value")
	assert.Contains(t, out, "Time")
	assert.Contains(t, out, `Value {instance="localhost:9090", job="prometheus"}`)
	// epoch-ms 1721606400000 -> RFC3339 UTC.
	assert.Contains(t, out, "2024-07-22T00:00:00Z")
	assert.Contains(t, out, "1.5")
}

func TestRenderTable_SQLTableFrame(t *testing.T) {
	resp := decodeRenderFixture(t, "testdata/sql_table.json")

	var buf bytes.Buffer
	require.NoError(t, explore.RenderTable(&buf, resp, explore.RenderOptions{}))

	out := buf.String()

	assert.Contains(t, out, "# A")
	assert.Contains(t, out, "id")
	assert.Contains(t, out, "name")
	assert.Contains(t, out, "note")
	assert.Contains(t, out, "alpha")
	assert.Contains(t, out, "beta")
	assert.Contains(t, out, "second row note")
	assert.Contains(t, out, "third row note")

	// null cells render as empty, not the literal "null".
	assert.NotContains(t, out, "null")
}

func TestRenderTable_TruncatesWideCells(t *testing.T) {
	long := strings.Repeat("x", 80)
	resp := &explore.QueryResponse{
		Results: map[string]explore.FrameResult{
			"A": {
				Frames: []explore.Frame{
					{
						Schema: explore.FrameSchema{
							RefID:  "A",
							Fields: []explore.FieldSchema{{Name: "note", Type: "string"}},
						},
						Data: explore.FrameData{
							Values: [][]json.RawMessage{{json.RawMessage(`"` + long + `"`)}},
						},
					},
				},
			},
		},
	}

	var buf bytes.Buffer
	require.NoError(t, explore.RenderTable(&buf, resp, explore.RenderOptions{MaxCellWidth: 10}))

	out := buf.String()
	assert.Contains(t, out, strings.Repeat("x", 9)+"…")
	assert.NotContains(t, out, long)
}

func TestRenderTable_MultiFrameSequential(t *testing.T) {
	prom := decodeRenderFixture(t, "testdata/prometheus_range.json")
	sql := decodeRenderFixture(t, "testdata/sql_table.json")

	frames := append(append([]explore.Frame{}, prom.Results["A"].Frames...), sql.Results["A"].Frames...)
	resp := &explore.QueryResponse{
		Results: map[string]explore.FrameResult{"A": {Frames: frames}},
	}

	var buf bytes.Buffer
	require.NoError(t, explore.RenderTable(&buf, resp, explore.RenderOptions{}))

	out := buf.String()
	promIdx := strings.Index(out, "value")
	sqlIdx := strings.Index(out, "id")
	require.NotEqual(t, -1, promIdx)
	require.NotEqual(t, -1, sqlIdx)
	assert.Less(t, promIdx, sqlIdx, "frames should render in response order")

	// a blank line separates the two frame blocks.
	assert.Contains(t, out, "\n\n")
}

func TestRenderTable_EmptyResponse(t *testing.T) {
	var buf bytes.Buffer
	require.NoError(t, explore.RenderTable(&buf, &explore.QueryResponse{}, explore.RenderOptions{}))
	assert.Equal(t, "No data.\n", buf.String())
}

func TestRenderTable_NilResponse(t *testing.T) {
	var buf bytes.Buffer
	require.NoError(t, explore.RenderTable(&buf, nil, explore.RenderOptions{}))
	assert.Equal(t, "No data.\n", buf.String())
}

func TestRenderTable_NoFramesForRefID(t *testing.T) {
	resp := &explore.QueryResponse{
		Results: map[string]explore.FrameResult{"A": {}},
	}

	var buf bytes.Buffer
	require.NoError(t, explore.RenderTable(&buf, resp, explore.RenderOptions{}))
	assert.Equal(t, "A: no data\n", buf.String())
}

func TestRenderTable_FrameWithZeroRows(t *testing.T) {
	resp := &explore.QueryResponse{
		Results: map[string]explore.FrameResult{
			"A": {
				Frames: []explore.Frame{
					{
						Schema: explore.FrameSchema{
							RefID:  "A",
							Fields: []explore.FieldSchema{{Name: "value", Type: "number"}},
						},
						Data: explore.FrameData{Values: [][]json.RawMessage{{}}},
					},
				},
			},
		},
	}

	var buf bytes.Buffer
	require.NoError(t, explore.RenderTable(&buf, resp, explore.RenderOptions{}))

	out := buf.String()
	assert.Contains(t, out, "# A")
	assert.Contains(t, out, "no data")
}

func TestRenderTable_JSONYAMLCodecsUntruncated(t *testing.T) {
	resp := decodeRenderFixture(t, "testdata/sql_table.json")

	codecs := []format.Codec{format.NewJSONCodec(), format.NewYAMLCodec()}
	for _, codec := range codecs {
		t.Run(string(codec.Format()), func(t *testing.T) {
			var buf bytes.Buffer
			require.NoError(t, codec.Encode(&buf, resp))

			var roundTripped explore.QueryResponse
			require.NoError(t, codec.Decode(&buf, &roundTripped))

			// Compare via re-marshaled JSON rather than deep Go-struct
			// equality: a nil json.RawMessage and a literal "null"
			// RawMessage both marshal to the same JSON null and are
			// semantically identical, but goccy/go-yaml's JSON-compatible
			// decode produces a nil slice for YAML's null, which is not a
			// bug in our decode/render code, just a representational
			// difference for an empty vs. explicit-null cell.
			wantJSON, err := json.Marshal(resp)
			require.NoError(t, err)

			gotJSON, err := json.Marshal(&roundTripped)
			require.NoError(t, err)

			assert.JSONEq(t, string(wantJSON), string(gotJSON))
		})
	}
}

// decodeRenderFixture reads and decodes a testdata fixture, failing the test
// on any error.
func decodeRenderFixture(t *testing.T, path string) *explore.QueryResponse {
	t.Helper()

	f, err := os.Open(path)
	require.NoError(t, err)
	defer f.Close()

	resp, err := explore.Decode(f)
	require.NoError(t, err)

	return resp
}
