package alerts_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/grafana/grafanapi/cmd/grafanapi/alerts"
	"github.com/grafana/grafanapi/cmd/grafanapi/fail"
	"github.com/grafana/grafanapi/internal/testutils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The one rule group every rule-operation test works in. Which group a rule lives in is what a
// pause rewrites, so it is spelled out once rather than passed around.
const (
	opFolderUID  = "probe-folder"
	opGroupTitle = "probe-group"
)

// opRuleBody builds one provisioned rule as Grafana reports it, carrying what a rule operation
// reads off it: the provenance that decides the X-Disable-Provenance header, and the state a pause
// or resume flips.
func opRuleBody(uid, provenance string, paused bool) string {
	return fmt.Sprintf(
		`{"uid":%q,"title":%q,"folderUID":%q,"ruleGroup":%q,"orgID":1,"isPaused":%t,"provenance":%q,
		  "condition":"C","for":"1m","execErrState":"Error","noDataState":"NoData",
		  "data":[{"refId":"A","datasourceUid":"ds-uid-1","model":{"refId":"A"}}]}`,
		uid, uid+"-title", opFolderUID, opGroupTitle, paused, provenance,
	)
}

// opGroupBody builds the rule group holding the given rules.
func opGroupBody(rules ...string) string {
	return fmt.Sprintf(
		`{"folderUid":%q,"title":%q,"interval":60,"rules":[%s]}`,
		opFolderUID, opGroupTitle, strings.Join(rules, ","),
	)
}

// opGroupPath is the provisioning address of that group.
func opGroupPath() string {
	return groupPath(opFolderUID, opGroupTitle)
}

// opRulePath is the provisioning address of one alert rule, which both the lookup every operation
// performs and the delete itself use.
func opRulePath(uid string) string {
	return alertRulesPath + "/" + uid
}

// pausedByUID decodes a rule-group write body into the isPaused flag of each rule it carries. It
// is what says whether a group PUT flipped exactly the named rules and left the group's others
// alone.
func pausedByUID(t *testing.T, body string) map[string]bool {
	t.Helper()

	var decoded struct {
		Rules []struct {
			UID      string `json:"uid"`
			IsPaused bool   `json:"isPaused"`
		} `json:"rules"`
	}

	require.NoError(t, json.Unmarshal([]byte(body), &decoded))

	paused := make(map[string]bool, len(decoded.Rules))
	for _, rule := range decoded.Rules {
		paused[rule.UID] = rule.IsPaused
	}

	return paused
}

// runRuleOpCmd executes one `alerts rules <op> UID...` invocation against configFile.
func runRuleOpCmd(t *testing.T, configFile string, args ...string) testutils.CommandResult {
	t.Helper()

	var result testutils.CommandResult

	testCase := testutils.CommandTestCase{
		Cmd:     alerts.Command(),
		Command: append(append([]string{"rules"}, args...), "--config", configFile),
		Assertions: []testutils.CommandAssertion{
			func(_ *testing.T, res testutils.CommandResult) { result = res },
		},
	}
	testCase.Run(t)

	return result
}

// setupRuleOp scripts the given routes and points a temp config at them.
func setupRuleOp(t *testing.T, routes map[string]pullStub) (*pushFixture, string) {
	t.Helper()

	fixture := newPushFixture(t, routes)
	configFile := newTestConfig(t, fixture.Server)

	return fixture, configFile
}

// Test_RulesPauseCommand_pausesAGroupOnce is the happy path and decision 7 seen from the command:
// two rules of one group are one group write, the rules nobody named keep their state, and every
// named rule gets its own result line.
func Test_RulesPauseCommand_pausesAGroupOnce(t *testing.T) {
	fixture, configFile := setupRuleOp(t, map[string]pullStub{
		pushRoute(http.MethodGet, opRulePath("rule-uid-1")): okStub(opRuleBody("rule-uid-1", "api", false)),
		pushRoute(http.MethodGet, opRulePath("rule-uid-2")): okStub(opRuleBody("rule-uid-2", "api", false)),
		pushRoute(http.MethodGet, opGroupPath()): okStub(opGroupBody(
			opRuleBody("rule-uid-1", "api", false),
			opRuleBody("rule-uid-2", "api", false),
			opRuleBody("rule-uid-3", "api", false),
		)),
		pushRoute(http.MethodPut, opGroupPath()): okStub(opGroupBody()),
	})

	result := runRuleOpCmd(t, configFile, "pause", "rule-uid-1", "rule-uid-2")
	require.NoError(t, result.Err)

	assert.Contains(t, result.Stdout, "✔ rule-uid-1 (rule-uid-1-title in probe-group) paused")
	assert.Contains(t, result.Stdout, "✔ rule-uid-2 (rule-uid-2-title in probe-group) paused")
	assert.Contains(t, result.Stdout, "2 rule(s) paused, 0 unchanged, 0 error(s)")

	writes := fixture.requestsFor(http.MethodPut, opGroupPath())
	require.Len(t, writes, 1, "two rules of one group are one group write")
	assert.Nil(t, writes[0].header, "a group Grafana has claimed keeps its claim")

	assert.Equal(t,
		map[string]bool{"rule-uid-1": true, "rule-uid-2": true, "rule-uid-3": false},
		pausedByUID(t, writes[0].body),
		"a group write must not disturb the rules nobody named",
	)
}

// Test_RulesResumeCommand_resumes is the other direction: the same wiring, the same single write,
// isPaused cleared.
func Test_RulesResumeCommand_resumes(t *testing.T) {
	fixture, configFile := setupRuleOp(t, map[string]pullStub{
		pushRoute(http.MethodGet, opRulePath("rule-uid-1")): okStub(opRuleBody("rule-uid-1", "", true)),
		pushRoute(http.MethodGet, opGroupPath()):            okStub(opGroupBody(opRuleBody("rule-uid-1", "", true))),
		pushRoute(http.MethodPut, opGroupPath()):            okStub(opGroupBody()),
	})

	result := runRuleOpCmd(t, configFile, "resume", "rule-uid-1")
	require.NoError(t, result.Err)

	assert.Contains(t, result.Stdout, "✔ rule-uid-1 (rule-uid-1-title in probe-group) resumed")
	assert.Contains(t, result.Stdout, "1 rule(s) resumed, 0 unchanged, 0 error(s)")

	writes := fixture.requestsFor(http.MethodPut, opGroupPath())
	require.Len(t, writes, 1)
	require.NotNil(t, writes[0].header, "an unclaimed group stays unclaimed")
	assert.Equal(t, "true", *writes[0].header)
	assert.Equal(t, map[string]bool{"rule-uid-1": false}, pausedByUID(t, writes[0].body))
}

// Test_RulesPauseCommand_alreadyPausedIsUnchanged pins what keeps `pause` idempotent: a rule
// already in the requested state is reported, not rewritten, and the command still exits 0.
func Test_RulesPauseCommand_alreadyPausedIsUnchanged(t *testing.T) {
	fixture, configFile := setupRuleOp(t, map[string]pullStub{
		pushRoute(http.MethodGet, opRulePath("rule-uid-1")): okStub(opRuleBody("rule-uid-1", "api", true)),
		pushRoute(http.MethodGet, opGroupPath()):            okStub(opGroupBody(opRuleBody("rule-uid-1", "api", true))),
	})

	result := runRuleOpCmd(t, configFile, "pause", "rule-uid-1")
	require.NoError(t, result.Err)

	assert.Contains(t, result.Stdout, "✔ rule-uid-1 (rule-uid-1-title in probe-group) already paused")
	assert.Contains(t, result.Stdout, "0 rule(s) paused, 1 unchanged, 0 error(s)")
	assert.Empty(t, fixture.writes(), "a rule already paused needs no write at all")
}

// Test_RulesDeleteCommand_deletes is the delete path end to end: the rule is looked up first, then
// deleted with the header its stored provenance implies - absent for a rule Grafana has claimed,
// "true" for one it has not.
func Test_RulesDeleteCommand_deletes(t *testing.T) {
	cases := map[string]struct {
		stored string
		want   *string
	}{
		"claimed by the API": {stored: "api"},
		"unclaimed":          {stored: "", want: new("true")},
	}

	for name, testCase := range cases {
		t.Run(name, func(t *testing.T) {
			fixture, configFile := setupRuleOp(t, map[string]pullStub{
				pushRoute(http.MethodGet, opRulePath("rule-uid-1")):    okStub(opRuleBody("rule-uid-1", testCase.stored, false)),
				pushRoute(http.MethodDelete, opRulePath("rule-uid-1")): {status: http.StatusNoContent},
			})

			result := runRuleOpCmd(t, configFile, "delete", "rule-uid-1")
			require.NoError(t, result.Err)

			assert.Contains(t, result.Stdout, "✔ rule-uid-1 (rule-uid-1-title in probe-group) deleted")
			assert.Contains(t, result.Stdout, "1 rule(s) deleted, 0 unchanged, 0 error(s)")

			deleted := fixture.requestsFor(http.MethodDelete, opRulePath("rule-uid-1"))
			require.Len(t, deleted, 1)
			assert.Equal(t, testCase.want, deleted[0].header)
		})
	}
}

// Test_RulesDeleteCommand_unknownUID pins the reason every operation looks a rule up first: a UID
// no rule carries has to read as a typo (exit 1) rather than as the 500 a raw DELETE answers with,
// and it must cost no write.
func Test_RulesDeleteCommand_unknownUID(t *testing.T) {
	fixture, configFile := setupRuleOp(t, map[string]pullStub{
		pushRoute(http.MethodGet, opRulePath("rule-uid-gone")): {status: http.StatusNotFound, body: `{"message":"not found"}`},
	})

	result := runRuleOpCmd(t, configFile, "delete", "rule-uid-gone")
	require.Error(t, result.Err)
	assert.Contains(t, result.Err.Error(), "1 rule(s) failed to delete")
	assert.Contains(t, result.Err.Error(), "alerts: rule rule-uid-gone not found")

	// The whole point of the exported sentinel: without its own converter arm this renders under
	// the generic "Unexpected error" heading, which reads as a bug rather than as a typo.
	detailed := fail.ErrorToDetailedError(result.Err)
	assert.Equal(t, "Alert rule not found", detailed.Summary)
	assert.Nil(t, detailed.ExitCode, "a typo must not claim the auth exit code")

	assert.Contains(t, result.Stdout, "✘ rule-uid-gone alerts: rule rule-uid-gone not found")
	assert.Contains(t, result.Stdout, "0 rule(s) deleted, 0 unchanged, 1 error(s)")
	assert.Empty(t, fixture.writes(), "an unknown uid must not delete anything")
}

// Test_RulesDeleteCommand_labelsARuleWithNoGroup covers the middle rung of the label ladder: a
// server that reports a rule's title but not the group it lives in must still name it, with empty
// parentheses nowhere in sight. Almost every field of the model is an optional pointer.
func Test_RulesDeleteCommand_labelsARuleWithNoGroup(t *testing.T) {
	_, configFile := setupRuleOp(t, map[string]pullStub{
		pushRoute(http.MethodGet, opRulePath("rule-uid-1")):    okStub(`{"uid":"rule-uid-1","title":"orphan"}`),
		pushRoute(http.MethodDelete, opRulePath("rule-uid-1")): {status: http.StatusNoContent},
	})

	result := runRuleOpCmd(t, configFile, "delete", "rule-uid-1")
	require.NoError(t, result.Err)

	assert.Contains(t, result.Stdout, "✔ rule-uid-1 (orphan) deleted")
	assert.NotContains(t, result.Stdout, "in )")
}

// Test_RulesDeleteCommand_scrubsControlCharacters pins that a whole result line gets the same
// treatment as a table cell: a rule title is arbitrary user input, and a newline inside one would
// split the line a script is reading in two - or open a forged "✔ ... deleted" line of its own.
// A right-to-left override would re-order everything printed after it without changing a byte.
//
// Both the success and the failure line are covered, because the failure line is the one that also
// renders an error text beside the label: the scrub has to cover the line, not the label.
func Test_RulesDeleteCommand_scrubsControlCharacters(t *testing.T) {
	// `line`, a newline, a forged success line, a right-to-left override and `one`.
	const forgedRule = `{"uid":"rule-uid-1","title":"line\n✔ rule-uid-9 forged\u202Eone",` +
		`"ruleGroup":"probe\tgroup"}`

	const label = "rule-uid-1 (line ✔ rule-uid-9 forged one in probe group)"

	t.Run("on a deleted rule", func(t *testing.T) {
		_, configFile := setupRuleOp(t, map[string]pullStub{
			pushRoute(http.MethodGet, opRulePath("rule-uid-1")):    okStub(forgedRule),
			pushRoute(http.MethodDelete, opRulePath("rule-uid-1")): {status: http.StatusNoContent},
		})

		result := runRuleOpCmd(t, configFile, "delete", "rule-uid-1")
		require.NoError(t, result.Err)

		assert.Contains(t, result.Stdout, "✔ "+label+" deleted")
		assert.NotContains(t, result.Stdout, "\n✔ rule-uid-9 forged")
		assert.NotContains(t, result.Stdout, "\u202e")
	})

	t.Run("on a refused rule", func(t *testing.T) {
		_, configFile := setupRuleOp(t, map[string]pullStub{
			pushRoute(http.MethodGet, opRulePath("rule-uid-1")): okStub(forgedRule),
			pushRoute(http.MethodDelete, opRulePath("rule-uid-1")): {
				status: http.StatusForbidden,
				body:   `{"message":"You'll need additional permissions"}`,
			},
		})

		result := runRuleOpCmd(t, configFile, "delete", "rule-uid-1")
		require.Error(t, result.Err)

		assert.Contains(t, result.Stdout, "✘ "+label+" alerts: delete rule rule-uid-1:")
		assert.NotContains(t, result.Stdout, "\n✔ rule-uid-9 forged")
		assert.NotContains(t, result.Stdout, "\u202e")
	})
}

// Test_RuleOpCommands_dryRunWritesNothing is the promise --dry-run makes on all three: the report
// says what would happen, in the future tense, and nothing does.
func Test_RuleOpCommands_dryRunWritesNothing(t *testing.T) {
	cases := map[string]struct {
		verb string
		want string
	}{
		"pause":  {verb: "pause", want: "would be paused"},
		"resume": {verb: "resume", want: "would be resumed"},
		"delete": {verb: "delete", want: "would be deleted"},
	}

	for name, testCase := range cases {
		t.Run(name, func(t *testing.T) {
			fixture, configFile := setupRuleOp(t, map[string]pullStub{
				pushRoute(http.MethodGet, opRulePath("rule-uid-1")): okStub(opRuleBody("rule-uid-1", "api", true)),
				pushRoute(http.MethodGet, opGroupPath()):            okStub(opGroupBody(opRuleBody("rule-uid-1", "api", true))),
			})

			result := runRuleOpCmd(t, configFile, testCase.verb, "rule-uid-1", "--dry-run")
			require.NoError(t, result.Err)

			assert.Contains(t, result.Stdout, testCase.want)
			assert.Empty(t, fixture.writes(), "a dry run must not write")
		})
	}
}

// Test_RuleOpCommands_apiError401 is the stale-session regression test for the three write
// commands: a dead session must render as exit 2 whatever the run was doing when it found out.
func Test_RuleOpCommands_apiError401(t *testing.T) {
	unauthorized := pullStub{status: http.StatusUnauthorized, body: `{"message":"Unauthorized"}`}

	cases := map[string]struct {
		args   []string
		routes map[string]pullStub
	}{
		"on the rule lookup": {
			args:   []string{"delete", "rule-uid-1"},
			routes: map[string]pullStub{pushRoute(http.MethodGet, opRulePath("rule-uid-1")): unauthorized},
		},
		"on the group write": {
			args: []string{"pause", "rule-uid-1"},
			routes: map[string]pullStub{
				pushRoute(http.MethodGet, opRulePath("rule-uid-1")): okStub(opRuleBody("rule-uid-1", "api", false)),
				pushRoute(http.MethodGet, opGroupPath()):            okStub(opGroupBody(opRuleBody("rule-uid-1", "api", false))),
				pushRoute(http.MethodPut, opGroupPath()):            unauthorized,
			},
		},
	}

	for name, testCase := range cases {
		t.Run(name, func(t *testing.T) {
			_, configFile := setupRuleOp(t, testCase.routes)

			result := runRuleOpCmd(t, configFile, testCase.args...)
			require.Error(t, result.Err)

			detailed := fail.ErrorToDetailedError(result.Err)
			require.NotNil(t, detailed.ExitCode)
			assert.Equal(t, 2, *detailed.ExitCode)
			assert.Contains(t, detailed.Summary, "stale or unauthorized")
		})
	}
}

// Test_RulesDeleteCommand_forbidden is Task 6's rendering reached through a rule operation: a write
// Grafana refuses must say the user lacks the permission, which only survives because the
// aggregate error wraps the first failure rather than counting it.
func Test_RulesDeleteCommand_forbidden(t *testing.T) {
	_, configFile := setupRuleOp(t, map[string]pullStub{
		pushRoute(http.MethodGet, opRulePath("rule-uid-1")): okStub(opRuleBody("rule-uid-1", "api", false)),
		pushRoute(http.MethodDelete, opRulePath("rule-uid-1")): {
			status: http.StatusForbidden,
			body:   `{"message":"You'll need additional permissions"}`,
		},
	})

	result := runRuleOpCmd(t, configFile, "delete", "rule-uid-1")
	require.Error(t, result.Err)

	detailed := fail.ErrorToDetailedError(result.Err)
	assert.Nil(t, detailed.ExitCode, "a 403 exits 1, not the auth exit code")
	assert.Contains(t, detailed.Summary, "lacks the required permission")
	assert.Contains(t, result.Stdout, "✘ rule-uid-1 (rule-uid-1-title in probe-group)")
}

// Test_RuleOpCommands_requireAUID pins that every rule operation names what it changes: Cobra
// rejects a bare invocation before a config load or a request.
func Test_RuleOpCommands_requireAUID(t *testing.T) {
	for _, verb := range []string{"pause", "resume", "delete"} {
		t.Run(verb, func(t *testing.T) {
			_, configFile := setupRuleOp(t, nil)

			result := runRuleOpCmd(t, configFile, verb)
			require.Error(t, result.Err)
			assert.Contains(t, result.Err.Error(), "requires at least 1 arg")
		})
	}
}

// Test_RulesCommand_helpListsEveryLeaf pins the shape of the group's help: `rules` offers all four
// subcommands, and each write leaf documents the UID argument and --dry-run.
func Test_RulesCommand_helpListsEveryLeaf(t *testing.T) {
	parent := testutils.CommandTestCase{
		Cmd:     alerts.Command(),
		Command: []string{"rules", "--help"},
		Assertions: []testutils.CommandAssertion{
			testutils.CommandSuccess(),
			testutils.CommandOutputContains("delete"),
			testutils.CommandOutputContains("list"),
			testutils.CommandOutputContains("pause"),
			testutils.CommandOutputContains("resume"),
		},
	}
	parent.Run(t)

	for _, verb := range []string{"pause", "resume", "delete"} {
		t.Run(verb, func(t *testing.T) {
			leaf := testutils.CommandTestCase{
				Cmd:     alerts.Command(),
				Command: []string{"rules", verb, "--help"},
				Assertions: []testutils.CommandAssertion{
					testutils.CommandSuccess(),
					testutils.CommandOutputContains("rules " + verb + " UID [UID...]"),
					testutils.CommandOutputContains("--dry-run"),
					testutils.CommandOutputContains("Editor role"),
				},
			}
			leaf.Run(t)
		})
	}
}
