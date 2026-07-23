package launchd

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
)

// bootoutNotLoadedExitCode is the exit status "launchctl bootout" uses when the target service is
// not currently loaded (as opposed to a genuine failure such as permission denied or launchd being
// unavailable). Observed as "Boot-out failed: 3: No such process" on stderr; 3 is also EX_UNAVAILABLE
// (sysexits.h), which launchctl reuses for "no such service" outcomes across bootout/bootstrap/print.
// Detection combines this exit code with a conservative stderr substring match (see
// isBootoutNotLoaded) rather than relying on either signal alone, since neither is documented as a
// stable contract by Apple.
const bootoutNotLoadedExitCode = 3

// printNotLoadedExitCode is the exit status "launchctl print <target>" uses when the target service
// is not currently loaded, as opposed to a genuine failure (permission denied, launchd unavailable,
// a malformed target, ...). Observed directly against a real launchctl (macOS 15/Sequoia class): exit
// 113, stderr "Bad request.\nCould not find service \"<label>\" in domain for user gui: <uid>". Unlike
// bootoutNotLoadedExitCode, this is not a sysexits.h code launchctl reuses elsewhere as far as this
// package has observed, but detection still combines it with a conservative stderr substring match
// (see isPrintNotLoaded) rather than relying on either signal alone, since neither is documented as a
// stable contract by Apple.
const printNotLoadedExitCode = 113

// ErrNotLoaded is returned by Bootout when launchctl reports the target service is not currently
// loaded, as distinct from a genuine failure (permission denied, launchd unavailable, etc.).
// Callers doing an idempotent boot-out (install's "boot out any previous instance" and uninstall)
// should ignore this specific error via errors.Is, but must treat any other Bootout error as real.
var ErrNotLoaded = errors.New("launchd: service not loaded")

// uidFunc returns the current user's numeric UID, used to build the GUI-domain launchctl targets
// (UserDomainTarget/UserServiceTarget). It defaults to os.Getuid; tests substitute a fixed value
// via SetUIDFunc so target formatting is verified against a known, deterministic UID rather than
// whatever account happens to run the test suite.
//
//nolint:gochecknoglobals // test seam; see SetUIDFunc
var uidFunc = os.Getuid

// commandFunc constructs the *exec.Cmd execController runs. It defaults to exec.Command; tests
// substitute a fake via SetCommandFunc that records the exact argv passed and returns a harmless,
// always-available command in place of the real launchctl - so execController's argv construction
// (subcommand + target/path) is verified without ever invoking real launchctl or touching real
// launchd state.
//
//nolint:gochecknoglobals // test seam; see SetCommandFunc
var commandFunc = exec.Command

// UserDomainTarget returns the modern launchctl GUI-domain target for the current user:
// "gui/<uid>". It is the domainTarget Bootstrap loads the keep-alive agent's plist into.
func UserDomainTarget() string {
	return fmt.Sprintf("gui/%d", uidFunc())
}

// UserServiceTarget returns the modern launchctl GUI-domain service target for the grafanapi
// keep-alive agent: "gui/<uid>/<Label>". It is the serviceTarget Bootout and Print operate on.
func UserServiceTarget() string {
	return fmt.Sprintf("%s/%s", UserDomainTarget(), Label)
}

// SetUIDFunc overrides the uid seam used by UserDomainTarget/UserServiceTarget. It exists for
// tests; it returns a function that restores the previously configured seam.
func SetUIDFunc(f func() int) func() {
	previous := uidFunc
	uidFunc = f

	return func() { uidFunc = previous }
}

// SetCommandFunc overrides the seam execController uses to build the *exec.Cmd it runs. It exists
// for tests (see commandFunc); it returns a function that restores the previously configured seam.
func SetCommandFunc(f func(name string, args ...string) *exec.Cmd) func() {
	previous := commandFunc
	commandFunc = f

	return func() { commandFunc = previous }
}

// Controller abstracts the three launchctl subcommands "session keepalive install/status/
// uninstall" need: Bootstrap loads a plist into a domain (making its schedule active), Bootout
// unloads a previously-bootstrapped service, and Print queries whether a service is currently
// loaded. The only production implementation (NewExecController) shells out to the real launchctl
// binary; every test uses a fake (testutils.FakeController) instead, so no test in this repository
// invokes real launchctl or touches real launchd state.
type Controller interface {
	// Bootstrap loads the plist at plistPath into domainTarget (typically UserDomainTarget()),
	// equivalent to "launchctl bootstrap <domainTarget> <plistPath>". Bootstrapping a service that
	// is already loaded fails; callers wanting an idempotent (re)install call Bootout first,
	// ignoring a "not loaded" failure, before calling Bootstrap.
	Bootstrap(domainTarget, plistPath string) error
	// Bootout unloads the service at serviceTarget (typically UserServiceTarget()), equivalent to
	// "launchctl bootout <serviceTarget>". Booting out a service that is not currently loaded fails
	// with ErrNotLoaded specifically; callers wanting idempotent uninstall/reinstall ignore that via
	// errors.Is, but must treat any other error as a genuine failure (the service may still be
	// loaded).
	Bootout(serviceTarget string) error
	// Print reports whether serviceTarget is currently loaded, returning launchctl's textual dump
	// of its state on success, equivalent to "launchctl print <serviceTarget>". A failure because
	// the service is not currently loaded returns ErrNotLoaded specifically (mirroring Bootout,
	// detected the same conservative exit-code+stderr-text way - see isPrintNotLoaded); any other
	// failure (permission denied, launchd unavailable, a malformed target, ...) returns a distinct,
	// non-ErrNotLoaded error. Callers must not conflate the two - see ClassifyLoadState, which is
	// built on exactly this distinction.
	Print(serviceTarget string) (string, error)
}

// LoadState is the three-way result ClassifyLoadState reports for whether a launchd service is
// currently loaded, as determined by ctrl.Print. It is deliberately not a bool: collapsing "Print
// failed for a reason we don't recognize" into either Loaded or NotLoaded is exactly the false-
// success bug ClassifyLoadState (formerly ConfirmNotLoaded) exists to avoid - callers must be able
// to tell "confirmed absent" apart from "we genuinely do not know".
type LoadState int

const (
	// LoadStateLoaded means Print succeeded: the service is confirmed currently loaded.
	LoadStateLoaded LoadState = iota
	// LoadStateNotLoaded means Print failed with ErrNotLoaded specifically: the service is
	// confirmed not currently loaded.
	LoadStateNotLoaded
	// LoadStateInconclusive means Print failed for some other reason (permission denied, launchd
	// unavailable, an unrecognized launchctl version's wording, ...). Whether the service is loaded
	// could not be determined; callers must not treat this the same as LoadStateNotLoaded.
	LoadStateInconclusive
)

// String renders state for log/warning/error messages.
func (state LoadState) String() string {
	switch state {
	case LoadStateLoaded:
		return "loaded"
	case LoadStateNotLoaded:
		return "not loaded"
	case LoadStateInconclusive:
		return "inconclusive"
	default:
		return fmt.Sprintf("LoadState(%d)", int(state))
	}
}

// ClassifyLoadState disambiguates a Bootout failure that isBootoutNotLoaded's conservative exit-
// code+text match did not recognize as "not loaded" - e.g. a launchctl version that reports the
// same "no such service" outcome with wording this package has never observed. Rather than
// loosening that text match (which risks misclassifying a genuine failure as benign), it asks a
// different, independent question via ctrl.Print: is the service loaded right now?
//
//   - Print succeeds: the service is confirmed currently loaded (LoadStateLoaded) - the caller must
//     treat the original Bootout failure as genuine, since the service may still be loaded.
//   - Print fails with ErrNotLoaded (the same sentinel Bootout uses, both detected via a
//     conservative exit-code+stderr-text match - see isPrintNotLoaded): the service is confirmed
//     absent (LoadStateNotLoaded) - the original Bootout failure was benign, and the caller should
//     proceed exactly as it would for a directly-recognized ErrNotLoaded.
//   - Print fails for any other reason (permission denied, launchd unavailable, an unrecognized
//     launchctl version's wording, ...): the state could not be determined (LoadStateInconclusive).
//     Treating this the same as LoadStateNotLoaded is exactly the false-success bug this function
//     exists to prevent: an inconclusive Print must never be read as "confirmed absent".
//
// This is a second signal layered on top of isBootoutNotLoaded, not a replacement for it: it only
// runs once that match has already failed to classify the error, and only for the callers (install,
// uninstall) doing an idempotent boot-out that need to tell "already gone" apart from "a real
// problem occurred" apart from "we don't know".
func ClassifyLoadState(ctrl Controller, serviceTarget string) LoadState {
	_, err := ctrl.Print(serviceTarget)

	switch {
	case err == nil:
		return LoadStateLoaded
	case errors.Is(err, ErrNotLoaded):
		return LoadStateNotLoaded
	default:
		return LoadStateInconclusive
	}
}

// NewExecController returns the real, production Controller: it shells out to the system
// "launchctl" binary with exactly the three fixed subcommands the Controller interface exposes.
// No test in this repository exercises it directly; every test substitutes a fake instead.
//
// We have to return an interface here.
//
//nolint:ireturn
func NewExecController() Controller {
	return execController{}
}

// execController is the production Controller. Every method below shells out to "launchctl" with
// a fixed, literal subcommand ("bootstrap"/"bootout"/"print") — never a user- or config-supplied
// command name. Only the *arguments* to that subcommand (a domain/service target, or a plist
// path) are variable. This is exactly what gosec's G204 ("subprocess launched with variable")
// exists to flag, but it does not fire here because the call goes through the commandFunc seam
// (a package-level func var, not a literal exec.Command/exec.CommandContext selector) rather than
// directly - no nolint is needed or present.
type execController struct{}

func (execController) Bootstrap(domainTarget, plistPath string) error {
	cmd := commandFunc("launchctl", "bootstrap", domainTarget, plistPath)

	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("launchctl bootstrap %s: %w: %s", domainTarget, err, bytes.TrimSpace(out))
	}

	return nil
}

func (execController) Bootout(serviceTarget string) error {
	cmd := commandFunc("launchctl", "bootout", serviceTarget)

	out, err := cmd.CombinedOutput()
	if err == nil {
		return nil
	}

	if isBootoutNotLoaded(err, out) {
		return ErrNotLoaded
	}

	return fmt.Errorf("launchctl bootout %s: %w: %s", serviceTarget, err, bytes.TrimSpace(out))
}

func (execController) Print(serviceTarget string) (string, error) {
	cmd := commandFunc("launchctl", "print", serviceTarget)

	out, err := cmd.Output()
	if err == nil {
		return string(out), nil
	}

	if isPrintNotLoaded(err) {
		return "", ErrNotLoaded
	}

	return "", fmt.Errorf("launchctl print %s: %w", serviceTarget, err)
}

// isBootoutNotLoaded reports whether a failed "launchctl bootout" invocation failed specifically
// because the target service is not currently loaded, rather than a genuine failure (permission
// denied, launchd unavailable, malformed target, etc.). It is intentionally conservative: it only
// matches the shapes actually observed from launchctl, requiring BOTH of the following (not
// either alone, since exit code 3 is reused for other "no such service" outcomes and the wording
// alone could theoretically appear in an unrelated failure) -
//
//   - exit status bootoutNotLoadedExitCode (3); and
//   - stderr containing "no such process" (the wording launchctl prints for this case, e.g.
//     "Boot-out failed: 3: No such process") or "could not find" (seen on some launchd/macOS
//     versions instead, e.g. "Could not find service").
//
// Any other failure (including exit code 3 with unrecognized output, or recognized text without
// exit code 3) is treated as a real error, not a "not loaded" outcome - callers must not silently
// swallow it.
func isBootoutNotLoaded(err error, output []byte) bool {
	lower := bytes.ToLower(bytes.TrimSpace(output))
	textMatch := bytes.Contains(lower, []byte("no such process")) || bytes.Contains(lower, []byte("could not find"))

	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode() == bootoutNotLoadedExitCode && textMatch
	}

	return false
}

// isPrintNotLoaded reports whether a failed "launchctl print" invocation failed specifically
// because the target service is not currently loaded, rather than a genuine failure (permission
// denied, launchd unavailable, a malformed target, etc.). Mirrors isBootoutNotLoaded's rigor: it
// requires BOTH exit status printNotLoadedExitCode (113) AND stderr containing "could not find"
// (the wording observed from a real launchctl, e.g. "Could not find service \"<label>\" in domain
// for user gui: <uid>"), not either signal alone.
//
// cmd.Output() (used by execController.Print) does not capture stderr into the returned bytes, but
// does populate it on the *exec.ExitError it returns (Cmd.Stderr is left nil, which is exactly the
// case Output documents as capturing stderr onto the error) - so, unlike isBootoutNotLoaded, this
// reads stderr off of err itself rather than taking a separate output parameter.
func isPrintNotLoaded(err error) bool {
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		return false
	}

	lower := bytes.ToLower(bytes.TrimSpace(exitErr.Stderr))
	textMatch := bytes.Contains(lower, []byte("could not find"))

	return exitErr.ExitCode() == printNotLoadedExitCode && textMatch
}
