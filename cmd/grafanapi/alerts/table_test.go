// Package alerts (whitebox, not alerts_test) is intentional here: these tests drive the
// unexported table codecs directly to cover their Format/Encode/Decode branches, the same way
// cmd/grafanapi/explore/table_test.go does.
//
//nolint:testpackage // whitebox test: needs access to the unexported table codec types
package alerts

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/grafana/grafana-openapi-client-go/models"
	"github.com/grafana/grafanapi/internal/alerts"
	"github.com/grafana/grafanapi/internal/format"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// rulesTableHeader is the exact header line the rules codec must emit once tabwriter has padded
// it, split back into fields so the assertions do not depend on the column widths.
func rulesTableHeader() []string {
	return []string{"FOLDER", "GROUP", "TITLE", "UID", "TYPE", "STATE", "HEALTH", "FOR", "PAUSED", "ALERTS"}
}

// encodeRules renders rules through the codec and returns the output's lines, with each line's
// tabwriter padding collapsed into single spaces so rows can be compared field by field.
func encodeRules(t *testing.T, rules any) []string {
	t.Helper()

	var buf bytes.Buffer
	require.NoError(t, (&rulesTableCodec{}).Encode(&buf, rules))

	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	for i, line := range lines {
		lines[i] = strings.Join(strings.Fields(line), " ")
	}

	return lines
}

func TestRulesTableCodec_Format(t *testing.T) {
	assert.Equal(t, format.Format(tableFormat), (&rulesTableCodec{}).Format())
}

func TestRulesTableCodec_Decode_ReturnsUnsupportedError(t *testing.T) {
	err := (&rulesTableCodec{}).Decode(bytes.NewReader(nil), nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "does not support decoding")
}

func TestRulesTableCodec_Encode_TypeAssertionFailure(t *testing.T) {
	var buf bytes.Buffer

	err := (&rulesTableCodec{}).Encode(&buf, "not-a-rule-list")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported input type")
	assert.Empty(t, buf.String())
}

// TestRulesTableCodec_Encode_EmptyListRendersHeaderOnly pins that an empty result still prints
// the header, so a filter that matches nothing does not look like a broken command.
func TestRulesTableCodec_Encode_EmptyListRendersHeaderOnly(t *testing.T) {
	lines := encodeRules(t, []alerts.Rule{})
	require.Len(t, lines, 1)
	assert.Equal(t, strings.Join(rulesTableHeader(), " "), lines[0])
}

// TestRulesTableCodec_Encode_RendersRows covers the three STATE renderings the join can produce
// (a real state, "recording", "unknown") and the exact FOR text formatDuration produces.
func TestRulesTableCodec_Encode_RendersRows(t *testing.T) {
	evaluated := time.Date(2026, time.September, 9, 10, 11, 12, 0, time.UTC)

	rules := []alerts.Rule{
		{
			UID: "rule-firing", Title: "probe-rule", Type: "alerting",
			FolderUID: "probe-folder", Folder: "Probe Folder", RuleGroup: "probe-group",
			For: "1m", IsPaused: false, State: "firing", Health: "ok",
			LastEvaluation: &evaluated, ActiveAlerts: 2,
		},
		{
			UID: "rule-recording", Title: "probe-record", Type: "recording",
			FolderUID: "probe-folder", Folder: "Probe Folder", RuleGroup: "probe-group",
			For: "0s", State: "recording", Health: "ok",
		},
		{
			UID: "rule-fresh", Title: "fresh", Type: "alerting",
			FolderUID: "other-folder", RuleGroup: "other-group",
			For: "5m", IsPaused: true, State: "unknown",
		},
	}

	lines := encodeRules(t, rules)
	require.Len(t, lines, 4)

	// Expected rows are spelled as field slices rather than one string so that a legitimately
	// repeated cell value (a recording rule's TYPE and STATE) is not read as a typo by dupword.
	assert.Equal(t, strings.Join(rulesTableHeader(), " "), lines[0])
	assert.Equal(t, strings.Join([]string{
		"Probe Folder", "probe-group", "probe-rule", "rule-firing", "alerting", "firing", "ok", "1m", "false", "2",
	}, " "), lines[1])
	assert.Equal(t, strings.Join([]string{
		"Probe Folder", "probe-group", "probe-record", "rule-recording", "recording", "recording", "ok", "0s", "false", "0",
	}, " "), lines[2])
	// No folder title and no health: the state endpoint never reported this rule.
	assert.Equal(t, strings.Join([]string{
		"other-group", "fresh", "rule-fresh", "alerting", "unknown", "5m", "true", "0",
	}, " "), lines[3])
}

// TestRulesTableCodec_Encode_ExactLayout asserts the rendered output byte for byte, unlike the
// helper-based tests above which collapse tabwriter padding and therefore cannot see a cell
// landing in the wrong column. In particular it pins the row whose FOLDER and HEALTH cells are
// empty (an unjoined rule, or every row in degraded mode): those must be space-padded, not
// tab-indented, or the row shifts to the terminal's next tab stop.
func TestRulesTableCodec_Encode_ExactLayout(t *testing.T) {
	evaluated := time.Date(2026, time.September, 9, 10, 11, 12, 0, time.UTC)

	rules := []alerts.Rule{
		{
			UID: "rule-firing", Title: "probe-rule", Type: "alerting",
			FolderUID: "probe-folder", Folder: "Parent/Probe Folder", RuleGroup: "probe-group",
			For: "1m", State: "firing", Health: "ok", LastEvaluation: &evaluated, ActiveAlerts: 2,
		},
		{
			UID: "rule-fresh", Title: "fresh", Type: "alerting",
			FolderUID: "other-folder", RuleGroup: "other-group",
			For: "5m", IsPaused: true, State: "unknown",
		},
	}

	var buf bytes.Buffer
	require.NoError(t, (&rulesTableCodec{}).Encode(&buf, rules))

	expected := strings.Join([]string{
		"FOLDER               GROUP        TITLE       UID          TYPE      STATE    HEALTH  FOR  PAUSED  ALERTS",
		"Parent/Probe Folder  probe-group  probe-rule  rule-firing  alerting  firing   ok      1m   false   2",
		"                     other-group  fresh       rule-fresh   alerting  unknown          5m   true    0",
		"",
	}, "\n")

	assert.Equal(t, expected, buf.String())
	assert.NotContains(t, buf.String(), "\t", "cells must be space-padded, never tab-indented")
}

// TestRulesTableCodec_Encode_PreservesInputOrder pins that the codec renders what it is given
// verbatim: sorting is internal/alerts.ListRules' job, not the renderer's.
func TestRulesTableCodec_Encode_PreservesInputOrder(t *testing.T) {
	rules := []alerts.Rule{
		{UID: "z", Title: "z", Type: "alerting", State: "inactive"},
		{UID: "a", Title: "a", Type: "alerting", State: "inactive"},
	}

	lines := encodeRules(t, rules)
	require.Len(t, lines, 3)
	assert.Contains(t, lines[1], "z")
	assert.Contains(t, lines[2], "a")
}

// contactPointsTableHeader is the exact header line the contact-points codec must emit, as
// fields, so the assertions do not depend on tabwriter's column widths.
func contactPointsTableHeader() []string {
	return []string{"NAME", "TYPE", "UID", "PROVENANCE"}
}

// encodeContactPoints renders points through the codec and returns the output's lines with each
// line's tabwriter padding collapsed, so rows can be compared field by field.
func encodeContactPoints(t *testing.T, points any) []string {
	t.Helper()

	var buf bytes.Buffer
	require.NoError(t, (&contactPointsTableCodec{}).Encode(&buf, points))

	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	for i, line := range lines {
		lines[i] = strings.Join(strings.Fields(line), " ")
	}

	return lines
}

func TestContactPointsTableCodec_Format(t *testing.T) {
	assert.Equal(t, format.Format(tableFormat), (&contactPointsTableCodec{}).Format())
}

func TestContactPointsTableCodec_Decode_ReturnsUnsupportedError(t *testing.T) {
	err := (&contactPointsTableCodec{}).Decode(bytes.NewReader(nil), nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "does not support decoding")
}

func TestContactPointsTableCodec_Encode_TypeAssertionFailure(t *testing.T) {
	var buf bytes.Buffer

	// A []*models.EmbeddedContactPoint is not a models.ContactPoints as far as a type assertion
	// on `any` is concerned, so this also guards the codec against the wrong-but-plausible input.
	err := (&contactPointsTableCodec{}).Encode(&buf, []alerts.Rule{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported input type")
	assert.Empty(t, buf.String())
}

func TestContactPointsTableCodec_Encode_EmptyListRendersHeaderOnly(t *testing.T) {
	lines := encodeContactPoints(t, models.ContactPoints{})
	require.Len(t, lines, 1)
	assert.Equal(t, strings.Join(contactPointsTableHeader(), " "), lines[0])
}

// TestContactPointsTableCodec_Encode_RendersRows covers a fully populated contact point, one
// whose *string Type is nil (the deref that would panic), and an empty provenance.
func TestContactPointsTableCodec_Encode_RendersRows(t *testing.T) {
	slack := "slack"

	points := models.ContactPoints{
		{Name: "email receiver", Type: nil, UID: "", Provenance: ""},
		{Name: "probe-slack", Type: &slack, UID: "cp-uid-1", Provenance: "api"},
	}

	lines := encodeContactPoints(t, points)
	require.Len(t, lines, 3)

	// Expected rows are spelled as field slices rather than one string so that repeated cell
	// values are not read as a typo by dupword.
	assert.Equal(t, strings.Join(contactPointsTableHeader(), " "), lines[0])
	assert.Equal(t, "email receiver", lines[1], "a nil Type and an empty UID/PROVENANCE collapse away")
	assert.Equal(t, strings.Join([]string{
		"probe-slack", "slack", "cp-uid-1", "api",
	}, " "), lines[2])
}

// TestContactPointsTableCodec_Encode_ExactLayout asserts the rendered output byte for byte: the
// collapsed-field assertions above cannot tell "one value in the NAME column" from "the same
// value in some other column", and the nil-Type row is exactly where that would go unnoticed.
func TestContactPointsTableCodec_Encode_ExactLayout(t *testing.T) {
	slack := "slack"

	points := models.ContactPoints{
		{Name: "email receiver", Type: nil, UID: "", Provenance: ""},
		{Name: "probe-slack", Type: &slack, UID: "cp-uid-1", Provenance: "api"},
	}

	var buf bytes.Buffer
	require.NoError(t, (&contactPointsTableCodec{}).Encode(&buf, points))

	expected := strings.Join([]string{
		"NAME            TYPE   UID       PROVENANCE",
		// The nil Type, empty UID and empty PROVENANCE are padded in place, so NAME stays NAME.
		"email receiver" + strings.Repeat(" ", 19),
		"probe-slack     slack  cp-uid-1  api",
		"",
	}, "\n")

	assert.Equal(t, expected, buf.String())
	assert.NotContains(t, buf.String(), "\t", "cells must be space-padded, never tab-indented")
}

// TestContactPointsTableCodec_Encode_SkipsNilEntries guards the renderer against a nil element in
// the payload slice, which would otherwise panic on Name. internal/alerts.ListContactPoints
// already drops nil elements, so this is the codec's own defence for any other caller.
func TestContactPointsTableCodec_Encode_SkipsNilEntries(t *testing.T) {
	points := models.ContactPoints{nil, {Name: "kept"}}

	lines := encodeContactPoints(t, points)
	require.Len(t, lines, 2)
	assert.Equal(t, "kept", lines[1])
}

// TestRulesTableCodec_Encode_SanitizesControlCharacters is the row-integrity regression test.
// A rule title and a folder title are arbitrary user input: an embedded tab is read by the
// tabwriter as a column terminator (shifting every later cell of the row one column right) and an
// embedded newline as a row terminator (splitting one rule across two lines). Both must be
// replaced, so the row keeps exactly one line and exactly the right number of columns.
func TestRulesTableCodec_Encode_SanitizesControlCharacters(t *testing.T) {
	rules := []alerts.Rule{{
		Folder:       "Probe\tFolder",
		RuleGroup:    "probe\ngroup",
		Title:        "evil\r\ttitle\n",
		UID:          "u1",
		Type:         "alerting",
		State:        "firing",
		Health:       "ok",
		For:          "1m",
		ActiveAlerts: 1,
	}}

	var buf bytes.Buffer
	require.NoError(t, (&rulesTableCodec{}).Encode(&buf, rules))

	out := buf.String()
	assert.NotContains(t, out, "\t", "cells must be space-padded, never tab-separated")
	assert.NotContains(t, out, "\r")

	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	require.Len(t, lines, 2, "the header plus exactly one row: a newline in a title must not split it")

	fields := strings.Fields(lines[1])
	assert.Equal(t, []string{
		"Probe", "Folder", "probe", "group", "evil", "title", "u1", "alerting", "firing", "ok", "1m", "false", "1",
	}, fields, "the control characters became spaces inside their own cells")

	// Column alignment is the real invariant: with the header padded to the same widths, the
	// row's last two cells still line up under PAUSED and ALERTS.
	assert.Equal(t, strings.Index(lines[0], "ALERTS"), strings.LastIndex(lines[1], "1"))
}

// TestRulesTableCodec_Encode_SanitizesFormatCharacters covers the other half of the sanitizer:
// Unicode's format category (Cf) is not "control characters", but it holds the bidirectional
// overrides, and a terminal honouring one of those re-orders the rest of the line on screen - so a
// row whose bytes look perfectly aligned is unreadable. They must be replaced like any control
// character.
func TestRulesTableCodec_Encode_SanitizesFormatCharacters(t *testing.T) {
	// Spelled as escapes, not literals: the runes under test are exactly the ones bidichk and
	// staticcheck's ST1018 refuse to see in source, and a reader cannot tell them apart anyway.
	// U+202E RIGHT-TO-LEFT OVERRIDE, U+200B ZERO WIDTH SPACE, U+00AD SOFT HYPHEN.
	rules := []alerts.Rule{{
		Folder:    "Probe\u202eFolder",
		RuleGroup: "probe\u200bgroup",
		Title:     "ti\u00adtle",
		UID:       "u1",
		Type:      "alerting",
		State:     "firing",
		Health:    "ok",
		For:       "1m",
	}}

	var buf bytes.Buffer
	require.NoError(t, (&rulesTableCodec{}).Encode(&buf, rules))

	out := buf.String()
	assert.NotContains(t, out, "\u202e", "a bidi override must never reach the terminal")
	assert.NotContains(t, out, "\u200b")
	assert.NotContains(t, out, "\u00ad")

	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	require.Len(t, lines, 2)
	assert.Equal(t,
		[]string{"Probe", "Folder", "probe", "group", "ti", "tle", "u1", "alerting", "firing", "ok", "1m", "false", "0"},
		strings.Fields(lines[1]),
	)
}

// TestContactPointsTableCodec_Encode_SanitizesControlCharacters is the same row-integrity
// guarantee for the contact-points codec, whose NAME is equally user-supplied.
func TestContactPointsTableCodec_Encode_SanitizesControlCharacters(t *testing.T) {
	webhook := "web\thook"

	points := models.ContactPoints{{Name: "team\nalpha", Type: &webhook, UID: "u\t1", Provenance: "a\rpi"}}

	var buf bytes.Buffer
	require.NoError(t, (&contactPointsTableCodec{}).Encode(&buf, points))

	out := buf.String()
	assert.NotContains(t, out, "\t")
	assert.NotContains(t, out, "\r")

	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	require.Len(t, lines, 2)
	assert.Equal(t,
		[]string{"team", "alpha", "web", "hook", "u", "1", "a", "pi"},
		strings.Fields(lines[1]),
	)
}
