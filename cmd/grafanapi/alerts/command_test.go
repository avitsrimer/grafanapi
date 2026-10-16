package alerts_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	goyaml "github.com/goccy/go-yaml"
	"github.com/grafana/grafana-app-sdk/logging"
	"github.com/grafana/grafanapi/cmd/grafanapi/alerts"
	"github.com/grafana/grafanapi/cmd/grafanapi/config"
	"github.com/grafana/grafanapi/cmd/grafanapi/fail"
	"github.com/grafana/grafanapi/internal/keychain"
	"github.com/grafana/grafanapi/internal/logs"
	"github.com/grafana/grafanapi/internal/testutils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The endpoints this group's subcommands drive, spelled out (rather than derived) so that a
// change to the generated client's base path or to the raw state URL fails these tests instead
// of silently querying the wrong URL.
const (
	alertRulesPath    = "/api/v1/provisioning/alert-rules"
	contactPointsPath = "/api/v1/provisioning/contact-points"
	statePath         = "/api/prometheus/grafana/api/v1/rules"
	// rotatePath is not driven by this group at all: it is where the session-rotating transport
	// (internal/config.SessionSource, wrapped around every authenticated transport) goes on its own
	// when Grafana answers 401. It is scripted as a 401 too, which is what a truly dead session
	// looks like, so the original 401 surfaces unchanged and the exit-2 tests below assert the
	// rendering rather than a rotation artifact.
	rotatePath = "/api/user/auth-tokens/rotate"
)

// contactPointsBody is the contact-points fixture: the UID-less default email receiver every
// Grafana ships with, and a provisioned slack point whose secret the server has already replaced
// with "[REDACTED]".
const contactPointsBody = `[
  {
    "uid": "cp-uid-1",
    "name": "probe-slack",
    "type": "slack",
    "provenance": "api",
    "settings": {"recipient": "#probe", "url": "[REDACTED]"}
  },
  {
    "uid": "",
    "name": "email receiver",
    "type": "email",
    "disableResolveMessage": true,
    "settings": {"addresses": "admin@localhost"}
  }
]`

// provisionedRules is the definition half of the fixture: one alerting rule in "Probe Folder"
// that the state fixture also lists, one recording rule (no evaluation state by nature), and one
// alerting rule in another folder that the state endpoint does not know about yet.
const provisionedRules = `[
  {
    "uid": "rule-firing",
    "title": "probe-rule",
    "folderUID": "probe-folder",
    "ruleGroup": "probe-group",
    "for": "1m",
    "provenance": "api",
    "labels": {"team": "payments-oncall"},
    "annotations": {"summary": "high error rate"},
    "updated": "2026-09-08T09:08:07Z"
  },
  {
    "uid": "rule-recording",
    "title": "probe-record",
    "folderUID": "probe-folder",
    "ruleGroup": "probe-group",
    "record": {"from": "A", "metric": "probe_ratio"}
  },
  {
    "uid": "rule-quiet",
    "title": "quiet-rule",
    "folderUID": "other-folder",
    "ruleGroup": "other-group",
    "for": "5m"
  }
]`

// ruleStates is the state half of the fixture. "rule-quiet" is deliberately reported as
// "inactive" so the --state normal alias has something to match, while "rule-recording" is
// listed without a state at all.
const ruleStates = `{
  "status": "success",
  "data": {
    "groups": [
      {
        "name": "probe-group",
        "file": "Probe Folder",
        "folderUid": "probe-folder",
        "rules": [
          {
            "name": "probe-rule",
            "uid": "rule-firing",
            "type": "alerting",
            "state": "firing",
            "health": "ok",
            "lastEvaluation": "2026-09-09T10:11:12Z",
            "alerts": [{"state": "Alerting"}, {"state": "Normal"}]
          },
          {
            "name": "probe-record",
            "uid": "rule-recording",
            "type": "recording",
            "health": "ok"
          }
        ]
      },
      {
        "name": "other-group",
        "file": "Parent Folder/Other Folder",
        "folderUid": "other-folder",
        "rules": [
          {
            "name": "quiet-rule",
            "uid": "rule-quiet",
            "type": "alerting",
            "state": "inactive",
            "health": "ok"
          }
        ]
      }
    ]
  }
}`

// emptyStates is the state payload of a Grafana with no evaluated rules.
const emptyStates = `{"status": "success", "data": {"groups": []}}`

// failUnexpectedPath is the catch-all route every server below registers, so that a request to a
// path the test did not script fails the test instead of silently getting the mux's own 404 - the
// shape a command quietly querying the wrong URL would otherwise take.
func failUnexpectedPath(t *testing.T) http.HandlerFunc {
	t.Helper()

	return func(w http.ResponseWriter, r *http.Request) {
		assert.Fail(t, "unexpected request path", r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
	}
}

// newTestServer scripts both endpoints the command drives, failing the test on a request to any
// other path. Handlers use assert (not require) since they run on the server's own goroutine,
// where a FailNow-based require would only abort the handler, not the test.
func newTestServer(t *testing.T, rulesStatus int, rulesBody, statesBody string) *httptest.Server {
	t.Helper()

	return newTestServerWithStateStatus(t, rulesStatus, rulesBody, http.StatusOK, statesBody)
}

// newTestServerWithStateStatus is newTestServer with the state endpoint's status code under test
// control too, so degraded mode (a non-401 state failure) can be driven end to end.
func newTestServerWithStateStatus(
	t *testing.T,
	rulesStatus int, rulesBody string,
	statesStatus int, statesBody string,
) *httptest.Server {
	t.Helper()

	mux := http.NewServeMux()
	mux.HandleFunc(alertRulesPath, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(rulesStatus)
		_, err := w.Write([]byte(rulesBody))
		assert.NoError(t, err)
	})
	mux.HandleFunc(statePath, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(statesStatus)
		_, err := w.Write([]byte(statesBody))
		assert.NoError(t, err)
	})
	mux.HandleFunc(rotatePath, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	})
	mux.HandleFunc("/", failUnexpectedPath(t))

	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	return server
}

// newTestConfig writes a temp config file whose current context points at server and has org-id
// set (so LoadConfig's validation does not attempt Grafana Cloud stack-id discovery), and seeds
// the fake keychain with a session cookie for that context.
//
// The fake store is uninstalled through t.Cleanup rather than by a closure the caller has to
// defer: forgetting that defer leaves the real macOS Keychain wired up for every test that runs
// after it, which fails somewhere else entirely - or blocks on an ACL prompt.
func newTestConfig(t *testing.T, server *httptest.Server) string {
	t.Helper()

	return newTestConfigInOrg(t, server, 1)
}

// newTestConfigInOrg is newTestConfig with the context's org-id under test control, which is what
// lets a push prove it claims created rules for the org the *context* names rather than the one
// the file happens to carry.
func newTestConfigInOrg(t *testing.T, server *httptest.Server, orgID int) string {
	t.Helper()

	configFile := testutils.CreateTempFile(t, fmt.Sprintf(
		"current-context: test\ncontexts:\n  test:\n    grafana:\n      server: %s\n      org-id: %d\n",
		server.URL, orgID,
	))

	store := testutils.NewFakeKeychainStore()
	require.NoError(t, store.Set(keychain.Account("test"), "the-cookie"))
	t.Cleanup(config.SetKeychainStore(store))

	return configFile
}

// newRulesFixture is the happy-path setup shared by most tests: both endpoints serving the
// fixtures above, plus a matching config file.
func newRulesFixture(t *testing.T) string {
	t.Helper()

	return newTestConfig(t, newTestServer(t, http.StatusOK, provisionedRules, ruleStates))
}

// runRulesList executes `alerts rules list` with args appended to --config, returning the
// command result for assertions the CommandAssertion helpers do not cover.
func runRulesList(t *testing.T, configFile string, args ...string) testutils.CommandResult {
	t.Helper()

	var result testutils.CommandResult

	testCase := testutils.CommandTestCase{
		Cmd:     alerts.Command(),
		Command: append([]string{"rules", "list", "--config", configFile}, args...),
		Assertions: []testutils.CommandAssertion{
			func(_ *testing.T, res testutils.CommandResult) { result = res },
		},
	}
	testCase.Run(t)

	return result
}

// decodedTitles decodes json/yaml output into the rule titles it lists, in order.
func decodedTitles(t *testing.T, decoded []map[string]any) []string {
	t.Helper()

	titles := make([]string, 0, len(decoded))
	for _, rule := range decoded {
		title, ok := rule["title"].(string)
		require.True(t, ok, "every rule must carry a string title")
		titles = append(titles, title)
	}

	return titles
}

func Test_RulesListCommand_tableOutput(t *testing.T) {
	configFile := newRulesFixture(t)

	result := runRulesList(t, configFile)
	require.NoError(t, result.Err)

	for _, header := range []string{"FOLDER", "GROUP", "TITLE", "UID", "TYPE", "STATE", "HEALTH", "FOR", "PAUSED", "ALERTS"} {
		assert.Contains(t, result.Stdout, header)
	}

	assert.Contains(t, result.Stdout, "probe-rule")
	assert.Contains(t, result.Stdout, "firing")
	assert.Contains(t, result.Stdout, "recording")
	assert.Contains(t, result.Stdout, "inactive")

	// Sorted by folder title, then group, then title: "Other Folder" before "Probe Folder", and
	// within the latter "probe-record" before "probe-rule".
	quietIdx := strings.Index(result.Stdout, "quiet-rule")
	recordIdx := strings.Index(result.Stdout, "probe-record")
	ruleIdx := strings.Index(result.Stdout, "probe-rule")
	require.NotEqual(t, -1, quietIdx)
	require.NotEqual(t, -1, recordIdx)
	require.NotEqual(t, -1, ruleIdx)
	assert.Less(t, quietIdx, recordIdx)
	assert.Less(t, recordIdx, ruleIdx)
}

// Test_RulesListCommand_tableOutputHidesLabels pins that the table renders only the documented
// columns: the fixture's labels, annotations, provenance and folder UIDs are all present in the
// payload and must all stay a json/yaml-only concern.
func Test_RulesListCommand_tableOutputHidesLabels(t *testing.T) {
	configFile := newRulesFixture(t)

	result := runRulesList(t, configFile)
	require.NoError(t, result.Err)

	assert.NotContains(t, result.Stdout, "probe-folder", "the table shows folder paths, not UIDs")
	assert.NotContains(t, result.Stdout, "payments-oncall", "labels are json/yaml only")
	assert.NotContains(t, result.Stdout, "team", "label keys are json/yaml only")
	assert.NotContains(t, result.Stdout, "high error rate", "annotations are json/yaml only")
	assert.NotContains(t, result.Stdout, "summary", "annotation keys are json/yaml only")
	assert.NotContains(t, result.Stdout, "api", "provenance is a contact-points column, not a rules one")

	// json output, by contrast, carries all of them.
	jsonResult := runRulesList(t, configFile, "-o", "json")
	require.NoError(t, jsonResult.Err)
	assert.Contains(t, jsonResult.Stdout, "payments-oncall")
	assert.Contains(t, jsonResult.Stdout, "high error rate")
	assert.Contains(t, jsonResult.Stdout, "probe-folder")
	assert.Contains(t, jsonResult.Stdout, `"provenance": "api"`)
}

func Test_RulesListCommand_jsonOutput(t *testing.T) {
	configFile := newRulesFixture(t)

	result := runRulesList(t, configFile, "-o", "json")
	require.NoError(t, result.Err)

	var decoded []map[string]any
	require.NoError(t, json.Unmarshal([]byte(result.Stdout), &decoded))
	require.Len(t, decoded, 3)
	assert.Equal(t, []string{"quiet-rule", "probe-record", "probe-rule"}, decodedTitles(t, decoded))

	firing := decoded[2]
	assert.Equal(t, "rule-firing", firing["uid"])
	assert.Equal(t, "firing", firing["state"])
	assert.Equal(t, "probe-folder", firing["folderUID"])
	assert.Equal(t, "Probe Folder", firing["folder"])
	assert.Equal(t, "1m", firing["for"])
	assert.InDelta(t, 1.0, firing["activeAlerts"], 0, "Normal instances must not count as active")
}

// Test_RulesListCommand_jsonEmptyListIsArray pins the non-nil slice contract: `jq '.[]'` pipes
// must keep working when nothing matches.
func Test_RulesListCommand_jsonEmptyListIsArray(t *testing.T) {
	configFile := newTestConfig(t, newTestServer(t, http.StatusOK, `[]`, emptyStates))

	result := runRulesList(t, configFile, "-o", "json")
	require.NoError(t, result.Err)
	assert.Equal(t, "[]", strings.TrimSpace(result.Stdout))
}

func Test_RulesListCommand_yamlOutput(t *testing.T) {
	configFile := newRulesFixture(t)

	result := runRulesList(t, configFile, "-o", "yaml")
	require.NoError(t, result.Err)

	var decoded []map[string]any
	require.NoError(t, goyaml.Unmarshal([]byte(result.Stdout), &decoded))
	require.Len(t, decoded, 3)
	assert.Equal(t, []string{"quiet-rule", "probe-record", "probe-rule"}, decodedTitles(t, decoded))
	// Rule carries explicit yaml tags, so the keys are the documented camelCase ones.
	assert.Equal(t, "Probe Folder", decoded[2]["folder"])
}

func Test_RulesListCommand_unknownOutputFormat(t *testing.T) {
	configFile := newRulesFixture(t)

	result := runRulesList(t, configFile, "-o", "toml")
	require.Error(t, result.Err)
	assert.Contains(t, result.Err.Error(), "unknown output format")
}

func Test_RulesListCommand_invalidState(t *testing.T) {
	configFile := newRulesFixture(t)

	result := runRulesList(t, configFile, "--state", "bogus")
	require.Error(t, result.Err)
	assert.Contains(t, result.Err.Error(), `invalid rule state "bogus"`)
	assert.Contains(t, result.Err.Error(), "firing")
	assert.Contains(t, result.Err.Error(), "normal")
	// Validation runs before LoadConfig and before any network call, so no rule ever renders.
	assert.NotContains(t, result.Stdout, "probe-rule")
}

func Test_RulesListCommand_filters(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want []string
	}{
		{name: "state firing", args: []string{"--state", "firing"}, want: []string{"probe-rule"}},
		{name: "state mixed case", args: []string{"--state", "Firing"}, want: []string{"probe-rule"}},
		{name: "state normal alias", args: []string{"--state", "normal"}, want: []string{"quiet-rule"}},
		{name: "state recording", args: []string{"--state", "recording"}, want: []string{"probe-record"}},
		{name: "state unknown", args: []string{"--state", "unknown"}, want: []string{}},
		{name: "state unavailable", args: []string{"--state", "unavailable"}, want: []string{}},
		{name: "folder by uid", args: []string{"--folder", "probe-folder"}, want: []string{"probe-record", "probe-rule"}},
		{name: "folder by path", args: []string{"--folder", "Probe Folder"}, want: []string{"probe-record", "probe-rule"}},
		{
			name: "nested folder by full path",
			args: []string{"--folder", "Parent Folder/Other Folder"},
			want: []string{"quiet-rule"},
		},
		{name: "nested folder by last segment", args: []string{"--folder", "Other Folder"}, want: []string{"quiet-rule"}},
		{name: "nested folder parent alone", args: []string{"--folder", "Parent Folder"}, want: []string{}},
		{name: "folder wrong case", args: []string{"--folder", "probe folder"}, want: []string{}},
		{
			name: "folder and state",
			args: []string{"--folder", "Probe Folder", "--state", "firing"},
			want: []string{"probe-rule"},
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			configFile := newRulesFixture(t)

			result := runRulesList(t, configFile, append([]string{"-o", "json"}, testCase.args...)...)
			require.NoError(t, result.Err)

			var decoded []map[string]any
			require.NoError(t, json.Unmarshal([]byte(result.Stdout), &decoded))
			assert.Equal(t, testCase.want, decodedTitles(t, decoded))
		})
	}
}

// Test_RulesListCommand_apiError500 pins the exit-code contract for a non-auth failure: the
// error surfaces with the status code and no explicit exit code, i.e. the default exit 1.
func Test_RulesListCommand_apiError500(t *testing.T) {
	configFile := newTestConfig(t,
		newTestServer(t, http.StatusInternalServerError, `{"message":"boom"}`, ruleStates))

	result := runRulesList(t, configFile)
	require.Error(t, result.Err)
	assert.Contains(t, result.Err.Error(), "list alert rules")
	assert.Contains(t, result.Err.Error(), "status 500")
	assert.Nil(t, fail.ErrorToDetailedError(result.Err).ExitCode, "a 500 must not claim the auth exit code")
}

// Test_RulesListCommand_apiError401 is the stale-session regression test: a dead session must
// still render as exit 2 through the whole command stack.
func Test_RulesListCommand_apiError401(t *testing.T) {
	configFile := newTestConfig(t,
		newTestServer(t, http.StatusUnauthorized, `{"message":"Unauthorized"}`, ruleStates))

	result := runRulesList(t, configFile)
	require.Error(t, result.Err)

	detailed := fail.ErrorToDetailedError(result.Err)
	require.NotNil(t, detailed.ExitCode)
	assert.Equal(t, 2, *detailed.ExitCode)
	assert.Contains(t, detailed.Summary, "stale or unauthorized")
}

// Test_RulesListCommand_stateEndpoint401WithFilter is the command-level precedence test: when the
// *state* endpoint is the one answering 401 and a state-dependent filter is set, the two failure
// branches compete, and the session one has to win end to end - exit 2 and the stale-session
// summary, not the exit-1 "cannot apply <filter>" message that would send the user off to retry
// with fewer flags while their cookie is dead.
func Test_RulesListCommand_stateEndpoint401WithFilter(t *testing.T) {
	cases := map[string][]string{
		"state firing":   {"--state", "firing"},
		"folder by path": {"--folder", "Probe Folder"},
	}

	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			server := newTestServerWithStateStatus(t,
				http.StatusOK, provisionedRules,
				http.StatusUnauthorized, `{"message":"Unauthorized"}`)

			configFile := newTestConfig(t, server)

			result := runRulesList(t, configFile, args...)
			require.Error(t, result.Err)

			detailed := fail.ErrorToDetailedError(result.Err)
			require.NotNil(t, detailed.ExitCode)
			assert.Equal(t, 2, *detailed.ExitCode)
			assert.Contains(t, detailed.Summary, "stale or unauthorized")

			require.NotContains(t, result.Err.Error(), "cannot apply",
				"a dead session must not be reported as an unanswerable filter")
		})
	}
}

// Test_RulesListCommand_loadConfigFailure pins that a missing config file fails before anything
// else: the fake keychain store is installed so the failure cannot come from a real-Keychain
// prompt or timeout, and the error must be the plain exit-1 kind (no auth exit code claimed).
func Test_RulesListCommand_loadConfigFailure(t *testing.T) {
	t.Cleanup(config.SetKeychainStore(testutils.NewFakeKeychainStore()))

	result := runRulesList(t, "/nonexistent/path/config.yaml")
	require.Error(t, result.Err)
	assert.Contains(t, result.Err.Error(), "/nonexistent/path/config.yaml")
	assert.Nil(t, fail.ErrorToDetailedError(result.Err).ExitCode, "a config error must not claim the auth exit code")
	assert.NotContains(t, result.Stdout, "FOLDER", "no rule table may be rendered when the config never loaded")
}

// runRulesListCapturingLogs runs `alerts rules list` the way the real binary does with respect to
// logging: cmd/grafanapi/root installs a logs.NewHandler(os.Stderr) logger in the command's
// context, so here the same handler writes into a stderr buffer that is also the command's error
// writer. Stdout and that stderr buffer are returned separately, which is what lets a test prove
// a warning cannot land in an `-o json` pipe.
func runRulesListCapturingLogs(t *testing.T, configFile string, args ...string) (string, string, error) {
	t.Helper()

	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	logger := logging.NewSLogLogger(logs.NewHandler(stderr, &logs.Options{Level: slog.LevelWarn}))

	cmd := alerts.Command()
	cmd.SilenceErrors = true
	cmd.SilenceUsage = true
	cmd.SetOut(stdout)
	cmd.SetErr(stderr)
	cmd.SetContext(logging.Context(t.Context(), logger))
	cmd.SetArgs(append([]string{"rules", "list", "--config", configFile}, args...))

	err := cmd.Execute()

	return stdout.String(), stderr.String(), err
}

// Test_RulesListCommand_degradedStateEndpoint is the end-to-end degraded-mode test: an
// *unfiltered* list survives a non-401 state failure, exiting 0 with every alerting rule listed as
// "unavailable", and its warning must go to stderr only - `-o json` output has to stay
// machine-readable for a jq pipe.
//
// The exit-0 rows are the reason the state has to be its own value: stderr is invisible to
// `... -o json | jq 'select(.state == "firing")'`, so the only thing that can tell that pipe the
// state half was never fetched is a `"state":"unavailable"` on the rows themselves. Filtered lists
// are the test below.
func Test_RulesListCommand_degradedStateEndpoint(t *testing.T) {
	server := newTestServerWithStateStatus(t,
		http.StatusOK, provisionedRules,
		http.StatusInternalServerError, `{"message":"boom"}`)

	configFile := newTestConfig(t, server)

	stdout, stderr, err := runRulesListCapturingLogs(t, configFile, "-o", "json")
	require.NoError(t, err)

	var decoded []map[string]any
	require.NoError(t, json.Unmarshal([]byte(stdout), &decoded), "stdout must stay valid JSON in degraded mode")
	require.Len(t, decoded, 3)

	for _, rule := range decoded {
		assert.NotContains(t, rule, "folder", "folder paths come only from the state endpoint")
	}

	// Every alerting row carries the degraded marker, and no row is reported as the "unknown" a
	// healthy state view would have produced.
	for _, rule := range decoded {
		if rule["type"] == "recording" {
			assert.Equal(t, "recording", rule["state"], "a recording rule needs no state view")

			continue
		}

		assert.Equal(t, "unavailable", rule["state"])
	}

	assert.NotContains(t, stdout, "unknown")
	assert.NotContains(t, stdout, "alert rule state unavailable", "the warning must not pollute stdout")

	assert.Contains(t, stderr, "alert rule state unavailable")
	assert.Contains(t, stderr, "FOLDER is empty")
}

// Test_RulesListCommand_degradedStateEndpointFiltered is the false-negative regression test at the
// command level: during a state-endpoint outage a state-dependent filter must fail, not print `[]`
// and exit 0 - the piped recipes in the README and the skill cannot tell the difference between
// "nothing is firing" and "the state view was never fetched".
//
// The two values the join assigns without any state view - "recording" and "unavailable" - stay
// usable, and the failure must be the plain exit-1 kind: the session is not the problem.
func Test_RulesListCommand_degradedStateEndpointFiltered(t *testing.T) {
	cases := []struct {
		name  string
		args  []string
		want  string
		fails bool
	}{
		{name: "state firing", args: []string{"--state", "firing"}, want: "--state firing", fails: true},
		{name: "state normal alias", args: []string{"--state", "normal"}, want: "--state inactive", fails: true},
		{name: "folder by path", args: []string{"--folder", "Probe Folder"}, want: "--folder", fails: true},
		{name: "folder by uid", args: []string{"--folder", "probe-folder"}, want: "--folder", fails: true},
		// "unknown" means "read the state view, found no entry", so it cannot be answered without
		// that view either - answering it here would return every alerting rule under a filter
		// the user meant as "not yet evaluated".
		{name: "state unknown", args: []string{"--state", "unknown"}, want: "--state unknown", fails: true},
		{name: "state unavailable still degrades", args: []string{"--state", "unavailable"}},
		{name: "state recording still degrades", args: []string{"--state", "recording"}},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			server := newTestServerWithStateStatus(t,
				http.StatusOK, provisionedRules,
				http.StatusInternalServerError, `{"message":"boom"}`)

			configFile := newTestConfig(t, server)

			result := runRulesList(t, configFile, append([]string{"-o", "json"}, testCase.args...)...)

			if !testCase.fails {
				require.NoError(t, result.Err)

				var decoded []map[string]any
				require.NoError(t, json.Unmarshal([]byte(result.Stdout), &decoded))
				assert.NotEmpty(t, decoded, "a state-independent filter still answers in degraded mode")

				for _, rule := range decoded {
					assert.NotEqual(t, "unknown", rule["state"],
						"a degraded row must never claim the state view answered")
				}

				return
			}

			require.Error(t, result.Err)
			assert.Contains(t, result.Err.Error(), "cannot apply "+testCase.want)
			assert.Contains(t, result.Err.Error(), "unexpected status 500")
			// Nothing is encoded at all: no rule, and no empty result set for a pipe to read as
			// an answer. (Stdout still carries Cobra's usage text, which the harness prints.)
			assert.NotContains(t, result.Stdout, "probe-rule")

			var decoded []map[string]any
			require.Error(t, json.Unmarshal([]byte(result.Stdout), &decoded),
				"a failed filter must not leave anything a jq pipe would accept on stdout")
			assert.Nil(t, fail.ErrorToDetailedError(result.Err).ExitCode,
				"a state outage must not claim the auth exit code")
		})
	}
}

// Test_RulesListCommand_contextFlag exercises the group's persistent --context flag: naming the
// (only) context explicitly must behave exactly like relying on current-context.
func Test_RulesListCommand_contextFlag(t *testing.T) {
	configFile := newRulesFixture(t)

	result := runRulesList(t, configFile, "--context", "test", "-o", "json")
	require.NoError(t, result.Err)

	var decoded []map[string]any
	require.NoError(t, json.Unmarshal([]byte(result.Stdout), &decoded))
	assert.Len(t, decoded, 3)

	missing := runRulesList(t, configFile, "--context", "nope")
	require.Error(t, missing.Err)
}

// newContactPointsServer scripts only the contact-points endpoint, failing the test on a request
// to any other path. Handlers use assert (not require) since they run on the server's own
// goroutine, where a FailNow-based require would only abort the handler.
func newContactPointsServer(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()

	mux := http.NewServeMux()
	mux.HandleFunc(contactPointsPath, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, err := w.Write([]byte(body))
		assert.NoError(t, err)
	})
	mux.HandleFunc(rotatePath, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	})
	mux.HandleFunc("/", failUnexpectedPath(t))

	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	return server
}

// runContactPointsList executes `alerts contact-points list` with args appended to --config.
func runContactPointsList(t *testing.T, configFile string, args ...string) testutils.CommandResult {
	t.Helper()

	var result testutils.CommandResult

	testCase := testutils.CommandTestCase{
		Cmd:     alerts.Command(),
		Command: append([]string{"contact-points", "list", "--config", configFile}, args...),
		Assertions: []testutils.CommandAssertion{
			func(_ *testing.T, res testutils.CommandResult) { result = res },
		},
	}
	testCase.Run(t)

	return result
}

// newContactPointsFixture is the happy-path setup: the contact-points endpoint serving
// contactPointsBody, plus a matching config file.
func newContactPointsFixture(t *testing.T) string {
	t.Helper()

	return newTestConfig(t, newContactPointsServer(t, http.StatusOK, contactPointsBody))
}

func Test_ContactPointsListCommand_tableOutput(t *testing.T) {
	configFile := newContactPointsFixture(t)

	result := runContactPointsList(t, configFile)
	require.NoError(t, result.Err)

	for _, header := range []string{"NAME", "TYPE", "UID", "PROVENANCE"} {
		assert.Contains(t, result.Stdout, header)
	}

	// Sorted by name: the UID-less email receiver first.
	emailIdx := strings.Index(result.Stdout, "email receiver")
	slackIdx := strings.Index(result.Stdout, "probe-slack")
	require.NotEqual(t, -1, emailIdx)
	require.NotEqual(t, -1, slackIdx)
	assert.Less(t, emailIdx, slackIdx)

	assert.Contains(t, result.Stdout, "cp-uid-1")
	assert.Contains(t, result.Stdout, "api")

	// Settings are a json/yaml-only concern: neither the redacted secret nor the recipient that
	// sits next to it belongs in the table.
	assert.NotContains(t, result.Stdout, "REDACTED")
	assert.NotContains(t, result.Stdout, "#probe")
}

// Test_ContactPointsListCommand_jsonOutput pins that json output carries the whole settings
// object, secrets included as the server's own "[REDACTED]" placeholder.
func Test_ContactPointsListCommand_jsonOutput(t *testing.T) {
	configFile := newContactPointsFixture(t)

	result := runContactPointsList(t, configFile, "-o", "json")
	require.NoError(t, result.Err)

	var decoded []map[string]any
	require.NoError(t, json.Unmarshal([]byte(result.Stdout), &decoded))
	require.Len(t, decoded, 2)

	assert.Equal(t, "email receiver", decoded[0]["name"])
	assert.Equal(t, "probe-slack", decoded[1]["name"])
	assert.Equal(t, "slack", decoded[1]["type"])
	assert.Equal(t, "cp-uid-1", decoded[1]["uid"])
	assert.Equal(t, "api", decoded[1]["provenance"])

	settings, ok := decoded[1]["settings"].(map[string]any)
	require.True(t, ok, "json output must carry the settings object")
	assert.Equal(t, "[REDACTED]", settings["url"])
}

func Test_ContactPointsListCommand_jsonEmptyListIsArray(t *testing.T) {
	configFile := newTestConfig(t, newContactPointsServer(t, http.StatusOK, `[]`))

	result := runContactPointsList(t, configFile, "-o", "json")
	require.NoError(t, result.Err)
	assert.Equal(t, "[]", strings.TrimSpace(result.Stdout))
}

// Test_ContactPointsListCommand_yamlOutput pins the yaml key shape of the generated model, which
// carries json tags but no yaml tags. goccy/go-yaml falls back to the json tag when a field has
// no yaml tag, so the keys are the same camelCase ones json output uses (and `omitempty` is
// honoured, so the UID-less receiver has no `uid` key at all) - it is NOT the lowercased Go field
// names the plan expected. Asserted here so a codec-option change (internal/format's
// yaml.UseJSONMarshaler / tag fallback) cannot silently rename every key.
func Test_ContactPointsListCommand_yamlOutput(t *testing.T) {
	configFile := newContactPointsFixture(t)

	result := runContactPointsList(t, configFile, "-o", "yaml")
	require.NoError(t, result.Err)

	var decoded []map[string]any
	require.NoError(t, goyaml.Unmarshal([]byte(result.Stdout), &decoded))
	require.Len(t, decoded, 2)

	assert.Equal(t, "probe-slack", decoded[1]["name"])
	assert.Equal(t, "slack", decoded[1]["type"])
	assert.Equal(t, "api", decoded[1]["provenance"])
	assert.Equal(t, "cp-uid-1", decoded[1]["uid"])

	assert.Equal(t, true, decoded[0]["disableResolveMessage"])
	assert.NotContains(t, decoded[0], "disableresolvemessage", "the json tag wins over the Go field name")
	assert.NotContains(t, decoded[0], "uid", "omitempty drops the UID-less receiver's empty uid")

	settings, ok := decoded[1]["settings"].(map[string]any)
	require.True(t, ok, "yaml output must carry the settings object")
	assert.Equal(t, "[REDACTED]", settings["url"])
}

func Test_ContactPointsListCommand_unknownOutputFormat(t *testing.T) {
	configFile := newContactPointsFixture(t)

	result := runContactPointsList(t, configFile, "-o", "toml")
	require.Error(t, result.Err)
	assert.Contains(t, result.Err.Error(), "unknown output format")
}

func Test_ContactPointsListCommand_apiError500(t *testing.T) {
	configFile := newTestConfig(t,
		newContactPointsServer(t, http.StatusInternalServerError, `{"message":"boom"}`))

	result := runContactPointsList(t, configFile)
	require.Error(t, result.Err)
	assert.Contains(t, result.Err.Error(), "list contact points")
	assert.Contains(t, result.Err.Error(), "status 500")
	assert.Nil(t, fail.ErrorToDetailedError(result.Err).ExitCode, "a 500 must not claim the auth exit code")
}

// Test_ContactPointsListCommand_apiError401 is the stale-session regression test for the second
// subcommand: a dead session must render as exit 2 here too.
func Test_ContactPointsListCommand_apiError401(t *testing.T) {
	configFile := newTestConfig(t,
		newContactPointsServer(t, http.StatusUnauthorized, `{"message":"Unauthorized"}`))

	result := runContactPointsList(t, configFile)
	require.Error(t, result.Err)

	detailed := fail.ErrorToDetailedError(result.Err)
	require.NotNil(t, detailed.ExitCode)
	assert.Equal(t, 2, *detailed.ExitCode)
	assert.Contains(t, detailed.Summary, "stale or unauthorized")
}

func Test_ContactPointsListCommand_loadConfigFailure(t *testing.T) {
	t.Cleanup(config.SetKeychainStore(testutils.NewFakeKeychainStore()))

	result := runContactPointsList(t, "/nonexistent/path/config.yaml")
	require.Error(t, result.Err)
	assert.Contains(t, result.Err.Error(), "/nonexistent/path/config.yaml")
	assert.Nil(t, fail.ErrorToDetailedError(result.Err).ExitCode, "a config error must not claim the auth exit code")
	assert.NotContains(t, result.Stdout, "PROVENANCE", "no contact-point table may be rendered when the config never loaded")
}

// Test_AlertsCommand_parentsPrintHelp pins that no parent has a RunE: running them bare
// prints help and exits 0 rather than erroring.
func Test_AlertsCommand_parentsPrintHelp(t *testing.T) {
	cases := map[string][]string{
		"group":          {},
		"rules":          {"rules"},
		"contact-points": {"contact-points"},
		"--help":         {"--help"},
	}

	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			testCase := testutils.CommandTestCase{
				Cmd:     alerts.Command(),
				Command: args,
				Assertions: []testutils.CommandAssertion{
					testutils.CommandSuccess(),
					testutils.CommandOutputContains("Usage:"),
				},
			}
			testCase.Run(t)
		})
	}
}
