package alerts

import (
	"fmt"
	"io"

	"github.com/grafana/grafanapi/internal/alerts"
)

// detailIndent prefixes every detail line after the first, so a rule group's inner changes read
// as belonging to the row above them rather than as plan entries of their own.
const detailIndent = "  "

// planSummary counts a plan by action, in the wording `push --dry-run` ends on.
//
// Unchanged entries are counted rather than hidden: "nothing to do" is the answer a dry run is
// most often asked for, and a plan listing only the two groups that differ would leave the user
// guessing whether the rest was read at all.
func planSummary(changes []alerts.Change) string {
	var create, update, unchanged int

	for _, change := range changes {
		switch change.Action {
		case alerts.ActionCreate:
			create++
		case alerts.ActionUpdate:
			update++
		case alerts.ActionUnchanged:
			unchanged++
		}
	}

	return fmt.Sprintf("%d to create, %d to update, %d unchanged", create, update, unchanged)
}

// renderPlan writes a plan as ACTION / KIND / ID / DETAIL rows followed by a summary line.
//
// It is a plain renderer rather than a format.Codec registered with cmdio.Options: `alerts push`
// has no -o flag (like `resources push`), so there is no format to select between and nothing to
// decode back - the plan is a rendering of what a push would do, not a serialization of it.
//
// A change's first detail line shares its row; the remaining ones follow on rows of their own,
// indented under the DETAIL column, since a rule group's detail is a list (one interval line plus
// one line per rule) and packing it into a single cell would make the widest row set the width of
// the whole table. Every cell goes through cell(): a rule title is arbitrary user input, and a tab
// or newline inside one would shift the rest of the row into the wrong columns.
func renderPlan(out io.Writer, changes []alerts.Change) error {
	table := newTableWriter(out)

	fmt.Fprint(table, "ACTION\tKIND\tID\tDETAIL\n")

	for _, change := range changes {
		// An entry without detail still needs a DETAIL cell for the row to have the same shape
		// as every other one, which is what lets the tabwriter align the columns.
		details := change.Detail
		if len(details) == 0 {
			details = []string{""}
		}

		fmt.Fprintf(table, "%s\t%s\t%s\t%s\n",
			cell(string(change.Action)), cell(string(change.Kind)), cell(change.ID), cell(details[0]))

		for _, detail := range details[1:] {
			fmt.Fprintf(table, "\t\t\t%s%s\n", detailIndent, cell(detail))
		}
	}

	if err := table.Flush(); err != nil {
		return fmt.Errorf("rendering plan: %w", err)
	}

	fmt.Fprintln(out, planSummary(changes))

	return nil
}
