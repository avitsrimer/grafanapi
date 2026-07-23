package testutils

import (
	"sync"

	"github.com/grafana/grafanapi/internal/launchd"
)

// ControllerCall records one invocation made to a FakeController, in call order, for tests that
// need to assert exactly which launchctl operations ran and in what sequence (e.g. "install must
// Bootout before Bootstrap").
type ControllerCall struct {
	// Method is the Controller method invoked: "Bootstrap", "Bootout", or "Print".
	Method string
	// Target is the domain or service target passed to the call.
	Target string
	// PlistPath is set only for a Bootstrap call: the plist path passed to it.
	PlistPath string
}

// FakeController is an in-memory launchd.Controller for tests, shared by the internal/launchd,
// cmd/grafanapi/session, and cmd/grafanapi/config test suites: it never shells out to the real
// launchctl, records every call it receives (in order, via Calls), and returns whatever error/
// output a test scripted via the exported fields below.
type FakeController struct {
	mu sync.Mutex

	// BootstrapErr is returned by every Bootstrap call.
	BootstrapErr error
	// BootoutErr is returned by every Bootout call.
	BootoutErr error
	// PrintOutput is returned by every Print call alongside PrintErr.
	PrintOutput string
	// PrintErr is returned by every Print call. Its identity - not just its non-nilness - drives
	// launchd.ClassifyLoadState, mirroring how the real execController.Print returns ErrNotLoaded
	// specifically for a recognized "not loaded" shape and a distinct error for anything else:
	//   - nil: Print "succeeds" -> LoadStateLoaded (service confirmed loaded).
	//   - launchd.ErrNotLoaded: mirrors a recognized "not loaded" Print failure -> LoadStateNotLoaded
	//     (service confirmed absent).
	//   - any other non-nil error: mirrors an unrecognized/genuine Print failure (permission denied,
	//     launchd unavailable, ...) -> LoadStateInconclusive. Do NOT script an arbitrary
	//     errors.New("...") expecting it to be read as "not loaded" - only launchd.ErrNotLoaded is.
	PrintErr error

	calls []ControllerCall
}

// NewFakeController returns a FakeController with no scripted errors: Bootstrap and Bootout
// succeed and Print succeeds with an empty output, until a test overrides one of the exported
// fields.
func NewFakeController() *FakeController {
	return &FakeController{}
}

// Bootstrap records the call and returns BootstrapErr.
func (f *FakeController) Bootstrap(domainTarget, plistPath string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.calls = append(f.calls, ControllerCall{Method: "Bootstrap", Target: domainTarget, PlistPath: plistPath})

	return f.BootstrapErr
}

// Bootout records the call and returns BootoutErr.
func (f *FakeController) Bootout(serviceTarget string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.calls = append(f.calls, ControllerCall{Method: "Bootout", Target: serviceTarget})

	return f.BootoutErr
}

// Print records the call and returns PrintOutput, PrintErr.
func (f *FakeController) Print(serviceTarget string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.calls = append(f.calls, ControllerCall{Method: "Print", Target: serviceTarget})

	return f.PrintOutput, f.PrintErr
}

// Calls returns a copy of every call recorded so far, in the order they were made.
func (f *FakeController) Calls() []ControllerCall {
	f.mu.Lock()
	defer f.mu.Unlock()

	out := make([]ControllerCall, len(f.calls))
	copy(out, f.calls)

	return out
}

// _ documents (and compile-checks) that FakeController satisfies launchd.Controller.
var _ launchd.Controller = (*FakeController)(nil)
