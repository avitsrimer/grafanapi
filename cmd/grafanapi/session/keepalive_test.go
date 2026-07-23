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
		{name: "7d rejected (above 6d)", interval: "168h", wantErr: true},
		{name: "1m accepted (lower bound)", interval: "1m", wantErr: false},
		{name: "6d accepted (upper bound)", interval: "144h", wantErr: false},
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
