// Package session (whitebox, not session_test) is intentional here: the "direct" table-driven
// test of dueContexts (and a couple of assertions on refreshContext/sortedContextNames) exercises
// unexported internals directly, per docs/plans/20260723-session-keepalive.md Task 7's testing
// strategy. The command-level integration tests in this file could equally live in an external
// _test package (they only use exported surface - session.Command/SetKeychainStore), but are kept
// here alongside the whitebox tests for a single, cohesive test file.
//
//nolint:testpackage // whitebox test: needs direct access to dueContexts/refreshContext
package session

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/grafana/grafanapi/cmd/grafanapi/fail"
	"github.com/grafana/grafanapi/internal/config"
	"github.com/grafana/grafanapi/internal/keychain"
	"github.com/grafana/grafanapi/internal/testutils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newRotateServer returns an httptest.Server implementing POST /api/user/auth-tokens/rotate: on
// status == http.StatusOK it sets a fresh grafana_session cookie, otherwise it responds with
// status and no cookie. The returned counter records how many times it was hit, so tests can
// assert whether a given context's rotation was actually attempted.
func newRotateServer(t *testing.T, status int) (*httptest.Server, *int32) {
	t.Helper()

	var calls int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&calls, 1)

		if status != http.StatusOK {
			w.WriteHeader(status)

			return
		}

		http.SetCookie(w, &http.Cookie{
			Name: config.SessionCookieName, Value: "new-cookie", Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode,
		})
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)

	return server, &calls
}

// singleContextConfig writes a temp config file with exactly one context named "default",
// pointing at server, and returns its path.
func singleContextConfig(t *testing.T, server string) string {
	t.Helper()

	return testutils.CreateTempFile(t, fmt.Sprintf(
		"current-context: default\ncontexts:\n  default:\n    grafana:\n      server: %s\n", server,
	))
}

func Test_RefreshCmd_Single_Success(t *testing.T) {
	server, calls := newRotateServer(t, http.StatusOK)

	store := testutils.NewFakeKeychainStore()
	require.NoError(t, store.Set(keychain.Account("default"), "old-cookie"))
	restore := SetKeychainStore(store)
	defer restore()

	configFile := singleContextConfig(t, server.URL)

	testCase := testutils.CommandTestCase{
		Cmd:     Command(),
		Command: []string{"refresh", "--config", configFile},
		Assertions: []testutils.CommandAssertion{
			testutils.CommandSuccess(),
			testutils.CommandOutputContains("refreshed session for context default"),
		},
	}
	testCase.Run(t)

	assert.Equal(t, int32(1), atomic.LoadInt32(calls))

	cookie, ok := store.Value(keychain.Account("default"))
	require.True(t, ok)
	assert.Equal(t, "new-cookie", cookie)
}

// Test_RefreshCmd_Single_NoCookieNeverInOutput asserts the rotated cookie value never appears in
// stdout, even on success - the plan's hard security requirement that the session cookie is
// never printed or logged.
func Test_RefreshCmd_Single_NoCookieNeverInOutput(t *testing.T) {
	server, _ := newRotateServer(t, http.StatusOK)

	store := testutils.NewFakeKeychainStore()
	require.NoError(t, store.Set(keychain.Account("default"), "old-cookie"))
	restore := SetKeychainStore(store)
	defer restore()

	configFile := singleContextConfig(t, server.URL)

	testCase := testutils.CommandTestCase{
		Cmd:     Command(),
		Command: []string{"refresh", "--config", configFile},
		Assertions: []testutils.CommandAssertion{
			testutils.CommandSuccess(),
			func(t *testing.T, result testutils.CommandResult) {
				t.Helper()
				assert.NotContains(t, result.Stdout, "new-cookie")
				assert.NotContains(t, result.Stdout, "old-cookie")
			},
		},
	}
	testCase.Run(t)
}

func Test_RefreshCmd_Single_Rejected(t *testing.T) {
	server, _ := newRotateServer(t, http.StatusUnauthorized)

	store := testutils.NewFakeKeychainStore()
	require.NoError(t, store.Set(keychain.Account("default"), "old-cookie"))
	restore := SetKeychainStore(store)
	defer restore()

	configFile := singleContextConfig(t, server.URL)

	var gotErr error
	testCase := testutils.CommandTestCase{
		Cmd:     Command(),
		Command: []string{"refresh", "--config", configFile},
		Assertions: []testutils.CommandAssertion{
			func(_ *testing.T, result testutils.CommandResult) { gotErr = result.Err },
		},
	}
	testCase.Run(t)

	require.Error(t, gotErr)

	detailedErr := fail.ErrorToDetailedError(gotErr)
	require.NotNil(t, detailedErr.ExitCode)
	assert.Equal(t, 2, *detailedErr.ExitCode)
	assert.Equal(t, "Grafana session is stale or unauthorized", detailedErr.Summary)
}

// multiContextConfig writes a temp config file with three contexts: "live" (server points at
// liveServer), "dead" (server points at deadServer), and "nocookie" (server points at liveServer
// too, but deliberately never given a stored cookie).
func multiContextConfig(t *testing.T, liveServer, deadServer string) string {
	t.Helper()

	return testutils.CreateTempFile(t, fmt.Sprintf(`current-context: live
contexts:
  live:
    grafana:
      server: %s
  dead:
    grafana:
      server: %s
  nocookie:
    grafana:
      server: %s
`, liveServer, deadServer, liveServer))
}

func Test_RefreshCmd_All_LiveDeadNoCookie(t *testing.T) {
	liveServer, liveCalls := newRotateServer(t, http.StatusOK)
	deadServer, deadCalls := newRotateServer(t, http.StatusUnauthorized)

	store := testutils.NewFakeKeychainStore()
	require.NoError(t, store.Set(keychain.Account("live"), "live-cookie"))
	require.NoError(t, store.Set(keychain.Account("dead"), "dead-cookie"))
	// "nocookie" deliberately has no stored cookie.
	restore := SetKeychainStore(store)
	defer restore()

	configFile := multiContextConfig(t, liveServer.URL, deadServer.URL)

	var gotErr error
	testCase := testutils.CommandTestCase{
		Cmd:     Command(),
		Command: []string{"refresh", "--all", "--config", configFile},
		Assertions: []testutils.CommandAssertion{
			testutils.CommandOutputContains("refreshed session for context live"),
			func(_ *testing.T, result testutils.CommandResult) { gotErr = result.Err },
		},
	}
	testCase.Run(t)

	assert.Equal(t, int32(1), atomic.LoadInt32(liveCalls), "the live context must be attempted")
	assert.Equal(t, int32(1), atomic.LoadInt32(deadCalls), "the dead context must also be attempted")

	require.Error(t, gotErr)
	detailedErr := fail.ErrorToDetailedError(gotErr)
	require.NotNil(t, detailedErr.ExitCode)
	assert.Equal(t, 2, *detailedErr.ExitCode)
}

func Test_RefreshCmd_All_NetworkOnlyFailureExitsOne(t *testing.T) {
	store := testutils.NewFakeKeychainStore()
	require.NoError(t, store.Set(keychain.Account("default"), "old-cookie"))
	restore := SetKeychainStore(store)
	defer restore()

	// Port 0 on the loopback address is never listening: every request to it fails as a network
	// (connection refused) error, not a 401.
	configFile := singleContextConfig(t, "http://127.0.0.1:0")

	var gotErr error
	testCase := testutils.CommandTestCase{
		Cmd:     Command(),
		Command: []string{"refresh", "--all", "--config", configFile},
		Assertions: []testutils.CommandAssertion{
			func(_ *testing.T, result testutils.CommandResult) { gotErr = result.Err },
		},
	}
	testCase.Run(t)

	require.Error(t, gotErr)
	detailedErr := fail.ErrorToDetailedError(gotErr)
	assert.Nil(t, detailedErr.ExitCode, "a network-only failure must not use the auth exit code (defaults to exit 1)")
}

func Test_RefreshCmd_Due_OnlyStaleRotated(t *testing.T) {
	freshServer, freshCalls := newRotateServer(t, http.StatusOK)
	staleServer, staleCalls := newRotateServer(t, http.StatusOK)
	unsetServer, unsetCalls := newRotateServer(t, http.StatusOK)

	store := testutils.NewFakeKeychainStore()
	require.NoError(t, store.Set(keychain.Account("fresh"), "fresh-cookie"))
	store.SetModified(keychain.Account("fresh"), time.Now().Add(-1*time.Minute))
	require.NoError(t, store.Set(keychain.Account("stale"), "stale-cookie"))
	store.SetModified(keychain.Account("stale"), time.Now().Add(-24*time.Hour))
	require.NoError(t, store.Set(keychain.Account("unset"), "unset-cookie"))
	restore := SetKeychainStore(store)
	defer restore()

	configFile := testutils.CreateTempFile(t, fmt.Sprintf(`current-context: fresh
contexts:
  fresh:
    grafana:
      server: %s
      live-window: 12h
  stale:
    grafana:
      server: %s
      live-window: 12h
  unset:
    grafana:
      server: %s
`, freshServer.URL, staleServer.URL, unsetServer.URL))

	testCase := testutils.CommandTestCase{
		Cmd:     Command(),
		Command: []string{"refresh", "--due", "--config", configFile},
		Assertions: []testutils.CommandAssertion{
			testutils.CommandSuccess(),
			testutils.CommandOutputContains("refreshed session for context stale"),
		},
	}
	testCase.Run(t)

	assert.Equal(t, int32(0), atomic.LoadInt32(freshCalls), "a fresh context must not be rotated")
	assert.Equal(t, int32(1), atomic.LoadInt32(staleCalls), "a stale context must be rotated")
	assert.Equal(t, int32(0), atomic.LoadInt32(unsetCalls), "a context with no live-window must not be rotated")
}

func Test_RefreshCmd_Due_DeadDueContextExitsTwo(t *testing.T) {
	staleServer, _ := newRotateServer(t, http.StatusUnauthorized)

	store := testutils.NewFakeKeychainStore()
	require.NoError(t, store.Set(keychain.Account("stale"), "stale-cookie"))
	store.SetModified(keychain.Account("stale"), time.Now().Add(-24*time.Hour))
	restore := SetKeychainStore(store)
	defer restore()

	configFile := testutils.CreateTempFile(t, fmt.Sprintf(`current-context: stale
contexts:
  stale:
    grafana:
      server: %s
      live-window: 12h
`, staleServer.URL))

	var gotErr error
	testCase := testutils.CommandTestCase{
		Cmd:     Command(),
		Command: []string{"refresh", "--due", "--config", configFile},
		Assertions: []testutils.CommandAssertion{
			func(_ *testing.T, result testutils.CommandResult) { gotErr = result.Err },
		},
	}
	testCase.Run(t)

	require.Error(t, gotErr)
	detailedErr := fail.ErrorToDetailedError(gotErr)
	require.NotNil(t, detailedErr.ExitCode)
	assert.Equal(t, 2, *detailedErr.ExitCode)
}

func Test_RefreshCmd_Due_NothingDue(t *testing.T) {
	server, calls := newRotateServer(t, http.StatusOK)

	store := testutils.NewFakeKeychainStore()
	require.NoError(t, store.Set(keychain.Account("default"), "cookie"))
	store.SetModified(keychain.Account("default"), time.Now())
	restore := SetKeychainStore(store)
	defer restore()

	configFile := testutils.CreateTempFile(t, fmt.Sprintf(`current-context: default
contexts:
  default:
    grafana:
      server: %s
      live-window: 12h
`, server.URL))

	testCase := testutils.CommandTestCase{
		Cmd:     Command(),
		Command: []string{"refresh", "--due", "--config", configFile},
		Assertions: []testutils.CommandAssertion{
			testutils.CommandSuccess(),
			testutils.CommandOutputContains("nothing due"),
		},
	}
	testCase.Run(t)

	assert.Equal(t, int32(0), atomic.LoadInt32(calls))
}

func Test_RefreshCmd_MutuallyExclusiveFlags(t *testing.T) {
	configFile := testutils.CreateTempFile(t, "contexts:\n  default: {}\n")

	cases := []struct {
		name           string
		args           []string
		errorSubstring string
	}{
		{
			name:           "all and due",
			args:           []string{"refresh", "--all", "--due", "--config", configFile},
			errorSubstring: "cannot be used together",
		},
		{
			name:           "all and context",
			args:           []string{"refresh", "--all", "--context", "default", "--config", configFile},
			errorSubstring: "cannot be combined",
		},
		{
			name:           "due and context",
			args:           []string{"refresh", "--due", "--context", "default", "--config", configFile},
			errorSubstring: "cannot be combined",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			testutils.CommandTestCase{
				Cmd:     Command(),
				Command: tc.args,
				Assertions: []testutils.CommandAssertion{
					testutils.CommandErrorContains(tc.errorSubstring),
				},
			}.Run(t)
		})
	}
}

func Test_RefreshCmd_UnknownContext(t *testing.T) {
	configFile := testutils.CreateTempFile(t, "current-context: default\ncontexts:\n  default: {}\n")

	testCase := testutils.CommandTestCase{
		Cmd:     Command(),
		Command: []string{"refresh", "--context", "missing", "--config", configFile},
		Assertions: []testutils.CommandAssertion{
			testutils.CommandErrorContains(`invalid context "missing"`),
		},
	}
	testCase.Run(t)
}

func Test_RefreshCmd_NoStoredCookie(t *testing.T) {
	configFile := testutils.CreateTempFile(t, "current-context: default\ncontexts:\n  default:\n    grafana:\n      server: http://example.invalid\n")

	store := testutils.NewFakeKeychainStore()
	restore := SetKeychainStore(store)
	defer restore()

	testCase := testutils.CommandTestCase{
		Cmd:     Command(),
		Command: []string{"refresh", "--config", configFile},
		Assertions: []testutils.CommandAssertion{
			testutils.CommandErrorContains("no stored session cookie"),
		},
	}
	testCase.Run(t)
}

// Test_DueContexts_Table is the direct, table-driven test of the pure dueContexts selector,
// covering every branch: unset, fresh, stale, invalid window (warns, skipped), and no Keychain
// item (skipped, no warning).
func Test_DueContexts_Table(t *testing.T) {
	now := time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC)

	cfg := &config.Config{
		Contexts: map[string]*config.Context{
			"no-window": {Name: "no-window", Grafana: &config.GrafanaConfig{Server: "https://a.example"}},
			"fresh":     {Name: "fresh", Grafana: &config.GrafanaConfig{Server: "https://b.example", LiveWindow: "12h"}},
			"stale":     {Name: "stale", Grafana: &config.GrafanaConfig{Server: "https://c.example", LiveWindow: "12h"}},
			"no-cookie": {Name: "no-cookie", Grafana: &config.GrafanaConfig{Server: "https://d.example", LiveWindow: "12h"}},
			"invalid":   {Name: "invalid", Grafana: &config.GrafanaConfig{Server: "https://e.example", LiveWindow: "not-a-duration"}},
			"no-config": {Name: "no-config", Grafana: nil},
		},
	}

	mtimes := map[string]time.Time{
		keychain.Account("fresh"): now.Add(-1 * time.Hour),
		keychain.Account("stale"): now.Add(-13 * time.Hour),
	}

	modAt := func(account string) (time.Time, error) {
		mtime, ok := mtimes[account]
		if !ok {
			return time.Time{}, keychain.ErrNotFound
		}

		return mtime, nil
	}

	due, warnings := dueContexts(cfg, now, modAt)

	dueNames := make([]string, 0, len(due))
	for _, gCtx := range due {
		dueNames = append(dueNames, gCtx.Name)
	}

	assert.Equal(t, []string{"stale"}, dueNames)
	require.Len(t, warnings, 1)
	assert.Contains(t, warnings[0], `"invalid"`)
}

func Test_SortedContextNames(t *testing.T) {
	cfg := &config.Config{
		Contexts: map[string]*config.Context{
			"zeta":  {Name: "zeta"},
			"alpha": {Name: "alpha"},
			"mid":   {Name: "mid"},
		},
	}

	assert.Equal(t, []string{"alpha", "mid", "zeta"}, sortedContextNames(cfg))
}
