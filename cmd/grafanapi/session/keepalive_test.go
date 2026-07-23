package session_test

import (
	"encoding/json"
	"errors"
	"os"
	"testing"

	"github.com/grafana/grafanapi/cmd/grafanapi/session"
	"github.com/grafana/grafanapi/internal/launchd"
	"github.com/grafana/grafanapi/internal/testutils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// keepaliveHome points $HOME at a fresh temporary directory for the duration of the test (via
// t.Setenv, restored automatically by the testing package) and returns it. internal/launchd's path
// helpers (LaunchAgentsDir/PlistPath/LogDir/LogPath) are all derived from $HOME - see
// launchd.TestPathHelpers_HonorHOME - so this is how "session keepalive" tests avoid touching the
// real ~/Library/LaunchAgents.
func keepaliveHome(t *testing.T) string {
	t.Helper()

	home := t.TempDir()
	t.Setenv("HOME", home)

	return home
}

// oneContextWithWindowConfig writes a temp config file with a single context ("default") whose
// live-window is set to "12h" - every test in this file that needs at least one opted-in context
// uses the same window, since only its presence (not its exact value) matters to install/status/
// uninstall; only the due-selection tests in refresh_test.go need varied window values.
func oneContextWithWindowConfig(t *testing.T) string {
	t.Helper()

	return testutils.CreateTempFile(t, `current-context: default
contexts:
  default:
    grafana:
      server: https://example.invalid
      live-window: 12h
`)
}

// noWindowConfig writes a temp config file with a single context that never opts into keep-alive.
func noWindowConfig(t *testing.T) string {
	t.Helper()

	return testutils.CreateTempFile(t, `current-context: default
contexts:
  default:
    grafana:
      server: https://example.invalid
`)
}

func Test_KeepaliveInstall_WritesPlistAndBootstraps(t *testing.T) {
	home := keepaliveHome(t)
	configFile := oneContextWithWindowConfig(t)

	fake := testutils.NewFakeController()
	restore := session.SetController(fake)
	defer restore()

	testCase := testutils.CommandTestCase{
		Cmd:     session.Command(),
		Command: []string{"keepalive", "install", "--config", configFile},
		Assertions: []testutils.CommandAssertion{
			testutils.CommandSuccess(),
			testutils.CommandOutputContains("installed keepalive LaunchAgent"),
		},
		Env: map[string]string{"HOME": home},
	}
	testCase.Run(t)

	// min(live-window) is 12h; derived interval is 12h/2=6h, well within [15m,12h], so no clamping.
	spec, err := launchd.Inspect(launchd.PlistPath())
	require.NoError(t, err)
	assert.Equal(t, int(6*60*60), spec.IntervalSeconds)
	assert.Equal(t, []string{"session", "refresh", "--due"}, spec.Args)
	assert.Equal(t, launchd.Label, spec.Label)

	calls := fake.Calls()
	require.Len(t, calls, 2)
	assert.Equal(t, "Bootout", calls[0].Method)
	assert.Equal(t, launchd.UserServiceTarget(), calls[0].Target)
	assert.Equal(t, "Bootstrap", calls[1].Method)
	assert.Equal(t, launchd.UserDomainTarget(), calls[1].Target)
	assert.Equal(t, launchd.PlistPath(), calls[1].PlistPath)
}

func Test_KeepaliveInstall_Idempotent(t *testing.T) {
	home := keepaliveHome(t)
	configFile := oneContextWithWindowConfig(t)

	fake := testutils.NewFakeController()
	restore := session.SetController(fake)
	defer restore()

	for range 2 {
		testCase := testutils.CommandTestCase{
			Cmd:     session.Command(),
			Command: []string{"keepalive", "install", "--config", configFile},
			Assertions: []testutils.CommandAssertion{
				testutils.CommandSuccess(),
			},
			Env: map[string]string{"HOME": home},
		}
		testCase.Run(t)
	}

	// Bootout->Bootstrap must have run twice, once per install, with no error either time.
	calls := fake.Calls()
	require.Len(t, calls, 4)
	assert.Equal(t, "Bootout", calls[0].Method)
	assert.Equal(t, "Bootstrap", calls[1].Method)
	assert.Equal(t, "Bootout", calls[2].Method)
	assert.Equal(t, "Bootstrap", calls[3].Method)
}

func Test_KeepaliveInstall_BootstrapFailure_DegradesToWarning(t *testing.T) {
	home := keepaliveHome(t)
	configFile := oneContextWithWindowConfig(t)

	fake := testutils.NewFakeController()
	fake.BootstrapErr = errors.New("boom")
	restore := session.SetController(fake)
	defer restore()

	testCase := testutils.CommandTestCase{
		Cmd:     session.Command(),
		Command: []string{"keepalive", "install", "--config", configFile},
		Assertions: []testutils.CommandAssertion{
			testutils.CommandSuccess(),
			testutils.CommandOutputContains("could not load it via launchctl"),
		},
		Env: map[string]string{"HOME": home},
	}
	testCase.Run(t)

	// The plist must still have been written even though bootstrapping it failed.
	_, err := os.Stat(launchd.PlistPath())
	require.NoError(t, err)
}

// Test_KeepaliveInstall_BootoutNotLoaded_ProceedsSilently covers the expected, common case:
// there is no previous instance loaded, so Bootout fails with launchd.ErrNotLoaded. Install must
// treat this as routine (no warning) and proceed to Bootstrap as normal.
func Test_KeepaliveInstall_BootoutNotLoaded_ProceedsSilently(t *testing.T) {
	home := keepaliveHome(t)
	configFile := oneContextWithWindowConfig(t)

	fake := testutils.NewFakeController()
	fake.BootoutErr = launchd.ErrNotLoaded
	restore := session.SetController(fake)
	defer restore()

	testutils.CommandTestCase{
		Cmd:     session.Command(),
		Command: []string{"keepalive", "install", "--config", configFile},
		Assertions: []testutils.CommandAssertion{
			testutils.CommandSuccess(),
			testutils.CommandOutputContains("installed keepalive LaunchAgent"),
			func(t *testing.T, result testutils.CommandResult) {
				t.Helper()
				assert.NotContains(t, result.Stdout, "could not boot out")
			},
		},
		Env: map[string]string{"HOME": home},
	}.Run(t)

	// Bootstrap must still have been attempted even though Bootout reported "not loaded".
	calls := fake.Calls()
	require.Len(t, calls, 2)
	assert.Equal(t, "Bootstrap", calls[1].Method)
}

// Test_KeepaliveInstall_BootoutGenuineFailure_WarnsAndContinues covers review finding "install and
// uninstall unconditionally discard Bootout's error": a genuine Bootout failure (not the expected
// "not loaded" case) must surface as a warning, not be silently swallowed - but install must still
// continue on to Bootstrap, since the plist rewrite + Bootstrap may still succeed. Print (the
// disambiguation ClassifyLoadState performs) succeeds by default (FakeController.PrintErr is nil),
// i.e. reports the service still loaded (LoadStateLoaded), so the Bootout failure is confirmed
// genuine here.
func Test_KeepaliveInstall_BootoutGenuineFailure_WarnsAndContinues(t *testing.T) {
	home := keepaliveHome(t)
	configFile := oneContextWithWindowConfig(t)

	fake := testutils.NewFakeController()
	fake.BootoutErr = errors.New("permission denied")
	restore := session.SetController(fake)
	defer restore()

	testutils.CommandTestCase{
		Cmd:     session.Command(),
		Command: []string{"keepalive", "install", "--config", configFile},
		Assertions: []testutils.CommandAssertion{
			testutils.CommandSuccess(),
			testutils.CommandOutputContains("could not boot out the previous LaunchAgent instance"),
			testutils.CommandOutputContains("permission denied"),
			testutils.CommandOutputContains("installed keepalive LaunchAgent"),
		},
		Env: map[string]string{"HOME": home},
	}.Run(t)

	// Bootstrap must still have been attempted despite the Bootout warning, after Print confirmed
	// the service was still loaded (disambiguating the unrecognized Bootout error).
	calls := fake.Calls()
	require.Len(t, calls, 3)
	assert.Equal(t, "Bootout", calls[0].Method)
	assert.Equal(t, "Print", calls[1].Method)
	assert.Equal(t, "Bootstrap", calls[2].Method)
}

// Test_KeepaliveInstall_BootoutUnrecognizedNotLoaded_ProceedsSilently covers the review finding: a
// launchctl version whose Bootout failure wording isBootoutNotLoaded does not recognize (so Bootout
// returns a generic error, not ErrNotLoaded) must still be treated as benign when Print confirms the
// service is not loaded (ClassifyLoadState reports LoadStateNotLoaded) - install proceeds to
// Bootstrap without warning, exactly as it would for a directly-recognized ErrNotLoaded.
func Test_KeepaliveInstall_BootoutUnrecognizedNotLoaded_ProceedsSilently(t *testing.T) {
	home := keepaliveHome(t)
	configFile := oneContextWithWindowConfig(t)

	fake := testutils.NewFakeController()
	fake.BootoutErr = errors.New("some unrecognized launchctl wording")
	fake.PrintErr = launchd.ErrNotLoaded // Print confirms: not loaded.
	restore := session.SetController(fake)
	defer restore()

	testutils.CommandTestCase{
		Cmd:     session.Command(),
		Command: []string{"keepalive", "install", "--config", configFile},
		Assertions: []testutils.CommandAssertion{
			testutils.CommandSuccess(),
			testutils.CommandOutputContains("installed keepalive LaunchAgent"),
			func(t *testing.T, result testutils.CommandResult) {
				t.Helper()
				assert.NotContains(t, result.Stdout, "could not boot out")
			},
		},
		Env: map[string]string{"HOME": home},
	}.Run(t)

	calls := fake.Calls()
	require.Len(t, calls, 3)
	assert.Equal(t, "Bootout", calls[0].Method)
	assert.Equal(t, "Print", calls[1].Method)
	assert.Equal(t, "Bootstrap", calls[2].Method)
}

// Test_KeepaliveInstall_BootoutUnrecognizedInconclusive_WarnsDifferentlyAndContinues covers the
// third state finding's fix requires: when Bootout fails with unrecognized wording AND Print itself
// fails for an unrelated reason (permission denied, launchd unreachable, ...), ClassifyLoadState
// must report LoadStateInconclusive - install must not silently proceed as if the previous instance
// were confirmed absent (that conflation was the bug), but it also must not abort: it warns with a
// message distinct from the "still loaded" case and continues to Bootstrap.
func Test_KeepaliveInstall_BootoutUnrecognizedInconclusive_WarnsDifferentlyAndContinues(t *testing.T) {
	home := keepaliveHome(t)
	configFile := oneContextWithWindowConfig(t)

	fake := testutils.NewFakeController()
	fake.BootoutErr = errors.New("some unrecognized launchctl wording")
	fake.PrintErr = errors.New("launchd unavailable") // Neither success nor ErrNotLoaded: inconclusive.
	restore := session.SetController(fake)
	defer restore()

	testutils.CommandTestCase{
		Cmd:     session.Command(),
		Command: []string{"keepalive", "install", "--config", configFile},
		Assertions: []testutils.CommandAssertion{
			testutils.CommandSuccess(),
			testutils.CommandOutputContains("installed keepalive LaunchAgent"),
			testutils.CommandOutputContains("could not determine whether it is still loaded"),
			testutils.CommandOutputContains("some unrecognized launchctl wording"),
		},
		Env: map[string]string{"HOME": home},
	}.Run(t)

	// Bootstrap must still have been attempted despite the inconclusive warning.
	calls := fake.Calls()
	require.Len(t, calls, 3)
	assert.Equal(t, "Bootout", calls[0].Method)
	assert.Equal(t, "Print", calls[1].Method)
	assert.Equal(t, "Bootstrap", calls[2].Method)
}

func Test_KeepaliveInstall_NoWindow_Errors(t *testing.T) {
	home := keepaliveHome(t)
	configFile := noWindowConfig(t)

	fake := testutils.NewFakeController()
	restore := session.SetController(fake)
	defer restore()

	testCase := testutils.CommandTestCase{
		Cmd:     session.Command(),
		Command: []string{"keepalive", "install", "--config", configFile},
		Assertions: []testutils.CommandAssertion{
			testutils.CommandErrorContains("no context opts into keep-alive"),
		},
		Env: map[string]string{"HOME": home},
	}
	testCase.Run(t)

	assert.Empty(t, fake.Calls(), "install must not touch launchd when no context opts in")

	_, err := os.Stat(launchd.PlistPath())
	assert.Error(t, err, "no plist should be written when no context opts in")
}

func Test_KeepaliveInstall_ExplicitInterval_Bounds(t *testing.T) {
	cases := []struct {
		name     string
		interval string
		wantErr  bool
	}{
		{name: "30s rejected (below 1m)", interval: "30s", wantErr: true},
		// Genuinely exercises the "d" (day) suffix time.ParseDuration lacks - a plain
		// pflag.DurationVar (the pre-fix flag type) cannot parse either of these at all, which is
		// exactly the bug review finding 2 flagged: the previous "168h"/"144h" literals here
		// masked it.
		{name: "7d rejected (above 6d)", interval: "7d", wantErr: true},
		{name: "1m accepted (lower bound)", interval: "1m", wantErr: false},
		{name: "6d accepted (upper bound)", interval: "6d", wantErr: false},
		// An explicit, out-of-bounds zero must be rejected - not silently treated as "unset"
		// (finding 5): --interval's default is now the empty string, so "0s" is a distinguishable,
		// explicit (and invalid, since it is below the 1m floor) value.
		{name: "0s rejected (explicit zero is not \"unset\")", interval: "0s", wantErr: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			home := keepaliveHome(t)
			configFile := oneContextWithWindowConfig(t)

			fake := testutils.NewFakeController()
			restore := session.SetController(fake)
			defer restore()

			var gotErr error
			testCase := testutils.CommandTestCase{
				Cmd:     session.Command(),
				Command: []string{"keepalive", "install", "--interval", tc.interval, "--config", configFile},
				Assertions: []testutils.CommandAssertion{
					func(_ *testing.T, result testutils.CommandResult) { gotErr = result.Err },
				},
				Env: map[string]string{"HOME": home},
			}
			testCase.Run(t)

			if tc.wantErr {
				require.Error(t, gotErr)
				return
			}

			require.NoError(t, gotErr)
		})
	}
}

func Test_KeepaliveStatus_NotInstalled(t *testing.T) {
	home := keepaliveHome(t)

	fake := testutils.NewFakeController()
	fake.PrintErr = errors.New("not loaded")
	restore := session.SetController(fake)
	defer restore()

	testCase := testutils.CommandTestCase{
		Cmd:     session.Command(),
		Command: []string{"keepalive", "status"},
		Assertions: []testutils.CommandAssertion{
			testutils.CommandSuccess(),
			testutils.CommandOutputContains("installed: no"),
		},
		Env: map[string]string{"HOME": home},
	}
	testCase.Run(t)
}

func Test_KeepaliveStatus_InstalledAndLoaded(t *testing.T) {
	home := keepaliveHome(t)
	configFile := oneContextWithWindowConfig(t)

	fake := testutils.NewFakeController()
	restore := session.SetController(fake)
	defer restore()

	testutils.CommandTestCase{
		Cmd:     session.Command(),
		Command: []string{"keepalive", "install", "--config", configFile},
		Assertions: []testutils.CommandAssertion{
			testutils.CommandSuccess(),
		},
		Env: map[string]string{"HOME": home},
	}.Run(t)

	testutils.CommandTestCase{
		Cmd:     session.Command(),
		Command: []string{"keepalive", "status"},
		Assertions: []testutils.CommandAssertion{
			testutils.CommandSuccess(),
			testutils.CommandOutputContains("installed: yes"),
			testutils.CommandOutputContains("loaded:    yes"),
		},
		Env: map[string]string{"HOME": home},
	}.Run(t)
}

// Test_KeepaliveStatus_LoadedButNotInstalled covers the "stale loaded" state finding 3 flagged:
// launchctl still reports the agent as loaded (Print succeeds) even though its plist is absent
// (Installed is false, e.g. because it was deleted by hand, or bootstrapped from elsewhere). The
// text codec must surface that mismatch instead of silently rendering only "installed: no" - the
// json/yaml codecs always report Loaded regardless of Installed, so text must have parity.
func Test_KeepaliveStatus_LoadedButNotInstalled(t *testing.T) {
	home := keepaliveHome(t)

	// No install is performed, so no plist exists - but the fake controller's Print succeeds by
	// default, simulating launchd still reporting the service as loaded.
	fake := testutils.NewFakeController()
	restore := session.SetController(fake)
	defer restore()

	testutils.CommandTestCase{
		Cmd:     session.Command(),
		Command: []string{"keepalive", "status"},
		Assertions: []testutils.CommandAssertion{
			testutils.CommandSuccess(),
			testutils.CommandOutputContains("installed: no"),
			testutils.CommandOutputContains("warning:"),
			testutils.CommandOutputContains("still loaded"),
		},
		Env: map[string]string{"HOME": home},
	}.Run(t)
}

func Test_KeepaliveStatus_JSON(t *testing.T) {
	home := keepaliveHome(t)
	configFile := oneContextWithWindowConfig(t)

	fake := testutils.NewFakeController()
	restore := session.SetController(fake)
	defer restore()

	testutils.CommandTestCase{
		Cmd:     session.Command(),
		Command: []string{"keepalive", "install", "--config", configFile},
		Assertions: []testutils.CommandAssertion{
			testutils.CommandSuccess(),
		},
		Env: map[string]string{"HOME": home},
	}.Run(t)

	var stdout string
	testutils.CommandTestCase{
		Cmd:     session.Command(),
		Command: []string{"keepalive", "status", "-o", "json"},
		Assertions: []testutils.CommandAssertion{
			testutils.CommandSuccess(),
			func(_ *testing.T, result testutils.CommandResult) { stdout = result.Stdout },
		},
		Env: map[string]string{"HOME": home},
	}.Run(t)

	var decoded struct {
		Installed       bool   `json:"installed"`
		Loaded          bool   `json:"loaded"`
		IntervalSeconds int    `json:"intervalSeconds"`
		Binary          string `json:"binary"`
		PlistPath       string `json:"plistPath"`
		LogPath         string `json:"logPath"`
	}
	require.NoError(t, json.Unmarshal([]byte(stdout), &decoded))

	assert.True(t, decoded.Installed)
	assert.True(t, decoded.Loaded)
	assert.Equal(t, int(6*60*60), decoded.IntervalSeconds)
	assert.NotEmpty(t, decoded.Binary)
	assert.Equal(t, launchd.PlistPath(), decoded.PlistPath)
	assert.Equal(t, launchd.LogPath(), decoded.LogPath)
}

func Test_KeepaliveUninstall_RemovesPlistAndIsIdempotent(t *testing.T) {
	home := keepaliveHome(t)
	configFile := oneContextWithWindowConfig(t)

	fake := testutils.NewFakeController()
	restore := session.SetController(fake)
	defer restore()

	testutils.CommandTestCase{
		Cmd:     session.Command(),
		Command: []string{"keepalive", "install", "--config", configFile},
		Assertions: []testutils.CommandAssertion{
			testutils.CommandSuccess(),
		},
		Env: map[string]string{"HOME": home},
	}.Run(t)

	_, err := os.Stat(launchd.PlistPath())
	require.NoError(t, err, "sanity: the plist must exist before uninstall")

	for range 2 {
		testutils.CommandTestCase{
			Cmd:     session.Command(),
			Command: []string{"keepalive", "uninstall"},
			Assertions: []testutils.CommandAssertion{
				testutils.CommandSuccess(),
				testutils.CommandOutputContains("removed keepalive LaunchAgent"),
			},
			Env: map[string]string{"HOME": home},
		}.Run(t)
	}

	_, err = os.Stat(launchd.PlistPath())
	assert.True(t, os.IsNotExist(err), "the plist must be gone after uninstall")

	calls := fake.Calls()
	require.GreaterOrEqual(t, len(calls), 2)
	for _, call := range calls[len(calls)-2:] {
		assert.Equal(t, "Bootout", call.Method)
	}
}

// Test_KeepaliveUninstall_BootoutNotLoaded_ProceedsSilently covers the common idempotent case:
// the agent is already gone from launchd (Bootout fails with launchd.ErrNotLoaded), so uninstall
// must still remove the plist and report success without any warning.
func Test_KeepaliveUninstall_BootoutNotLoaded_ProceedsSilently(t *testing.T) {
	home := keepaliveHome(t)
	configFile := oneContextWithWindowConfig(t)

	installFake := testutils.NewFakeController()
	restore := session.SetController(installFake)
	testutils.CommandTestCase{
		Cmd:     session.Command(),
		Command: []string{"keepalive", "install", "--config", configFile},
		Assertions: []testutils.CommandAssertion{
			testutils.CommandSuccess(),
		},
		Env: map[string]string{"HOME": home},
	}.Run(t)
	restore()

	fake := testutils.NewFakeController()
	fake.BootoutErr = launchd.ErrNotLoaded
	restore = session.SetController(fake)
	defer restore()

	testutils.CommandTestCase{
		Cmd:     session.Command(),
		Command: []string{"keepalive", "uninstall"},
		Assertions: []testutils.CommandAssertion{
			testutils.CommandSuccess(),
			testutils.CommandOutputContains("removed keepalive LaunchAgent"),
		},
		Env: map[string]string{"HOME": home},
	}.Run(t)

	_, err := os.Stat(launchd.PlistPath())
	assert.True(t, os.IsNotExist(err), "the plist must be removed when Bootout reports \"not loaded\"")
}

// Test_KeepaliveUninstall_BootoutGenuineFailure_FailsAndKeepsPlist covers review finding "uninstall
// then deletes the plist and reports success even when the agent is still loaded - false success":
// a genuine Bootout failure (not the expected "not loaded" case) must fail the command, and the
// plist must be left in place so the user is not told the agent is gone when it may still be loaded.
func Test_KeepaliveUninstall_BootoutGenuineFailure_FailsAndKeepsPlist(t *testing.T) {
	home := keepaliveHome(t)
	configFile := oneContextWithWindowConfig(t)

	installFake := testutils.NewFakeController()
	restore := session.SetController(installFake)
	testutils.CommandTestCase{
		Cmd:     session.Command(),
		Command: []string{"keepalive", "install", "--config", configFile},
		Assertions: []testutils.CommandAssertion{
			testutils.CommandSuccess(),
		},
		Env: map[string]string{"HOME": home},
	}.Run(t)
	restore()

	fake := testutils.NewFakeController()
	fake.BootoutErr = errors.New("permission denied")
	restore = session.SetController(fake)
	defer restore()

	testutils.CommandTestCase{
		Cmd:     session.Command(),
		Command: []string{"keepalive", "uninstall"},
		Assertions: []testutils.CommandAssertion{
			testutils.CommandErrorContains("may still be loaded"),
			testutils.CommandErrorContains("permission denied"),
		},
		Env: map[string]string{"HOME": home},
	}.Run(t)

	_, err := os.Stat(launchd.PlistPath())
	require.NoError(t, err, "the plist must NOT be removed when Bootout fails for a genuine reason")
}

// Test_KeepaliveUninstall_BootoutUnrecognizedNotLoaded_ProceedsSilently covers the review finding:
// a launchctl version whose Bootout failure wording isBootoutNotLoaded does not recognize (Bootout
// returns a generic error, not ErrNotLoaded) must still be treated as benign - and uninstall must
// remain idempotent - when Print confirms the service is not currently loaded (ClassifyLoadState
// reports LoadStateNotLoaded).
func Test_KeepaliveUninstall_BootoutUnrecognizedNotLoaded_ProceedsSilently(t *testing.T) {
	home := keepaliveHome(t)
	configFile := oneContextWithWindowConfig(t)

	installFake := testutils.NewFakeController()
	restore := session.SetController(installFake)
	testutils.CommandTestCase{
		Cmd:     session.Command(),
		Command: []string{"keepalive", "install", "--config", configFile},
		Assertions: []testutils.CommandAssertion{
			testutils.CommandSuccess(),
		},
		Env: map[string]string{"HOME": home},
	}.Run(t)
	restore()

	fake := testutils.NewFakeController()
	fake.BootoutErr = errors.New("some unrecognized launchctl wording")
	fake.PrintErr = launchd.ErrNotLoaded // Print confirms: not loaded.
	restore = session.SetController(fake)
	defer restore()

	testutils.CommandTestCase{
		Cmd:     session.Command(),
		Command: []string{"keepalive", "uninstall"},
		Assertions: []testutils.CommandAssertion{
			testutils.CommandSuccess(),
			testutils.CommandOutputContains("removed keepalive LaunchAgent"),
		},
		Env: map[string]string{"HOME": home},
	}.Run(t)

	_, err := os.Stat(launchd.PlistPath())
	assert.True(t, os.IsNotExist(err),
		"the plist must be removed when Print confirms the service is not loaded, even though "+
			"Bootout's error used unrecognized wording")

	calls := fake.Calls()
	require.Len(t, calls, 2)
	assert.Equal(t, "Bootout", calls[0].Method)
	assert.Equal(t, "Print", calls[1].Method)
}

// Test_KeepaliveUninstall_BootoutUnrecognizedStillLoaded_FailsAndKeepsPlist covers the other side
// of the same disambiguation: when a Bootout failure with unrecognized wording is paired with Print
// confirming the service IS still loaded, uninstall must fail and keep the plist - exactly like any
// other genuine Bootout failure.
func Test_KeepaliveUninstall_BootoutUnrecognizedStillLoaded_FailsAndKeepsPlist(t *testing.T) {
	home := keepaliveHome(t)
	configFile := oneContextWithWindowConfig(t)

	installFake := testutils.NewFakeController()
	restore := session.SetController(installFake)
	testutils.CommandTestCase{
		Cmd:     session.Command(),
		Command: []string{"keepalive", "install", "--config", configFile},
		Assertions: []testutils.CommandAssertion{
			testutils.CommandSuccess(),
		},
		Env: map[string]string{"HOME": home},
	}.Run(t)
	restore()

	fake := testutils.NewFakeController()
	fake.BootoutErr = errors.New("some unrecognized launchctl wording")
	// PrintErr left nil: Print succeeds, i.e. confirms the service is still loaded.
	restore = session.SetController(fake)
	defer restore()

	testutils.CommandTestCase{
		Cmd:     session.Command(),
		Command: []string{"keepalive", "uninstall"},
		Assertions: []testutils.CommandAssertion{
			testutils.CommandErrorContains("may still be loaded"),
			testutils.CommandErrorContains("some unrecognized launchctl wording"),
		},
		Env: map[string]string{"HOME": home},
	}.Run(t)

	_, err := os.Stat(launchd.PlistPath())
	require.NoError(t, err,
		"the plist must NOT be removed when Print confirms the service is still loaded")

	calls := fake.Calls()
	require.Len(t, calls, 2)
	assert.Equal(t, "Bootout", calls[0].Method)
	assert.Equal(t, "Print", calls[1].Method)
}

// Test_KeepaliveUninstall_BootoutUnrecognizedInconclusive_FailsWithDistinctMessageAndKeepsPlist is
// the critical regression case for the false-success bug: a genuine Bootout failure with
// unrecognized wording, paired with Print ALSO failing for an unrelated reason (permission denied,
// launchd unreachable, ...) - the old ConfirmNotLoaded collapsed "Print errored" into "confirmed
// absent" here, so uninstall would delete the plist and report success even though the agent might
// still be loaded. ClassifyLoadState must report LoadStateInconclusive, and uninstall must fail
// (keeping the plist) with a message that says the state could not be determined - distinct from
// the "still loaded" wording used when Print actually confirms the service is loaded.
func Test_KeepaliveUninstall_BootoutUnrecognizedInconclusive_FailsWithDistinctMessageAndKeepsPlist(t *testing.T) {
	home := keepaliveHome(t)
	configFile := oneContextWithWindowConfig(t)

	installFake := testutils.NewFakeController()
	restore := session.SetController(installFake)
	testutils.CommandTestCase{
		Cmd:     session.Command(),
		Command: []string{"keepalive", "install", "--config", configFile},
		Assertions: []testutils.CommandAssertion{
			testutils.CommandSuccess(),
		},
		Env: map[string]string{"HOME": home},
	}.Run(t)
	restore()

	fake := testutils.NewFakeController()
	fake.BootoutErr = errors.New("some unrecognized launchctl wording")
	fake.PrintErr = errors.New("launchd unavailable") // Neither success nor ErrNotLoaded: inconclusive.
	restore = session.SetController(fake)
	defer restore()

	testutils.CommandTestCase{
		Cmd:     session.Command(),
		Command: []string{"keepalive", "uninstall"},
		Assertions: []testutils.CommandAssertion{
			testutils.CommandErrorContains("could not determine whether it is still loaded"),
			testutils.CommandErrorContains("some unrecognized launchctl wording"),
			func(t *testing.T, result testutils.CommandResult) {
				t.Helper()
				assert.NotContains(t, result.Err.Error(), "may still be loaded",
					"the inconclusive message must be distinct from the confirmed-loaded message")
			},
		},
		Env: map[string]string{"HOME": home},
	}.Run(t)

	_, err := os.Stat(launchd.PlistPath())
	require.NoError(t, err,
		"the plist must NOT be removed when the load state could not be determined")

	calls := fake.Calls()
	require.Len(t, calls, 2)
	assert.Equal(t, "Bootout", calls[0].Method)
	assert.Equal(t, "Print", calls[1].Method)
}
