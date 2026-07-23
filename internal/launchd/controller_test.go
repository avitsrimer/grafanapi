package launchd_test

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"testing"

	"github.com/grafana/grafanapi/internal/launchd"
	"github.com/grafana/grafanapi/internal/testutils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestUserDomainTarget_FormatsFromStubbedUID(t *testing.T) {
	restore := launchd.SetUIDFunc(func() int { return 501 })
	defer restore()

	assert.Equal(t, "gui/501", launchd.UserDomainTarget())
}

func TestUserServiceTarget_FormatsFromStubbedUID(t *testing.T) {
	restore := launchd.SetUIDFunc(func() int { return 501 })
	defer restore()

	assert.Equal(t, "gui/501/"+launchd.Label, launchd.UserServiceTarget())
}

func TestUserDomainTarget_RootUID(t *testing.T) {
	restore := launchd.SetUIDFunc(func() int { return 0 })
	defer restore()

	assert.Equal(t, "gui/0", launchd.UserDomainTarget())
}

func TestSetUIDFunc_Restores(t *testing.T) {
	restore := launchd.SetUIDFunc(func() int { return 42 })
	assert.Equal(t, "gui/42", launchd.UserDomainTarget())

	restore()

	// Restored to the real os.Getuid seam - just confirm it no longer reports the stubbed value.
	assert.NotEqual(t, "gui/42", launchd.UserDomainTarget())
}

// fakeCommand returns a commandFunc-shaped fake that records the exact argv it was called with
// into captured, then returns a harmless, always-succeeding command ("true") in place of the real
// launchctl - so execController's argv construction is verified without ever invoking launchctl or
// touching real launchd state.
func fakeCommand(captured *[]string) func(name string, args ...string) *exec.Cmd {
	return func(name string, args ...string) *exec.Cmd {
		*captured = append([]string{name}, args...)

		return exec.CommandContext(context.Background(), "true")
	}
}

func TestExecController_Bootstrap_ArgvConstruction(t *testing.T) {
	var captured []string

	restore := launchd.SetCommandFunc(fakeCommand(&captured))
	defer restore()

	controller := launchd.NewExecController()

	err := controller.Bootstrap("gui/501", "/tmp/keepalive.plist")
	require.NoError(t, err)
	assert.Equal(t, []string{"launchctl", "bootstrap", "gui/501", "/tmp/keepalive.plist"}, captured)
}

func TestExecController_Bootout_ArgvConstruction(t *testing.T) {
	var captured []string

	restore := launchd.SetCommandFunc(fakeCommand(&captured))
	defer restore()

	controller := launchd.NewExecController()

	err := controller.Bootout("gui/501/io.github.avitsrimer.grafanapi.keepalive")
	require.NoError(t, err)
	assert.Equal(t,
		[]string{"launchctl", "bootout", "gui/501/io.github.avitsrimer.grafanapi.keepalive"},
		captured,
	)
}

func TestExecController_Print_ArgvConstruction(t *testing.T) {
	var captured []string

	restore := launchd.SetCommandFunc(fakeCommand(&captured))
	defer restore()

	controller := launchd.NewExecController()

	out, err := controller.Print("gui/501/io.github.avitsrimer.grafanapi.keepalive")
	require.NoError(t, err)
	assert.Empty(t, out) // "true" produces no output
	assert.Equal(t,
		[]string{"launchctl", "print", "gui/501/io.github.avitsrimer.grafanapi.keepalive"},
		captured,
	)
}

func TestExecController_Bootstrap_ErrorIncludesTargetAndOutput(t *testing.T) {
	restore := launchd.SetCommandFunc(func(string, ...string) *exec.Cmd {
		// "false" always exits non-zero with no output, exercising the error-wrapping branch
		// without ever running launchctl.
		return exec.CommandContext(context.Background(), "false")
	})
	defer restore()

	controller := launchd.NewExecController()

	err := controller.Bootstrap("gui/501", "/tmp/keepalive.plist")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "launchctl bootstrap gui/501")
}

func TestExecController_Bootout_ErrorIncludesTarget(t *testing.T) {
	restore := launchd.SetCommandFunc(func(string, ...string) *exec.Cmd {
		return exec.CommandContext(context.Background(), "false")
	})
	defer restore()

	controller := launchd.NewExecController()

	err := controller.Bootout("gui/501/io.github.avitsrimer.grafanapi.keepalive")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "launchctl bootout gui/501/io.github.avitsrimer.grafanapi.keepalive")
}

func TestExecController_Print_ErrorIncludesTarget(t *testing.T) {
	restore := launchd.SetCommandFunc(func(string, ...string) *exec.Cmd {
		return exec.CommandContext(context.Background(), "false")
	})
	defer restore()

	controller := launchd.NewExecController()

	_, err := controller.Print("gui/501/io.github.avitsrimer.grafanapi.keepalive")
	require.Error(t, err)
	require.NotErrorIs(t, err, launchd.ErrNotLoaded)
	assert.Contains(t, err.Error(), "launchctl print gui/501/io.github.avitsrimer.grafanapi.keepalive")
}

// fakeFailingCommand returns a commandFunc-shaped fake that, in place of the real launchctl, runs
// a tiny shell script writing stderrText to stderr and exiting with exitCode - so Bootout's
// "not loaded" detection (isBootoutNotLoaded) is exercised against known, controlled exit
// code/output combinations without ever invoking real launchctl.
func fakeFailingCommand(exitCode int, stderrText string) func(name string, args ...string) *exec.Cmd {
	return func(string, ...string) *exec.Cmd {
		script := fmt.Sprintf("echo %q 1>&2; exit %d", stderrText, exitCode)

		return exec.CommandContext(context.Background(), "sh", "-c", script)
	}
}

func TestExecController_Bootout_NotLoaded_ReturnsErrNotLoaded(t *testing.T) {
	// Exit 3 + "No such process" is the shape actually observed from "launchctl bootout" against a
	// service that is not currently loaded (e.g. "Boot-out failed: 3: No such process").
	restore := launchd.SetCommandFunc(fakeFailingCommand(3, "Boot-out failed: 3: No such process"))
	defer restore()

	controller := launchd.NewExecController()

	err := controller.Bootout("gui/501/io.github.avitsrimer.grafanapi.keepalive")
	require.Error(t, err)
	assert.ErrorIs(t, err, launchd.ErrNotLoaded)
}

func TestExecController_Bootout_NotLoaded_AlternateWording_ReturnsErrNotLoaded(t *testing.T) {
	// Some launchd/macOS versions report the same outcome with different wording ("Could not find
	// service") - still exit 3, still a "not loaded" outcome, not a genuine failure.
	restore := launchd.SetCommandFunc(fakeFailingCommand(3, "Could not find service"))
	defer restore()

	controller := launchd.NewExecController()

	err := controller.Bootout("gui/501/io.github.avitsrimer.grafanapi.keepalive")
	require.Error(t, err)
	assert.ErrorIs(t, err, launchd.ErrNotLoaded)
}

func TestExecController_Bootout_GenuineFailure_IsNotErrNotLoaded(t *testing.T) {
	// A real failure (e.g. permission denied) uses a different exit code and wording entirely - it
	// must be surfaced as a genuine error, never mistaken for "not loaded".
	restore := launchd.SetCommandFunc(fakeFailingCommand(1, "Boot-out failed: 1: Operation not permitted"))
	defer restore()

	controller := launchd.NewExecController()

	err := controller.Bootout("gui/501/io.github.avitsrimer.grafanapi.keepalive")
	require.Error(t, err)
	require.NotErrorIs(t, err, launchd.ErrNotLoaded)
	assert.Contains(t, err.Error(), "launchctl bootout gui/501/io.github.avitsrimer.grafanapi.keepalive")
}

func TestExecController_Bootout_ExitCode3WithUnrecognizedOutput_IsNotErrNotLoaded(t *testing.T) {
	// Exit code 3 alone (without the known wording) is not enough to call it "not loaded" - the
	// detection requires both signals together, since launchctl reuses exit code 3 for other "no
	// such service" outcomes too.
	restore := launchd.SetCommandFunc(fakeFailingCommand(3, "some unrelated launchctl failure"))
	defer restore()

	controller := launchd.NewExecController()

	err := controller.Bootout("gui/501/io.github.avitsrimer.grafanapi.keepalive")
	require.Error(t, err)
	require.NotErrorIs(t, err, launchd.ErrNotLoaded)
}

func TestExecController_Bootout_RecognizedTextWrongExitCode_IsNotErrNotLoaded(t *testing.T) {
	// The known wording with an unrelated exit code is also not enough - both signals must agree.
	restore := launchd.SetCommandFunc(fakeFailingCommand(1, "No such process"))
	defer restore()

	controller := launchd.NewExecController()

	err := controller.Bootout("gui/501/io.github.avitsrimer.grafanapi.keepalive")
	require.Error(t, err)
	require.NotErrorIs(t, err, launchd.ErrNotLoaded)
}

// TestExecController_Print_NotLoaded_ReturnsErrNotLoaded covers the shape actually observed from a
// real "launchctl print" against a service that is not currently loaded: exit 113, stderr
// containing "Could not find service ...". execController.Print must classify this as ErrNotLoaded,
// not a generic error - see isPrintNotLoaded.
func TestExecController_Print_NotLoaded_ReturnsErrNotLoaded(t *testing.T) {
	restore := launchd.SetCommandFunc(fakeFailingCommand(113,
		"Bad request.\nCould not find service \"io.github.avitsrimer.grafanapi.keepalive\" in domain for user gui: 501"))
	defer restore()

	controller := launchd.NewExecController()

	_, err := controller.Print("gui/501/io.github.avitsrimer.grafanapi.keepalive")
	require.Error(t, err)
	assert.ErrorIs(t, err, launchd.ErrNotLoaded)
}

// TestExecController_Print_GenuineFailure_IsNotErrNotLoaded covers a real failure (e.g. permission
// denied): a different exit code and wording entirely, so it must never be mistaken for "not
// loaded".
func TestExecController_Print_GenuineFailure_IsNotErrNotLoaded(t *testing.T) {
	restore := launchd.SetCommandFunc(fakeFailingCommand(1, "print failed: 1: Operation not permitted"))
	defer restore()

	controller := launchd.NewExecController()

	_, err := controller.Print("gui/501/io.github.avitsrimer.grafanapi.keepalive")
	require.Error(t, err)
	require.NotErrorIs(t, err, launchd.ErrNotLoaded)
	assert.Contains(t, err.Error(), "launchctl print gui/501/io.github.avitsrimer.grafanapi.keepalive")
}

// TestExecController_Print_ExitCode113WithUnrecognizedOutput_IsNotErrNotLoaded covers the same
// "both signals required" rigor as isBootoutNotLoaded: exit code 113 alone, without the recognized
// wording, is not enough to call it "not loaded".
func TestExecController_Print_ExitCode113WithUnrecognizedOutput_IsNotErrNotLoaded(t *testing.T) {
	restore := launchd.SetCommandFunc(fakeFailingCommand(113, "some unrelated launchctl failure"))
	defer restore()

	controller := launchd.NewExecController()

	_, err := controller.Print("gui/501/io.github.avitsrimer.grafanapi.keepalive")
	require.Error(t, err)
	require.NotErrorIs(t, err, launchd.ErrNotLoaded)
}

// TestExecController_Print_RecognizedTextWrongExitCode_IsNotErrNotLoaded covers the other half: the
// known wording with an unrelated exit code is also not enough - both signals must agree.
func TestExecController_Print_RecognizedTextWrongExitCode_IsNotErrNotLoaded(t *testing.T) {
	restore := launchd.SetCommandFunc(fakeFailingCommand(1, "Could not find service"))
	defer restore()

	controller := launchd.NewExecController()

	_, err := controller.Print("gui/501/io.github.avitsrimer.grafanapi.keepalive")
	require.Error(t, err)
	require.NotErrorIs(t, err, launchd.ErrNotLoaded)
}

// TestClassifyLoadState_PrintSucceeds_ReportsLoaded covers the disambiguation ClassifyLoadState
// performs for a Bootout failure isBootoutNotLoaded did not recognize (e.g. unrecognized exit
// code/wording from a launchctl version this package has never observed): Print succeeding means
// the service is confirmed still loaded, so ClassifyLoadState must report LoadStateLoaded - the
// original Bootout failure was genuine, not a benign "not loaded" outcome.
func TestClassifyLoadState_PrintSucceeds_ReportsLoaded(t *testing.T) {
	fake := testutils.NewFakeController()

	assert.Equal(t, launchd.LoadStateLoaded,
		launchd.ClassifyLoadState(fake, "gui/501/io.github.avitsrimer.grafanapi.keepalive"))

	calls := fake.Calls()
	require.Len(t, calls, 1)
	assert.Equal(t, "Print", calls[0].Method)
}

// TestClassifyLoadState_PrintErrNotLoaded_ReportsNotLoaded covers Print failing with ErrNotLoaded
// specifically (the same sentinel Bootout uses, both detected via a conservative exit-code+text
// match): ClassifyLoadState must report LoadStateNotLoaded - the original Bootout failure was
// benign.
func TestClassifyLoadState_PrintErrNotLoaded_ReportsNotLoaded(t *testing.T) {
	fake := testutils.NewFakeController()
	fake.PrintErr = launchd.ErrNotLoaded

	assert.Equal(t, launchd.LoadStateNotLoaded,
		launchd.ClassifyLoadState(fake, "gui/501/io.github.avitsrimer.grafanapi.keepalive"))

	calls := fake.Calls()
	require.Len(t, calls, 1)
	assert.Equal(t, "Print", calls[0].Method)
}

// TestClassifyLoadState_PrintGenericError_ReportsInconclusive is the critical regression case: a
// Print failure that is neither success nor a recognized ErrNotLoaded (e.g. permission denied,
// launchd itself unreachable) must NOT be read as "confirmed absent" - that conflation is exactly
// the false-success bug this package was fixed to remove. ClassifyLoadState must report
// LoadStateInconclusive.
func TestClassifyLoadState_PrintGenericError_ReportsInconclusive(t *testing.T) {
	fake := testutils.NewFakeController()
	fake.PrintErr = errors.New("launchd unavailable")

	assert.Equal(t, launchd.LoadStateInconclusive,
		launchd.ClassifyLoadState(fake, "gui/501/io.github.avitsrimer.grafanapi.keepalive"))

	calls := fake.Calls()
	require.Len(t, calls, 1)
	assert.Equal(t, "Print", calls[0].Method)
}

// TestExecController_Print_AbsenceClassification exercises ClassifyLoadState against the real,
// exec-backed Controller: a "print" invocation failing with the recognized absence shape (as it
// does against a service that is not loaded) must classify as LoadStateNotLoaded end-to-end,
// through the same commandFunc seam Bootout uses - not just against the in-memory FakeController.
func TestExecController_Print_AbsenceClassification(t *testing.T) {
	restore := launchd.SetCommandFunc(fakeFailingCommand(113, "Could not find service"))
	defer restore()

	controller := launchd.NewExecController()

	assert.Equal(t, launchd.LoadStateNotLoaded,
		launchd.ClassifyLoadState(controller, "gui/501/io.github.avitsrimer.grafanapi.keepalive"))
}

// TestExecController_Print_InconclusiveClassification covers the other side end-to-end: a "print"
// invocation failing for an unrecognized reason must classify as LoadStateInconclusive, never
// LoadStateNotLoaded.
func TestExecController_Print_InconclusiveClassification(t *testing.T) {
	restore := launchd.SetCommandFunc(fakeFailingCommand(1, "Operation not permitted"))
	defer restore()

	controller := launchd.NewExecController()

	assert.Equal(t, launchd.LoadStateInconclusive,
		launchd.ClassifyLoadState(controller, "gui/501/io.github.avitsrimer.grafanapi.keepalive"))
}
