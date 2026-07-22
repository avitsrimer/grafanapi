package explore

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"
	"time"
)

// defaultMaxCellWidth is the RenderOptions.MaxCellWidth used when the caller
// leaves it unset (zero or negative).
const defaultMaxCellWidth = 60

// RenderTable writes a human-friendly table rendering of resp to w. Results
// are iterated in sorted refID order and, within each, frames in response
// order, so output is deterministic. Each frame is preceded by a "# <refID>
// [name]" header; frames render sequentially with a blank line between them.
// An empty response, a refID with no frames, or a frame with no rows each
// render a graceful "no data" line instead of nothing or a panic.
func RenderTable(w io.Writer, resp *QueryResponse, opts RenderOptions) error {
	maxWidth := opts.MaxCellWidth
	if maxWidth <= 0 {
		maxWidth = defaultMaxCellWidth
	}

	if resp == nil || len(resp.Results) == 0 {
		_, err := fmt.Fprintln(w, "No data.")
		return err
	}

	refIDs := make([]string, 0, len(resp.Results))
	for id := range resp.Results {
		refIDs = append(refIDs, id)
	}

	sort.Strings(refIDs)

	tab := tabwriter.NewWriter(w, 0, 4, 2, ' ', tabwriter.TabIndent)

	first := true
	for _, refID := range refIDs {
		result := resp.Results[refID]

		if len(result.Frames) == 0 {
			if !first {
				fmt.Fprintln(tab)
			}
			first = false

			fmt.Fprintf(tab, "%s: no data\n", refID)

			continue
		}

		for _, frame := range result.Frames {
			if !first {
				fmt.Fprintln(tab)
			}
			first = false

			if err := renderFrame(tab, refID, frame, maxWidth); err != nil {
				return err
			}
		}
	}

	return tab.Flush()
}

// RenderOptions configures RenderTable.
type RenderOptions struct {
	// MaxCellWidth truncates any rendered cell longer than this many
	// characters, appending an ellipsis. Zero (or negative) uses the
	// default of 60.
	MaxCellWidth int
}

// renderFrame writes a single frame's header, column headers, and rows to w.
func renderFrame(w io.Writer, refID string, frame Frame, maxWidth int) error {
	header := "# " + refID
	if frame.Schema.Name != "" {
		header += " " + frame.Schema.Name
	}

	if _, err := fmt.Fprintln(w, header); err != nil {
		return err
	}

	rows := frame.RowCount()
	if len(frame.Schema.Fields) == 0 || rows == 0 {
		_, err := fmt.Fprintln(w, "no data")
		return err
	}

	headerCells := make([]string, len(frame.Schema.Fields))
	for i, field := range frame.Schema.Fields {
		headerCells[i] = columnHeader(field)
	}

	if _, err := fmt.Fprintln(w, strings.Join(headerCells, "\t")); err != nil {
		return err
	}

	for row := range rows {
		cells := make([]string, len(frame.Schema.Fields))
		for col, field := range frame.Schema.Fields {
			cells[col] = truncate(formatCell(frame.Cell(col, row), field.Type), maxWidth)
		}

		if _, err := fmt.Fprintln(w, strings.Join(cells, "\t")); err != nil {
			return err
		}
	}

	return nil
}

// columnHeader renders a field's header cell: its name, plus any labels
// appended compactly and sorted by key, e.g. `value {instance="…", job="…"}`.
func columnHeader(field FieldSchema) string {
	if len(field.Labels) == 0 {
		return field.Name
	}

	keys := make([]string, 0, len(field.Labels))
	for k := range field.Labels {
		keys = append(keys, k)
	}

	sort.Strings(keys)

	pairs := make([]string, len(keys))
	for i, k := range keys {
		pairs[i] = fmt.Sprintf("%s=%q", k, field.Labels[k])
	}

	return fmt.Sprintf("%s {%s}", field.Name, strings.Join(pairs, ", "))
}

// formatCell renders a single cell's raw JSON value as plain text according
// to its schema type: "time" as an epoch-ms number rendered RFC3339 UTC,
// null as empty, "string" unquoted, and everything else as its plain JSON
// scalar.
func formatCell(raw json.RawMessage, fieldType string) string {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return ""
	}

	switch fieldType {
	case "time":
		return formatTimeCell(raw, trimmed)
	case "string":
		var s string
		if err := json.Unmarshal(raw, &s); err == nil {
			return s
		}

		return trimmed
	default:
		return trimmed
	}
}

// formatTimeCell renders an epoch-ms JSON number as RFC3339 UTC, falling
// back to the raw trimmed text when the value isn't a number (defensive).
func formatTimeCell(raw json.RawMessage, trimmed string) string {
	var ms int64
	if err := json.Unmarshal(raw, &ms); err != nil {
		return trimmed
	}

	return time.UnixMilli(ms).UTC().Format(time.RFC3339)
}

// truncate shortens s to at most maxWidth runes, appending an ellipsis when
// truncated. maxWidth <= 0 disables truncation.
func truncate(s string, maxWidth int) string {
	if maxWidth <= 0 {
		return s
	}

	runes := []rune(s)
	if len(runes) <= maxWidth {
		return s
	}

	if maxWidth <= 1 {
		return string(runes[:maxWidth])
	}

	return string(runes[:maxWidth-1]) + "…"
}
