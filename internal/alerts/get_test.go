package alerts_test

import (
	"net/http"
	"testing"

	"github.com/go-openapi/runtime"
	goapi "github.com/grafana/grafana-openapi-client-go/client"
	"github.com/grafana/grafanapi/internal/alerts"
	"github.com/grafana/grafanapi/internal/config"
	"github.com/grafana/grafanapi/internal/grafana"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// getRuleBody is one alerting rule as the provisioning API reports it, with the read-only keys a
// pulled bundle strips (id, updated, provenance) and a classic-condition threshold the definition
// has to carry as an integer.
const getRuleBody = `{
  "id": 7,
  "uid": "rule-firing",
  "title": "probe-rule",
  "folderUID": "probe-folder",
  "ruleGroup": "probe-group",
  "orgID": 1,
  "condition": "C",
  "for": "1m",
  "execErrState": "Error",
  "noDataState": "NoData",
  "provenance": "api",
  "updated": "2026-09-08T09:08:07Z",
  "data": [
    {"refId": "A", "datasourceUid": "ds-uid-1", "model": {"refId": "A", "id": 3}},
    {"refId": "C", "datasourceUid": "-100", "model": {"conditions": [{"evaluator": {"params": [600], "type": "gt"}}]}}
  ]
}`

const getRecordingRuleBody = `{
  "uid": "rule-recording",
  "title": "probe-record",
  "folderUID": "probe-folder",
  "ruleGroup": "probe-group",
  "for": "0s",
  "record": {"from": "A", "metric": "probe_ratio"}
}`

// newGetRuleFixture serves one rule at its provisioning address plus the state endpoint.
func newGetRuleFixture(t *testing.T, uid string, rule, states stubResponse) (*config.Context, *goapi.GrafanaHTTPAPI) {
	t.Helper()

	server := alertsServer(t, map[string]stubResponse{
		rulePath(uid): rule,
		statePath:     states,
	})

	gCtx := &config.Context{
		Grafana: &config.GrafanaConfig{Server: server.URL, SessionCookie: "abc123"},
	}

	gClient, err := grafana.ClientFromContext(gCtx)
	require.NoError(t, err)

	return gCtx, gClient
}

func TestGetRule_DefinitionCarriesTheQueriesAndTheBundleProjection(t *testing.T) {
	gCtx, gClient := newGetRuleFixture(t, "rule-firing", okResponse(getRuleBody), okResponse(ruleStates))

	detail, err := alerts.GetRule(t.Context(), gCtx, gClient, "rule-firing")
	require.NoError(t, err)

	definition, ok := detail.Definition.(map[string]any)
	require.True(t, ok, "the definition is the generic value a bundle file is encoded from")

	assert.Equal(t, "C", definition["condition"])
	assert.Equal(t, "NoData", definition["noDataState"])
	assert.Equal(t, "Error", definition["execErrState"])
	assert.Len(t, definition["data"], 2)

	for _, key := range []string{"id", "updated", "provenance"} {
		assert.NotContains(t, definition, key, "%s is read-only and a pulled file never carries it", key)
	}

	data, ok := definition["data"].([]any)
	require.True(t, ok)

	query, ok := data[0].(map[string]any)
	require.True(t, ok)

	model, ok := query["model"].(map[string]any)
	require.True(t, ok)
	assert.Contains(t, model, "id", "the strip is targeted: a query model may carry its own id")
}

func TestGetRule_IntegralThresholdStaysIntegral(t *testing.T) {
	gCtx, gClient := newGetRuleFixture(t, "rule-firing", okResponse(getRuleBody), okResponse(ruleStates))

	detail, err := alerts.GetRule(t.Context(), gCtx, gClient, "rule-firing")
	require.NoError(t, err)

	definition, ok := detail.Definition.(map[string]any)
	require.True(t, ok)

	data, ok := definition["data"].([]any)
	require.True(t, ok)

	condition, ok := data[1].(map[string]any)
	require.True(t, ok)

	model, ok := condition["model"].(map[string]any)
	require.True(t, ok)

	conditions, ok := model["conditions"].([]any)
	require.True(t, ok)

	evaluator, ok := conditions[0].(map[string]any)["evaluator"].(map[string]any)
	require.True(t, ok)

	assert.Equal(t, []any{int64(600)}, evaluator["params"], "600 must not turn into 600.0 in yaml")
}

func TestGetRule_JoinsTheStateOnUID(t *testing.T) {
	gCtx, gClient := newGetRuleFixture(t, "rule-firing", okResponse(getRuleBody), okResponse(ruleStates))

	detail, err := alerts.GetRule(t.Context(), gCtx, gClient, "rule-firing")
	require.NoError(t, err)

	assert.Equal(t, "rule-firing", detail.Rule.UID)
	assert.Equal(t, "probe-rule", detail.Rule.Title)
	assert.Equal(t, "firing", detail.Rule.State)
	assert.Equal(t, "ok", detail.Rule.Health)
	assert.Equal(t, 1, detail.Rule.ActiveAlerts)
	assert.Equal(t, "api", detail.Rule.Provenance, "the summary keeps the provenance the definition drops")
}

func TestGetRule_RecordingRule(t *testing.T) {
	gCtx, gClient := newGetRuleFixture(t, "rule-recording", okResponse(getRecordingRuleBody), okResponse(ruleStates))

	detail, err := alerts.GetRule(t.Context(), gCtx, gClient, "rule-recording")
	require.NoError(t, err)

	assert.Equal(t, "recording", detail.Rule.Type)
	assert.Equal(t, "recording", detail.Rule.State)
}

func TestGetRule_UnknownUID(t *testing.T) {
	gCtx, gClient := newGetRuleFixture(t, "rule-uid-9",
		stubResponse{status: http.StatusNotFound, body: `{"message":"not found"}`}, okResponse(ruleStates))

	_, err := alerts.GetRule(t.Context(), gCtx, gClient, "rule-uid-9")
	require.Error(t, err)
	require.ErrorIs(t, err, alerts.ErrRuleNotFound)
	assert.Equal(t, "alerts: rule rule-uid-9 not found", err.Error())
}

// TestGetRule_RuleEndpoint401IsFatal: fetchRule wraps the response, so the 401 is reachable with
// errors.As and never by identity.
func TestGetRule_RuleEndpoint401IsFatal(t *testing.T) {
	gCtx, gClient := newGetRuleFixture(t, "rule-firing",
		stubResponse{status: http.StatusUnauthorized, body: `{"message":"Unauthorized"}`}, okResponse(ruleStates))

	_, err := alerts.GetRule(t.Context(), gCtx, gClient, "rule-firing")
	require.Error(t, err)

	var apiErr *runtime.APIError
	require.ErrorAs(t, err, &apiErr)
	assert.Equal(t, http.StatusUnauthorized, apiErr.Code)
}

func TestGetRule_StateEndpoint401IsFatal(t *testing.T) {
	gCtx, gClient := newGetRuleFixture(t, "rule-firing", okResponse(getRuleBody),
		stubResponse{status: http.StatusUnauthorized, body: `{"message":"Unauthorized"}`})

	_, err := alerts.GetRule(t.Context(), gCtx, gClient, "rule-firing")
	require.Error(t, err)

	apiErr := &runtime.APIError{}
	require.ErrorAs(t, err, &apiErr)
	assert.Equal(t, http.StatusUnauthorized, apiErr.Code)
	assert.Equal(t, err, error(apiErr), "FetchRuleStates returns the 401 itself, unwrapped")
}

// TestGetRule_DegradedStateEndpoint: a state outage is a warning plus State "unavailable", exactly
// as an unfiltered `rules list` reports it - never an error and never a plausible "unknown".
func TestGetRule_DegradedStateEndpoint(t *testing.T) {
	gCtx, gClient := newGetRuleFixture(t, "rule-firing", okResponse(getRuleBody),
		stubResponse{status: http.StatusInternalServerError, body: `{"message":"boom"}`})

	ctx, logged := captureLogs(t)

	detail, err := alerts.GetRule(ctx, gCtx, gClient, "rule-firing")
	require.NoError(t, err)

	assert.Equal(t, "unavailable", detail.Rule.State)
	assert.Contains(t, logged.String(), "alert rule state unavailable")
	assert.NotNil(t, detail.Definition, "the definition does not depend on the state view")
}

func TestGetRule_UnjoinedRuleIsUnknown(t *testing.T) {
	gCtx, gClient := newGetRuleFixture(t, "rule-firing", okResponse(getRuleBody),
		okResponse(`{"status":"success","data":{"groups":[]}}`))

	detail, err := alerts.GetRule(t.Context(), gCtx, gClient, "rule-firing")
	require.NoError(t, err)
	assert.Equal(t, "unknown", detail.Rule.State)
}

func TestGetRule_NilClientAndContext(t *testing.T) {
	gCtx, gClient := newGetRuleFixture(t, "rule-firing", okResponse(getRuleBody), okResponse(ruleStates))

	_, err := alerts.GetRule(t.Context(), gCtx, nil, "rule-firing")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no grafana client configured")

	for name, nilCtx := range map[string]*config.Context{"nil context": nil, "nil grafana config": {}} {
		t.Run(name, func(t *testing.T) {
			_, err := alerts.GetRule(t.Context(), nilCtx, gClient, "rule-firing")
			require.Error(t, err)
			assert.Contains(t, err.Error(), "no grafana context configured")
		})
	}
}
