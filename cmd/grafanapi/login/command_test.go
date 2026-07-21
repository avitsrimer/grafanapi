package login_test

import (
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/grafana/grafanapi/cmd/grafanapi/login"
	"github.com/grafana/grafanapi/internal/keychain"
	"github.com/grafana/grafanapi/internal/testutils"
	"github.com/stretchr/testify/require"
)

// fakeKeychainStore is an in-memory keychain.Store used to assert what login persists (or
// doesn't persist) without touching the real platform Keychain.
type fakeKeychainStore struct {
	cookies map[string]string
}

func (f *fakeKeychainStore) Set(account, secret string) error {
	if f.cookies == nil {
		f.cookies = map[string]string{}
	}

	f.cookies[account] = secret

	return nil
}

func (f *fakeKeychainStore) Get(account string) (string, error) {
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

	testCase := testutils.CommandTestCase{
		Cmd:     login.Command(),
		Command: []string{"--config", configFile, "--server", server.URL},
		Assertions: []testutils.CommandAssertion{
			testutils.CommandErrorContains("could not validate session cookie"),
		},
	}
	testCase.Run(t)

	written, err := os.ReadFile(configFile)
	require.NoError(t, err)
	require.Equal(t, initialContents, string(written))

	_, err = store.Get(keychain.Account("default"))
	require.ErrorIs(t, err, keychain.ErrNotFound)
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
