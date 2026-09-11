// Package onerror implements the shared --on-error flag: how a command that operates on many
// items independently (resources pull/push/delete/get/validate, alerts push) treats per-item
// failures - ignore them, keep going but exit non-zero, or stop at the first one.
//
// It is a tiny Cobra-adjacent package rather than part of any one command group so that every
// group binding the flag gets identical help text and identical semantics - which is why the help
// says "items" rather than "resources": the flag is on `alerts push` too, where the items are
// contact points, a policy tree and rule groups. The internal/ packages that actually perform the
// work never import it: like remote.PushRequest they take a plain StopOnError bool, and the cmd
// layer owns the Mode and applies FailOnErrors to the result.
package onerror

import (
	"fmt"

	"github.com/spf13/pflag"
)

const (
	// Ignore continues processing all items and exits 0, even if some failed.
	Ignore Mode = "ignore"

	// Fail continues processing all items but exits with a non-zero code
	// if any operations failed. This is the default mode.
	Fail Mode = "fail"

	// Abort stops processing on the first error and exits with a non-zero code.
	Abort Mode = "abort"
)

// BindFlag registers the --on-error flag on the given flag set.
func BindFlag(flags *pflag.FlagSet, target *Mode) {
	*target = Fail
	flags.StringVar(
		(*string)(target),
		"on-error",
		string(Fail),
		`How to handle errors while processing items:
  ignore — continue processing all items and exit 0
  fail   — continue processing all items and exit 1 if any failed (default)
  abort  — stop on the first error and exit 1`,
	)
}

// Mode controls how commands handle per-item operation errors.
type Mode string

// StopOnError reports whether the mode should abort processing on the first error.
func (m Mode) StopOnError() bool {
	return m == Abort
}

// FailOnErrors reports whether the mode should exit with a non-zero code when any errors occurred.
func (m Mode) FailOnErrors() bool {
	return m == Fail || m == Abort
}

// Validate returns an error if the mode value is not one of the recognized options.
func (m Mode) Validate() error {
	switch m {
	case Ignore, Fail, Abort:
		return nil
	default:
		return fmt.Errorf("invalid --on-error value %q: must be one of ignore, fail, abort", string(m))
	}
}
