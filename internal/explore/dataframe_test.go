package explore_test

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/grafana/grafanapi/internal/explore"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDecode(t *testing.T) {
	tests := []struct {
		name          string
		fixture       string
		wantRefIDs    []string
		wantFirstErr  string
		wantFirstMsg  string
		wantRowCounts map[string]int
	}{
		{
			name:          "prometheus range vector",
			fixture:       "testdata/prometheus_range.json",
			wantRefIDs:    []string{"A"},
			wantRowCounts: map[string]int{"A": 3},
		},
		{
			name:          "sql table with mixed columns and nulls",
			fixture:       "testdata/sql_table.json",
			wantRefIDs:    []string{"A"},
			wantRowCounts: map[string]int{"A": 3},
		},
		{
			name:         "multistatus partial error",
			fixture:      "testdata/multistatus_partial.json",
			wantRefIDs:   []string{"A"},
			wantFirstErr: "A",
			wantFirstMsg: "parse error at char 5: unexpected identifier",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f, err := os.Open(tt.fixture)
			require.NoError(t, err)
			defer f.Close()

			resp, err := explore.Decode(f)
			require.NoError(t, err)
			require.NotNil(t, resp)

			for _, refID := range tt.wantRefIDs {
				_, ok := resp.Results[refID]
				assert.Truef(t, ok, "expected results to contain refId %q", refID)
			}

			gotRefID, gotMsg := resp.FirstError()
			assert.Equal(t, tt.wantFirstErr, gotRefID)
			assert.Equal(t, tt.wantFirstMsg, gotMsg)

			for refID, wantRows := range tt.wantRowCounts {
				result := resp.Results[refID]
				require.Len(t, result.Frames, 1)
				assert.Equal(t, wantRows, result.Frames[0].RowCount())
			}
		})
	}
}

func TestDecode_MalformedJSONErrors(t *testing.T) {
	_, err := explore.Decode(strings.NewReader(`{"results": not-json`))
	require.Error(t, err)
}

func TestQueryResponse_FirstError_MultiRefIDSortedOrder(t *testing.T) {
	resp := &explore.QueryResponse{
		Results: map[string]explore.FrameResult{
			"C": {Error: "should not be picked"},
			"A": {},
			"B": {Error: "B failed first in sorted order"},
		},
	}

	refID, msg := resp.FirstError()
	assert.Equal(t, "B", refID)
	assert.Equal(t, "B failed first in sorted order", msg)
}

func TestDecode_MissingResultsIsEmptyNotError(t *testing.T) {
	resp, err := explore.Decode(strings.NewReader("{}"))
	require.NoError(t, err)
	require.NotNil(t, resp)
	assert.Empty(t, resp.Results)

	refID, msg := resp.FirstError()
	assert.Empty(t, refID)
	assert.Empty(t, msg)
}

func TestFrame_Schema(t *testing.T) {
	f, err := os.Open("testdata/prometheus_range.json")
	require.NoError(t, err)
	defer f.Close()

	resp, err := explore.Decode(f)
	require.NoError(t, err)

	result := resp.Results["A"]
	require.Len(t, result.Frames, 1)

	frame := result.Frames[0]
	assert.Equal(t, "value", frame.Schema.Name)
	assert.Equal(t, "A", frame.Schema.RefID)
	require.Len(t, frame.Schema.Fields, 2)

	assert.Equal(t, "Time", frame.Schema.Fields[0].Name)
	assert.Equal(t, "time", frame.Schema.Fields[0].Type)

	assert.Equal(t, "Value", frame.Schema.Fields[1].Name)
	assert.Equal(t, "number", frame.Schema.Fields[1].Type)
	assert.Equal(t, map[string]string{
		"instance": "localhost:9090",
		"job":      "prometheus",
	}, frame.Schema.Fields[1].Labels)
}

func TestFrame_Cell(t *testing.T) {
	f, err := os.Open("testdata/sql_table.json")
	require.NoError(t, err)
	defer f.Close()

	resp, err := explore.Decode(f)
	require.NoError(t, err)

	frame := resp.Results["A"].Frames[0]

	// name column, second row: "beta".
	var name string
	require.NoError(t, json.Unmarshal(frame.Cell(1, 1), &name))
	assert.Equal(t, "beta", name)

	// note column, first row: explicit JSON null.
	assert.JSONEq(t, "null", string(frame.Cell(2, 0)))

	// out-of-range column and row are defensively treated as null rather
	// than panicking.
	assert.JSONEq(t, "null", string(frame.Cell(99, 0)))
	assert.JSONEq(t, "null", string(frame.Cell(0, 99)))
}

func TestQueryResponse_FirstError_NoError(t *testing.T) {
	f, err := os.Open("testdata/prometheus_range.json")
	require.NoError(t, err)
	defer f.Close()

	resp, err := explore.Decode(f)
	require.NoError(t, err)

	refID, msg := resp.FirstError()
	assert.Empty(t, refID)
	assert.Empty(t, msg)
}
