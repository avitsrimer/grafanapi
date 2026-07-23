package launchd_test

import (
	"context"
	"os/exec"
	"testing"

	"github.com/grafana/grafanapi/internal/launchd"
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
	assert.Contains(t, err.Error(), "launchctl print gui/501/io.github.avitsrimer.grafanapi.keepalive")
}
