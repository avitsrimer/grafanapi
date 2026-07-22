// Package explore implements ad-hoc datasource queries against Grafana's
// /api/ds/query endpoint, mirroring the Explore UI: resolving a datasource,
// building a query for its type, executing the request, and rendering the
// response.
package explore

import (
	"encoding/json"
	"io"
	"sort"
)

// Decode decodes a Grafana /api/ds/query response body into a QueryResponse.
// A missing "results" key is a valid, empty response, not an error.
func Decode(r io.Reader) (*QueryResponse, error) {
	var resp QueryResponse
	if err := json.NewDecoder(r).Decode(&resp); err != nil {
		return nil, err
	}

	return &resp, nil
}

// QueryResponse is the decoded body of a /api/ds/query response, keyed by
// the refId of each query in the request.
type QueryResponse struct {
	Results map[string]FrameResult `json:"results"`
}

// FrameResult is the per-refId portion of a QueryResponse: either a list of
// data frames, or a non-empty Error describing why the query failed.
type FrameResult struct {
	Status int     `json:"status,omitempty"`
	Frames []Frame `json:"frames,omitempty"`
	Error  string  `json:"error,omitempty"`
}

// Frame is a single Grafana data frame: a schema describing its columns and
// the column-major data backing them.
type Frame struct {
	Schema FrameSchema `json:"schema"`
	Data   FrameData   `json:"data"`
}

// FrameSchema describes a Frame's identity and its columns.
type FrameSchema struct {
	Name   string        `json:"name,omitempty"`
	RefID  string        `json:"refId,omitempty"`
	Fields []FieldSchema `json:"fields"`
}

// FieldSchema describes a single column of a Frame.
type FieldSchema struct {
	Name   string            `json:"name"`
	Type   string            `json:"type"` // "time", "number", "string", "boolean", ...
	Labels map[string]string `json:"labels,omitempty"`
}

// FrameData holds a Frame's values, column-major: Values[col][row]. Cells
// are kept as json.RawMessage so a renderer can format per-column by schema
// type (e.g. epoch-ms time -> RFC3339) and JSON/YAML output can re-emit them
// untouched.
type FrameData struct {
	Values [][]json.RawMessage `json:"values"`
}

// FirstError returns the refID and message of the first non-empty per-refId
// error in the response, in sorted refID order. It returns two empty strings
// when there is no error.
func (r *QueryResponse) FirstError() (string, string) {
	refIDs := make([]string, 0, len(r.Results))
	for id := range r.Results {
		refIDs = append(refIDs, id)
	}

	sort.Strings(refIDs)

	for _, id := range refIDs {
		if err := r.Results[id].Error; err != "" {
			return id, err
		}
	}

	return "", ""
}

// RowCount returns the number of rows in the frame: the length of its
// longest column, or 0 when the frame has no columns.
func (f Frame) RowCount() int {
	rows := 0
	for _, col := range f.Data.Values {
		if len(col) > rows {
			rows = len(col)
		}
	}

	return rows
}

// Cell returns the raw JSON value at the given column and row. It returns a
// JSON null when the column or row is out of range, defensively handling
// ragged columns rather than panicking.
func (f Frame) Cell(col, row int) json.RawMessage {
	if col < 0 || col >= len(f.Data.Values) {
		return json.RawMessage("null")
	}

	column := f.Data.Values[col]
	if row < 0 || row >= len(column) {
		return json.RawMessage("null")
	}

	return column[row]
}
