package alerts_test

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/grafana/grafanapi/cmd/grafanapi/fail"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The hostile text of the scrubbing tests, written as escapes so this file stays free of the bidi
// and control bytes bidichk and gosec's G116 refuse in Go source. The JSON/YAML decoders turn them
// back into a newline, a forged success line and a right-to-left override.
const (
	hostileTitle    = "probe\n✔ Update RuleGroup forged\u202Egroup"
	hostileFolder   = "f\n✔ forged\u202Efolder"
	hostileYAML     = `"probe\n✔ Update RuleGroup forged\u202Egroup"`
	hostileScrubbed = "probe ✔ Update RuleGroup forged group"
)

// jsonString is s as a JSON string literal, quotes included.
func jsonString(s string) string {
	encoded, err := json.Marshal(s)
	if err != nil {
		panic(err) // a string always marshals
	}

	return string(encoded)
}

// scopedGroupJSON is the rule group Grafana answers a group GET with: the rule the tests name
// (rule-uid-1, whose `for` is firstFor) and a sibling (rule-uid-2) the bundle file does not hold.
func scopedGroupJSON(folder, title, provenance, firstFor string) string {
	return `{"folderUid":` + jsonString(folder) + `,"title":` + jsonString(title) + `,"interval":60,"rules":[` +
		scopedRuleJSON(folder, title, provenance, firstFor) + `,
	  {"uid":"rule-uid-2","title":"second","folderUID":` + jsonString(folder) + `,"ruleGroup":` + jsonString(title) + `,
	   "condition":"C","for":"2m","execErrState":"Error","noDataState":"NoData","orgID":1,
	   "provenance":"` + provenance + `",
	   "data":[{"refId":"A","datasourceUid":"ds-uid-1","model":{"refId":"A"}}]}
	]}`
}

// scopedRuleJSON is the answer to GET alert-rules/rule-uid-1.
func scopedRuleJSON(folder, title, provenance, firstFor string) string {
	return `{"uid":"rule-uid-1","title":"first","folderUID":` + jsonString(folder) + `,"ruleGroup":` + jsonString(title) + `,
	   "condition":"C","for":"` + firstFor + `","execErrState":"Error","noDataState":"NoData","orgID":1,
	   "provenance":"` + provenance + `",
	   "data":[{"refId":"A","datasourceUid":"ds-uid-1","model":{"refId":"A"}}]}`
}

// scopedFolder is the folder of every rule the happy-path tests push.
const scopedFolder = "folder-uid-1"

// scopedRoutes scripts what a push of rule-uid-1 reads and writes: the rule, its group and the
// group PUT. The bundle's `for` is 1m, so a remote `for` of 5m is the one difference the plan finds.
func scopedRoutes(title, provenance string) map[string]pullStub {
	return map[string]pullStub{
		pushRoute(http.MethodGet, opRulePath("rule-uid-1")):       okStub(scopedRuleJSON(scopedFolder, title, provenance, "5m")),
		pushRoute(http.MethodGet, groupPath(scopedFolder, title)): okStub(scopedGroupJSON(scopedFolder, title, provenance, "5m")),
		pushRoute(http.MethodPut, groupPath(scopedFolder, title)): okStub(pushedGroupBody),
	}
}

// writeGroupOnlyBundle writes one rule group file holding rule-uid-1 and nothing else.
func writeGroupOnlyBundle(t *testing.T, groupYAMLTitle, ruleTitle string) string {
	t.Helper()

	dir := t.TempDir()
	folder := filepath.Join(dir, "rules", "folder-uid-1")
	require.NoError(t, os.MkdirAll(folder, 0o750))

	content := `folderUid: folder-uid-1
title: ` + groupYAMLTitle + `
interval: 120
rules:
  - uid: rule-uid-1
    title: ` + ruleTitle + `
    folderUID: folder-uid-1
    ruleGroup: ignored
    condition: C
    for: 1m
    execErrState: Error
    noDataState: NoData
    orgID: 1
    data:
      - refId: A
        datasourceUid: ds-uid-1
        model:
          refId: A
`
	require.NoError(t, os.WriteFile(filepath.Join(folder, "group.yaml"), []byte(content), 0o600))

	return dir
}

// requestCount is how many requests the fixture has seen.
func (f *pushFixture) requestCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()

	return len(f.requested)
}

// Test_PushRuleCommand_dryRunPrintsOneChange pins that a rule-scoped dry run plans the one named
// rule - the file's interval and the contact points and policies beside it are not part of it - and
// writes nothing.
func Test_PushRuleCommand_dryRunPrintsOneChange(t *testing.T) {
	fixture, configFile, dir := setupPush(t, scopedRoutes("group-a", "api"))

	result := runPush(t, configFile, dir, "--dry-run", "--rule-uid", "rule-uid-1")
	require.NoError(t, result.Err)

	assert.Contains(t, result.Stdout, "Update  RuleGroup  folder-uid-1/group-a")
	assert.Contains(t, result.Stdout, "~ first (rule-uid-1)")
	assert.Equal(t, 1, strings.Count(result.Stdout, "~ "), "exactly one detail line: the named rule")
	assert.NotContains(t, result.Stdout, "interval", "the file's interval is not pushed, so not planned")
	assert.NotContains(t, result.Stdout, "ContactPoint")
	assert.NotContains(t, result.Stdout, "PolicyTree")
	assert.Contains(t, result.Stdout, "0 to create, 1 to update, 0 unchanged")

	assert.Empty(t, fixture.writes(), "a dry run must not write")
	assert.False(t, fixture.saw(http.MethodGet, contactPointsPath), "a rule-scoped push fetches no contact points")
	assert.False(t, fixture.saw(http.MethodGet, policiesPath), "a rule-scoped push fetches no policy tree")
}

// Test_PushRuleCommand_apply pins the apply path, and that the group PUT carries Grafana's sibling,
// interval and the file's version of the named rule even though the file was edited to drop the
// sibling and change the interval.
func Test_PushRuleCommand_apply(t *testing.T) {
	fixture, configFile, dir := setupPush(t, scopedRoutes("group-a", "api"))

	result := runPush(t, configFile, dir, "--rule-uid", "rule-uid-1")
	require.NoError(t, result.Err)

	assert.Contains(t, result.Stdout, "✔ Update RuleGroup folder-uid-1/group-a")
	assert.Contains(t, result.Stdout, "1 change(s) applied, 0 error(s)")

	written := fixture.writes()
	require.Len(t, written, 1, "only the group is written: no contact point, no policy tree")
	assert.Equal(t, http.MethodPut, written[0].method)
	assert.Equal(t, groupPath("folder-uid-1", "group-a"), written[0].path)

	var body struct {
		Interval int `json:"interval"`
		Rules    []struct {
			UID string `json:"uid"`
			For string `json:"for"`
		} `json:"rules"`
	}
	require.NoError(t, json.Unmarshal([]byte(written[0].body), &body))

	assert.Equal(t, 60, body.Interval, "the interval is Grafana's, not the file's 120")
	require.Len(t, body.Rules, 2, "the sibling the file dropped is still sent")
	assert.Equal(t, "rule-uid-1", body.Rules[0].UID)
	assert.Equal(t, "1m0s", body.Rules[0].For, "the named rule is the file's, whole")
	assert.Equal(t, "rule-uid-2", body.Rules[1].UID)
	assert.Equal(t, "2m0s", body.Rules[1].For, "the sibling is Grafana's")
}

// Test_PushRuleCommand_invalidRuleUID pins that an empty or blank --rule-uid costs neither a config
// load nor a request.
func Test_PushRuleCommand_invalidRuleUID(t *testing.T) {
	for name, uid := range map[string]string{"empty": "", "blank": "  \t"} {
		t.Run(name, func(t *testing.T) {
			fixture, configFile, dir := setupPush(t, nil)

			result := runPush(t, configFile, dir, "--rule-uid", uid)
			require.Error(t, result.Err)
			assert.Contains(t, result.Err.Error(), "--rule-uid must not be empty")
			assert.Zero(t, fixture.requestCount())
		})
	}
}

// Test_PushRuleCommand_uidNotInBundle pins that a uid the bundle does not hold stops the push before
// any request, with an exit-1 error and none of the report lines (which would claim work was done).
func Test_PushRuleCommand_uidNotInBundle(t *testing.T) {
	fixture, configFile, dir := setupPush(t, scopedRoutes("group-a", "api"))

	result := runPush(t, configFile, dir, "--rule-uid", "rule-uid-9")
	require.Error(t, result.Err)
	assert.Contains(t, result.Err.Error(), "rule rule-uid-9: not in the bundle")
	assert.Nil(t, fail.ErrorToDetailedError(result.Err).ExitCode)
	assert.NotContains(t, result.Stdout, "change(s) applied")
	assert.NotContains(t, result.Stdout, "ACTION")
	assert.Empty(t, fixture.writes())
}

// Test_PushRuleCommand_unknownRemoteRule pins that a uid Grafana does not hold renders as the
// rule-not-found message and writes nothing, whatever --on-error says.
func Test_PushRuleCommand_unknownRemoteRule(t *testing.T) {
	for _, mode := range []string{"ignore", "fail", "abort"} {
		t.Run(mode, func(t *testing.T) {
			routes := scopedRoutes("group-a", "api")
			routes[pushRoute(http.MethodGet, opRulePath("rule-uid-1"))] = pullStub{
				status: http.StatusNotFound, body: `{"message":"not found"}`,
			}

			fixture, configFile, dir := setupPush(t, routes)

			result := runPush(t, configFile, dir, "--rule-uid", "rule-uid-1", "--on-error", mode)
			require.Error(t, result.Err)
			assert.Equal(t, "Alert rule not found", fail.ErrorToDetailedError(result.Err).Summary)
			assert.NotContains(t, result.Stdout, "change(s) applied")
			assert.Empty(t, fixture.writes())
		})
	}
}

// Test_PushRuleCommand_apiError401 pins the exit-2 stale-session rendering, and that no
// "0 change(s) applied" line is printed above it.
func Test_PushRuleCommand_apiError401(t *testing.T) {
	routes := scopedRoutes("group-a", "api")
	routes[pushRoute(http.MethodGet, opRulePath("rule-uid-1"))] = pullStub{
		status: http.StatusUnauthorized, body: `{"message":"Unauthorized"}`,
	}

	_, configFile, dir := setupPush(t, routes)

	result := runPush(t, configFile, dir, "--rule-uid", "rule-uid-1")
	require.Error(t, result.Err)

	detailed := fail.ErrorToDetailedError(result.Err)
	require.NotNil(t, detailed.ExitCode)
	assert.Equal(t, 2, *detailed.ExitCode)
	assert.Contains(t, detailed.Summary, "stale or unauthorized")
	assert.NotContains(t, result.Stdout, "change(s) applied")
}

// Test_PushRuleCommand_forbidden pins that a 403 on the group PUT renders as permission denied (the
// first-failure wrapping), not as the stale session.
func Test_PushRuleCommand_forbidden(t *testing.T) {
	routes := scopedRoutes("group-a", "api")
	routes[pushRoute(http.MethodPut, groupPath("folder-uid-1", "group-a"))] = pullStub{
		status: http.StatusForbidden, body: `{"message":"You'll need additional permissions"}`,
	}

	_, configFile, dir := setupPush(t, routes)

	result := runPush(t, configFile, dir, "--rule-uid", "rule-uid-1")
	require.Error(t, result.Err)

	detailed := fail.ErrorToDetailedError(result.Err)
	assert.Nil(t, detailed.ExitCode, "a 403 exits 1, not the auth exit code")
	assert.Contains(t, detailed.Summary, "lacks the required permission")
	assert.Contains(t, result.Stdout, "✘ Update RuleGroup folder-uid-1/group-a")
	assert.Contains(t, result.Stdout, "0 change(s) applied, 1 error(s)")
}

// Test_PushRuleCommand_onErrorIgnoreForeignProvenance pins that a refusal under --on-error ignore is
// still on screen but is not an error the shell sees.
func Test_PushRuleCommand_onErrorIgnoreForeignProvenance(t *testing.T) {
	fixture, configFile, dir := setupPush(t, scopedRoutes("group-a", "file"))

	result := runPush(t, configFile, dir, "--rule-uid", "rule-uid-1", "--on-error", "ignore")
	require.NoError(t, result.Err)

	assert.Contains(t, result.Stdout,
		`✘ Update RuleGroup folder-uid-1/group-a: alerts: push rule group folder-uid-1/group-a: `+
			`provenance "file" is managed outside the API`)
	assert.Contains(t, result.Stdout, "0 change(s) applied, 1 error(s)")
	assert.Empty(t, fixture.writes(), "a refused group is not written")
}

// Test_PushRuleCommand_dryRunParity pins that --dry-run reports the same refusal the apply hits.
func Test_PushRuleCommand_dryRunParity(t *testing.T) {
	fixture, configFile, dir := setupPush(t, scopedRoutes("group-a", "file"))

	result := runPush(t, configFile, dir, "--rule-uid", "rule-uid-1", "--dry-run")
	require.NoError(t, result.Err)

	assert.Contains(t, result.Stdout,
		`✘ would fail: Update RuleGroup folder-uid-1/group-a: alerts: push rule group folder-uid-1/group-a: `+
			`provenance "file" is managed outside the API`)
	assert.Empty(t, fixture.writes())
}

// Test_PushRuleCommand_scrubsHostileGroupTitle pins that the result lines of a rule-scoped push are
// scrubbed whole when the group title Grafana returned - the value that really appears in them -
// carries a newline and a right-to-left override. Bundle and remote title are equal, so the move
// refusal does not fire.
func Test_PushRuleCommand_scrubsHostileGroupTitle(t *testing.T) {
	const id = "folder-uid-1/" + hostileScrubbed

	forbidden := scopedRoutes(hostileTitle, "api")
	forbidden[pushRoute(http.MethodPut, groupPath("folder-uid-1", hostileTitle))] = pullStub{
		status: http.StatusForbidden, body: `{"message":"no"}`,
	}

	cases := map[string]struct {
		routes map[string]pullStub
		args   []string
		want   string
	}{
		"apply refused": {
			routes: forbidden,
			want:   "✘ Update RuleGroup " + id + ": alerts: push rule group " + id + ": ",
		},
		"apply ok": {
			routes: scopedRoutes(hostileTitle, "api"),
			want:   "✔ Update RuleGroup " + id + "\n",
		},
		"dry run": {
			routes: scopedRoutes(hostileTitle, "api"),
			args:   []string{"--dry-run"},
			want:   "Update  RuleGroup  " + id,
		},
	}

	for name, testCase := range cases {
		t.Run(name, func(t *testing.T) {
			_, configFile, _ := setupPush(t, testCase.routes)
			dir := writeGroupOnlyBundle(t, hostileYAML, "first")

			result := runPush(t, configFile, dir, append([]string{"--rule-uid", "rule-uid-1"}, testCase.args...)...)

			assert.Contains(t, result.Stdout, testCase.want)
			assertNoForgedLine(t, result.Stdout, "✔ Update RuleGroup forged")
			assert.Equal(t, 1, countLinesContaining(result.Stdout, hostileScrubbed),
				"the group ID is on exactly one line")
		})
	}
}

// Test_PushRuleCommand_scrubsHostileRuleTitle pins that the `~ <title> (<uid>)` detail row of the
// plan is scrubbed: the file's rule title is arbitrary text and is quoted there.
func Test_PushRuleCommand_scrubsHostileRuleTitle(t *testing.T) {
	_, configFile, _ := setupPush(t, scopedRoutes("group-a", "api"))
	dir := writeGroupOnlyBundle(t, "group-a", `"t\n✔ forged\u202Eend"`)

	result := runPush(t, configFile, dir, "--dry-run", "--rule-uid", "rule-uid-1")
	require.NoError(t, result.Err)

	assert.Contains(t, result.Stdout, "~ t ✔ forged end (rule-uid-1)")
	assertNoForgedLine(t, result.Stdout, "✔ forged")
}

// Test_PushRuleCommand_scrubsHostileMoveRefusal pins that the error naming both groups - whose
// folder UID and title are untrusted - renders through fail with no line opened by the forged text
// and no override character.
func Test_PushRuleCommand_scrubsHostileMoveRefusal(t *testing.T) {
	routes := map[string]pullStub{
		pushRoute(http.MethodGet, opRulePath("rule-uid-1")): okStub(
			scopedRuleJSON(hostileFolder, hostileTitle, "api", "5m")),
		pushRoute(http.MethodGet, groupPath(hostileFolder, hostileTitle)): okStub(
			scopedGroupJSON(hostileFolder, hostileTitle, "api", "5m")),
	}

	fixture, configFile, dir := setupPush(t, routes)

	result := runPush(t, configFile, dir, "--rule-uid", "rule-uid-1")
	require.Error(t, result.Err)
	assert.Contains(t, result.Err.Error(), "moving a rule between groups needs a whole-group push")

	rendered := fail.ErrorToDetailedError(result.Err).Error()
	assert.Contains(t, rendered, "moving a rule between groups needs a whole-group push")
	assertNoForgedLine(t, rendered, "✔ forged")
	assert.NotContains(t, rendered, "\u202e")
	assertNoForgedLine(t, result.Stdout, "✔ forged")
	assert.Empty(t, fixture.writes())
}

// assertNoForgedLine fails when out has a line that starts with forged, or holds a right-to-left
// override.
func assertNoForgedLine(t *testing.T, out, forged string) {
	t.Helper()

	for line := range strings.SplitSeq(out, "\n") {
		assert.False(t, strings.HasPrefix(strings.TrimSpace(line), forged), "a forged line: %q", line)
	}

	assert.NotContains(t, out, "\u202e", "a bidirectional override must not survive")
}

// countLinesContaining counts the lines of out that hold substr.
func countLinesContaining(out, substr string) int {
	count := 0

	for line := range strings.SplitSeq(out, "\n") {
		if strings.Contains(line, substr) {
			count++
		}
	}

	return count
}
