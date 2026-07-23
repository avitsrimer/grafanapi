package login_test

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/grafana/grafanapi/cmd/grafanapi/fail"
	"github.com/grafana/grafanapi/cmd/grafanapi/login"
	"github.com/grafana/grafanapi/internal/keychain"
	"github.com/grafana/grafanapi/internal/testutils"
	"github.com/stretchr/testify/require"
)

// fakeKeychainStore is an in-memory keychain.Store used to assert what login persists (or
// doesn't persist) without touching the real platform Keychain. setErr, when non-nil, is
// returned by Set instead of storing the cookie, so tests can exercise the "keychain write
// failed" error path. getErr, when non-nil, is returned by Get instead of the usual lookup, so
// tests can simulate a transient keychain failure (as opposed to keychain.ErrNotFound) when login
// checks for a prior cookie before overwriting it.
type fakeKeychainStore struct {
	cookies map[string]string
	setErr  error
	getErr  error
}

func (f *fakeKeychainStore) Set(account, secret string) error {
	if f.setErr != nil {
		return f.setErr
	}

	if f.cookies == nil {
		f.cookies = map[string]string{}
	}

	f.cookies[account] = secret

	return nil
}

func (f *fakeKeychainStore) Get(account string) (string, error) {
	if f.getErr != nil {
		return "", f.getErr
	}

	cookie, ok := f.cookies[account]
	if !ok {
		return "", keychain.ErrNotFound
	}

	return cookie, nil
}

func (f *fakeKeychainStore) Delete(account string) error {
	delete(f.cookies, account)

	return nil
}

// ModifiedAt is a trivial stub satisfying the grown keychain.Store interface: these tests never
// exercise last-rotation-time lookups.
func (f *fakeKeychainStore) ModifiedAt(string) (time.Time, error) {
	return time.Time{}, keychain.ErrNotFound
}

// fakePrompter feeds canned answers (or a canned error) to login's server/cookie prompts, and
// records how many times each prompt was shown so tests can assert a prompt was (or wasn't)
// issued.
type fakePrompter struct {
	line    string
	lineErr error
	lineN   int

	secret    string
	secretErr error
	secretN   int
}

func (f *fakePrompter) PromptLine(string) (string, error) {
	f.lineN++
	if f.lineErr != nil {
		return "", f.lineErr
	}

	return f.line, nil
}

func (f *fakePrompter) PromptSecret(string) (string, error) {
	f.secretN++
	if f.secretErr != nil {
		return "", f.secretErr
	}

	return f.secret, nil
}

func newTestUserServer(t *testing.T, status int) *httptest.Server {
	t.Helper()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/user":
			w.WriteHeader(status)
		default:
			// Best-effort stack-id discovery hits /bootdata; let it 404 so login's discovery
			// attempt fails silently, leaving org-id/stack-id at zero, as it does whenever the
			// server doesn't expose bootdata.
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)

	return server
}

func Test_LoginCommand_success(t *testing.T) {
	server := newTestUserServer(t, http.StatusOK)

	configFile := testutils.CreateTempFile(t, "contexts:")

	store := &fakeKeychainStore{}
	restoreStore := login.SetKeychainStore(store)
	defer restoreStore()

	prompter := &fakePrompter{line: server.URL, secret: "the-cookie"}
	restorePrompter := login.SetPrompter(prompter)
	defer restorePrompter()

	testCase := testutils.CommandTestCase{
		Cmd:     login.Command(),
		Command: []string{"--config", configFile},
		Assertions: []testutils.CommandAssertion{
			testutils.CommandSuccess(),
			testutils.CommandOutputContains("Logged in"),
		},
	}
	testCase.Run(t)

	require.Equal(t, 1, prompter.lineN)
	require.Equal(t, 1, prompter.secretN)

	written, err := os.ReadFile(configFile)
	require.NoError(t, err)
	require.Contains(t, string(written), "server: "+server.URL)
	require.Contains(t, string(written), "current-context: default")
	require.NotContains(t, string(written), "the-cookie")

	cookie, err := store.Get(keychain.Account("default"))
	require.NoError(t, err)
	require.Equal(t, "the-cookie", cookie)
}

func Test_LoginCommand_serverFlagSkipsPrompt(t *testing.T) {
	server := newTestUserServer(t, http.StatusOK)

	configFile := testutils.CreateTempFile(t, "contexts:")

	store := &fakeKeychainStore{}
	restoreStore := login.SetKeychainStore(store)
	defer restoreStore()

	prompter := &fakePrompter{secret: "the-cookie"}
	restorePrompter := login.SetPrompter(prompter)
	defer restorePrompter()

	testCase := testutils.CommandTestCase{
		Cmd:     login.Command(),
		Command: []string{"--config", configFile, "--server", server.URL},
		Assertions: []testutils.CommandAssertion{
			testutils.CommandSuccess(),
		},
	}
	testCase.Run(t)

	require.Equal(t, 0, prompter.lineN, "the server prompt must not be shown when --server is set")
	require.Equal(t, 1, prompter.secretN)
}

func Test_LoginCommand_validationFailureLeavesConfigAndKeychainUntouched(t *testing.T) {
	server := newTestUserServer(t, http.StatusUnauthorized)

	initialContents := "current-context: default\ncontexts:\n  default: {}\n"
	configFile := testutils.CreateTempFile(t, initialContents)

	store := &fakeKeychainStore{}
	restoreStore := login.SetKeychainStore(store)
	defer restoreStore()

	prompter := &fakePrompter{secret: "the-cookie"}
	restorePrompter := login.SetPrompter(prompter)
	defer restorePrompter()

	var gotErr error
	testCase := testutils.CommandTestCase{
		Cmd:     login.Command(),
		Command: []string{"--config", configFile, "--server", server.URL},
		Assertions: []testutils.CommandAssertion{
			testutils.CommandErrorContains("could not validate session cookie"),
			func(_ *testing.T, result testutils.CommandResult) { gotErr = result.Err },
		},
	}
	testCase.Run(t)

	written, err := os.ReadFile(configFile)
	require.NoError(t, err)
	require.Equal(t, initialContents, string(written))

	_, err = store.Get(keychain.Account("default"))
	require.ErrorIs(t, err, keychain.ErrNotFound)

	// The 401 from session.VerifyCookie must route through fail's centralized rendering (rather
	// than falling through to the generic "Unexpected error" fallback), with exit code 2 and a
	// message specific to a rejected login prompt rather than the "run login update" wording used
	// for a stale, previously-working session.
	detailedErr := fail.ErrorToDetailedError(gotErr)
	require.NotNil(t, detailedErr)
	require.Equal(t, "Grafana rejected the provided session cookie", detailedErr.Summary)
	require.NotNil(t, detailedErr.ExitCode)
	require.Equal(t, 2, *detailedErr.ExitCode)
}

func Test_LoginCommand_emptyCookieFailsWithoutPersisting(t *testing.T) {
	server := newTestUserServer(t, http.StatusOK)

	configFile := testutils.CreateTempFile(t, "contexts:")

	store := &fakeKeychainStore{}
	restoreStore := login.SetKeychainStore(store)
	defer restoreStore()

	prompter := &fakePrompter{secret: "   "}
	restorePrompter := login.SetPrompter(prompter)
	defer restorePrompter()

	testCase := testutils.CommandTestCase{
		Cmd:     login.Command(),
		Command: []string{"--config", configFile, "--server", server.URL},
		Assertions: []testutils.CommandAssertion{
			testutils.CommandErrorContains("a session cookie is required"),
		},
	}
	testCase.Run(t)

	_, err := store.Get(keychain.Account("default"))
	require.ErrorIs(t, err, keychain.ErrNotFound)
}

func Test_LoginCommand_missingServerFailsWithoutPersisting(t *testing.T) {
	configFile := testutils.CreateTempFile(t, "contexts:")

	store := &fakeKeychainStore{}
	restoreStore := login.SetKeychainStore(store)
	defer restoreStore()

	// No --server flag, and the fake prompter returns an empty line: login must fail before
	// ever prompting for (or validating) a cookie.
	prompter := &fakePrompter{line: "  "}
	restorePrompter := login.SetPrompter(prompter)
	defer restorePrompter()

	testCase := testutils.CommandTestCase{
		Cmd:     login.Command(),
		Command: []string{"--config", configFile},
		Assertions: []testutils.CommandAssertion{
			testutils.CommandErrorContains("a Grafana server URL is required"),
		},
	}
	testCase.Run(t)

	require.Equal(t, 0, prompter.secretN, "the cookie must never be prompted for without a server")
}

func Test_LoginCommand_usesExistingContextName(t *testing.T) {
	server := newTestUserServer(t, http.StatusOK)

	initialContents := "current-context: staging\ncontexts:\n  staging:\n    grafana:\n      org-id: 42\n"
	configFile := testutils.CreateTempFile(t, initialContents)

	store := &fakeKeychainStore{}
	restoreStore := login.SetKeychainStore(store)
	defer restoreStore()

	prompter := &fakePrompter{secret: "the-cookie"}
	restorePrompter := login.SetPrompter(prompter)
	defer restorePrompter()

	testCase := testutils.CommandTestCase{
		Cmd:     login.Command(),
		Command: []string{"--config", configFile, "--server", server.URL},
		Assertions: []testutils.CommandAssertion{
			testutils.CommandSuccess(),
		},
	}
	testCase.Run(t)

	written, err := os.ReadFile(configFile)
	require.NoError(t, err)
	require.Contains(t, string(written), "current-context: staging")
	require.Contains(t, string(written), "org-id: 42")

	_, err = store.Get(keychain.Account("staging"))
	require.NoError(t, err)
}

// newTestOrgDiscoveryServer serves /api/user (always 200, so the cookie is always accepted) and
// /api/org with orgStatus/orgBody, so tests can drive login's on-prem org-id auto-detection
// fallback (GET /api/org, triggered when neither --org-id nor --stack-id was given and Grafana
// Cloud stack-id discovery -- /bootdata -- found nothing, which is also always the case here since
// /bootdata isn't handled and falls through to the default 404). orgHits counts how many times
// /api/org was hit, so tests can assert detection was (or wasn't) attempted at all.
func newTestOrgDiscoveryServer(t *testing.T, orgStatus int, orgBody string) (*httptest.Server, *int) {
	t.Helper()

	orgHits := new(int)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/user":
			w.WriteHeader(http.StatusOK)
		case "/api/org":
			*orgHits++
			w.WriteHeader(orgStatus)
			if orgBody != "" {
				_, _ = w.Write([]byte(orgBody))
			}
		default:
			// Grafana Cloud stack-id discovery hits /bootdata; let it 404 so it fails silently,
			// leaving org-id/stack-id at zero -- the on-prem case org detection is meant to cover.
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)

	return server, orgHits
}

func Test_LoginCommand_detectsOrgID(t *testing.T) {
	server, orgHits := newTestOrgDiscoveryServer(t, http.StatusOK, `{"id":1}`)

	configFile := testutils.CreateTempFile(t, "contexts:")

	store := &fakeKeychainStore{}
	restoreStore := login.SetKeychainStore(store)
	defer restoreStore()

	prompter := &fakePrompter{secret: "the-cookie"}
	restorePrompter := login.SetPrompter(prompter)
	defer restorePrompter()

	testCase := testutils.CommandTestCase{
		Cmd:     login.Command(),
		Command: []string{"--config", configFile, "--server", server.URL},
		Assertions: []testutils.CommandAssertion{
			testutils.CommandSuccess(),
			testutils.CommandOutputContains("Detected organization id 1"),
		},
	}
	testCase.Run(t)

	require.Equal(t, 1, *orgHits)

	written, err := os.ReadFile(configFile)
	require.NoError(t, err)
	require.Contains(t, string(written), "org-id: 1")
}

func Test_LoginCommand_detectsNonDefaultOrgID(t *testing.T) {
	server, orgHits := newTestOrgDiscoveryServer(t, http.StatusOK, `{"id":5}`)

	configFile := testutils.CreateTempFile(t, "contexts:")

	store := &fakeKeychainStore{}
	restoreStore := login.SetKeychainStore(store)
	defer restoreStore()

	prompter := &fakePrompter{secret: "the-cookie"}
	restorePrompter := login.SetPrompter(prompter)
	defer restorePrompter()

	testCase := testutils.CommandTestCase{
		Cmd:     login.Command(),
		Command: []string{"--config", configFile, "--server", server.URL},
		Assertions: []testutils.CommandAssertion{
			testutils.CommandSuccess(),
			testutils.CommandOutputContains("Detected organization id 5"),
		},
	}
	testCase.Run(t)

	require.Equal(t, 1, *orgHits)

	written, err := os.ReadFile(configFile)
	require.NoError(t, err)
	require.Contains(t, string(written), "org-id: 5")
}

func Test_LoginCommand_orgDetectionFailureFallsBackToOrgID1(t *testing.T) {
	server, orgHits := newTestOrgDiscoveryServer(t, http.StatusInternalServerError, "")

	configFile := testutils.CreateTempFile(t, "contexts:")

	store := &fakeKeychainStore{}
	restoreStore := login.SetKeychainStore(store)
	defer restoreStore()

	prompter := &fakePrompter{secret: "the-cookie"}
	restorePrompter := login.SetPrompter(prompter)
	defer restorePrompter()

	testCase := testutils.CommandTestCase{
		Cmd:     login.Command(),
		Command: []string{"--config", configFile, "--server", server.URL},
		Assertions: []testutils.CommandAssertion{
			testutils.CommandSuccess(),
			testutils.CommandOutputContains("could not detect organization id, defaulting to 1; override with --org-id"),
		},
	}
	testCase.Run(t)

	require.Equal(t, 1, *orgHits)

	written, err := os.ReadFile(configFile)
	require.NoError(t, err)
	require.Contains(t, string(written), "org-id: 1")
}

func Test_LoginCommand_explicitOrgIDSkipsDetection(t *testing.T) {
	server, orgHits := newTestOrgDiscoveryServer(t, http.StatusOK, `{"id":1}`)

	configFile := testutils.CreateTempFile(t, "contexts:")

	store := &fakeKeychainStore{}
	restoreStore := login.SetKeychainStore(store)
	defer restoreStore()

	prompter := &fakePrompter{secret: "the-cookie"}
	restorePrompter := login.SetPrompter(prompter)
	defer restorePrompter()

	testCase := testutils.CommandTestCase{
		Cmd:     login.Command(),
		Command: []string{"--config", configFile, "--server", server.URL, "--org-id", "7"},
		Assertions: []testutils.CommandAssertion{
			testutils.CommandSuccess(),
		},
	}
	testCase.Run(t)

	require.Equal(t, 0, *orgHits, "an explicit --org-id must skip /api/org detection entirely")

	written, err := os.ReadFile(configFile)
	require.NoError(t, err)
	require.Contains(t, string(written), "org-id: 7")
}

func Test_LoginCommand_explicitStackIDSkipsOrgDetection(t *testing.T) {
	server, orgHits := newTestOrgDiscoveryServer(t, http.StatusOK, `{"id":1}`)

	configFile := testutils.CreateTempFile(t, "contexts:")

	store := &fakeKeychainStore{}
	restoreStore := login.SetKeychainStore(store)
	defer restoreStore()

	prompter := &fakePrompter{secret: "the-cookie"}
	restorePrompter := login.SetPrompter(prompter)
	defer restorePrompter()

	testCase := testutils.CommandTestCase{
		Cmd:     login.Command(),
		Command: []string{"--config", configFile, "--server", server.URL, "--stack-id", "123"},
		Assertions: []testutils.CommandAssertion{
			testutils.CommandSuccess(),
		},
	}
	testCase.Run(t)

	require.Equal(t, 0, *orgHits, "an explicit --stack-id must skip /api/org detection entirely")

	written, err := os.ReadFile(configFile)
	require.NoError(t, err)
	require.Contains(t, string(written), "stack-id: 123")
	require.NotContains(t, string(written), "org-id:")
}

func Test_LoginUpdateCommand_success(t *testing.T) {
	server := newTestUserServer(t, http.StatusOK)

	initialContents := "current-context: default\ncontexts:\n  default:\n    grafana:\n      server: " + server.URL + "\n"
	configFile := testutils.CreateTempFile(t, initialContents)

	store := &fakeKeychainStore{}
	require.NoError(t, store.Set(keychain.Account("default"), "stale-cookie"))
	restoreStore := login.SetKeychainStore(store)
	defer restoreStore()

	prompter := &fakePrompter{secret: "fresh-cookie"}
	restorePrompter := login.SetPrompter(prompter)
	defer restorePrompter()

	testCase := testutils.CommandTestCase{
		Cmd:     login.Command(),
		Command: []string{"update", "--config", configFile},
		Assertions: []testutils.CommandAssertion{
			testutils.CommandSuccess(),
			testutils.CommandOutputContains("Refreshed session"),
		},
	}
	testCase.Run(t)

	require.Equal(t, 0, prompter.lineN, "login update must never prompt for the server")
	require.Equal(t, 1, prompter.secretN)

	written, err := os.ReadFile(configFile)
	require.NoError(t, err)
	require.Equal(t, initialContents, string(written), "login update must not modify the configuration file")

	cookie, err := store.Get(keychain.Account("default"))
	require.NoError(t, err)
	require.Equal(t, "fresh-cookie", cookie)
}

func Test_LoginUpdateCommand_explicitContextFlag(t *testing.T) {
	server := newTestUserServer(t, http.StatusOK)

	initialContents := "current-context: default\ncontexts:\n" +
		"  default:\n    grafana:\n      server: https://unused.example.com\n" +
		"  staging:\n    grafana:\n      server: " + server.URL + "\n"
	configFile := testutils.CreateTempFile(t, initialContents)

	store := &fakeKeychainStore{}
	restoreStore := login.SetKeychainStore(store)
	defer restoreStore()

	prompter := &fakePrompter{secret: "fresh-cookie"}
	restorePrompter := login.SetPrompter(prompter)
	defer restorePrompter()

	testCase := testutils.CommandTestCase{
		Cmd:     login.Command(),
		Command: []string{"update", "--config", configFile, "--context", "staging"},
		Assertions: []testutils.CommandAssertion{
			testutils.CommandSuccess(),
		},
	}
	testCase.Run(t)

	cookie, err := store.Get(keychain.Account("staging"))
	require.NoError(t, err)
	require.Equal(t, "fresh-cookie", cookie)

	_, err = store.Get(keychain.Account("default"))
	require.ErrorIs(t, err, keychain.ErrNotFound, "only the selected context's keychain item is touched")
}

func Test_LoginUpdateCommand_unknownContextFails(t *testing.T) {
	configFile := testutils.CreateTempFile(t, "contexts:")

	store := &fakeKeychainStore{}
	restoreStore := login.SetKeychainStore(store)
	defer restoreStore()

	prompter := &fakePrompter{secret: "fresh-cookie"}
	restorePrompter := login.SetPrompter(prompter)
	defer restorePrompter()

	testCase := testutils.CommandTestCase{
		Cmd:     login.Command(),
		Command: []string{"update", "--config", configFile, "--context", "missing"},
		Assertions: []testutils.CommandAssertion{
			testutils.CommandErrorContains(`context "missing" is not configured`),
		},
	}
	testCase.Run(t)

	require.Equal(t, 0, prompter.secretN, "the cookie must never be prompted for an unknown context")
}

func Test_LoginUpdateCommand_noCurrentContextFails(t *testing.T) {
	configFile := testutils.CreateTempFile(t, "contexts:")

	store := &fakeKeychainStore{}
	restoreStore := login.SetKeychainStore(store)
	defer restoreStore()

	prompter := &fakePrompter{secret: "fresh-cookie"}
	restorePrompter := login.SetPrompter(prompter)
	defer restorePrompter()

	testCase := testutils.CommandTestCase{
		Cmd:     login.Command(),
		Command: []string{"update", "--config", configFile},
		Assertions: []testutils.CommandAssertion{
			testutils.CommandErrorContains("no context specified and no current context is set"),
		},
	}
	testCase.Run(t)

	require.Equal(t, 0, prompter.secretN)
}

func Test_LoginUpdateCommand_validationFailureLeavesKeychainUntouched(t *testing.T) {
	server := newTestUserServer(t, http.StatusUnauthorized)

	initialContents := "current-context: default\ncontexts:\n  default:\n    grafana:\n      server: " + server.URL + "\n"
	configFile := testutils.CreateTempFile(t, initialContents)

	store := &fakeKeychainStore{}
	require.NoError(t, store.Set(keychain.Account("default"), "stale-cookie"))
	restoreStore := login.SetKeychainStore(store)
	defer restoreStore()

	prompter := &fakePrompter{secret: "fresh-cookie"}
	restorePrompter := login.SetPrompter(prompter)
	defer restorePrompter()

	var gotErr error
	testCase := testutils.CommandTestCase{
		Cmd:     login.Command(),
		Command: []string{"update", "--config", configFile},
		Assertions: []testutils.CommandAssertion{
			testutils.CommandErrorContains("could not validate session cookie"),
			func(_ *testing.T, result testutils.CommandResult) { gotErr = result.Err },
		},
	}
	testCase.Run(t)

	cookie, err := store.Get(keychain.Account("default"))
	require.NoError(t, err)
	require.Equal(t, "stale-cookie", cookie, "a failed validation must not overwrite the existing keychain item")

	written, err := os.ReadFile(configFile)
	require.NoError(t, err)
	require.Equal(t, initialContents, string(written))

	// Same as login's own validation-failure path: this must route through fail's centralized
	// rendering with exit code 2, using the "rejected" wording rather than "stale session" -- the
	// user just re-entered a cookie at this very `login update` prompt and it was rejected, so
	// telling them to run `login update` again would be circular.
	detailedErr := fail.ErrorToDetailedError(gotErr)
	require.NotNil(t, detailedErr)
	require.Equal(t, "Grafana rejected the provided session cookie", detailedErr.Summary)
	require.NotNil(t, detailedErr.ExitCode)
	require.Equal(t, 2, *detailedErr.ExitCode)
}

func Test_LoginUpdateCommand_emptyCookieFailsWithoutPersisting(t *testing.T) {
	server := newTestUserServer(t, http.StatusOK)

	initialContents := "current-context: default\ncontexts:\n  default:\n    grafana:\n      server: " + server.URL + "\n"
	configFile := testutils.CreateTempFile(t, initialContents)

	store := &fakeKeychainStore{}
	restoreStore := login.SetKeychainStore(store)
	defer restoreStore()

	prompter := &fakePrompter{secret: "   "}
	restorePrompter := login.SetPrompter(prompter)
	defer restorePrompter()

	testCase := testutils.CommandTestCase{
		Cmd:     login.Command(),
		Command: []string{"update", "--config", configFile},
		Assertions: []testutils.CommandAssertion{
			testutils.CommandErrorContains("a session cookie is required"),
		},
	}
	testCase.Run(t)

	_, err := store.Get(keychain.Account("default"))
	require.ErrorIs(t, err, keychain.ErrNotFound)
}

func Test_LoginCommand_preservesExistingTLSSettings(t *testing.T) {
	server := newTestUserServer(t, http.StatusOK)

	initialContents := "current-context: default\ncontexts:\n" +
		"  default:\n    grafana:\n      server: https://old.example.com\n" +
		"      org-id: 42\n      tls:\n        insecure-skip-verify: true\n" +
		"        server-name: custom.example.com\n"
	configFile := testutils.CreateTempFile(t, initialContents)

	store := &fakeKeychainStore{}
	restoreStore := login.SetKeychainStore(store)
	defer restoreStore()

	prompter := &fakePrompter{secret: "the-cookie"}
	restorePrompter := login.SetPrompter(prompter)
	defer restorePrompter()

	// Logging in again (e.g. against a new server) must not drop the existing TLS block: only
	// server/org-id/stack-id are explicitly overridden by login, TLS is always carried over.
	testCase := testutils.CommandTestCase{
		Cmd:     login.Command(),
		Command: []string{"--config", configFile, "--server", server.URL},
		Assertions: []testutils.CommandAssertion{
			testutils.CommandSuccess(),
		},
	}
	testCase.Run(t)

	written, err := os.ReadFile(configFile)
	require.NoError(t, err)
	require.Contains(t, string(written), "insecure-skip-verify: true")
	require.Contains(t, string(written), "server-name: custom.example.com")
	require.Contains(t, string(written), "org-id: 42")
}

func Test_LoginCommand_keychainSetFailureLeavesConfigUntouched(t *testing.T) {
	server := newTestUserServer(t, http.StatusOK)

	initialContents := "contexts:"
	configFile := testutils.CreateTempFile(t, initialContents)

	store := &fakeKeychainStore{setErr: errors.New("keychain unavailable")}
	restoreStore := login.SetKeychainStore(store)
	defer restoreStore()

	prompter := &fakePrompter{secret: "the-cookie"}
	restorePrompter := login.SetPrompter(prompter)
	defer restorePrompter()

	testCase := testutils.CommandTestCase{
		Cmd:     login.Command(),
		Command: []string{"--config", configFile, "--server", server.URL},
		Assertions: []testutils.CommandAssertion{
			testutils.CommandErrorContains("storing session cookie in keychain"),
		},
	}
	testCase.Run(t)

	written, err := os.ReadFile(configFile)
	require.NoError(t, err)
	require.Equal(t, initialContents, string(written), "the config file must not be written when the keychain Set fails")
}

func Test_LoginCommand_configWriteFailureRollsBackKeychainItem(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root ignores file permissions, so this test cannot force a write failure")
	}

	server := newTestUserServer(t, http.StatusOK)

	configFile := testutils.CreateTempFile(t, "contexts:")
	// config.Load only needs read access; config.Write reopens the file for writing and must
	// fail here, simulating any persistence failure that happens after the keychain Set already
	// succeeded.
	require.NoError(t, os.Chmod(configFile, 0o400))

	store := &fakeKeychainStore{}
	restoreStore := login.SetKeychainStore(store)
	defer restoreStore()

	prompter := &fakePrompter{secret: "the-cookie"}
	restorePrompter := login.SetPrompter(prompter)
	defer restorePrompter()

	testCase := testutils.CommandTestCase{
		Cmd:     login.Command(),
		Command: []string{"--config", configFile, "--server", server.URL},
		Assertions: []testutils.CommandAssertion{
			testutils.CommandErrorContains("writing configuration"),
		},
	}
	testCase.Run(t)

	_, err := store.Get(keychain.Account("default"))
	require.ErrorIs(t, err, keychain.ErrNotFound, "a failed config write must roll back the keychain item login just created")
}

// Test_LoginCommand_configWriteFailureRestoresPriorKeychainItemOnRelogin covers the re-login flow
// (login re-run against an already-authenticated context, which is expected to preserve the
// context's existing org-id/stack-id/TLS): keychainStore.Set overwrites the previously-valid
// cookie before config.Write runs, so if config.Write then fails, the rollback must restore the
// prior cookie rather than deleting the account outright -- otherwise a working credential would
// be destroyed by an unrelated config-write failure.
func Test_LoginCommand_configWriteFailureRestoresPriorKeychainItemOnRelogin(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root ignores file permissions, so this test cannot force a write failure")
	}

	server := newTestUserServer(t, http.StatusOK)

	initialContents := "current-context: default\ncontexts:\n  default:\n    grafana:\n      server: " + server.URL + "\n"
	configFile := testutils.CreateTempFile(t, initialContents)
	// config.Load only needs read access; config.Write reopens the file for writing and must fail
	// here, simulating any persistence failure that happens after the keychain Set already
	// overwrote the prior cookie.
	require.NoError(t, os.Chmod(configFile, 0o400))

	store := &fakeKeychainStore{}
	require.NoError(t, store.Set(keychain.Account("default"), "prior-valid-cookie"))
	restoreStore := login.SetKeychainStore(store)
	defer restoreStore()

	prompter := &fakePrompter{secret: "new-cookie"}
	restorePrompter := login.SetPrompter(prompter)
	defer restorePrompter()

	testCase := testutils.CommandTestCase{
		Cmd:     login.Command(),
		Command: []string{"--config", configFile, "--server", server.URL},
		Assertions: []testutils.CommandAssertion{
			testutils.CommandErrorContains("writing configuration"),
		},
	}
	testCase.Run(t)

	cookie, err := store.Get(keychain.Account("default"))
	require.NoError(t, err)
	require.Equal(t, "prior-valid-cookie", cookie,
		"a failed config write during re-login must restore the prior cookie instead of deleting it")
}

// Test_LoginCommand_configWriteFailureLeavesItemOnTransientGetError covers the narrow case where
// keychainStore.Get fails for a reason other than keychain.ErrNotFound while checking for a prior
// cookie (e.g. a transient platform keychain error), and config.Write then also fails. login
// cannot tell whether a prior cookie existed, so the rollback must not delete the keychain item --
// deleting would risk destroying a working credential it simply failed to read. It must also not
// pretend it restored anything; leaving the just-written cookie in place is the safe outcome.
func Test_LoginCommand_configWriteFailureLeavesItemOnTransientGetError(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root ignores file permissions, so this test cannot force a write failure")
	}

	server := newTestUserServer(t, http.StatusOK)

	initialContents := "current-context: default\ncontexts:\n  default:\n    grafana:\n      server: " + server.URL + "\n"
	configFile := testutils.CreateTempFile(t, initialContents)
	require.NoError(t, os.Chmod(configFile, 0o400))

	store := &fakeKeychainStore{getErr: errors.New("keychain: transient failure")}
	restoreStore := login.SetKeychainStore(store)
	defer restoreStore()

	prompter := &fakePrompter{secret: "new-cookie"}
	restorePrompter := login.SetPrompter(prompter)
	defer restorePrompter()

	testCase := testutils.CommandTestCase{
		Cmd:     login.Command(),
		Command: []string{"--config", configFile, "--server", server.URL},
		Assertions: []testutils.CommandAssertion{
			testutils.CommandErrorContains("writing configuration"),
		},
	}
	testCase.Run(t)

	// getErr only affects the "check for a prior cookie" read; Set still writes for real, so the
	// new cookie must still be there afterwards -- the rollback must not have deleted it.
	store.getErr = nil
	cookie, err := store.Get(keychain.Account("default"))
	require.NoError(t, err)
	require.Equal(t, "new-cookie", cookie,
		"a config write failure must not delete the keychain item when login couldn't tell whether a prior cookie existed")
}

func Test_LoginUpdateCommand_keychainSetFailureSurfacesError(t *testing.T) {
	server := newTestUserServer(t, http.StatusOK)

	initialContents := "current-context: default\ncontexts:\n  default:\n    grafana:\n      server: " + server.URL + "\n"
	configFile := testutils.CreateTempFile(t, initialContents)

	store := &fakeKeychainStore{}
	require.NoError(t, store.Set(keychain.Account("default"), "stale-cookie"))
	store.setErr = errors.New("keychain unavailable")
	restoreStore := login.SetKeychainStore(store)
	defer restoreStore()

	prompter := &fakePrompter{secret: "fresh-cookie"}
	restorePrompter := login.SetPrompter(prompter)
	defer restorePrompter()

	testCase := testutils.CommandTestCase{
		Cmd:     login.Command(),
		Command: []string{"update", "--config", configFile},
		Assertions: []testutils.CommandAssertion{
			testutils.CommandErrorContains("storing session cookie in keychain"),
		},
	}
	testCase.Run(t)

	cookie, err := store.Get(keychain.Account("default"))
	require.NoError(t, err)
	require.Equal(t, "stale-cookie", cookie, "a failed keychain Set must not leave the stale cookie overwritten with a half-applied value")

	written, err := os.ReadFile(configFile)
	require.NoError(t, err)
	require.Equal(t, initialContents, string(written), "login update must never modify the configuration file, even on keychain failure")
}

func Test_LoginCommand_explicitContextFlag(t *testing.T) {
	server := newTestUserServer(t, http.StatusOK)

	configFile := testutils.CreateTempFile(t, "contexts:")

	store := &fakeKeychainStore{}
	restoreStore := login.SetKeychainStore(store)
	defer restoreStore()

	prompter := &fakePrompter{secret: "the-cookie"}
	restorePrompter := login.SetPrompter(prompter)
	defer restorePrompter()

	testCase := testutils.CommandTestCase{
		Cmd:     login.Command(),
		Command: []string{"--config", configFile, "--server", server.URL, "--context", "dev"},
		Assertions: []testutils.CommandAssertion{
			testutils.CommandSuccess(),
		},
	}
	testCase.Run(t)

	written, err := os.ReadFile(configFile)
	require.NoError(t, err)
	require.Contains(t, string(written), "current-context: dev")

	_, err = store.Get(keychain.Account("dev"))
	require.NoError(t, err)
}

func Test_LoginCommand_cookieStdinSuccess(t *testing.T) {
	server := newTestUserServer(t, http.StatusOK)

	configFile := testutils.CreateTempFile(t, "contexts:")

	store := &fakeKeychainStore{}
	restoreStore := login.SetKeychainStore(store)
	defer restoreStore()

	// The prompter must never be consulted when --cookie-stdin is set: if it were, this fake
	// would hand back "unused-cookie" instead of the value piped through stdin.
	prompter := &fakePrompter{secret: "unused-cookie"}
	restorePrompter := login.SetPrompter(prompter)
	defer restorePrompter()

	testCase := testutils.CommandTestCase{
		Cmd:     login.Command(),
		Command: []string{"--config", configFile, "--server", server.URL, "--cookie-stdin"},
		Stdin:   strings.NewReader("the-cookie\r\n"),
		Assertions: []testutils.CommandAssertion{
			testutils.CommandSuccess(),
			testutils.CommandOutputContains("Logged in"),
		},
	}
	testCase.Run(t)

	require.Equal(t, 0, prompter.lineN, "--server was given, so the server prompt must not be shown")
	require.Equal(t, 0, prompter.secretN, "--cookie-stdin must read the cookie from stdin, not the prompter")

	cookie, err := store.Get(keychain.Account("default"))
	require.NoError(t, err)
	require.Equal(t, "the-cookie", cookie, "trailing whitespace/newline from stdin must be trimmed")
}

func Test_LoginCommand_cookieStdinEmptyStdinFails(t *testing.T) {
	server := newTestUserServer(t, http.StatusOK)

	configFile := testutils.CreateTempFile(t, "contexts:")

	store := &fakeKeychainStore{}
	restoreStore := login.SetKeychainStore(store)
	defer restoreStore()

	prompter := &fakePrompter{}
	restorePrompter := login.SetPrompter(prompter)
	defer restorePrompter()

	testCase := testutils.CommandTestCase{
		Cmd:     login.Command(),
		Command: []string{"--config", configFile, "--server", server.URL, "--cookie-stdin"},
		Stdin:   strings.NewReader("   \n"),
		Assertions: []testutils.CommandAssertion{
			testutils.CommandErrorContains("stdin was empty"),
		},
	}
	testCase.Run(t)

	_, err := store.Get(keychain.Account("default"))
	require.ErrorIs(t, err, keychain.ErrNotFound)
}

func Test_LoginCommand_cookieStdinWithoutServerFails(t *testing.T) {
	configFile := testutils.CreateTempFile(t, "contexts:")

	store := &fakeKeychainStore{}
	restoreStore := login.SetKeychainStore(store)
	defer restoreStore()

	prompter := &fakePrompter{}
	restorePrompter := login.SetPrompter(prompter)
	defer restorePrompter()

	testCase := testutils.CommandTestCase{
		Cmd:     login.Command(),
		Command: []string{"--config", configFile, "--cookie-stdin"},
		// No stdin is wired up at all: --cookie-stdin without --server must fail before ever
		// trying to read it.
		Assertions: []testutils.CommandAssertion{
			testutils.CommandErrorContains("--cookie-stdin requires --server"),
		},
	}
	testCase.Run(t)

	require.Equal(t, 0, prompter.lineN, "the server prompt must not be shown either")
	require.Equal(t, 0, prompter.secretN, "the cookie must never be prompted for without a server")
}

func Test_LoginUpdateCommand_cookieStdinSuccess(t *testing.T) {
	server := newTestUserServer(t, http.StatusOK)

	initialContents := "current-context: default\ncontexts:\n  default:\n    grafana:\n      server: " + server.URL + "\n"
	configFile := testutils.CreateTempFile(t, initialContents)

	store := &fakeKeychainStore{}
	require.NoError(t, store.Set(keychain.Account("default"), "stale-cookie"))
	restoreStore := login.SetKeychainStore(store)
	defer restoreStore()

	prompter := &fakePrompter{secret: "unused-cookie"}
	restorePrompter := login.SetPrompter(prompter)
	defer restorePrompter()

	testCase := testutils.CommandTestCase{
		Cmd:     login.Command(),
		Command: []string{"update", "--config", configFile, "--cookie-stdin"},
		Stdin:   strings.NewReader("fresh-cookie\n"),
		Assertions: []testutils.CommandAssertion{
			testutils.CommandSuccess(),
			testutils.CommandOutputContains("Refreshed session"),
		},
	}
	testCase.Run(t)

	require.Equal(t, 0, prompter.secretN, "--cookie-stdin must read the cookie from stdin, not the prompter")

	cookie, err := store.Get(keychain.Account("default"))
	require.NoError(t, err)
	require.Equal(t, "fresh-cookie", cookie)

	written, err := os.ReadFile(configFile)
	require.NoError(t, err)
	require.Equal(t, initialContents, string(written), "login update must not modify the configuration file")
}

func Test_LoginUpdateCommand_cookieStdinEmptyStdinFails(t *testing.T) {
	server := newTestUserServer(t, http.StatusOK)

	initialContents := "current-context: default\ncontexts:\n  default:\n    grafana:\n      server: " + server.URL + "\n"
	configFile := testutils.CreateTempFile(t, initialContents)

	store := &fakeKeychainStore{}
	restoreStore := login.SetKeychainStore(store)
	defer restoreStore()

	prompter := &fakePrompter{}
	restorePrompter := login.SetPrompter(prompter)
	defer restorePrompter()

	testCase := testutils.CommandTestCase{
		Cmd:     login.Command(),
		Command: []string{"update", "--config", configFile, "--cookie-stdin"},
		Stdin:   strings.NewReader(""),
		Assertions: []testutils.CommandAssertion{
			testutils.CommandErrorContains("stdin was empty"),
		},
	}
	testCase.Run(t)

	_, err := store.Get(keychain.Account("default"))
	require.ErrorIs(t, err, keychain.ErrNotFound)
}
