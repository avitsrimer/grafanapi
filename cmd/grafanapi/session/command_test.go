package session_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/grafana/grafanapi/cmd/grafanapi/fail"
	"github.com/grafana/grafanapi/cmd/grafanapi/session"
	"github.com/grafana/grafanapi/internal/keychain"
	"github.com/grafana/grafanapi/internal/testutils"
	"github.com/stretchr/testify/require"
)

// newRotateServer scripts the single endpoint `session keepalive` drives:
// POST /api/user/auth-tokens/rotate. It answers status; on 200 it also sets a fresh
// grafana_session cookie ("rotated-N", N = number of rotate calls so far) so tests can assert the
// rotated value was persisted. The counter is atomic since handlers run on the server's own
// goroutines.
func newRotateServer(t *testing.T, status int) (*httptest.Server, *atomic.Int64) {
	t.Helper()

	calls := &atomic.Int64{}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/user/auth-tokens/rotate", func(w http.ResponseWriter, _ *http.Request) {
		n := calls.Add(1)

		if status == http.StatusOK {
			http.SetCookie(w, &http.Cookie{Name: "grafana_session", Value: "rotated-" + string(rune('0'+n))})
		}

		w.WriteHeader(status)
	})

	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	return server, calls
}

// newTestConfig writes a temp config file with two contexts ("one", "two") pointing at server,
// seeds the fake keychain with a session cookie for each, and installs the fake store as the
// session package's keychain seam.
func newTestConfig(t *testing.T, server *httptest.Server) (string, *testutils.FakeKeychainStore) {
	t.Helper()

	configFile := testutils.CreateTempFile(t, `
current-context: one
contexts:
  one:
    grafana:
      server: `+server.URL+`
      org-id: 1
  two:
    grafana:
      server: `+server.URL+`
      org-id: 1
`)

	store := testutils.NewFakeKeychainStore()
	require.NoError(t, store.Set(keychain.Account("one"), "stale-cookie-one"))
	require.NoError(t, store.Set(keychain.Account("two"), "stale-cookie-two"))
	t.Cleanup(session.SetKeychainStore(store))

	return configFile, store
}

func Test_KeepaliveCommand_rotatesEveryContext(t *testing.T) {
	server, calls := newRotateServer(t, http.StatusOK)
	configFile, store := newTestConfig(t, server)

	testCase := testutils.CommandTestCase{
		Cmd:     session.Command(),
		Command: []string{"keepalive", "--config", configFile},
		Assertions: []testutils.CommandAssertion{
			testutils.CommandSuccess(),
			testutils.CommandOutputContains("one"),
			testutils.CommandOutputContains("two"),
			testutils.CommandOutputContains("session rotated"),
		},
	}

	testCase.Run(t)

	require.Equal(t, int64(2), calls.Load(), "expected one rotate call per context")

	one, ok := store.Value(keychain.Account("one"))
	require.True(t, ok)
	require.Contains(t, one, "rotated-")

	two, ok := store.Value(keychain.Account("two"))
	require.True(t, ok)
	require.Contains(t, two, "rotated-")
}

func Test_KeepaliveCommand_staleSessionFailsWithExitCode2(t *testing.T) {
	server, _ := newRotateServer(t, http.StatusUnauthorized)
	configFile, _ := newTestConfig(t, server)

	testCase := testutils.CommandTestCase{
		Cmd:     session.Command(),
		Command: []string{"keepalive", "--config", configFile},
		Assertions: []testutils.CommandAssertion{
			testutils.CommandErrorContains("unauthorized"),
			testutils.CommandOutputContains("session is stale"),
			testutils.CommandOutputContains("login update --context one"),
		},
	}

	testCase.Run(t)
}

func Test_KeepaliveCommand_staleSessionMapsToStaleDetailedError(t *testing.T) {
	server, _ := newRotateServer(t, http.StatusUnauthorized)
	configFile, _ := newTestConfig(t, server)

	cmd := session.Command()
	cmd.SetOut(io.Discard)
	cmd.SetArgs([]string{"keepalive", "--config", configFile})

	err := cmd.Execute()
	require.Error(t, err)

	detailed := fail.ErrorToDetailedError(err)
	require.Contains(t, detailed.Summary, "stale")
	require.NotNil(t, detailed.ExitCode)
	require.Equal(t, 2, *detailed.ExitCode)
}

func Test_KeepaliveCommand_notLoggedInContextIsSkippedNotFailed(t *testing.T) {
	server, calls := newRotateServer(t, http.StatusOK)
	configFile, _ := newTestConfig(t, server)

	// Drop context "two"'s cookie: it becomes a never-logged-in context.
	store := testutils.NewFakeKeychainStore()
	require.NoError(t, store.Set(keychain.Account("one"), "stale-cookie-one"))
	t.Cleanup(session.SetKeychainStore(store))

	testCase := testutils.CommandTestCase{
		Cmd:     session.Command(),
		Command: []string{"keepalive", "--config", configFile},
		Assertions: []testutils.CommandAssertion{
			testutils.CommandSuccess(),
			testutils.CommandOutputContains("Context one: session rotated"),
			testutils.CommandOutputContains("Context two: skipped (not logged in"),
		},
	}

	testCase.Run(t)

	require.Equal(t, int64(1), calls.Load(), "only the logged-in context should rotate")
}

func Test_KeepaliveCommand_contextFlagSelectsSingleContext(t *testing.T) {
	server, calls := newRotateServer(t, http.StatusOK)
	configFile, _ := newTestConfig(t, server)

	testCase := testutils.CommandTestCase{
		Cmd:     session.Command(),
		Command: []string{"keepalive", "--config", configFile, "--context", "two"},
		Assertions: []testutils.CommandAssertion{
			testutils.CommandSuccess(),
			testutils.CommandOutputContains("Context two: session rotated"),
		},
	}

	testCase.Run(t)

	require.Equal(t, int64(1), calls.Load())
}

func Test_KeepaliveCommand_unknownContextFlagErrors(t *testing.T) {
	server, _ := newRotateServer(t, http.StatusOK)
	configFile, _ := newTestConfig(t, server)

	testCase := testutils.CommandTestCase{
		Cmd:     session.Command(),
		Command: []string{"keepalive", "--config", configFile, "--context", "nope"},
		Assertions: []testutils.CommandAssertion{
			testutils.CommandErrorContains("nope"),
		},
	}

	testCase.Run(t)
}
