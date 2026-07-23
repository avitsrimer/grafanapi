package config_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/grafana/grafanapi/cmd/grafanapi/config"
	"github.com/grafana/grafanapi/internal/keychain"
	"github.com/grafana/grafanapi/internal/launchd"
	"github.com/grafana/grafanapi/internal/testutils"
	"github.com/stretchr/testify/require"
)

// fakeKeychainStore is an in-memory keychain.Store used to test credential resolution wired into
// Options.LoadConfig/LoadRESTConfig without touching the real platform Keychain.
type fakeKeychainStore struct {
	cookies     map[string]string
	errOnGet    error
	errOnDelete error
}

func (f *fakeKeychainStore) Set(account, secret string) error {
	if f.cookies == nil {
		f.cookies = map[string]string{}
	}

	f.cookies[account] = secret

	return nil
}

func (f *fakeKeychainStore) Get(account string) (string, error) {
	if f.errOnGet != nil {
		return "", f.errOnGet
	}

	cookie, ok := f.cookies[account]
	if !ok {
		return "", keychain.ErrNotFound
	}

	return cookie, nil
}

func (f *fakeKeychainStore) Delete(account string) error {
	if f.errOnDelete != nil {
		return f.errOnDelete
	}

	delete(f.cookies, account)

	return nil
}

// ModifiedAt is a trivial stub satisfying the grown keychain.Store interface: these tests never
// exercise last-rotation-time lookups.
func (f *fakeKeychainStore) ModifiedAt(string) (time.Time, error) {
	return time.Time{}, keychain.ErrNotFound
}

func Test_CurrentContextCommand(t *testing.T) {
	testCase := testutils.CommandTestCase{
		Cmd:     config.Command(),
		Command: []string{"current-context", "--config", "testdata/config.yaml"},
		Assertions: []testutils.CommandAssertion{
			testutils.CommandSuccess(),
			testutils.CommandOutputContains("local"),
		},
	}

	testCase.Run(t)
}

func Test_UseContextCommand(t *testing.T) {
	cfg := `current-context: old
contexts:
  old: {}
  new: {}`

	configFile := testutils.CreateTempFile(t, cfg)

	initialConfigTest := testutils.CommandTestCase{
		Cmd:     config.Command(),
		Command: []string{"current-context", "--config", configFile},
		Assertions: []testutils.CommandAssertion{
			testutils.CommandSuccess(),
			testutils.CommandOutputContains("old"),
		},
	}
	initialConfigTest.Run(t)

	changeConfigTest := testutils.CommandTestCase{
		Cmd:     config.Command(),
		Command: []string{"use-context", "--config", configFile, "new"},
		Assertions: []testutils.CommandAssertion{
			testutils.CommandSuccess(),
			testutils.CommandOutputContains("Context set to \"new\""),
		},
	}
	changeConfigTest.Run(t)

	newConfigTest := testutils.CommandTestCase{
		Cmd:     config.Command(),
		Command: []string{"current-context", "--config", configFile},
		Assertions: []testutils.CommandAssertion{
			testutils.CommandSuccess(),
			testutils.CommandOutputContains("new"),
		},
	}
	newConfigTest.Run(t)
}

func Test_UseContextCommand_withUnknownContext(t *testing.T) {
	testCase := testutils.CommandTestCase{
		Cmd:     config.Command(),
		Command: []string{"use-context", "--config", "testdata/config.yaml", "unknown-context"},
		Assertions: []testutils.CommandAssertion{
			testutils.CommandErrorContains("invalid context \"unknown-context\": context not found"),
		},
	}
	testCase.Run(t)
}

func Test_ViewCommand(t *testing.T) {
	testCase := testutils.CommandTestCase{
		Cmd:     config.Command(),
		Command: []string{"view", "--config", "testdata/config.yaml"},
		Assertions: []testutils.CommandAssertion{
			testutils.CommandSuccess(),
			testutils.CommandOutputContains(`contexts:
  local:
    grafana:
      server: http://localhost:3000/
  prod:
    grafana:
      server: https://grafana.example.com/
current-context: local`),
		},
	}

	testCase.Run(t)
}

func Test_ViewCommand_raw(t *testing.T) {
	testCase := testutils.CommandTestCase{
		Cmd:     config.Command(),
		Command: []string{"view", "--config", "testdata/config.yaml", "--raw"},
		Assertions: []testutils.CommandAssertion{
			testutils.CommandSuccess(),
			testutils.CommandOutputContains(`contexts:
  local:
    grafana:
      server: http://localhost:3000/
  prod:
    grafana:
      server: https://grafana.example.com/
current-context: local`),
		},
	}

	testCase.Run(t)
}

func Test_ViewCommand_minify(t *testing.T) {
	testCase := testutils.CommandTestCase{
		Cmd:     config.Command(),
		Command: []string{"view", "--config", "testdata/config.yaml", "--minify"},
		Assertions: []testutils.CommandAssertion{
			testutils.CommandSuccess(),
			testutils.CommandOutputContains(`contexts:
  local:
    grafana:
      server: http://localhost:3000/
current-context: local`),
		},
	}

	testCase.Run(t)
}

func Test_ViewCommand_minify_explicitContext(t *testing.T) {
	testCase := testutils.CommandTestCase{
		Cmd:     config.Command(),
		Command: []string{"view", "--config", "testdata/config.yaml", "--minify", "--context", "prod"},
		Assertions: []testutils.CommandAssertion{
			testutils.CommandSuccess(),
			testutils.CommandOutputContains(`contexts:
  prod:
    grafana:
      server: https://grafana.example.com/
current-context: prod`),
		},
	}

	testCase.Run(t)
}

func Test_ViewCommand_outputJson(t *testing.T) {
	testCase := testutils.CommandTestCase{
		Cmd:     config.Command(),
		Command: []string{"view", "--config", "testdata/config.yaml", "-o", "json"},
		Assertions: []testutils.CommandAssertion{
			testutils.CommandSuccess(),
			testutils.CommandOutputContains(`{
  "contexts": {
    "local": {
      "grafana": {
        "server": "http://localhost:3000/"
      }
    },
    "prod": {
      "grafana": {
        "server": "https://grafana.example.com/"
      }
    }
  },
  "current-context": "local"
}`),
		},
	}

	testCase.Run(t)
}

func Test_ViewCommand_failsWithNonExistentConfigFile(t *testing.T) {
	testCase := testutils.CommandTestCase{
		Cmd:     config.Command(),
		Command: []string{"view", "--config", "does-not-exist.yaml"},
		Assertions: []testutils.CommandAssertion{
			testutils.CommandErrorContains("no such file or directory"),
		},
	}

	testCase.Run(t)
}

func Test_ViewCommand_failsWithUnknownContext(t *testing.T) {
	testCase := testutils.CommandTestCase{
		Cmd:     config.Command(),
		Command: []string{"view", "--config", "testdata/config.yaml", "--context", "unknown-context"},
		Assertions: []testutils.CommandAssertion{
			testutils.CommandErrorContains("invalid context \"unknown-context\": context not found"),
		},
	}
	testCase.Run(t)
}

func Test_SetCommand(t *testing.T) {
	cfg := `current-context: dev`

	configFile := testutils.CreateTempFile(t, cfg)

	changeConfigTest := testutils.CommandTestCase{
		Cmd:     config.Command(),
		Command: []string{"set", "--config", configFile, "contexts.dev.grafana.server", "https://grafana-dev.example"},
		Assertions: []testutils.CommandAssertion{
			testutils.CommandSuccess(),
		},
	}
	changeConfigTest.Run(t)

	viewCmd := testutils.CommandTestCase{
		Cmd:     config.Command(),
		Command: []string{"view", "--config", configFile, "--minify"},
		Assertions: []testutils.CommandAssertion{
			testutils.CommandSuccess(),
			testutils.CommandOutputContains(`contexts:
  dev:
    grafana:
      server: https://grafana-dev.example
current-context: dev`),
		},
	}
	viewCmd.Run(t)
}

func Test_UnsetCommand(t *testing.T) {
	cfg := `contexts:
  dev:
    grafana:
      server: https://grafana-dev.example
      org-id: 99
current-context: dev`

	configFile := testutils.CreateTempFile(t, cfg)

	changeConfigTest := testutils.CommandTestCase{
		Cmd:     config.Command(),
		Command: []string{"unset", "--config", configFile, "contexts.dev.grafana.org-id"},
		Assertions: []testutils.CommandAssertion{
			testutils.CommandSuccess(),
		},
	}
	changeConfigTest.Run(t)

	viewCmd := testutils.CommandTestCase{
		Cmd:     config.Command(),
		Command: []string{"view", "--config", configFile, "--minify"},
		Assertions: []testutils.CommandAssertion{
			testutils.CommandSuccess(),
			testutils.CommandOutputContains(`contexts:
  dev:
    grafana:
      server: https://grafana-dev.example
current-context: dev`),
		},
	}
	viewCmd.Run(t)
}

func Test_UnsetCommand_removesEntireContextFromKeychain(t *testing.T) {
	cfg := `contexts:
  dev:
    grafana:
      server: https://grafana-dev.example
      org-id: 99
current-context: dev`

	configFile := testutils.CreateTempFile(t, cfg)

	store := &fakeKeychainStore{cookies: map[string]string{
		keychain.Account("dev"): "cookie-value",
	}}
	restore := config.SetKeychainStore(store)
	defer restore()

	testCase := testutils.CommandTestCase{
		Cmd:     config.Command(),
		Command: []string{"unset", "--config", configFile, "contexts.dev"},
		Assertions: []testutils.CommandAssertion{
			testutils.CommandSuccess(),
		},
	}
	testCase.Run(t)

	_, err := store.Get(keychain.Account("dev"))
	require.ErrorIs(t, err, keychain.ErrNotFound, "unsetting an entire context must also purge its Keychain item")
}

func Test_UnsetCommand_nestedFieldDoesNotTouchKeychain(t *testing.T) {
	cfg := `contexts:
  dev:
    grafana:
      server: https://grafana-dev.example
      org-id: 99
current-context: dev`

	configFile := testutils.CreateTempFile(t, cfg)

	store := &fakeKeychainStore{cookies: map[string]string{
		keychain.Account("dev"): "cookie-value",
	}}
	restore := config.SetKeychainStore(store)
	defer restore()

	testCase := testutils.CommandTestCase{
		Cmd:     config.Command(),
		Command: []string{"unset", "--config", configFile, "contexts.dev.grafana.org-id"},
		Assertions: []testutils.CommandAssertion{
			testutils.CommandSuccess(),
		},
	}
	testCase.Run(t)

	cookie, err := store.Get(keychain.Account("dev"))
	require.NoError(t, err, "unsetting a nested field must not touch the Keychain item for the context")
	require.Equal(t, "cookie-value", cookie)
}

func Test_UnsetCommand_keychainDeleteFailureDoesNotFailCommand(t *testing.T) {
	cfg := `contexts:
  dev:
    grafana:
      server: https://grafana-dev.example
current-context: dev`

	configFile := testutils.CreateTempFile(t, cfg)

	store := &fakeKeychainStore{errOnDelete: errors.New("keychain: unsupported platform")}
	restore := config.SetKeychainStore(store)
	defer restore()

	testCase := testutils.CommandTestCase{
		Cmd:     config.Command(),
		Command: []string{"unset", "--config", configFile, "contexts.dev"},
		Assertions: []testutils.CommandAssertion{
			testutils.CommandSuccess(),
		},
	}
	testCase.Run(t)
}

func Test_ViewCommand_withEnvironmentVariables(t *testing.T) {
	testCase := testutils.CommandTestCase{
		Cmd:     config.Command(),
		Command: []string{"view", "--config", "testdata/partial-config.yaml", "--minify", "--raw"},
		Assertions: []testutils.CommandAssertion{
			testutils.CommandSuccess(),
			testutils.CommandOutputEquals(`contexts:
  prod:
    grafana:
      server: https://grafana.example.com/
      org-id: 84
current-context: prod
`),
		},
		// NOTE(Task 3): GRAFANA_TOKEN was removed along with GrafanaConfig.APIToken
		// (session-cookie auth replaces it and is never settable via env var); this
		// test now exercises GRAFANA_ORG_ID instead to keep coverage of env-var
		// overrides applying on top of a partial config file.
		Env: map[string]string{
			"GRAFANA_ORG_ID": "84",
		},
	}

	testCase.Run(t)
}

func Test_ViewCommand_withEnvVar(t *testing.T) {
	testCase := testutils.CommandTestCase{
		Cmd:     config.Command(),
		Command: []string{"view", "--minify"},
		Assertions: []testutils.CommandAssertion{
			testutils.CommandSuccess(),
			testutils.CommandOutputContains("local"),
			testutils.CommandOutputContains("http://localhost:3000/"),
		},
		Env: map[string]string{
			"GRAFANAPI_CONFIG": "testdata/config.yaml",
		},
	}

	testCase.Run(t)
}

func Test_LoadConfig_resolvesSessionCookieFromKeychain(t *testing.T) {
	cfg := `current-context: dev
contexts:
  dev:
    grafana:
      server: https://grafana-dev.example
      org-id: 99`

	configFile := testutils.CreateTempFile(t, cfg)

	store := &fakeKeychainStore{cookies: map[string]string{
		keychain.Account("dev"): "cookie-value",
	}}
	restore := config.SetKeychainStore(store)
	defer restore()

	opts := &config.Options{ConfigFile: configFile}
	loaded, err := opts.LoadConfig(context.Background())
	require.NoError(t, err)
	require.Equal(t, "cookie-value", loaded.GetCurrentContext().Grafana.SessionCookie)
}

func Test_LoadConfig_missingKeychainItemLeavesCookieEmpty(t *testing.T) {
	cfg := `current-context: dev
contexts:
  dev:
    grafana:
      server: https://grafana-dev.example
      org-id: 99`

	configFile := testutils.CreateTempFile(t, cfg)

	store := &fakeKeychainStore{}
	restore := config.SetKeychainStore(store)
	defer restore()

	opts := &config.Options{ConfigFile: configFile}
	loaded, err := opts.LoadConfig(context.Background())
	require.NoError(t, err)
	require.Empty(t, loaded.GetCurrentContext().Grafana.SessionCookie)
}

func Test_LoadConfig_keychainErrorSurfacesAsLoadError(t *testing.T) {
	cfg := `current-context: dev
contexts:
  dev:
    grafana:
      server: https://grafana-dev.example
      org-id: 99`

	configFile := testutils.CreateTempFile(t, cfg)

	store := &fakeKeychainStore{errOnGet: errors.New("keychain: boom")}
	restore := config.SetKeychainStore(store)
	defer restore()

	opts := &config.Options{ConfigFile: configFile}
	_, err := opts.LoadConfig(context.Background())
	require.ErrorContains(t, err, "keychain: boom")
}

func Test_LoadConfig_resolvesCookieForFlagSelectedContext(t *testing.T) {
	cfg := `current-context: dev
contexts:
  dev:
    grafana:
      server: https://grafana-dev.example
      org-id: 99
  staging:
    grafana:
      server: https://grafana-staging.example
      org-id: 100`

	configFile := testutils.CreateTempFile(t, cfg)

	store := &fakeKeychainStore{cookies: map[string]string{
		keychain.Account("staging"): "staging-cookie",
	}}
	restore := config.SetKeychainStore(store)
	defer restore()

	// --context selects "staging" even though "dev" is the file's current-context; the resolved
	// cookie (and validation) must apply to "staging", not "dev".
	opts := &config.Options{ConfigFile: configFile, Context: "staging"}
	loaded, err := opts.LoadConfig(context.Background())
	require.NoError(t, err)
	require.Equal(t, "staging", loaded.CurrentContext)
	require.Equal(t, "staging-cookie", loaded.GetCurrentContext().Grafana.SessionCookie)
}

func Test_LoadRESTConfig_resolvesSessionCookieFromKeychain(t *testing.T) {
	cfg := `current-context: dev
contexts:
  dev:
    grafana:
      server: https://grafana-dev.example
      org-id: 99`

	configFile := testutils.CreateTempFile(t, cfg)

	store := &fakeKeychainStore{cookies: map[string]string{
		keychain.Account("dev"): "cookie-value",
	}}
	restore := config.SetKeychainStore(store)
	defer restore()

	opts := &config.Options{ConfigFile: configFile}
	restCfg, err := opts.LoadRESTConfig(context.Background())
	require.NoError(t, err)
	require.NotNil(t, restCfg.WrapTransport)
}

func Test_CheckCommand_reportsKeychainErrorPerContext(t *testing.T) {
	cfg := `current-context: dev
contexts:
  dev:
    grafana:
      server: https://grafana-dev.example
      org-id: 99`

	configFile := testutils.CreateTempFile(t, cfg)

	store := &fakeKeychainStore{errOnGet: errors.New("keychain: boom")}
	restore := config.SetKeychainStore(store)
	defer restore()

	testCase := testutils.CommandTestCase{
		Cmd:     config.Command(),
		Command: []string{"check", "--config", configFile},
		Assertions: []testutils.CommandAssertion{
			testutils.CommandSuccess(),
			testutils.CommandOutputContains("Session cookie"),
			testutils.CommandOutputContains("keychain: boom"),
		},
	}
	testCase.Run(t)
}

// checkHome points $HOME at a fresh temporary directory for the duration of the test, so
// internal/launchd's path helpers (PlistPath, etc., all derived from $HOME) never touch the real
// ~/Library/LaunchAgents. It returns the directory.
func checkHome(t *testing.T) string {
	t.Helper()

	home := t.TempDir()
	t.Setenv("HOME", home)

	return home
}

// writeKeepAlivePlist writes spec as a valid keep-alive plist at launchd.PlistPath() (which must
// already resolve under a test-controlled $HOME - see checkHome), so "config check" observes an
// "installed" LaunchAgent without ever running "session keepalive install".
func writeKeepAlivePlist(t *testing.T, spec launchd.AgentSpec) {
	t.Helper()

	require.NoError(t, os.MkdirAll(filepath.Dir(launchd.PlistPath()), 0o755))

	file, err := os.Create(launchd.PlistPath())
	require.NoError(t, err)
	defer file.Close()

	require.NoError(t, launchd.Generate(file, spec))
}

func Test_CheckCommand_KeepAlive_NotInstalled(t *testing.T) {
	home := checkHome(t)

	cfg := `current-context: dev
contexts:
  dev:
    grafana:
      server: https://grafana-dev.example`

	configFile := testutils.CreateTempFile(t, cfg)

	fakeController := testutils.NewFakeController()
	restoreController := config.SetKeepaliveController(fakeController)
	defer restoreController()

	store := &fakeKeychainStore{}
	restoreStore := config.SetKeychainStore(store)
	defer restoreStore()

	testCase := testutils.CommandTestCase{
		Cmd:     config.Command(),
		Command: []string{"check", "--config", configFile},
		Assertions: []testutils.CommandAssertion{
			testutils.CommandSuccess(),
			testutils.CommandOutputContains("Keep-alive"),
			testutils.CommandOutputContains("not installed"),
			testutils.CommandOutputContains("live-window=not set"),
		},
		Env: map[string]string{"HOME": home},
	}
	testCase.Run(t)

	// No launchctl call is made when the plist does not exist.
	require.Empty(t, fakeController.Calls())
}

func Test_CheckCommand_KeepAlive_InstalledAndLoaded(t *testing.T) {
	home := checkHome(t)

	spec := launchd.DefaultAgentSpec("/opt/homebrew/bin/grafanapi", 6*time.Hour)
	writeKeepAlivePlist(t, spec)

	cfg := `current-context: dev
contexts:
  dev:
    grafana:
      server: https://grafana-dev.example
      live-window: 12h`

	configFile := testutils.CreateTempFile(t, cfg)

	fakeController := testutils.NewFakeController()
	restoreController := config.SetKeepaliveController(fakeController)
	defer restoreController()

	store := &fakeKeychainStore{}
	restoreStore := config.SetKeychainStore(store)
	defer restoreStore()

	testCase := testutils.CommandTestCase{
		Cmd:     config.Command(),
		Command: []string{"check", "--config", configFile},
		Assertions: []testutils.CommandAssertion{
			testutils.CommandSuccess(),
			testutils.CommandOutputContains("Keep-alive"),
			testutils.CommandOutputContains("installed"),
			testutils.CommandOutputContains("loaded: yes"),
			testutils.CommandOutputContains("6h0m0s"),
			testutils.CommandOutputContains("/opt/homebrew/bin/grafanapi"),
			testutils.CommandOutputContains("live-window=12h"),
		},
		Env: map[string]string{"HOME": home},
	}
	testCase.Run(t)

	calls := fakeController.Calls()
	require.Len(t, calls, 1)
	require.Equal(t, "Print", calls[0].Method)
	require.Equal(t, launchd.UserServiceTarget(), calls[0].Target)
}

func Test_CheckCommand_KeepAlive_NotLoaded(t *testing.T) {
	home := checkHome(t)

	spec := launchd.DefaultAgentSpec("/opt/homebrew/bin/grafanapi", 6*time.Hour)
	writeKeepAlivePlist(t, spec)

	cfg := `current-context: dev
contexts:
  dev:
    grafana:
      server: https://grafana-dev.example`

	configFile := testutils.CreateTempFile(t, cfg)

	fakeController := testutils.NewFakeController()
	fakeController.PrintErr = errors.New("not loaded")
	restoreController := config.SetKeepaliveController(fakeController)
	defer restoreController()

	store := &fakeKeychainStore{}
	restoreStore := config.SetKeychainStore(store)
	defer restoreStore()

	testCase := testutils.CommandTestCase{
		Cmd:     config.Command(),
		Command: []string{"check", "--config", configFile},
		Assertions: []testutils.CommandAssertion{
			testutils.CommandSuccess(),
			testutils.CommandOutputContains("loaded: no"),
		},
		Env: map[string]string{"HOME": home},
	}
	testCase.Run(t)
}

func Test_CheckCommand_KeepAlive_LastRotationAge(t *testing.T) {
	home := checkHome(t)

	cfg := `current-context: dev
contexts:
  dev:
    grafana:
      server: https://grafana-dev.example
      live-window: 12h`

	configFile := testutils.CreateTempFile(t, cfg)

	fakeController := testutils.NewFakeController()
	restoreController := config.SetKeepaliveController(fakeController)
	defer restoreController()

	fakeStore := testutils.NewFakeKeychainStore()
	require.NoError(t, fakeStore.Set(keychain.Account("dev"), "cookie-value"))
	fakeStore.SetModified(keychain.Account("dev"), time.Now().Add(-3*time.Hour))
	restoreStore := config.SetKeychainStore(fakeStore)
	defer restoreStore()

	testCase := testutils.CommandTestCase{
		Cmd:     config.Command(),
		Command: []string{"check", "--config", configFile},
		Assertions: []testutils.CommandAssertion{
			testutils.CommandSuccess(),
			testutils.CommandOutputContains("live-window=12h"),
			testutils.CommandOutputContains("rotated 3h0m0s ago"),
		},
		Env: map[string]string{"HOME": home},
	}
	testCase.Run(t)
}

func Test_CheckCommand_KeepAlive_NoStoredCookieRendersDash(t *testing.T) {
	home := checkHome(t)

	cfg := `current-context: dev
contexts:
  dev:
    grafana:
      server: https://grafana-dev.example`

	configFile := testutils.CreateTempFile(t, cfg)

	fakeController := testutils.NewFakeController()
	restoreController := config.SetKeepaliveController(fakeController)
	defer restoreController()

	store := &fakeKeychainStore{}
	restoreStore := config.SetKeychainStore(store)
	defer restoreStore()

	testCase := testutils.CommandTestCase{
		Cmd:     config.Command(),
		Command: []string{"check", "--config", configFile},
		Assertions: []testutils.CommandAssertion{
			testutils.CommandSuccess(),
			testutils.CommandOutputContains("live-window=not set, last-rotation=—"),
		},
		Env: map[string]string{"HOME": home},
	}
	testCase.Run(t)
}

func Test_CheckCommand_KeepAlive_InspectionErrorDegradesToUnknown(t *testing.T) {
	home := checkHome(t)

	// Write a malformed (truncated, non-XML) plist so os.Stat sees it as installed but
	// launchd.Inspect fails to parse it.
	require.NoError(t, os.MkdirAll(filepath.Dir(launchd.PlistPath()), 0o755))
	require.NoError(t, os.WriteFile(launchd.PlistPath(), []byte("not a plist"), 0o600))

	cfg := `current-context: dev
contexts:
  dev:
    grafana:
      server: https://grafana-dev.example`

	configFile := testutils.CreateTempFile(t, cfg)

	fakeController := testutils.NewFakeController()
	restoreController := config.SetKeepaliveController(fakeController)
	defer restoreController()

	store := &fakeKeychainStore{}
	restoreStore := config.SetKeychainStore(store)
	defer restoreStore()

	testCase := testutils.CommandTestCase{
		Cmd:     config.Command(),
		Command: []string{"check", "--config", configFile},
		Assertions: []testutils.CommandAssertion{
			testutils.CommandSuccess(),
			testutils.CommandOutputContains("could not be inspected"),
			testutils.CommandOutputContains("interval: unknown"),
			testutils.CommandOutputContains("binary: unknown"),
		},
		Env: map[string]string{"HOME": home},
	}
	testCase.Run(t)
}

func Test_ViewCommand_withEnvironmentVariablesAndEmptyConfig(t *testing.T) {
	configFile := testutils.CreateTempFile(t, "contexts:")

	testCase := testutils.CommandTestCase{
		Cmd:     config.Command(),
		Command: []string{"view", "--config", configFile, "--minify", "--raw"},
		Assertions: []testutils.CommandAssertion{
			testutils.CommandSuccess(),
			testutils.CommandOutputEquals(`contexts:
  default:
    grafana:
      server: https://grafana.example.com/
      org-id: 7
current-context: default
`),
		},
		// NOTE(Task 3): GRAFANA_TOKEN was removed along with GrafanaConfig.APIToken;
		// GRAFANA_ORG_ID exercises the same "env vars populate an empty config"
		// behavior with a still-supported field.
		Env: map[string]string{
			"GRAFANA_SERVER": "https://grafana.example.com/",
			"GRAFANA_ORG_ID": "7",
		},
	}

	testCase.Run(t)
}
