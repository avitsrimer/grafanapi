package alerts_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-openapi/runtime"
	"github.com/grafana/grafana-app-sdk/logging"
	goapi "github.com/grafana/grafana-openapi-client-go/client"
	"github.com/grafana/grafanapi/internal/alerts"
	"github.com/grafana/grafanapi/internal/config"
	"github.com/grafana/grafanapi/internal/grafana"
	"github.com/grafana/grafanapi/internal/logs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The two provisioning endpoints the domain layer reads. They are spelled out here (rather than
// derived) so that a change to the generated client's base path or path patterns fails these
// tests instead of silently querying the wrong URL. statePath lives in state_test.go.
const (
	alertRulesPath    = "/api/v1/provisioning/alert-rules"
	contactPointsPath = "/api/v1/provisioning/contact-points"
)

// provisionedRules is the definition half of the happy-path fixture: one alerting rule that the
// state fixture below also lists, and one recording rule (Record set) that by nature has no
// evaluation state.
const provisionedRules = `[
  {
    "uid": "rule-firing",
    "title": "probe-rule",
    "folderUID": "probe-folder",
    "ruleGroup": "probe-group",
    "for": "1m",
    "isPaused": false,
    "provenance": "api",
    "labels": {"team": "probe"},
    "annotations": {"summary": "probe"},
    "updated": "2026-09-08T09:08:07Z"
  },
  {
    "uid": "rule-recording",
    "title": "probe-record",
    "folderUID": "probe-folder",
    "ruleGroup": "probe-group",
    "for": "0s",
    "record": {"from": "A", "metric": "probe_ratio"}
  }
]`

// ruleStates is the state half of the happy-path fixture. It deliberately also lists
// "rule-orphan", which has no provisioning definition, to pin that such entries are ignored.
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
            "totals": {"alerting": 1, "normal": 1},
            "alerts": [{"state": "Alerting"}, {"state": "Normal"}]
          },
          {
            "name": "probe-record",
            "uid": "rule-recording",
            "type": "recording",
            "health": "ok",
            "lastEvaluation": "2026-09-09T10:11:12Z"
          },
          {
            "name": "orphan",
            "uid": "rule-orphan",
            "type": "alerting",
            "state": "pending",
            "health": "ok"
          }
        ]
      }
    ]
  }
}`

// stubResponse is one canned HTTP response in an alertsServer route table.
type stubResponse struct {
	status int
	body   string
}

// alertsServer serves routes (path -> response) as JSON, failing the test on any request to a
// path the route table does not cover.
func alertsServer(t *testing.T, routes map[string]stubResponse) *httptest.Server {
	t.Helper()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		route, ok := routes[r.URL.Path]
		if !ok {
			assert.Fail(t, "unexpected request path", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)

			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(route.status)

		_, err := w.Write([]byte(route.body))
		assert.NoError(t, err)
	}))
	t.Cleanup(server.Close)

	return server
}

// newRulesFixture wires a context and a generated client against a server serving the
// provisioning rules and the state endpoint with the given status/body pairs.
func newRulesFixture(t *testing.T, defs, states stubResponse) (*config.Context, *goapi.GrafanaHTTPAPI) {
	t.Helper()

	server := alertsServer(t, map[string]stubResponse{
		alertRulesPath: defs,
		statePath:      states,
	})

	gCtx := &config.Context{
		Grafana: &config.GrafanaConfig{Server: server.URL, SessionCookie: "abc123"},
	}

	gClient, err := grafana.ClientFromContext(gCtx)
	require.NoError(t, err)

	return gCtx, gClient
}

func okResponse(body string) stubResponse {
	return stubResponse{status: http.StatusOK, body: body}
}

// captureLogs returns a context carrying a logger that writes into the returned buffer, wired
// exactly the way cmd/grafanapi/root wires the real one (logs.NewHandler + NewSLogLogger +
// logging.Context) except that root's writer is os.Stderr. It is what lets these tests assert on
// the degraded-mode warning, which is otherwise the only user-visible sign of degraded mode.
func captureLogs(t *testing.T) (context.Context, *bytes.Buffer) {
	t.Helper()

	buf := &bytes.Buffer{}
	logger := logging.NewSLogLogger(logs.NewHandler(buf, &logs.Options{Level: slog.LevelWarn}))

	return logging.Context(t.Context(), logger), buf
}

func TestKnownStates(t *testing.T) {
	assert.Equal(
		t,
		[]string{"firing", "pending", "recovering", "inactive", "unknown", "recording", "unavailable"},
		alerts.KnownStates(),
	)
}

// TestKnownStatesIsNotShared pins that KnownStates hands out a fresh slice, so a caller
// appending the "normal" alias to it (as the error path in ListRules does) cannot corrupt the
// list every other caller sees.
func TestKnownStatesIsNotShared(t *testing.T) {
	first := alerts.KnownStates()
	first[0] = "mutated"

	assert.Equal(t, "firing", alerts.KnownStates()[0])
}

func TestNormalizeState(t *testing.T) {
	cases := map[string]string{
		"":            "",
		"   ":         "",
		"firing":      "firing",
		"Firing":      "firing",
		"  FIRING  ":  "firing",
		"normal":      "inactive",
		"Normal":      "inactive",
		"inactive":    "inactive",
		"recovering":  "recovering",
		"bogus":       "bogus",
		" recording ": "recording",
	}

	for input, want := range cases {
		t.Run(input, func(t *testing.T) {
			assert.Equal(t, want, alerts.NormalizeState(input))
		})
	}
}

// TestListRules_Join is the happy path: definition fields from provisioning, state fields from
// the prometheus view, folder title only available from the latter, recording rules reported as
// "recording" rather than "unknown", and state entries without a definition dropped.
func TestListRules_Join(t *testing.T) {
	gCtx, gClient := newRulesFixture(t, okResponse(provisionedRules), okResponse(ruleStates))

	rules, err := alerts.ListRules(t.Context(), gCtx, gClient, alerts.ListRulesOptions{})
	require.NoError(t, err)
	require.Len(t, rules, 2, "the orphan state entry must not produce a rule")

	// Sorted by folder, group, then title: probe-record before probe-rule.
	assert.Equal(t, "probe-record", rules[0].Title)

	firing := rules[1]
	assert.Equal(t, "rule-firing", firing.UID)
	assert.Equal(t, "probe-rule", firing.Title)
	assert.Equal(t, "alerting", firing.Type)
	assert.Equal(t, "probe-folder", firing.FolderUID)
	assert.Equal(t, "Probe Folder", firing.Folder)
	assert.Equal(t, "probe-group", firing.RuleGroup)
	assert.Equal(t, "1m", firing.For)
	assert.False(t, firing.IsPaused)
	assert.Equal(t, "api", firing.Provenance)
	assert.Equal(t, map[string]string{"team": "probe"}, firing.Labels)
	assert.Equal(t, map[string]string{"summary": "probe"}, firing.Annotations)
	require.NotNil(t, firing.Updated)
	assert.Equal(t, 2026, firing.Updated.Year())
	assert.Equal(t, "firing", firing.State)
	assert.Equal(t, "ok", firing.Health)
	require.NotNil(t, firing.LastEvaluation)
	assert.Equal(t, 1, firing.ActiveAlerts, "the Normal instance must not count as active")

	recording := rules[0]
	assert.Equal(t, "recording", recording.Type)
	assert.Equal(t, "recording", recording.State, "a recording rule must never read as unknown")
	assert.Equal(t, "0s", recording.For)
	assert.Empty(t, recording.Provenance)
}

// TestListRules_UnjoinedAlertingRuleIsUnknown covers the "definitions are primary" policy: a
// rule the state endpoint does not list (never evaluated yet) is still reported.
func TestListRules_UnjoinedAlertingRuleIsUnknown(t *testing.T) {
	const defs = `[{"uid":"u1","title":"fresh","folderUID":"f1","ruleGroup":"g","for":"5m"}]`

	gCtx, gClient := newRulesFixture(t, okResponse(defs), okResponse(`{"status":"success","data":{"groups":[]}}`))

	rules, err := alerts.ListRules(t.Context(), gCtx, gClient, alerts.ListRulesOptions{})
	require.NoError(t, err)
	require.Len(t, rules, 1)

	assert.Equal(t, "unknown", rules[0].State)
	assert.Empty(t, rules[0].Folder)
	assert.Empty(t, rules[0].Health)
	assert.Nil(t, rules[0].LastEvaluation)
	assert.Zero(t, rules[0].ActiveAlerts)
}

// TestListRules_UnjoinedRuleOmitsLastEvaluation pins the omitempty on a *time.Time: an
// unevaluated rule must not report a zero timestamp in json output.
func TestListRules_UnjoinedRuleOmitsLastEvaluation(t *testing.T) {
	const defs = `[{"uid":"u1","title":"fresh","folderUID":"f1","ruleGroup":"g"}]`

	gCtx, gClient := newRulesFixture(t, okResponse(defs), okResponse(`{"status":"success","data":{"groups":[]}}`))

	rules, err := alerts.ListRules(t.Context(), gCtx, gClient, alerts.ListRulesOptions{})
	require.NoError(t, err)

	encoded, err := json.Marshal(rules)
	require.NoError(t, err)
	assert.NotContains(t, string(encoded), "lastEvaluation")
	assert.Contains(t, string(encoded), `"state":"unknown"`)
}

// TestListRules_AllNilPointersDoNotPanic covers ruleFromProvisioned's guarded derefs: every
// "required" field of models.ProvisionedAlertRule is a pointer, and a server omitting them must
// not crash the command.
func TestListRules_AllNilPointersDoNotPanic(t *testing.T) {
	gCtx, gClient := newRulesFixture(t, okResponse(`[{}]`), okResponse(`{"status":"success","data":{"groups":[]}}`))

	rules, err := alerts.ListRules(t.Context(), gCtx, gClient, alerts.ListRulesOptions{})
	require.NoError(t, err)
	require.Len(t, rules, 1)

	assert.Empty(t, rules[0].UID)
	assert.Empty(t, rules[0].Title)
	assert.Equal(t, "alerting", rules[0].Type)
	assert.Equal(t, "0s", rules[0].For)
	assert.Equal(t, "unknown", rules[0].State)
}

// TestListRules_ForFormatting is the formatDuration table, exercised through the public API
// since the helper itself is unexported.
func TestListRules_ForFormatting(t *testing.T) {
	const defs = `[
	  {"uid":"a","title":"a","for":"1m"},
	  {"uid":"b","title":"b","for":"1h"},
	  {"uid":"c","title":"c","for":"90s"},
	  {"uid":"d","title":"d","for":"0s"},
	  {"uid":"e","title":"e","for":"10s"},
	  {"uid":"f","title":"f","for":"2h30m"},
	  {"uid":"g","title":"g"},
	  {"uid":"h","title":"h","for":"-5m"}
	]`

	gCtx, gClient := newRulesFixture(t, okResponse(defs), okResponse(`{"status":"success","data":{"groups":[]}}`))

	rules, err := alerts.ListRules(t.Context(), gCtx, gClient, alerts.ListRulesOptions{})
	require.NoError(t, err)

	got := make(map[string]string, len(rules))
	for _, rule := range rules {
		got[rule.UID] = rule.For
	}

	assert.Equal(t, map[string]string{
		"a": "1m",
		"b": "1h",
		"c": "1m30s",
		"d": "0s",
		"e": "10s",
		"f": "2h30m",
		"g": "0s",
		"h": "0s",
	}, got)
}

// TestListRules_SortedByFolderGroupTitle pins the output order, which the table renderer relies
// on for readability.
func TestListRules_SortedByFolderGroupTitle(t *testing.T) {
	const defs = `[
	  {"uid":"u1","title":"z","folderUID":"f1","ruleGroup":"g1"},
	  {"uid":"u2","title":"a","folderUID":"f1","ruleGroup":"g1"},
	  {"uid":"u3","title":"a","folderUID":"f1","ruleGroup":"g0"},
	  {"uid":"u4","title":"a","folderUID":"f2","ruleGroup":"g0"}
	]`

	const states = `{"status":"success","data":{"groups":[
	  {"name":"g1","file":"Alpha","rules":[{"uid":"u1","type":"alerting","state":"inactive"},{"uid":"u2","type":"alerting","state":"inactive"}]},
	  {"name":"g0","file":"Alpha","rules":[{"uid":"u3","type":"alerting","state":"inactive"}]},
	  {"name":"g0","file":"Beta","rules":[{"uid":"u4","type":"alerting","state":"inactive"}]}
	]}}`

	gCtx, gClient := newRulesFixture(t, okResponse(defs), okResponse(states))

	rules, err := alerts.ListRules(t.Context(), gCtx, gClient, alerts.ListRulesOptions{})
	require.NoError(t, err)

	order := make([]string, 0, len(rules))
	for _, rule := range rules {
		order = append(order, rule.UID)
	}

	assert.Equal(t, []string{"u3", "u2", "u1", "u4"}, order)
}

func TestListRules_FolderFilter(t *testing.T) {
	cases := []struct {
		name   string
		folder string
		want   []string
	}{
		{name: "by uid", folder: "probe-folder", want: []string{"rule-recording", "rule-firing"}},
		{name: "by folder path", folder: "Probe Folder", want: []string{"rule-recording", "rule-firing"}},
		{name: "wrong case", folder: "probe folder", want: []string{}},
		{name: "unknown folder", folder: "nope", want: []string{}},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			gCtx, gClient := newRulesFixture(t, okResponse(provisionedRules), okResponse(ruleStates))

			rules, err := alerts.ListRules(
				t.Context(), gCtx, gClient,
				alerts.ListRulesOptions{Folder: testCase.folder},
			)
			require.NoError(t, err)

			got := make([]string, 0, len(rules))
			for _, rule := range rules {
				got = append(got, rule.UID)
			}

			assert.Equal(t, testCase.want, got, "filtered UIDs")
		})
	}
}

func TestListRules_StateFilter(t *testing.T) {
	cases := []struct {
		name  string
		state string
		want  []string
	}{
		{name: "firing", state: "firing", want: []string{"rule-firing"}},
		{name: "mixed case", state: "Firing", want: []string{"rule-firing"}},
		{name: "padded", state: "  firing ", want: []string{"rule-firing"}},
		{name: "recording", state: "recording", want: []string{"rule-recording"}},
		{name: "normal alias", state: "normal", want: []string{}},
		{name: "inactive", state: "inactive", want: []string{}},
		{name: "recovering", state: "recovering", want: []string{}},
		{name: "unknown", state: "unknown", want: []string{}},
		// Nothing is "unavailable" while the state view answers; only a failed state call
		// stamps it (see TestListRules_DegradedStateEndpoint).
		{name: "unavailable", state: "unavailable", want: []string{}},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			gCtx, gClient := newRulesFixture(t, okResponse(provisionedRules), okResponse(ruleStates))

			rules, err := alerts.ListRules(
				t.Context(), gCtx, gClient,
				alerts.ListRulesOptions{State: testCase.state},
			)
			require.NoError(t, err)

			got := make([]string, 0, len(rules))
			for _, rule := range rules {
				got = append(got, rule.UID)
			}

			assert.Equal(t, testCase.want, got, "filtered UIDs")
		})
	}
}

// TestListRules_NormalAliasMatchesInactive complements the filter table: the alias has to
// actually match a rule the state endpoint calls "inactive".
func TestListRules_NormalAliasMatchesInactive(t *testing.T) {
	const defs = `[{"uid":"u1","title":"quiet","folderUID":"f1","ruleGroup":"g"}]`
	const states = `{"status":"success","data":{"groups":[{"name":"g","file":"F","rules":[
	  {"uid":"u1","type":"alerting","state":"inactive","health":"ok"}]}]}}`

	gCtx, gClient := newRulesFixture(t, okResponse(defs), okResponse(states))

	rules, err := alerts.ListRules(t.Context(), gCtx, gClient, alerts.ListRulesOptions{State: "normal"})
	require.NoError(t, err)
	require.Len(t, rules, 1)
	assert.Equal(t, "inactive", rules[0].State)
}

func TestListRules_UnknownStateFilterErrors(t *testing.T) {
	gCtx, gClient := newRulesFixture(t, okResponse(provisionedRules), okResponse(ruleStates))

	_, err := alerts.ListRules(t.Context(), gCtx, gClient, alerts.ListRulesOptions{State: "bogus"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), `invalid rule state "bogus"`)
	assert.Contains(t, err.Error(), "firing")
	assert.Contains(t, err.Error(), "normal")
}

func TestListRules_EmptyListIsNonNil(t *testing.T) {
	gCtx, gClient := newRulesFixture(t, okResponse(`[]`), okResponse(`{"status":"success","data":{"groups":[]}}`))

	rules, err := alerts.ListRules(t.Context(), gCtx, gClient, alerts.ListRulesOptions{})
	require.NoError(t, err)
	assert.NotNil(t, rules, "-o json must emit [] rather than null")
	assert.Empty(t, rules)
}

// TestListRules_DegradedStateEndpoint covers the state-failure policy for a request that does not
// depend on state: a non-401 failure warns and continues, so definitions are still listed - with
// State "unavailable" and no folder path. "unavailable" rather than "unknown" is the load-bearing
// part: it is the only trace of the degraded run in the machine-readable output, since the warning
// goes to stderr where a jq pipe cannot see it.
//
// The two --state values the join assigns without ever consulting the state view stay answerable,
// so they degrade too. "unknown" is *not* one of them any more - it is a claim about a healthy
// state view - and its fail case lives in the test below.
func TestListRules_DegradedStateEndpoint(t *testing.T) {
	stateFailure := stubResponse{status: http.StatusInternalServerError, body: `{"message":"boom"}`}

	t.Run("rules still listed as unavailable", func(t *testing.T) {
		gCtx, gClient := newRulesFixture(t, okResponse(provisionedRules), stateFailure)

		rules, err := alerts.ListRules(t.Context(), gCtx, gClient, alerts.ListRulesOptions{})
		require.NoError(t, err)
		require.Len(t, rules, 2)

		for _, rule := range rules {
			assert.Empty(t, rule.Folder, "folder titles are unavailable without the state endpoint")
		}

		byUID := make(map[string]string, len(rules))
		for _, rule := range rules {
			byUID[rule.UID] = rule.State
		}

		assert.Equal(t, "unavailable", byUID["rule-firing"],
			`a degraded run must be detectable in the output, not reported as the "unknown" of a healthy one`)
		assert.Equal(t, "recording", byUID["rule-recording"], "recording rules need no state endpoint")
	})

	t.Run("state unavailable is still answerable", func(t *testing.T) {
		gCtx, gClient := newRulesFixture(t, okResponse(provisionedRules), stateFailure)

		rules, err := alerts.ListRules(t.Context(), gCtx, gClient, alerts.ListRulesOptions{State: "unavailable"})
		require.NoError(t, err)
		require.Len(t, rules, 1, "every alerting rule is unavailable without the state view")
		assert.Equal(t, "rule-firing", rules[0].UID)
	})

	t.Run("state recording is still answerable", func(t *testing.T) {
		gCtx, gClient := newRulesFixture(t, okResponse(provisionedRules), stateFailure)

		rules, err := alerts.ListRules(t.Context(), gCtx, gClient, alerts.ListRulesOptions{State: "recording"})
		require.NoError(t, err)
		require.Len(t, rules, 1, "recording is decided by the definition, not by the state view")
		assert.Equal(t, "rule-recording", rules[0].UID)
	})
}

// TestListRules_UnavailableStateOnlyOnStateFailure is the other half of the two-value contract:
// while the state view answers, an alerting rule it did not list reads "unknown", never
// "unavailable". Without the distinction, `--state unknown` would silently mean "every alerting
// rule" during an outage and `-o json` would carry no sign that the state half was never fetched.
func TestListRules_UnavailableStateOnlyOnStateFailure(t *testing.T) {
	gCtx, gClient := newRulesFixture(
		t,
		okResponse(`[{"uid":"u1","title":"fresh","folderUID":"f1","ruleGroup":"g"}]`),
		okResponse(`{"status":"success","data":{"groups":[]}}`),
	)

	rules, err := alerts.ListRules(t.Context(), gCtx, gClient, alerts.ListRulesOptions{})
	require.NoError(t, err)
	require.Len(t, rules, 1)
	assert.Equal(t, "unknown", rules[0].State, "a healthy but empty state view means not-yet-evaluated")
}

// TestListRules_StateFilterIsCaseInsensitive pins that the (lowercased) --state value is compared
// against the endpoint's verbatim state case-insensitively. Grafana sends lowercase rule states
// today; if that ever changed, an exact comparison would make every --state filter match nothing
// and report it as a successful empty list.
func TestListRules_StateFilterIsCaseInsensitive(t *testing.T) {
	const defs = `[{"uid":"u1","title":"loud","folderUID":"f1","ruleGroup":"g"}]`
	const states = `{"status":"success","data":{"groups":[{"name":"g","file":"F","rules":[
	  {"uid":"u1","type":"alerting","state":"Firing","health":"ok"}]}]}}`

	gCtx, gClient := newRulesFixture(t, okResponse(defs), okResponse(states))

	rules, err := alerts.ListRules(t.Context(), gCtx, gClient, alerts.ListRulesOptions{State: "firing"})
	require.NoError(t, err)
	require.Len(t, rules, 1)
	assert.Equal(t, "Firing", rules[0].State, "the wire value is reported verbatim, only matched loosely")
}

// TestListRules_DegradedStateEndpointFailsStateDependentFilters is the false-negative regression
// test. Degrading a filtered request would apply the filter to the fabricated "unknown" state (or,
// for --folder, to folder paths that were never fetched) and return an empty list with a nil
// error: `--state firing -o json` would print [] and exit 0 during a state-endpoint outage,
// indistinguishable from "nothing is firing" and invisible to a jq pipe.
//
// The error must stay a plain one (exit 1), never a 401 (exit 2) - the session is fine here.
func TestListRules_DegradedStateEndpointFailsStateDependentFilters(t *testing.T) {
	stateFailure := stubResponse{status: http.StatusInternalServerError, body: `{"message":"boom"}`}

	cases := []struct {
		name string
		opts alerts.ListRulesOptions
		want string
	}{
		{name: "state firing", opts: alerts.ListRulesOptions{State: "firing"}, want: "--state firing"},
		// The alias is normalized before the decision, so the message names the API's own value.
		{name: "state normal alias", opts: alerts.ListRulesOptions{State: "normal"}, want: "--state inactive"},
		// "unknown" means "the state view was read and had no entry for this rule", so it needs
		// that view as much as "firing" does. Answering it in degraded mode would silently
		// redefine the filter as "every alerting rule" and exit 0; --state unavailable is the
		// filter that works during an outage.
		{name: "state unknown", opts: alerts.ListRulesOptions{State: "unknown"}, want: "--state unknown"},
		{name: "folder by path", opts: alerts.ListRulesOptions{Folder: "Probe Folder"}, want: "--folder"},
		// Even the UID form fails: a UID cannot be told apart from a path or a title, so there is
		// no honest way to answer "did this filter see everything it should have".
		{name: "folder by uid", opts: alerts.ListRulesOptions{Folder: "probe-folder"}, want: "--folder"},
		{
			name: "folder wins over state",
			opts: alerts.ListRulesOptions{Folder: "probe-folder", State: "firing"},
			want: "--folder",
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			gCtx, gClient := newRulesFixture(t, okResponse(provisionedRules), stateFailure)
			ctx, logged := captureLogs(t)

			rules, err := alerts.ListRules(ctx, gCtx, gClient, testCase.opts)
			require.Error(t, err)
			assert.Nil(t, rules)

			assert.Contains(t, err.Error(), "cannot apply "+testCase.want)
			// The underlying state failure is wrapped, so the user learns why.
			assert.Contains(t, err.Error(), "unexpected status 500")

			var apiErr *runtime.APIError
			assert.NotErrorAs(t, err, &apiErr, "a state outage must not claim the auth exit code")

			assert.NotContains(t, logged.String(), "STATE shows unavailable",
				"a failed request must not also claim it degraded")
		})
	}
}

// TestListRules_StateEndpoint401IsFatal and its provisioning twin below are the exit-2
// regression tests: a dead session must surface as a *runtime.APIError{401}, never be degraded
// into a warning.
func TestListRules_StateEndpoint401IsFatal(t *testing.T) {
	gCtx, gClient := newRulesFixture(
		t,
		okResponse(provisionedRules),
		stubResponse{status: http.StatusUnauthorized, body: `{"message":"Unauthorized"}`},
	)

	_, err := alerts.ListRules(t.Context(), gCtx, gClient, alerts.ListRulesOptions{})
	require.Error(t, err)

	var apiErr *runtime.APIError
	require.ErrorAs(t, err, &apiErr)
	assert.Equal(t, http.StatusUnauthorized, apiErr.Code)
}

// TestListRules_StateEndpoint401BeatsStateDependentFilter pins the *precedence* between the two
// state-failure branches, which TestListRules_StateEndpoint401IsFatal (unfiltered) and
// TestListRules_DegradedStateEndpointFailsStateDependentFilters (a 500) each leave open: with a
// state-dependent filter set, a 401 must still surface as the bare *runtime.APIError so
// cmd/grafanapi/fail renders the stale-session message and exit 2.
//
// The errors.As assertion alone cannot pin it - "cannot apply ...: %w" would wrap the 401 and
// still satisfy errors.As - so the message is asserted not to be the state-outage one. Otherwise a
// dead session during a filtered run would be reported as "cannot apply --state firing without the
// alert rule state", telling the user to retry a filter rather than to re-authenticate.
func TestListRules_StateEndpoint401BeatsStateDependentFilter(t *testing.T) {
	cases := []struct {
		name string
		opts alerts.ListRulesOptions
	}{
		{name: "state firing", opts: alerts.ListRulesOptions{State: "firing"}},
		{name: "folder by path", opts: alerts.ListRulesOptions{Folder: "Probe Folder"}},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			gCtx, gClient := newRulesFixture(
				t,
				okResponse(provisionedRules),
				stubResponse{status: http.StatusUnauthorized, body: `{"message":"Unauthorized"}`},
			)

			_, err := alerts.ListRules(t.Context(), gCtx, gClient, testCase.opts)
			require.Error(t, err)

			var apiErr *runtime.APIError
			require.ErrorAs(t, err, &apiErr)
			assert.Equal(t, http.StatusUnauthorized, apiErr.Code)

			require.NotContains(t, err.Error(), "cannot apply",
				"a dead session must not be reported as an unanswerable filter")
		})
	}
}

func TestListRules_Provisioning401IsFatal(t *testing.T) {
	gCtx, gClient := newRulesFixture(
		t,
		stubResponse{status: http.StatusUnauthorized, body: `{"message":"Unauthorized"}`},
		okResponse(ruleStates),
	)

	_, err := alerts.ListRules(t.Context(), gCtx, gClient, alerts.ListRulesOptions{})
	require.Error(t, err)

	var apiErr *runtime.APIError
	require.ErrorAs(t, err, &apiErr)
	assert.Equal(t, http.StatusUnauthorized, apiErr.Code)
}

func TestListRules_Provisioning500IsWrapped(t *testing.T) {
	gCtx, gClient := newRulesFixture(
		t,
		stubResponse{status: http.StatusInternalServerError, body: `{"message":"boom"}`},
		okResponse(ruleStates),
	)

	_, err := alerts.ListRules(t.Context(), gCtx, gClient, alerts.ListRulesOptions{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "list alert rules")
	assert.Contains(t, err.Error(), "status 500")
}

func TestListRules_NilClient(t *testing.T) {
	_, err := alerts.ListRules(t.Context(), nil, nil, alerts.ListRulesOptions{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no grafana client configured")
}

// TestListRules_NilContext pins the second guard: a caller passing a client but no context must
// get an error, not a plausible state-less rule list produced by degraded mode.
func TestListRules_NilContext(t *testing.T) {
	_, gClient := newRulesFixture(t, okResponse(provisionedRules), okResponse(ruleStates))

	for name, gCtx := range map[string]*config.Context{"nil context": nil, "nil grafana config": {}} {
		t.Run(name, func(t *testing.T) {
			_, err := alerts.ListRules(t.Context(), gCtx, gClient, alerts.ListRulesOptions{})
			require.Error(t, err)
			assert.Contains(t, err.Error(), "no grafana context configured")
		})
	}
}

// TestValidateState is the single validation path both the command layer and ListRules use.
func TestValidateState(t *testing.T) {
	for _, state := range append(alerts.KnownStates(), "", "  ", "Firing", alerts.NormalStateAlias) {
		t.Run("valid/"+state, func(t *testing.T) {
			normalized, err := alerts.ValidateState(state)
			require.NoError(t, err)
			assert.Equal(t, alerts.NormalizeState(state), normalized)
		})
	}

	for _, state := range []string{"bogus", "fire", "NORMALISH"} {
		t.Run("invalid/"+state, func(t *testing.T) {
			_, err := alerts.ValidateState(state)
			require.Error(t, err)
			// The value is reported as typed, and every accepted value is listed - including the
			// alias, which is not one of KnownStates.
			assert.Contains(t, err.Error(), `invalid rule state "`+state+`"`)
			assert.Contains(t, err.Error(), "firing")
			assert.Contains(t, err.Error(), alerts.NormalStateAlias)
		})
	}
}

// TestListRules_FolderFilterNestedPath is the nested-folder regression test: the state endpoint
// reports a folder as its full path ("Parent Folder/Child Folder", verified on Grafana 12.3.2),
// so --folder has to accept the UID, that path, and the path's last segment - the name the user
// sees everywhere else.
//
// It also pins folderLeaf's escaping rule against the two shapes Grafana 12.3.2 was observed to
// produce. A `/` inside a folder title is escaped as `\/`, so `Parent Folder/a\/b` is the folder
// "a/b" under "Parent Folder". A literal backslash is *not* escaped, so the child "Child" of a
// folder titled `bs\` is reported as `bs\/Child` - the exact same string a root folder titled
// `bs/Child` would produce. That ambiguity is irreducible from the payload, and it is resolved in
// favour of the escaped reading: `bs\/Child` is one segment, the title "bs/Child". A rule in a
// child of a backslash-terminated folder is therefore reachable by UID or full path but not by
// its leaf title, which the last three cases below pin.
func TestListRules_FolderFilterNestedPath(t *testing.T) {
	const defs = `[
	  {"uid":"u-nested","title":"nested","folderUID":"child-folder","ruleGroup":"g"},
	  {"uid":"u-slash","title":"slashy","folderUID":"slash-folder","ruleGroup":"g"},
	  {"uid":"u-backslash","title":"backslashy","folderUID":"bs-child","ruleGroup":"g"}
	]`

	const states = `{"status":"success","data":{"groups":[
	  {"name":"g","file":"Parent Folder/Child Folder","rules":[{"uid":"u-nested","type":"alerting","state":"firing"}]},
	  {"name":"g","file":"Parent Folder/a\\/b","rules":[{"uid":"u-slash","type":"alerting","state":"firing"}]},
	  {"name":"g","file":"bs\\/Child","rules":[{"uid":"u-backslash","type":"alerting","state":"firing"}]}]}}`

	cases := []struct {
		name   string
		folder string
		want   []string
	}{
		{name: "by uid", folder: "child-folder", want: []string{"u-nested"}},
		{name: "by full path", folder: "Parent Folder/Child Folder", want: []string{"u-nested"}},
		{name: "by last path segment", folder: "Child Folder", want: []string{"u-nested"}},
		{name: "escaped slash in title", folder: "a/b", want: []string{"u-slash"}},
		{name: "escaped path as reported", folder: `Parent Folder/a\/b`, want: []string{"u-slash"}},
		{name: "parent alone matches nothing", folder: "Parent Folder", want: []string{}},
		{name: "wrong case", folder: "child folder", want: []string{}},
		// A title ending in a backslash: the ambiguous path reads as a single escaped segment.
		{name: "backslash path by uid", folder: "bs-child", want: []string{"u-backslash"}},
		{name: "backslash path as reported", folder: `bs\/Child`, want: []string{"u-backslash"}},
		{name: "backslash path unescaped", folder: "bs/Child", want: []string{"u-backslash"}},
		{name: "backslash leaf title alone", folder: "Child", want: []string{}},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			gCtx, gClient := newRulesFixture(t, okResponse(defs), okResponse(states))

			rules, err := alerts.ListRules(
				t.Context(), gCtx, gClient,
				alerts.ListRulesOptions{Folder: testCase.folder},
			)
			require.NoError(t, err)

			got := make([]string, 0, len(rules))
			for _, rule := range rules {
				got = append(got, rule.UID)
			}

			assert.Equal(t, testCase.want, got, "filtered UIDs")
		})
	}
}

// TestListRules_FolderFilterSharedLeafTitle pins the documented consequence of accepting a leaf
// segment: it matches the folder *title*, not one folder. Grafana allows the same title under two
// different parents, so --folder "Reports" selects the rules of both - while either full path
// selects just its own folder, and the FOLDER column tells the two apart.
func TestListRules_FolderFilterSharedLeafTitle(t *testing.T) {
	const defs = `[
	  {"uid":"u-a","title":"a-rule","folderUID":"a-reports","ruleGroup":"g"},
	  {"uid":"u-b","title":"b-rule","folderUID":"b-reports","ruleGroup":"g"}
	]`

	const states = `{"status":"success","data":{"groups":[
	  {"name":"g","file":"A/Reports","rules":[{"uid":"u-a","type":"alerting","state":"firing"}]},
	  {"name":"g","file":"B/Reports","rules":[{"uid":"u-b","type":"alerting","state":"firing"}]}]}}`

	cases := []struct {
		name   string
		folder string
		want   []string
	}{
		{name: "shared leaf matches both", folder: "Reports", want: []string{"u-a", "u-b"}},
		{name: "full path selects one", folder: "A/Reports", want: []string{"u-a"}},
		{name: "other full path selects the other", folder: "B/Reports", want: []string{"u-b"}},
		{name: "uid selects one", folder: "b-reports", want: []string{"u-b"}},
		{name: "parent alone matches nothing", folder: "A", want: []string{}},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			gCtx, gClient := newRulesFixture(t, okResponse(defs), okResponse(states))

			rules, err := alerts.ListRules(
				t.Context(), gCtx, gClient,
				alerts.ListRulesOptions{Folder: testCase.folder},
			)
			require.NoError(t, err)

			got := make([]string, 0, len(rules))
			for _, rule := range rules {
				got = append(got, rule.UID)
			}

			assert.Equal(t, testCase.want, got, "filtered UIDs")
		})
	}
}

// unjoinedFolderDefs and unjoinedFolderStates are the fixture for the two tests below: a folder
// the state view does know (one of its rules is listed, one is not) and a folder it does not
// mention at all (every one of its rules unevaluated).
const unjoinedFolderDefs = `[
  {"uid":"u-joined","title":"joined","folderUID":"child-folder","ruleGroup":"g1"},
  {"uid":"u-unjoined","title":"unjoined","folderUID":"child-folder","ruleGroup":"g2"},
  {"uid":"u-lonely","title":"lonely","folderUID":"quiet-folder","ruleGroup":"g3"}
]`

const unjoinedFolderStates = `{"status":"success","data":{"groups":[
  {"name":"g1","file":"Parent Folder/Child Folder","folderUid":"child-folder","rules":[
    {"uid":"u-joined","type":"alerting","state":"firing","health":"ok"}]}]}}`

// TestListRules_FolderResolvedForUnjoinedRule is the regression test for the blank FOLDER cell: a
// rule the state view does not list (never evaluated) still belongs to a folder, and the response's
// *group* objects carry that folder's path next to its UID - so the path is resolvable by folder
// UID even with no per-rule state entry. Filling it is what keeps the documented "a rule with no
// state entry is still listed" policy honest for --folder, which the test below covers.
//
// The residual case is pinned too: a folder no group mentions at all has no path anywhere in the
// payload, so its rules keep an empty FOLDER.
func TestListRules_FolderResolvedForUnjoinedRule(t *testing.T) {
	gCtx, gClient := newRulesFixture(t, okResponse(unjoinedFolderDefs), okResponse(unjoinedFolderStates))

	rules, err := alerts.ListRules(t.Context(), gCtx, gClient, alerts.ListRulesOptions{})
	require.NoError(t, err)
	require.Len(t, rules, 3)

	byUID := make(map[string]alerts.Rule, len(rules))
	for _, rule := range rules {
		byUID[rule.UID] = rule
	}

	assert.Equal(t, "Parent Folder/Child Folder", byUID["u-joined"].Folder)
	assert.Equal(t, "firing", byUID["u-joined"].State)

	unjoined := byUID["u-unjoined"]
	assert.Equal(t, "Parent Folder/Child Folder", unjoined.Folder,
		"the folder path is resolvable by folder UID even with no state entry")
	assert.Equal(t, "unknown", unjoined.State, "resolving the folder must not fabricate a state")
	assert.Empty(t, unjoined.Health)
	assert.Nil(t, unjoined.LastEvaluation)
	assert.Zero(t, unjoined.ActiveAlerts)

	assert.Empty(t, byUID["u-lonely"].Folder,
		"a folder no group in the state response mentions has no path to resolve")
}

// TestListRules_FolderFilterMatchesUnjoinedRule is the false-negative regression test for
// --folder: with a healthy state endpoint, naming a folder by path or by leaf title used to drop
// every rule with no state entry (its Folder was empty, so only the UID form could match it),
// silently returning a short list and exiting 0.
//
// The last two cases pin the residual limit: a folder absent from the state response has no
// path anywhere in the payload, so its rules stay reachable by UID only.
func TestListRules_FolderFilterMatchesUnjoinedRule(t *testing.T) {
	cases := []struct {
		name   string
		folder string
		want   []string
	}{
		// Sorted by folder, then group: g1 before g2.
		{name: "by uid", folder: "child-folder", want: []string{"u-joined", "u-unjoined"}},
		{name: "by full path", folder: "Parent Folder/Child Folder", want: []string{"u-joined", "u-unjoined"}},
		{name: "by leaf title", folder: "Child Folder", want: []string{"u-joined", "u-unjoined"}},
		{name: "unmentioned folder by uid", folder: "quiet-folder", want: []string{"u-lonely"}},
		{name: "unmentioned folder by title", folder: "Quiet Folder", want: []string{}},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			gCtx, gClient := newRulesFixture(t, okResponse(unjoinedFolderDefs), okResponse(unjoinedFolderStates))

			rules, err := alerts.ListRules(
				t.Context(), gCtx, gClient,
				alerts.ListRulesOptions{Folder: testCase.folder},
			)
			require.NoError(t, err)

			got := make([]string, 0, len(rules))
			for _, rule := range rules {
				got = append(got, rule.UID)
			}

			assert.Equal(t, testCase.want, got, "filtered UIDs")
		})
	}
}

// TestListRules_FolderPathIgnoredWithoutFolderUID pins that the folder-path fallback is keyed on
// the group's own `folderUid`, not guessed: a state response whose groups carry only `file` (no
// folder UID at all) still joins per-rule state, but leaves an unlisted rule's FOLDER empty rather
// than attaching some other folder's path to it.
func TestListRules_FolderPathIgnoredWithoutFolderUID(t *testing.T) {
	const states = `{"status":"success","data":{"groups":[
	  {"name":"g1","file":"Parent Folder/Child Folder","rules":[
	    {"uid":"u-joined","type":"alerting","state":"firing","health":"ok"}]}]}}`

	gCtx, gClient := newRulesFixture(t, okResponse(unjoinedFolderDefs), okResponse(states))

	rules, err := alerts.ListRules(t.Context(), gCtx, gClient, alerts.ListRulesOptions{})
	require.NoError(t, err)

	folders := make(map[string]string, len(rules))
	for _, rule := range rules {
		folders[rule.UID] = rule.Folder
	}

	assert.Equal(t, "Parent Folder/Child Folder", folders["u-joined"], "the per-rule join is unaffected")
	assert.Empty(t, folders["u-unjoined"])
	assert.Empty(t, folders["u-lonely"])
}

// TestListRules_NilDefinitionElementSkipped covers the `def == nil` guard with an actual null
// element (an empty object exercises the nil-*field* derefs instead).
func TestListRules_NilDefinitionElementSkipped(t *testing.T) {
	const defs = `[null, {"uid":"u1","title":"kept","folderUID":"f1","ruleGroup":"g"}]`

	gCtx, gClient := newRulesFixture(t, okResponse(defs), okResponse(`{"status":"success","data":{"groups":[]}}`))

	rules, err := alerts.ListRules(t.Context(), gCtx, gClient, alerts.ListRulesOptions{})
	require.NoError(t, err)
	require.Len(t, rules, 1)
	assert.Equal(t, "kept", rules[0].Title)
}

// TestListRules_UnjoinedRuleOmitsUpdated pins the omitempty on Updated: the provisioning model
// marks `updated` optional, and a rule without one must not report a zero timestamp.
func TestListRules_UnjoinedRuleOmitsUpdated(t *testing.T) {
	const defs = `[{"uid":"u1","title":"fresh","folderUID":"f1","ruleGroup":"g"}]`

	gCtx, gClient := newRulesFixture(t, okResponse(defs), okResponse(`{"status":"success","data":{"groups":[]}}`))

	rules, err := alerts.ListRules(t.Context(), gCtx, gClient, alerts.ListRulesOptions{})
	require.NoError(t, err)
	require.Len(t, rules, 1)
	assert.Nil(t, rules[0].Updated)

	encoded, err := json.Marshal(rules)
	require.NoError(t, err)
	assert.NotContains(t, string(encoded), "updated")
	assert.NotContains(t, string(encoded), "0001-01-01")
}

// TestListRules_DegradedStateEndpointWarns asserts the warning itself: it is degraded mode's
// human-facing signal (the "unavailable" state being the machine-readable one), so deleting the
// Warn call must fail a test.
func TestListRules_DegradedStateEndpointWarns(t *testing.T) {
	gCtx, gClient := newRulesFixture(
		t,
		okResponse(provisionedRules),
		stubResponse{status: http.StatusInternalServerError, body: `{"message":"boom"}`},
	)

	ctx, logged := captureLogs(t)

	rules, err := alerts.ListRules(ctx, gCtx, gClient, alerts.ListRulesOptions{})
	require.NoError(t, err)
	require.Len(t, rules, 2)

	assert.Contains(t, logged.String(), "WARN")
	assert.Contains(t, logged.String(), "alert rule state unavailable")
	assert.Contains(t, logged.String(), "STATE shows unavailable")
	assert.Contains(t, logged.String(), "FOLDER is empty")
	// The underlying failure is attached, so the warning is diagnosable.
	assert.Contains(t, logged.String(), "unexpected status 500")
}

// TestListRules_DegradedStateEndpointStatusErrorWarnsWithDiagnostic covers the case degraded mode
// exists for: a 200 whose envelope reports a failure. The server's own error text must reach the
// warning, otherwise the user is told only that "something" was unavailable.
func TestListRules_DegradedStateEndpointStatusErrorWarnsWithDiagnostic(t *testing.T) {
	gCtx, gClient := newRulesFixture(
		t,
		okResponse(provisionedRules),
		okResponse(`{"status":"error","errorType":"bad_data","error":"failed to load rule state"}`),
	)

	ctx, logged := captureLogs(t)

	rules, err := alerts.ListRules(ctx, gCtx, gClient, alerts.ListRulesOptions{})
	require.NoError(t, err)
	require.Len(t, rules, 2)

	assert.Contains(t, logged.String(), "alert rule state unavailable")
	assert.Contains(t, logged.String(), "bad_data")
	assert.Contains(t, logged.String(), "failed to load rule state")
}

// TestListRules_ContextCancellationPropagates pins that the caller's context reaches the
// provisioning call: without .WithContext(ctx) a cancelled command would still fetch everything.
func TestListRules_ContextCancellationPropagates(t *testing.T) {
	gCtx, gClient := newRulesFixture(t, okResponse(provisionedRules), okResponse(ruleStates))

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, err := alerts.ListRules(ctx, gCtx, gClient, alerts.ListRulesOptions{})
	require.Error(t, err)
	require.ErrorIs(t, err, context.Canceled)
	assert.Contains(t, err.Error(), "list alert rules")
}
