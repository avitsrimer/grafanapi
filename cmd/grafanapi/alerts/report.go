package alerts

import (
	"io"

	cmdio "github.com/grafana/grafanapi/cmd/grafanapi/io"
)

// summaryPrinter picks the cmdio function the closing line of a report is printed with, from what
// the run achieved: a clean run is a success, a run that failed at everything it attempted is an
// error, and anything in between is a warning - the summary is still the line a user reads to find
// out which of the two happened.
//
// It is the one ladder `alerts push` and the three rule operations share; the three cmdio
// functions have the same signature, which is what lets the choice be made once and applied to
// whatever wording the caller closes with.
func summaryPrinter(succeeded, failed int) func(io.Writer, string, ...any) {
	switch {
	case failed == 0:
		return cmdio.Success
	case succeeded == 0:
		return cmdio.Error
	default:
		return cmdio.Warning
	}
}
