package launchd

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
)

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
	// "launchctl bootout <serviceTarget>". Booting out a service that is not currently loaded
	// fails; callers wanting idempotent uninstall/reinstall ignore that specific failure.
	Bootout(serviceTarget string) error
	// Print reports whether serviceTarget is currently loaded, returning launchctl's textual dump
	// of its state on success, equivalent to "launchctl print <serviceTarget>". An error means the
	// service is not loaded (or launchctl could not be run at all); callers treat any Print error
	// as Loaded=false rather than a hard failure.
	Print(serviceTarget string) (string, error)
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
	if err != nil {
		return fmt.Errorf("launchctl bootout %s: %w: %s", serviceTarget, err, bytes.TrimSpace(out))
	}

	return nil
}

func (execController) Print(serviceTarget string) (string, error) {
	cmd := commandFunc("launchctl", "print", serviceTarget)

	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("launchctl print %s: %w", serviceTarget, err)
	}

	return string(out), nil
}
