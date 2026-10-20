package alerts_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-openapi/runtime"
	"github.com/grafana/grafanapi/internal/alerts"
	"github.com/grafana/grafanapi/internal/config"
	"github.com/grafana/grafanapi/internal/keychain"
	"github.com/grafana/grafanapi/internal/testutils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// statePath is the endpoint FetchRuleStates hits; asserted in the handlers below so a change to
// buildStateURL cannot silently start querying the wrong URL.
const statePath = "/api/prometheus/grafana/api/v1/rules"

// nopStore is a keychain.Store that discards writes - used by tests whose SessionSource must
// persist a rotated cookie somewhere, but do not care where.
type nopStore struct{}

func (nopStore) Set(string, string) error   { return nil }
func (nopStore) Get(string) (string, error) { return "", keychain.ErrNotFound }
func (nopStore) Delete(string) error        { return nil }

// ModifiedAt is a trivial stub satisfying the keychain.Store interface: these tests never
// exercise last-rotation-time lookups.
func (nopStore) ModifiedAt(string) (time.Time, error) { return time.Time{}, keychain.ErrNotFound }

// stateServer serves body at statePath with status, recording the request headers it saw.
func stateServer(t *testing.T, status int, body string) (*httptest.Server, *http.Header) {
	t.Helper()

	var seen http.Header

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodGet, r.Method)
		assert.Equal(t, statePath, r.URL.Path)
		// No limit_alerts: the instance arrays are the only exact source for ActiveAlerts, so
		// asking Grafana to drop them would break the count. Pinned here rather than left to the
		// one URL-building test, because it is a correctness precondition of every fixture below.
		assert.Empty(t, r.URL.RawQuery, "the request must not limit the alert instances it needs")

		seen = r.Header.Clone()

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)

		_, err := w.Write([]byte(body))
		assert.NoError(t, err)
	}))
	t.Cleanup(server.Close)

	return server, &seen
}

// TestFetchRuleStates_Success covers the happy path against the payload shape the request
// actually asks for: per-rule `alerts` arrays, from which ActiveAlerts is counted exactly. The
// rule also carries the `totals` map that must be ignored - here it disagrees with the instances
// on purpose - and the case pins Folder coming from the group's `file` plus a recording rule in
// the same group.
func TestFetchRuleStates_Success(t *testing.T) {
	const body = `{
	  "status": "success",
	  "data": {
	    "groups": [
	      {
	        "name": "probe-group",
	        "file": "Probe Folder",
	        "folderUid": "probe-folder",
	        "interval": 60,
	        "rules": [
	          {
	            "name": "probe-rule",
	            "uid": "rule-firing",
	            "type": "alerting",
	            "state": "firing",
	            "health": "ok",
	            "lastEvaluation": "2026-09-09T10:11:12Z",
	            "totals": {"alerting": 1, "normal": 4, "pending": 1, "error": 99},
	            "alerts": [
	              {"state": "Alerting"},
	              {"state": "Pending"},
	              {"state": "Normal"},
	              {"state": "Normal"},
	              {"state": "Normal"},
	              {"state": "Normal"}
	            ]
	          },
	          {
	            "name": "probe-record",
	            "uid": "rule-recording",
	            "type": "recording",
	            "health": "ok",
	            "lastEvaluation": "0001-01-01T00:00:00Z"
	          }
	        ]
	      }
	    ]
	  }
	}`

	server, headers := stateServer(t, http.StatusOK, body)

	gCtx := &config.Context{
		Grafana: &config.GrafanaConfig{
			Server:        server.URL,
			SessionCookie: "the-cookie",
			OrgID:         7,
		},
	}

	states, err := alerts.FetchRuleStates(t.Context(), gCtx)
	require.NoError(t, err)
	require.Len(t, states.ByUID, 2)
	assert.Equal(t, map[string]string{"probe-folder": "Probe Folder"}, states.FolderPaths)

	assert.Equal(t, config.CookieHeaderValue("the-cookie"), headers.Get("Cookie"))
	assert.Equal(t, "7", headers.Get("X-Grafana-Org-Id"))

	firing := states.ByUID["rule-firing"]
	assert.Equal(t, "Probe Folder", firing.Folder)
	assert.Equal(t, "firing", firing.State)
	assert.Equal(t, "ok", firing.Health)
	assert.Equal(t, 2, firing.ActiveAlerts,
		"ActiveAlerts counts non-Normal instances, ignoring the disagreeing `totals` map")
	require.NotNil(t, firing.LastEvaluation)
	assert.Equal(t, time.Date(2026, 9, 9, 10, 11, 12, 0, time.UTC), firing.LastEvaluation.UTC())

	recording := states.ByUID["rule-recording"]
	assert.Equal(t, "Probe Folder", recording.Folder)
	assert.Empty(t, recording.State)
	assert.Zero(t, recording.ActiveAlerts)
	assert.Nil(t, recording.LastEvaluation, "a zero lastEvaluation must normalize to nil")
}

// TestFetchRuleStates_RecoveringPassesThrough pins that the recovering state (Grafana 12's
// keep-firing-for window) is carried verbatim rather than mapped onto firing or inactive.
func TestFetchRuleStates_RecoveringPassesThrough(t *testing.T) {
	const body = `{"status":"success","data":{"groups":[{"name":"g","file":"F","rules":[
	  {"name":"r","uid":"u1","type":"alerting","state":"recovering","health":"ok",
	   "alerts":[{"state":"Recovering"}]}]}]}}`

	server, _ := stateServer(t, http.StatusOK, body)

	gCtx := &config.Context{Grafana: &config.GrafanaConfig{Server: server.URL}}

	states, err := alerts.FetchRuleStates(t.Context(), gCtx)
	require.NoError(t, err)
	require.Contains(t, states.ByUID, "u1")
	assert.Equal(t, "recovering", states.ByUID["u1"].State)
	assert.Equal(t, 1, states.ByUID["u1"].ActiveAlerts)
}

func TestFetchRuleStates_EmptyGroupsYieldsEmptyMap(t *testing.T) {
	server, _ := stateServer(t, http.StatusOK, `{"status":"success","data":{"groups":[]}}`)

	gCtx := &config.Context{Grafana: &config.GrafanaConfig{Server: server.URL}}

	states, err := alerts.FetchRuleStates(t.Context(), gCtx)
	require.NoError(t, err)
	assert.Empty(t, states.ByUID)
	assert.NotNil(t, states.ByUID)
	assert.Empty(t, states.FolderPaths)
	assert.NotNil(t, states.FolderPaths)
}

// TestFetchRuleStates_FolderPaths covers the second map: the folder path is recorded per *group*,
// keyed by the group's `folderUid`, which is what lets ListRules resolve a folder path for a rule
// this response does not list at all. A group naming only one half of the mapping contributes
// nothing - an entry pointing at an empty path would look resolved while matching only an empty
// --folder.
func TestFetchRuleStates_FolderPaths(t *testing.T) {
	const body = `{"status":"success","data":{"groups":[
	  {"name":"g1","file":"Parent Folder/Child Folder","folderUid":"child-folder","rules":[
	    {"uid":"u1","type":"alerting","state":"firing"}]},
	  {"name":"g2","file":"Parent Folder/Child Folder","folderUid":"child-folder","rules":[
	    {"uid":"u2","type":"alerting","state":"inactive"}]},
	  {"name":"g3","file":"Other Folder","rules":[{"uid":"u3","type":"alerting","state":"inactive"}]},
	  {"name":"g4","folderUid":"pathless-folder","rules":[{"uid":"u4","type":"alerting","state":"inactive"}]}
	]}}`

	server, _ := stateServer(t, http.StatusOK, body)

	gCtx := &config.Context{Grafana: &config.GrafanaConfig{Server: server.URL}}

	states, err := alerts.FetchRuleStates(t.Context(), gCtx)
	require.NoError(t, err)

	assert.Equal(t, map[string]string{"child-folder": "Parent Folder/Child Folder"}, states.FolderPaths)
	assert.Len(t, states.ByUID, 4, "every rule still joins, whatever its group carried")
}

// TestFetchRuleStates_RuleWithoutUIDSkipped covers ruleStatesFrom's guard: a state entry with no
// uid cannot be joined onto a provisioning definition, so it must not occupy the "" key.
func TestFetchRuleStates_RuleWithoutUIDSkipped(t *testing.T) {
	const body = `{"status":"success","data":{"groups":[{"name":"g","file":"F","rules":[
	  {"name":"no-uid","type":"alerting","state":"inactive"},
	  {"name":"has-uid","uid":"u1","type":"alerting","state":"inactive"}]}]}}`

	server, _ := stateServer(t, http.StatusOK, body)

	gCtx := &config.Context{Grafana: &config.GrafanaConfig{Server: server.URL}}

	states, err := alerts.FetchRuleStates(t.Context(), gCtx)
	require.NoError(t, err)
	assert.Len(t, states.ByUID, 1)
	assert.NotContains(t, states.ByUID, "")
}

// TestFetchRuleStates_ActiveAlertsCountsInstances is the counting rule on its own: every
// non-Normal instance state counts, whatever its case, and an all-normal rule reports zero.
func TestFetchRuleStates_ActiveAlertsCountsInstances(t *testing.T) {
	const body = `{"status":"success","data":{"groups":[{"name":"g","file":"F","rules":[
	  {"uid":"u-mixed","type":"alerting","state":"firing","alerts":[
	    {"state":"Alerting"},{"state":"Alerting"},{"state":"Pending"},{"state":"NoData"},
	    {"state":"Error"},{"state":"Normal"},{"state":"normal"}]},
	  {"uid":"u-quiet","type":"alerting","state":"inactive","alerts":[
	    {"state":"Normal"},{"state":"Normal"},{"state":"Normal"}]}]}]}}`

	server, _ := stateServer(t, http.StatusOK, body)

	gCtx := &config.Context{Grafana: &config.GrafanaConfig{Server: server.URL}}

	states, err := alerts.FetchRuleStates(t.Context(), gCtx)
	require.NoError(t, err)
	assert.Equal(t, 5, states.ByUID["u-mixed"].ActiveAlerts, "Normal instances must not count, in either case")
	assert.Zero(t, states.ByUID["u-quiet"].ActiveAlerts)
}

// TestFetchRuleStates_ActiveAlertsIgnoresTotals is the regression test for the arithmetic this
// package deliberately does *not* do. Every fixture here is a shape observed on Grafana 12.3.2
// with exactly one alert instance per rule, so ActiveAlerts must be 1 or 0 for each - and both
// `sum(totals) - totals["normal"]` and "sum totals, skipping the error key" get at least one of
// them wrong, which is why `totals` is not decoded at all.
//
// The rows differ only in the rule's execErrState, which the endpoint does not report:
//
//	execErrState  alerts[].state      totals
//	Error         "Alerting"          {"alerting":1}
//	Error         "Error"             {"error":1}
//	OK            "Normal (Error)"    {"normal":1}
//	Alerting      "Alerting (Error)"  {"alerting":1,"error":1}
//
// The reviewer-requested {"normal":1,"error":1} shape is included too: 12.3.2 did not produce it
// for any execErrState, but a server that did must still report zero active instances for a
// lone Normal one.
func TestFetchRuleStates_ActiveAlertsIgnoresTotals(t *testing.T) {
	const body = `{"status":"success","data":{"groups":[{"name":"g","file":"F","rules":[
	  {"uid":"u-firing","type":"alerting","state":"firing",
	   "totals":{"alerting":1},"alerts":[{"state":"Alerting"}]},
	  {"uid":"u-errored","type":"alerting","state":"inactive","health":"error",
	   "totals":{"error":1},"alerts":[{"state":"Error"}]},
	  {"uid":"u-normal-error","type":"alerting","state":"inactive",
	   "totals":{"normal":1},"alerts":[{"state":"Normal (Error)"}]},
	  {"uid":"u-firing-error","type":"alerting","state":"firing","health":"error",
	   "totals":{"alerting":1,"error":1},"alerts":[{"state":"Alerting (Error)"}]},
	  {"uid":"u-normal-plus-error","type":"alerting","state":"inactive",
	   "totals":{"normal":1,"error":1},"alerts":[{"state":"Normal (Error)"}]}]}]}}`

	server, _ := stateServer(t, http.StatusOK, body)

	gCtx := &config.Context{Grafana: &config.GrafanaConfig{Server: server.URL}}

	states, err := alerts.FetchRuleStates(t.Context(), gCtx)
	require.NoError(t, err)

	assert.Equal(t, 1, states.ByUID["u-firing"].ActiveAlerts)
	assert.Equal(t, 1, states.ByUID["u-errored"].ActiveAlerts,
		`an Error instance is active even though skipping the "error" total would read 0`)
	assert.Zero(t, states.ByUID["u-normal-error"].ActiveAlerts,
		`"Normal (Error)" is a Normal instance: the state reason must be trimmed before comparing`)
	assert.Equal(t, 1, states.ByUID["u-firing-error"].ActiveAlerts,
		"one instance counted once, where sum(totals)-normal would read 2")
	assert.Zero(t, states.ByUID["u-normal-plus-error"].ActiveAlerts,
		`{"normal":1,"error":1} still describes a single Normal instance`)
}

// TestFetchRuleStates_ActiveAlertsTrimsStateReasons covers the rest of Grafana's state-reason
// vocabulary: the suffix never changes whether an instance is Normal.
func TestFetchRuleStates_ActiveAlertsTrimsStateReasons(t *testing.T) {
	const body = `{"status":"success","data":{"groups":[{"name":"g","file":"F","rules":[
	  {"uid":"u1","type":"alerting","state":"inactive","alerts":[
	    {"state":"Normal (KeepLast)"},{"state":"Normal (MissingSeries)"},{"state":"Normal (Paused)"},
	    {"state":"Alerting (KeepLast)"},{"state":"NoData (NoData)"}]}]}]}}`

	server, _ := stateServer(t, http.StatusOK, body)

	gCtx := &config.Context{Grafana: &config.GrafanaConfig{Server: server.URL}}

	states, err := alerts.FetchRuleStates(t.Context(), gCtx)
	require.NoError(t, err)
	assert.Equal(t, 2, states.ByUID["u1"].ActiveAlerts, "only the three reason-carrying Normal instances are inactive")
}

// TestFetchRuleStates_ActiveAlertsIgnoresStatelessInstances pins the malformed-payload decision:
// an instance carrying no state (or only whitespace) counts as Normal, so a rule with nothing
// firing can never report a non-zero ALERTS the user cannot explain.
func TestFetchRuleStates_ActiveAlertsIgnoresStatelessInstances(t *testing.T) {
	const body = `{"status":"success","data":{"groups":[{"name":"g","file":"F","rules":[
	  {"uid":"u-stateless","type":"alerting","state":"inactive","alerts":[
	    {"state":""},{"state":"   "},{}]},
	  {"uid":"u-mixed","type":"alerting","state":"firing","alerts":[
	    {"state":""},{"state":"Alerting"}]}]}]}}`

	server, _ := stateServer(t, http.StatusOK, body)

	gCtx := &config.Context{Grafana: &config.GrafanaConfig{Server: server.URL}}

	states, err := alerts.FetchRuleStates(t.Context(), gCtx)
	require.NoError(t, err)
	assert.Zero(t, states.ByUID["u-stateless"].ActiveAlerts, "an instance with no state is not evidence of an alert")
	assert.Equal(t, 1, states.ByUID["u-mixed"].ActiveAlerts, "the real instance still counts")
}

// TestFetchRuleStates_NestedFolderPath pins that Folder is the folder *full path* the endpoint
// reports, verified against Grafana 12.3.2: a nested folder is "Parent/Child", and a slash
// inside a folder title is escaped as `\/` within that path.
func TestFetchRuleStates_NestedFolderPath(t *testing.T) {
	const body = `{"status":"success","data":{"groups":[
	  {"name":"g1","file":"Parent Folder/Child Folder","rules":[{"uid":"u1","type":"alerting","state":"firing"}]},
	  {"name":"g2","file":"Parent Folder/a\\/b","rules":[{"uid":"u2","type":"alerting","state":"firing"}]}]}}`

	server, _ := stateServer(t, http.StatusOK, body)

	gCtx := &config.Context{Grafana: &config.GrafanaConfig{Server: server.URL}}

	states, err := alerts.FetchRuleStates(t.Context(), gCtx)
	require.NoError(t, err)
	assert.Equal(t, "Parent Folder/Child Folder", states.ByUID["u1"].Folder)
	assert.Equal(t, `Parent Folder/a\/b`, states.ByUID["u2"].Folder)
}

func TestFetchRuleStates_NonSuccessStatusErrors(t *testing.T) {
	server, _ := stateServer(t, http.StatusOK, `{"status":"error","errorType":"bad_data","error":"nope"}`)

	gCtx := &config.Context{Grafana: &config.GrafanaConfig{Server: server.URL}}

	_, err := alerts.FetchRuleStates(t.Context(), gCtx)
	require.Error(t, err)
	assert.Contains(t, err.Error(), `status "error"`)
	// The server's own diagnostic is the only explanation the user ever sees for this branch
	// (ListRules degrades it to a warning), so it has to reach the error message.
	assert.Contains(t, err.Error(), "bad_data")
	assert.Contains(t, err.Error(), "nope")
}

// TestFetchRuleStates_NonSuccessStatusPartialDiagnostic covers the two branches where the
// envelope carries only one of `errorType`/`error`. Either one on its own is still the only
// explanation the user gets, so neither may be dropped - and the message must not grow empty
// parentheses or a stray trailing colon around the missing half.
func TestFetchRuleStates_NonSuccessStatusPartialDiagnostic(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{
			name: "errorType only",
			body: `{"status":"error","errorType":"bad_data"}`,
			want: `alerts: alert rule state returned status "error" (bad_data)`,
		},
		{
			name: "error only",
			body: `{"status":"error","error":"nope"}`,
			want: `alerts: alert rule state returned status "error": nope`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server, _ := stateServer(t, http.StatusOK, tt.body)

			gCtx := &config.Context{Grafana: &config.GrafanaConfig{Server: server.URL}}

			_, err := alerts.FetchRuleStates(t.Context(), gCtx)
			require.Error(t, err)
			assert.Equal(t, tt.want, err.Error())
		})
	}
}

// TestFetchRuleStates_NonSuccessStatusWithoutDiagnostic covers the same branch for a server that
// reports a bare failing status, so the message must not grow empty parentheses or a stray colon.
func TestFetchRuleStates_NonSuccessStatusWithoutDiagnostic(t *testing.T) {
	server, _ := stateServer(t, http.StatusOK, `{"status":"error"}`)

	gCtx := &config.Context{Grafana: &config.GrafanaConfig{Server: server.URL}}

	_, err := alerts.FetchRuleStates(t.Context(), gCtx)
	require.Error(t, err)
	assert.Equal(t, `alerts: alert rule state returned status "error"`, err.Error())
}

func TestFetchRuleStates_MalformedJSONErrors(t *testing.T) {
	server, _ := stateServer(t, http.StatusOK, `{"status": not-json`)

	gCtx := &config.Context{Grafana: &config.GrafanaConfig{Server: server.URL}}

	_, err := alerts.FetchRuleStates(t.Context(), gCtx)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "decoding alert rule state")
}

// TestFetchRuleStates_NoOrgHeaderWhenUnset pins that an unset OrgID sends no header at all,
// rather than "0" - Grafana treats a literal 0 org as invalid.
func TestFetchRuleStates_NoOrgHeaderWhenUnset(t *testing.T) {
	server, headers := stateServer(t, http.StatusOK, `{"status":"success","data":{"groups":[]}}`)

	gCtx := &config.Context{Grafana: &config.GrafanaConfig{Server: server.URL}}

	_, err := alerts.FetchRuleStates(t.Context(), gCtx)
	require.NoError(t, err)
	assert.Empty(t, headers.Get("X-Grafana-Org-Id"))
}

// TestFetchRuleStates_DeadSession401 is the regression test for the 401->runtime.APIError
// mapping: once rotation itself is exhausted, FetchRuleStates must surface an unwrapped
// *runtime.APIError{Code: 401} - and in particular not a *url.Error, which
// fail.convertNetworkErrors would claim first (it runs before convertAuthErrors).
func TestFetchRuleStates_DeadSession401(t *testing.T) {
	rotateServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer rotateServer.Close()

	stateSrv, _ := stateServer(t, http.StatusUnauthorized, "unauthorized")

	src := config.NewSessionSource("dead-cookie", rotateServer.URL, nil, nopStore{}, "acct")

	gCtx := &config.Context{
		Grafana: &config.GrafanaConfig{
			Server:  stateSrv.URL,
			Session: src,
		},
	}

	_, err := alerts.FetchRuleStates(t.Context(), gCtx)
	require.Error(t, err)

	var apiErr *runtime.APIError
	require.ErrorAs(t, err, &apiErr)
	assert.Equal(t, http.StatusUnauthorized, apiErr.Code)

	var urlErr *url.Error
	assert.NotErrorAs(t, err, &urlErr, "a 401 must not be wrapped in *url.Error")
}

// TestFetchRuleStates_UnexpectedStatusTruncatesBody covers the default branch: a 500 with an
// oversized body must produce an error carrying only a bounded snippet of it.
func TestFetchRuleStates_UnexpectedStatusTruncatesBody(t *testing.T) {
	const bodyLen = 8192

	server, _ := stateServer(t, http.StatusInternalServerError, strings.Repeat("x", bodyLen))

	gCtx := &config.Context{Grafana: &config.GrafanaConfig{Server: server.URL}}

	_, err := alerts.FetchRuleStates(t.Context(), gCtx)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "500")
	assert.Contains(t, err.Error(), strings.Repeat("x", 2048))
	assert.NotContains(t, err.Error(), strings.Repeat("x", 2049))
}

func TestFetchRuleStates_UnexpectedStatusEmptyBodyOmitsTrailingColon(t *testing.T) {
	server, _ := stateServer(t, http.StatusServiceUnavailable, "")

	gCtx := &config.Context{Grafana: &config.GrafanaConfig{Server: server.URL}}

	_, err := alerts.FetchRuleStates(t.Context(), gCtx)
	require.Error(t, err)
	assert.Equal(t, "alerts: alert rule state: unexpected status 503", err.Error())
	assert.False(t, strings.HasSuffix(err.Error(), ": "), "error message must not end with a trailing colon-space")
}

// TestFetchRuleStates_NoGrafanaConfig covers both halves of the guard: no *config.Context at all,
// and a context whose Grafana half is missing.
func TestFetchRuleStates_NoGrafanaConfig(t *testing.T) {
	cases := map[string]*config.Context{
		"nil context":        nil,
		"nil grafana config": {},
	}

	for name, gCtx := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := alerts.FetchRuleStates(t.Context(), gCtx)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "no grafana context configured")
		})
	}
}

// TestFetchRuleStates_NilGoContextFailsRequestBuild covers the http.NewRequestWithContext error
// branch, which is only reachable with a nil context.Context. The nil is passed through a
// variable rather than as a literal so staticcheck's SA1012 check stays satisfied.
func TestFetchRuleStates_NilGoContextFailsRequestBuild(t *testing.T) {
	var nilCtx context.Context

	gCtx := &config.Context{Grafana: &config.GrafanaConfig{Server: "https://example.invalid"}}

	_, err := alerts.FetchRuleStates(nilCtx, gCtx)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "building request")
}

// TestFetchRuleStates_TransportErrorSurfaces covers the httputils.NewTransport error branch:
// malformed TLS material must fail the call rather than silently fall back to system trust.
func TestFetchRuleStates_TransportErrorSurfaces(t *testing.T) {
	gCtx := &config.Context{
		Grafana: &config.GrafanaConfig{
			Server: "https://example.invalid",
			TLS:    &config.TLS{CAData: []byte("not a certificate")},
		},
	}

	_, err := alerts.FetchRuleStates(t.Context(), gCtx)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "alerts:")
	assert.Contains(t, err.Error(), "ca-data")
}

// TestFetchRuleStates_ContextCancellationPropagates pins that the caller's context reaches the
// request: without it a cancelled command would keep waiting on Grafana.
func TestFetchRuleStates_ContextCancellationPropagates(t *testing.T) {
	release := make(chan struct{})

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		<-release
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(func() {
		close(release)
		server.Close()
	})

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	gCtx := &config.Context{Grafana: &config.GrafanaConfig{Server: server.URL}}

	_, err := alerts.FetchRuleStates(ctx, gCtx)
	require.Error(t, err)
	require.ErrorIs(t, err, context.Canceled)
}

// TestFetchRuleStates_RotateAndRetrySucceeds is the other half of the WrapWithSession contract
// (TestFetchRuleStates_DeadSession401 covers the exhausted case): a 401 on the first hit must be
// retried with the rotated cookie and succeed, so replacing WrapWithSession with the plain
// transport cannot pass unnoticed. Mirrors internal/explore's TestRun_BodyReplayOnRotate.
func TestFetchRuleStates_RotateAndRetrySucceeds(t *testing.T) {
	const oldCookie = "old-cookie"
	const newCookie = "new-cookie"

	rotateServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/api/user/auth-tokens/rotate", r.URL.Path)
		http.SetCookie(w, &http.Cookie{
			Name: config.SessionCookieName, Value: newCookie,
			Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode,
		})
		w.WriteHeader(http.StatusOK)
	}))
	defer rotateServer.Close()

	var hits atomic.Int32
	var secondCookie string

	stateSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, statePath, r.URL.Path)

		if hits.Add(1) == 1 {
			w.WriteHeader(http.StatusUnauthorized)

			return
		}

		secondCookie = r.Header.Get("Cookie")
		w.Header().Set("Content-Type", "application/json")
		_, err := w.Write([]byte(`{"status":"success","data":{"groups":[{"name":"g","file":"F","rules":[
		  {"uid":"u1","type":"alerting","state":"firing","alerts":[{"state":"Alerting"}]}]}]}}`))
		assert.NoError(t, err)
	}))
	defer stateSrv.Close()

	src := config.NewSessionSource(oldCookie, rotateServer.URL, nil, nopStore{}, "acct")

	gCtx := &config.Context{Grafana: &config.GrafanaConfig{Server: stateSrv.URL, Session: src}}

	states, err := alerts.FetchRuleStates(t.Context(), gCtx)
	require.NoError(t, err)

	assert.Equal(t, int32(2), hits.Load(), "the 401 must be retried exactly once")
	assert.Equal(t, config.CookieHeaderValue(newCookie), secondCookie)
	require.Contains(t, states.ByUID, "u1")
	assert.Equal(t, 1, states.ByUID["u1"].ActiveAlerts)
}

func TestFetchRuleStates_MalformedServerURLErrors(t *testing.T) {
	gCtx := &config.Context{Grafana: &config.GrafanaConfig{Server: "://not-a-valid-url"}}

	_, err := alerts.FetchRuleStates(t.Context(), gCtx)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid server address")
}

func TestFetchRuleStates_NetworkErrorOnClosedServer(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	server.Close() // closed before FetchRuleStates ever dials it

	gCtx := &config.Context{Grafana: &config.GrafanaConfig{Server: server.URL}}

	_, err := alerts.FetchRuleStates(t.Context(), gCtx)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "alert rule state request")
}

// TestFetchRuleStates_DeadSession401EmptyBodyHasNilPayload pins the other half of the 401
// branch: with no body to quote, the APIError carries no payload rather than an empty string.
func TestFetchRuleStates_DeadSession401EmptyBodyHasNilPayload(t *testing.T) {
	server, _ := stateServer(t, http.StatusUnauthorized, "")

	gCtx := &config.Context{Grafana: &config.GrafanaConfig{Server: server.URL}}

	_, err := alerts.FetchRuleStates(t.Context(), gCtx)
	require.Error(t, err)

	var apiErr *runtime.APIError
	require.ErrorAs(t, err, &apiErr)
	assert.Equal(t, http.StatusUnauthorized, apiErr.Code)
	assert.Nil(t, apiErr.Response)
}

// TestFetchRuleStates_ServerURLQueryAndFragmentDropped covers buildStateURL clearing whatever
// query and fragment the configured server URL carried: the request must be the bare endpoint,
// since a stray `limit_alerts` there would silently truncate the instances ActiveAlerts counts.
func TestFetchRuleStates_ServerURLQueryAndFragmentDropped(t *testing.T) {
	var gotQuery string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		_, err := w.Write([]byte(`{"status":"success","data":{"groups":[]}}`))
		assert.NoError(t, err)
	}))
	defer server.Close()

	gCtx := &config.Context{Grafana: &config.GrafanaConfig{Server: server.URL + "?limit_alerts=1&stale=1#frag"}}

	_, err := alerts.FetchRuleStates(t.Context(), gCtx)
	require.NoError(t, err)
	assert.Empty(t, gotQuery)
}

// TestFetchRuleStates_ServerURLPathPrefixPreserved covers buildStateURL's path preservation: a
// Grafana served under a subpath must have the endpoint appended to that prefix.
func TestFetchRuleStates_ServerURLPathPrefixPreserved(t *testing.T) {
	var gotPath string

	mux := http.NewServeMux()
	mux.HandleFunc("/grafana"+statePath, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, err := w.Write([]byte(`{"status":"success","data":{"groups":[]}}`))
		assert.NoError(t, err)
	})

	server := httptest.NewServer(mux)
	defer server.Close()

	gCtx := &config.Context{Grafana: &config.GrafanaConfig{Server: server.URL + "/grafana/"}}

	_, err := alerts.FetchRuleStates(t.Context(), gCtx)
	require.NoError(t, err)
	assert.Equal(t, "/grafana"+statePath, gotPath)
}

const stateOKBody = `{"status":"success","data":{"groups":[{"name":"g","file":"F","rules":[
  {"uid":"u1","type":"alerting","state":"firing","alerts":[{"state":"Alerting"}]}]}]}}`

func TestFetchRuleStates_RefusesOffOriginRedirect(t *testing.T) {
	trap := testutils.NewCookieTrap(t)
	server := testutils.NewRedirectServer(t, trap.URL+statePath+"?token=s3cret", http.StatusFound)
	gCtx := &config.Context{Grafana: &config.GrafanaConfig{Server: server.URL, SessionCookie: "abc123"}}

	states, err := alerts.FetchRuleStates(t.Context(), gCtx)
	require.ErrorIs(t, err, config.ErrUnsafeRedirect)
	assert.Empty(t, states.ByUID)
	assert.Zero(t, trap.Requests(), "the foreign host must never be contacted")
}

func TestFetchRuleStates_RefusesSchemeDowngradeRedirect(t *testing.T) {
	trap := testutils.NewCookieTrap(t)

	var requests atomic.Int32

	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		http.Redirect(w, r, trap.URL+statePath, http.StatusFound)
	}))
	t.Cleanup(server.Close)

	gCtx := &config.Context{Grafana: &config.GrafanaConfig{
		Server:        server.URL,
		SessionCookie: "abc123",
		TLS:           &config.TLS{Insecure: true},
	}}

	_, err := alerts.FetchRuleStates(t.Context(), gCtx)
	require.ErrorIs(t, err, config.ErrUnsafeRedirect)
	assert.Equal(t, int32(1), requests.Load())
	assert.Zero(t, trap.Requests())
}

func TestFetchRuleStates_FollowsSameOriginRedirect(t *testing.T) {
	for _, relative := range []bool{false, true} {
		kind := "absolute"
		if relative {
			kind = "relative"
		}

		t.Run(kind, func(t *testing.T) {
			var (
				cookies []string
				origin  string
			)

			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				cookies = append(cookies, r.Header.Get("Cookie"))

				if r.URL.Path == statePath {
					target := "/final/rules"
					if !relative {
						target = origin + target
					}

					w.Header().Set("Location", target)
					w.WriteHeader(http.StatusFound)

					return
				}

				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(stateOKBody))
			}))
			t.Cleanup(server.Close)

			origin = server.URL
			gCtx := &config.Context{Grafana: &config.GrafanaConfig{Server: server.URL, SessionCookie: "abc123"}}

			states, err := alerts.FetchRuleStates(t.Context(), gCtx)
			require.NoError(t, err)
			require.Contains(t, states.ByUID, "u1")

			want := config.CookieHeaderValue("abc123")
			assert.Equal(t, []string{want, want}, cookies, "the second hop must carry the cookie")
		})
	}
}
