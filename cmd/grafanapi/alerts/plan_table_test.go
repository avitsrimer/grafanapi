// Package alerts (whitebox, not alerts_test) is intentional here, for the same reason
// table_test.go is: the plan renderer is unexported - it is not a codec anyone selects with -o -
// and driving it directly is what lets these tests pin the row shape without going through a
// command, a config and an httptest server.
//
//nolint:testpackage // whitebox test: needs access to the unexported plan renderer
package alerts

import (
	"bytes"
	"strings"
	"testing"

	"github.com/grafana/grafanapi/internal/alerts"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// planTableHeader is the exact header the renderer must emit, split into fields so the assertions
// do not depend on the column widths tabwriter chose.
func planTableHeader() []string {
	return []string{"ACTION", "KIND", "ID", "DETAIL"}
}

// renderPlanLines renders a plan and returns its lines with each line's tabwriter padding (and the
// detail indent) collapsed into single spaces, so rows can be compared field by field.
func renderPlanLines(t *testing.T, changes []alerts.Change) []string {
	t.Helper()

	var buf bytes.Buffer
	require.NoError(t, renderPlan(&buf, changes))

	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	for i, line := range lines {
		lines[i] = strings.Join(strings.Fields(line), " ")
	}

	return lines
}

// TestRenderPlan_RowsAndSummary is the shape of a whole plan: one row per change in the order it
// was given, the first detail line on the change's own row, the rest on indented follow-on rows,
// and the summary counting each action.
func TestRenderPlan_RowsAndSummary(t *testing.T) {
	lines := renderPlanLines(t, []alerts.Change{
		{Action: alerts.ActionUnchanged, Kind: alerts.KindContactPoint, ID: "cp-uid-1"},
		{Action: alerts.ActionCreate, Kind: alerts.KindContactPoint, ID: "cp-uid-2"},
		{Action: alerts.ActionUpdate, Kind: alerts.KindRuleGroup, ID: "folder-uid-1/group-a", Detail: []string{
			"~ interval 60s→120s",
			"+ rule two (rule-uid-2)",
			"- rule three (rule-uid-3)",
		}},
	})

	require.Len(t, lines, 7, "header, three changes, two follow-on detail rows and the summary")

	assert.Equal(t, planTableHeader(), strings.Fields(lines[0]))
	assert.Equal(t, "Unchanged ContactPoint cp-uid-1", lines[1])
	assert.Equal(t, "Create ContactPoint cp-uid-2", lines[2])
	assert.Equal(t, "Update RuleGroup folder-uid-1/group-a ~ interval 60s→120s", lines[3])
	assert.Equal(t, "+ rule two (rule-uid-2)", lines[4], "a follow-on row carries only the detail")
	assert.Equal(t, "- rule three (rule-uid-3)", lines[5])
	assert.Equal(t, "1 to create, 1 to update, 1 unchanged", lines[6])
}

// TestRenderPlan_DetailIsIndented pins the indent itself, which the field-collapsing helper above
// deliberately throws away: a follow-on row has to read as belonging to the row above it rather
// than as a plan entry with an empty ACTION.
func TestRenderPlan_DetailIsIndented(t *testing.T) {
	var buf bytes.Buffer
	require.NoError(t, renderPlan(&buf, []alerts.Change{
		{Action: alerts.ActionUpdate, Kind: alerts.KindRuleGroup, ID: "folder-uid-1/group-a", Detail: []string{
			"~ interval 60s→120s",
			"+ rule two (rule-uid-2)",
		}},
	}))

	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	require.Len(t, lines, 4)

	assert.True(t, strings.HasPrefix(lines[1], "Update"), "the change itself starts at column zero")
	assert.True(t, strings.HasPrefix(lines[2], " "), "a follow-on detail row is indented under it")
	assert.Contains(t, lines[2], detailIndent+"+ rule two (rule-uid-2)")
}

// TestRenderPlan_SanitizesControlCharacters is the reason every cell goes through cell(): a rule
// title is arbitrary user input, and one tab or newline inside it would otherwise shift the rest
// of the row into the wrong columns or split one change across two lines.
func TestRenderPlan_SanitizesControlCharacters(t *testing.T) {
	lines := renderPlanLines(t, []alerts.Change{
		{
			Action: alerts.ActionUpdate,
			Kind:   alerts.KindRuleGroup,
			ID:     "folder-uid-1/gro\tup",
			Detail: []string{"+ broken\nrule (rule-uid-2)"},
		},
	})

	require.Len(t, lines, 3, "header, the single change and the summary - the row is never split")
	assert.Equal(t, "Update RuleGroup folder-uid-1/gro up + broken rule (rule-uid-2)", lines[1])
	assert.Equal(t, "0 to create, 1 to update, 0 unchanged", lines[2])
}

// TestRenderPlan_Empty covers the bundle that matches Grafana exactly and the one that describes
// nothing at all: the header and a summary of zeroes, rather than silence a user would read as a
// failure to run.
func TestRenderPlan_Empty(t *testing.T) {
	lines := renderPlanLines(t, nil)

	require.Len(t, lines, 2)
	assert.Equal(t, planTableHeader(), strings.Fields(lines[0]))
	assert.Equal(t, "0 to create, 0 to update, 0 unchanged", lines[1])
}
