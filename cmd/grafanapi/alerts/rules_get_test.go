package alerts_test

import (
	"encoding/json"
	"net/http"
	"testing"

	goyaml "github.com/goccy/go-yaml"
	"github.com/grafana/grafanapi/cmd/grafanapi/alerts"
	"github.com/grafana/grafanapi/cmd/grafanapi/fail"
	internalalerts "github.com/grafana/grafanapi/internal/alerts"
	"github.com/grafana/grafanapi/internal/testutils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// getRuleStates lists the one rule the get tests ask about.
const getRuleStates = `{"status":"success","data":{"groups":[{"name":"probe-group","file":"Probe Folder",
  "folderUid":"probe-folder","rules":[{"name":"probe-rule","uid":"rule-uid-1","type":"alerting",
  "state":"firing","health":"ok","alerts":[{"state":"Alerting"}]}]}]}}`

// getRuleBodyFor is one provisioned rule with a classic-condition threshold, titled title. The title
// is spliced in as a JSON string body, so a test can hand it a \u escape and keep raw control and
// bidi runes out of this source file.
func getRuleBodyFor(title string) string {
	return `{"id":7,"uid":"rule-uid-1","title":"` + title + `","folderUID":"probe-folder","ruleGroup":"probe-group",
	  "orgID":1,"condition":"C","for":"1m","execErrState":"Error","noDataState":"NoData","provenance":"api",
	  "updated":"2026-09-08T09:08:07Z",
	  "data":[{"refId":"C","datasourceUid":"-100","model":{"conditions":[{"evaluator":{"params":[600],"type":"gt"}}]}}]}`
}

// setupRulesGet scripts the rule GET and the state endpoint and points a temp config at them.
func setupRulesGet(t *testing.T, rule, states pullStub) (*pushFixture, string) {
	t.Helper()

	return setupRuleOp(t, map[string]pullStub{
		pushRoute(http.MethodGet, opRulePath("rule-uid-1")): rule,
		pushRoute(http.MethodGet, statePath):                states,
	})
}

func Test_RulesGetCommand_jsonRoundTrips(t *testing.T) {
	fixture, configFile := setupRulesGet(t, okStub(getRuleBodyFor("probe-rule")), okStub(getRuleStates))

	result := runRuleOpCmd(t, configFile, "get", "rule-uid-1", "-o", "json")
	require.NoError(t, result.Err)

	var detail internalalerts.RuleDetail
	require.NoError(t, json.Unmarshal([]byte(result.Stdout), &detail))

	assert.Equal(t, "probe-rule", detail.Rule.Title)
	assert.Equal(t, "firing", detail.Rule.State)

	definition, ok := detail.Definition.(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "C", definition["condition"])
	assert.NotContains(t, definition, "id")
	assert.NotContains(t, definition, "provenance")

	assert.Empty(t, fixture.writes())
	assert.False(t, fixture.saw(http.MethodGet, alertRulesPath), "get asks for one rule, never the listing")
}

func Test_RulesGetCommand_defaultsToYAMLWithIntegralThreshold(t *testing.T) {
	_, configFile := setupRulesGet(t, okStub(getRuleBodyFor("probe-rule")), okStub(getRuleStates))

	result := runRuleOpCmd(t, configFile, "get", "rule-uid-1")
	require.NoError(t, result.Err)

	var decoded map[string]any
	require.NoError(t, goyaml.Unmarshal([]byte(result.Stdout), &decoded))
	assert.Contains(t, decoded, "rule")
	assert.Contains(t, decoded, "definition")
	assert.Contains(t, result.Stdout, "- 600\n", "an integral threshold must not be written as 600.0")
	assert.NotContains(t, result.Stdout, "600.0")
}

func Test_RulesGetCommand_tableIsAValidationError(t *testing.T) {
	fixture, configFile := setupRulesGet(t, okStub(getRuleBodyFor("probe-rule")), okStub(getRuleStates))

	result := runRuleOpCmd(t, configFile, "get", "rule-uid-1", "-o", "table")
	require.Error(t, result.Err)
	assert.Contains(t, result.Err.Error(), "unknown output format 'table'")
	assert.Empty(t, fixture.requested, "validation fails before any request")
}

func Test_RulesGetCommand_requiresExactlyOneUID(t *testing.T) {
	fixture, configFile := setupRulesGet(t, okStub(getRuleBodyFor("probe-rule")), okStub(getRuleStates))

	for name, args := range map[string][]string{
		"none": {"get"},
		"two":  {"get", "rule-uid-1", "rule-uid-2"},
	} {
		t.Run(name, func(t *testing.T) {
			result := runRuleOpCmd(t, configFile, args...)
			require.Error(t, result.Err)
			assert.Contains(t, result.Err.Error(), "arg(s)")
		})
	}

	assert.Empty(t, fixture.requested)
}

func Test_RulesGetCommand_refusesABlankUID(t *testing.T) {
	for name, uid := range map[string]string{"empty": "", "blank": " "} {
		t.Run(name, func(t *testing.T) {
			fixture, configFile := setupRulesGet(t, okStub(getRuleBodyFor("probe-rule")), okStub(getRuleStates))

			result := runRuleOpCmd(t, configFile, "get", uid)
			require.Error(t, result.Err)
			assert.Contains(t, result.Err.Error(), "UID must not be empty")
			assert.Empty(t, fixture.requested, "a blank UID must not reach the network")
		})
	}
}

func Test_RulesGetCommand_unknownUID(t *testing.T) {
	_, configFile := setupRulesGet(t,
		pullStub{status: http.StatusNotFound, body: `{"message":"not found"}`}, okStub(getRuleStates))

	result := runRuleOpCmd(t, configFile, "get", "rule-uid-1")
	require.Error(t, result.Err)
	assert.Contains(t, result.Err.Error(), "alerts: rule rule-uid-1 not found")

	detailed := fail.ErrorToDetailedError(result.Err)
	assert.Equal(t, "Alert rule not found", detailed.Summary)
	assert.Nil(t, detailed.ExitCode)
}

func Test_RulesGetCommand_degradedStateStillPrints(t *testing.T) {
	_, configFile := setupRulesGet(t, okStub(getRuleBodyFor("probe-rule")),
		pullStub{status: http.StatusInternalServerError, body: `{"message":"boom"}`})

	result := runRuleOpCmd(t, configFile, "get", "rule-uid-1", "-o", "json")
	require.NoError(t, result.Err)

	var detail internalalerts.RuleDetail
	require.NoError(t, json.Unmarshal([]byte(result.Stdout), &detail))
	assert.Equal(t, "unavailable", detail.Rule.State)
}

func Test_RulesGetCommand_apiError401(t *testing.T) {
	_, configFile := setupRulesGet(t,
		pullStub{status: http.StatusUnauthorized, body: `{"message":"Unauthorized"}`}, okStub(getRuleStates))

	result := runRuleOpCmd(t, configFile, "get", "rule-uid-1")
	require.Error(t, result.Err)

	detailed := fail.ErrorToDetailedError(result.Err)
	require.NotNil(t, detailed.ExitCode)
	assert.Equal(t, 2, *detailed.ExitCode, "a dead session is the stale-session exit code")
}

// Test_RulesGetCommand_machineFormatsStayRaw: json and yaml carry the rule's own text, as `rules
// list` does, so a title holding a control and a bidi override must decode back to exactly that
// title. The runes are written as \u escapes in the JSON fixture, never as literals in this file.
func Test_RulesGetCommand_machineFormatsStayRaw(t *testing.T) {
	_, configFile := setupRulesGet(t, okStub(getRuleBodyFor(`bell\u0007 and \u202eflip`)), okStub(getRuleStates))

	const want = "bell\u0007 and \u202eflip"

	jsonResult := runRuleOpCmd(t, configFile, "get", "rule-uid-1", "-o", "json")
	require.NoError(t, jsonResult.Err)

	var fromJSON internalalerts.RuleDetail
	require.NoError(t, json.Unmarshal([]byte(jsonResult.Stdout), &fromJSON))
	assert.Equal(t, want, fromJSON.Rule.Title)

	yamlResult := runRuleOpCmd(t, configFile, "get", "rule-uid-1", "-o", "yaml")
	require.NoError(t, yamlResult.Err)

	var fromYAML struct {
		Rule struct {
			Title string `yaml:"title"`
		} `yaml:"rule"`
	}
	require.NoError(t, goyaml.Unmarshal([]byte(yamlResult.Stdout), &fromYAML))
	assert.Equal(t, want, fromYAML.Rule.Title)
}

func Test_RulesGetCommand_help(t *testing.T) {
	testCase := testutils.CommandTestCase{
		Cmd:     alerts.Command(),
		Command: []string{"rules", "get", "--help"},
		Assertions: []testutils.CommandAssertion{
			testutils.CommandSuccess(),
			testutils.CommandOutputContains("rules get UID"),
			testutils.CommandOutputContains("Viewer role"),
		},
	}
	testCase.Run(t)
}
