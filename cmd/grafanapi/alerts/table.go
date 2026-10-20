package alerts

import (
	"errors"
	"fmt"
	"io"
	"text/tabwriter"

	"github.com/grafana/grafana-openapi-client-go/models"
	"github.com/grafana/grafanapi/internal/alerts"
	"github.com/grafana/grafanapi/internal/format"
	"github.com/grafana/grafanapi/internal/termsafe"
)

// tableFormat is the name of this group's default output format, shared by every subcommand so
// the codec name registered with cmdio.Options and the default format can never drift apart.
const tableFormat = "table"

// errTableDecode is the error every table codec in this package returns from Decode: the table
// rendering is lossy and one-way by design, so there is nothing to decode back. A single
// package-level sentinel (gochecknoglobals allows an err-prefixed error var, as
// internal/config's errBootdataNonOK already relies on) keeps it comparable with errors.Is.
var errTableDecode = errors.New("table codec does not support decoding")

// rulesTableCodec implements format.Codec for `alerts rules list`'s default "table" output: it
// type-asserts the []alerts.Rule that internal/alerts.ListRules produced (already filtered and
// sorted) and renders a fixed subset of each rule's fields. The remaining fields (labels,
// annotations, folder UID, provenance, ...) are only shown by the json/yaml codecs.
//
// Mirrors cmd/grafanapi/datasources/table.go's tableCodec.
type rulesTableCodec struct{}

func (c *rulesTableCodec) Format() format.Format {
	return tableFormat
}

func (c *rulesTableCodec) Encode(output io.Writer, input any) error {
	rules, ok := input.([]alerts.Rule)
	if !ok {
		return fmt.Errorf("table codec: unsupported input type %T", input)
	}

	out := newTableWriter(output)

	fmt.Fprintf(out, "FOLDER\tGROUP\tTITLE\tUID\tTYPE\tSTATE\tHEALTH\tFOR\tPAUSED\tALERTS\n")

	for _, rule := range rules {
		fmt.Fprintf(out, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%t\t%d\n",
			cell(rule.Folder), cell(rule.RuleGroup), cell(rule.Title), cell(rule.UID), cell(rule.Type),
			cell(rule.State), cell(rule.Health), cell(rule.For), rule.IsPaused, rule.ActiveAlerts)
	}

	return out.Flush()
}

func (c *rulesTableCodec) Decode(io.Reader, any) error {
	return errTableDecode
}

// contactPointsTableCodec implements format.Codec for `alerts contact-points list`'s default
// "table" output. Settings are deliberately not a column: they are a nested object of arbitrary
// shape (already server-redacted) and belong to the json/yaml codecs.
type contactPointsTableCodec struct{}

func (c *contactPointsTableCodec) Format() format.Format {
	return tableFormat
}

func (c *contactPointsTableCodec) Encode(output io.Writer, input any) error {
	points, ok := input.(models.ContactPoints)
	if !ok {
		return fmt.Errorf("table codec: unsupported input type %T", input)
	}

	out := newTableWriter(output)

	fmt.Fprintf(out, "NAME\tTYPE\tUID\tPROVENANCE\n")

	for _, point := range points {
		if point == nil {
			continue
		}

		fmt.Fprintf(out, "%s\t%s\t%s\t%s\n",
			cell(point.Name), cell(contactPointType(point)), cell(point.UID), cell(point.Provenance))
	}

	return out.Flush()
}

func (c *contactPointsTableCodec) Decode(io.Reader, any) error {
	return errTableDecode
}

// cell renders one table cell, replacing every control and format character with a space.
//
// A folder title, rule name or contact-point name is arbitrary user input, and a tab or newline
// inside one is read by the tabwriter as a column or row terminator, while a bidirectional
// override makes a terminal re-order the rest of the line. See termsafe.Runes for the rule; the
// json/yaml codecs still carry the untouched value.
func cell(value string) string {
	return termsafe.Runes(value)
}

// contactPointType is a nil-safe accessor for the generated model's *string Type, which is a
// pointer because the provisioning API treats it as optional on writes.
func contactPointType(point *models.EmbeddedContactPoint) string {
	if point.Type == nil {
		return ""
	}

	return *point.Type
}

// newTableWriter returns the tabwriter every codec in this package renders through, so all of
// the group's table output shares one column layout.
//
// It deliberately omits the tabwriter.TabIndent the repo's other table renderers pass: that flag
// pads a *leading* empty cell with raw tab characters instead of spaces, and in this group the
// leading cell is routinely empty - FOLDER is blank for every rule whenever the state endpoint is
// unavailable - which would shift the whole row to the terminal's next tab stop.
func newTableWriter(output io.Writer) *tabwriter.Writer {
	return tabwriter.NewWriter(output, 0, 4, 2, ' ', tabwriter.DiscardEmptyColumns)
}
