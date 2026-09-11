package alerts_test

import (
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/grafana/grafanapi/cmd/grafanapi/alerts"
	"github.com/grafana/grafanapi/cmd/grafanapi/config"
	"github.com/grafana/grafanapi/cmd/grafanapi/fail"
	"github.com/grafana/grafanapi/internal/testutils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The bundle every push test reads from disk, written as the files a user would hand-edit rather
// than through WriteBundle: strict decoding of exactly these key spellings (folderUid on the
// group, folderUID on the rule) is part of what this command promises.
//
// Against pushRemoteGroup / pushRemotePoints / pushRemotePolicies below, each of the three objects
// differs in exactly one place, so the happy path plans one Update per kind.
const (
	pushGroupFile = `folderUid: folder-uid-1
title: group-a
interval: 120
rules:
  - uid: rule-uid-1
    title: first
    folderUID: folder-uid-1
    ruleGroup: group-a
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
	pushPointsFile = `- uid: cp-uid-1
  name: probe-slack
  type: slack
  settings:
    recipient: "#probe"
    url: "[REDACTED]"
`
	pushPoliciesFile = `receiver: probe-slack
`
)

// The remote side the bundle above is reconciled against: the same group with another interval,
// the same contact point with another recipient, and the default policy tree. Everything is
// claimed by the API ("api"), so every write mirrors that and sends no X-Disable-Provenance.
const (
	pushRemoteGroup = `{"folderUid":"folder-uid-1","title":"group-a","interval":60,"rules":[
	  {"uid":"rule-uid-1","title":"first","folderUID":"folder-uid-1","ruleGroup":"group-a",
	   "condition":"C","for":"1m","execErrState":"Error","noDataState":"NoData","orgID":1,
	   "provenance":"api","updated":"2026-09-08T09:08:07Z",
	   "data":[{"refId":"A","datasourceUid":"ds-uid-1","model":{"refId":"A"}}]}
	]}`
	pushRemotePoints = `[
	  {"uid":"cp-uid-1","name":"probe-slack","type":"slack","provenance":"api",
	   "settings":{"recipient":"#old","url":"[REDACTED]"}}
	]`
	pushRemotePolicies = `{"receiver":"grafana-default-email","provenance":"api"}`
)

// The same three objects as the instance already holds them, which is what the bundle above plans
// as three Unchanged entries - and therefore as a push that issues no write at all.
const (
	pushUnchangedGroup = `{"folderUid":"folder-uid-1","title":"group-a","interval":120,"rules":[
	  {"uid":"rule-uid-1","title":"first","folderUID":"folder-uid-1","ruleGroup":"group-a",
	   "condition":"C","for":"1m","execErrState":"Error","noDataState":"NoData","orgID":1,
	   "provenance":"api","updated":"2026-09-08T09:08:07Z",
	   "data":[{"refId":"A","datasourceUid":"ds-uid-1","model":{"refId":"A"}}]}
	]}`
	pushUnchangedPoints = `[
	  {"uid":"cp-uid-1","name":"probe-slack","type":"slack","provenance":"api",
	   "settings":{"recipient":"#probe","url":"[REDACTED]"}}
	]`
	pushUnchangedPolicies = `{"receiver":"probe-slack","provenance":"api"}`
)

// acceptedBody is what the provisioning API answers a contact-point or policy write with: 202 and
// a models.Ack. The generated readers accept exactly that, so a 200 here would fail for the wrong
// reason.
const acceptedBody = `{"message":"accepted"}`

// pushedGroupBody is the 200 a rule-group PUT answers with; its content is never read back.
const pushedGroupBody = `{"folderUid":"folder-uid-1","title":"group-a","interval":120,"rules":[]}`

// pointPath is the provisioning address of the bundle's single contact point. It is a constant
// rather than a uid-taking helper: unparam reports a shared test helper every caller passes the
// same argument to, and there is only ever one contact point in these fixtures.
const pointPath = contactPointsPath + "/cp-uid-1"

// recordedPush is one request a push or a rule operation made: enough to say whether anything was
// written, where, with which provenance instruction and with what in it.
//
// The header is a *string because absent and "true" are different instructions to Grafana, exactly
// as in internal/alerts' own write tests.
type recordedPush struct {
	method string
	path   string
	header *string
	body   string
}

// pushFixture is pull_test.go's recording server keyed by method AND path: a push GETs and writes
// the same addresses, so a path alone cannot say which of the two a response belongs to. It is
// the same split internal/alerts' own push tests make, for the same reason.
type pushFixture struct {
	*httptest.Server

	t *testing.T

	mu        sync.Mutex
	requested []recordedPush
}

// newPushFixture serves routes keyed by pushRoute, records every request, and fails the test on a
// request to a path the table does not script - the shape a command quietly writing to the wrong
// address would take. Handlers use assert (not require) since they run on the server's goroutine.
func newPushFixture(t *testing.T, routes map[string]pullStub) *pushFixture {
	t.Helper()

	fixture := &pushFixture{t: t}

	mux := http.NewServeMux()
	mux.HandleFunc(rotatePath, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		fixture.record(r)

		stub, ok := routes[pushRoute(r.Method, r.URL.Path)]
		if !ok {
			assert.Fail(t, "unexpected request", pushRoute(r.Method, r.URL.Path))
			w.WriteHeader(http.StatusNotFound)

			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(stub.status)
		_, err := w.Write([]byte(stub.body))
		assert.NoError(t, err)
	})

	fixture.Server = httptest.NewServer(mux)
	t.Cleanup(fixture.Close)

	return fixture
}

// pushBundleDir writes the three bundle files into a fresh temp directory and returns it.
func pushBundleDir(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()
	folder := filepath.Join(dir, "rules", "folder-uid-1")
	require.NoError(t, os.MkdirAll(folder, 0o750))

	files := map[string]string{
		filepath.Join(folder, "group-a.yaml"):     pushGroupFile,
		filepath.Join(dir, "contact-points.yaml"): pushPointsFile,
		filepath.Join(dir, "policies.yaml"):       pushPoliciesFile,
	}

	for path, content := range files {
		require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	}

	return dir
}

// disableProvenanceValue reads the X-Disable-Provenance header off a request as it was sent. It is
// a *string rather than a string because an absent header and a "true" one are two different
// instructions: one mirrors a claim Grafana already holds, the other gives the object up.
func disableProvenanceValue(r *http.Request) *string {
	value := r.Header.Get("X-Disable-Provenance")
	if value == "" {
		return nil
	}

	return &value
}

// pushRoute keys a route table by method and path.
func pushRoute(method, path string) string {
	return method + " " + path
}

// pushRoutes merges a test's own routes onto the reads and writes the happy path performs, so each
// test declares only what it is actually about.
func pushRoutes(routes map[string]pullStub) map[string]pullStub {
	base := map[string]pullStub{
		pushRoute(http.MethodGet, folderPath("folder-uid-1")):           okStub(`{"uid":"folder-uid-1"}`),
		pushRoute(http.MethodGet, groupPath("folder-uid-1", "group-a")): okStub(pushRemoteGroup),
		pushRoute(http.MethodGet, contactPointsPath):                    okStub(pushRemotePoints),
		pushRoute(http.MethodGet, policiesPath):                         okStub(pushRemotePolicies),
		pushRoute(http.MethodPut, groupPath("folder-uid-1", "group-a")): okStub(pushedGroupBody),
		pushRoute(http.MethodPut, pointPath):                            {status: http.StatusAccepted, body: acceptedBody},
		pushRoute(http.MethodPut, policiesPath):                         {status: http.StatusAccepted, body: acceptedBody},
	}

	maps.Copy(base, routes)

	return base
}

// runPush executes `alerts push` over dir with args appended to --config and --path.
//
// Usage is silenced the way cmd/grafanapi/root silences it in the real binary: run bare, cobra
// prints the whole usage block into the command's out writer on any error, which here is the very
// buffer the tests read as stdout. Leaving it on would make "the command printed nothing" an
// unassertable statement about output the user never sees.
func runPush(t *testing.T, configFile, dir string, args ...string) testutils.CommandResult {
	t.Helper()

	var result testutils.CommandResult

	cmd := alerts.Command()
	cmd.SilenceUsage = true

	testCase := testutils.CommandTestCase{
		Cmd:     cmd,
		Command: append([]string{"push", "--config", configFile, "-p", dir}, args...),
		Assertions: []testutils.CommandAssertion{
			func(_ *testing.T, res testutils.CommandResult) { result = res },
		},
	}
	testCase.Run(t)

	return result
}

// setupPush is the happy-path setup: the full route table, a matching config file and a bundle on
// disk. The fixture is returned so a test can assert what was (not) written.
func setupPush(t *testing.T, routes map[string]pullStub) (*pushFixture, string, string) {
	t.Helper()

	fixture := newPushFixture(t, pushRoutes(routes))
	configFile := newTestConfig(t, fixture.Server)

	return fixture, configFile, pushBundleDir(t)
}

func (f *pushFixture) record(r *http.Request) {
	body, err := io.ReadAll(r.Body)

	f.mu.Lock()
	defer f.mu.Unlock()

	f.requested = append(f.requested, recordedPush{
		method: r.Method,
		path:   r.URL.Path,
		header: disableProvenanceValue(r),
		body:   string(body),
	})

	// Last statement of the block on purpose: this runs on the server's goroutine, where a
	// FailNow-based require would abort the handler rather than the test.
	assert.NoError(f.t, err)
}

// requestsFor returns every recorded request to one method and path, in the order they arrived.
func (f *pushFixture) requestsFor(method, path string) []recordedPush {
	f.mu.Lock()
	defer f.mu.Unlock()

	var found []recordedPush

	for _, request := range f.requested {
		if request.method == method && request.path == path {
			found = append(found, request)
		}
	}

	return found
}

// saw reports whether the method/path pair was requested at least once.
func (f *pushFixture) saw(method, path string) bool {
	return len(f.requestsFor(method, path)) != 0
}

// writes returns every request that could have changed something, which is what a dry run has to
// have none of.
func (f *pushFixture) writes() []recordedPush {
	f.mu.Lock()
	defer f.mu.Unlock()

	var found []recordedPush

	for _, request := range f.requested {
		if request.method != http.MethodGet {
			found = append(found, request)
		}
	}

	return found
}

// Test_PushCommand_dryRunPrintsPlan is the promise --dry-run makes: the whole plan on stdout, and
// not one request that could change anything.
func Test_PushCommand_dryRunPrintsPlan(t *testing.T) {
	fixture, configFile, dir := setupPush(t, nil)

	result := runPush(t, configFile, dir, "--dry-run")
	require.NoError(t, result.Err)

	assert.Contains(t, result.Stdout, "ACTION")
	assert.Contains(t, result.Stdout, "Update  ContactPoint  cp-uid-1")
	assert.Contains(t, result.Stdout, "Update  PolicyTree    policies")
	assert.Contains(t, result.Stdout, "Update  RuleGroup     folder-uid-1/group-a")
	assert.Contains(t, result.Stdout, "~ interval 60s→120s", "the plan spells out what changes inside a group")
	assert.Contains(t, result.Stdout, "0 to create, 3 to update, 0 unchanged")

	assert.Empty(t, fixture.writes(), "a dry run must not write")
}

// Test_PushCommand_appliesEveryKind is the apply path: one line per change, a summary, and the
// three writes the plan promised.
func Test_PushCommand_appliesEveryKind(t *testing.T) {
	fixture, configFile, dir := setupPush(t, nil)

	result := runPush(t, configFile, dir)
	require.NoError(t, result.Err)

	assert.Contains(t, result.Stdout, "✔ Update ContactPoint cp-uid-1")
	assert.Contains(t, result.Stdout, "✔ Update PolicyTree policies")
	assert.Contains(t, result.Stdout, "✔ Update RuleGroup folder-uid-1/group-a")
	assert.Contains(t, result.Stdout, "3 change(s) applied, 0 error(s)")
	assert.NotContains(t, result.Stdout, "ACTION", "the plan table belongs to --dry-run alone")

	assert.True(t, fixture.saw(http.MethodPut, pointPath))
	assert.True(t, fixture.saw(http.MethodPut, policiesPath))
	assert.True(t, fixture.saw(http.MethodPut, groupPath("folder-uid-1", "group-a")))
	assert.False(t, fixture.saw(http.MethodGet, alertRulesPath), "a push reads only the groups it pushes")
}

// Test_PushCommand_onErrorFail is the default mode: an object another provisioner owns is refused,
// the rest of the push still runs, and the command exits non-zero for the failure.
func Test_PushCommand_onErrorFail(t *testing.T) {
	foreign := `{"folderUid":"folder-uid-1","title":"group-a","interval":60,"rules":[
	  {"uid":"rule-uid-1","title":"first","folderUID":"folder-uid-1","ruleGroup":"group-a",
	   "condition":"C","for":"1m","execErrState":"Error","noDataState":"NoData","orgID":1,
	   "provenance":"file","data":[{"refId":"A","datasourceUid":"ds-uid-1","model":{"refId":"A"}}]}
	]}`

	fixture, configFile, dir := setupPush(t, map[string]pullStub{
		pushRoute(http.MethodGet, groupPath("folder-uid-1", "group-a")): okStub(foreign),
	})

	result := runPush(t, configFile, dir)
	require.Error(t, result.Err)
	assert.Contains(t, result.Err.Error(), "1 change(s) failed to push")
	assert.Contains(t, result.Err.Error(), `provenance "file" is managed outside the API`)
	assert.Nil(t, fail.ErrorToDetailedError(result.Err).ExitCode, "a refused object must not claim the auth exit code")

	assert.Contains(t, result.Stdout, "✘ Update RuleGroup folder-uid-1/group-a")
	assert.Contains(t, result.Stdout, "2 change(s) applied, 1 error(s)")

	assert.True(t, fixture.saw(http.MethodPut, pointPath), "the other changes are still applied")
	assert.False(t, fixture.saw(http.MethodPut, groupPath("folder-uid-1", "group-a")), "a refused group is not written")
}

// Test_PushCommand_onErrorIgnore is the same push under --on-error ignore: the failure is still
// reported on stdout, but it is not an error the shell sees.
func Test_PushCommand_onErrorIgnore(t *testing.T) {
	foreign := `{"folderUid":"folder-uid-1","title":"group-a","interval":60,"rules":[
	  {"uid":"rule-uid-1","title":"first","folderUID":"folder-uid-1","ruleGroup":"group-a",
	   "condition":"C","for":"1m","execErrState":"Error","noDataState":"NoData","orgID":1,
	   "provenance":"file","data":[{"refId":"A","datasourceUid":"ds-uid-1","model":{"refId":"A"}}]}
	]}`

	_, configFile, dir := setupPush(t, map[string]pullStub{
		pushRoute(http.MethodGet, groupPath("folder-uid-1", "group-a")): okStub(foreign),
	})

	result := runPush(t, configFile, dir, "--on-error", "ignore")
	require.NoError(t, result.Err)

	assert.Contains(t, result.Stdout, "✘ Update RuleGroup folder-uid-1/group-a")
	assert.Contains(t, result.Stdout, "2 change(s) applied, 1 error(s)")
}

// Test_PushCommand_invalidOnError pins that the shared --on-error vocabulary is validated before
// anything else: an unknown mode costs neither a config load nor a request.
func Test_PushCommand_invalidOnError(t *testing.T) {
	_, configFile, dir := setupPush(t, nil)

	result := runPush(t, configFile, dir, "--on-error", "bogus")
	require.Error(t, result.Err)
	assert.Contains(t, result.Err.Error(), `invalid --on-error value "bogus"`)
	assert.Contains(t, result.Err.Error(), "ignore, fail, abort")
}

// Test_PushCommand_forbidden is Task 6's rendering reached through a real command: a write Grafana
// refuses must say the user lacks the permission - and must not suggest re-authenticating, since
// the session was accepted. The aggregate error therefore wraps the first failure rather than
// only counting it.
func Test_PushCommand_forbidden(t *testing.T) {
	_, configFile, dir := setupPush(t, map[string]pullStub{
		pushRoute(http.MethodPut, pointPath): {
			status: http.StatusForbidden,
			body:   `{"message":"You'll need additional permissions"}`,
		},
	})

	result := runPush(t, configFile, dir)
	require.Error(t, result.Err)

	detailed := fail.ErrorToDetailedError(result.Err)
	assert.Nil(t, detailed.ExitCode, "a 403 exits 1, not the auth exit code")
	assert.Contains(t, detailed.Summary, "lacks the required permission")
	assert.Contains(t, result.Stdout, "✘ Update ContactPoint cp-uid-1")
}

// Test_PushCommand_apiError401 is the stale-session regression test: a dead session must render as
// exit 2 through this command too, whatever it was doing when it found out.
//
// This 401 lands on the fetch the plan is built from, so the push never reached an object: nothing
// was applied, nothing failed, and the summary line is therefore *not* printed. Reporting a zero
// result unconditionally put "0 change(s) applied, 0 error(s)" above the real error.
func Test_PushCommand_apiError401(t *testing.T) {
	_, configFile, dir := setupPush(t, map[string]pullStub{
		pushRoute(http.MethodGet, groupPath("folder-uid-1", "group-a")): {
			status: http.StatusUnauthorized,
			body:   `{"message":"Unauthorized"}`,
		},
	})

	result := runPush(t, configFile, dir)
	require.Error(t, result.Err)

	detailed := fail.ErrorToDetailedError(result.Err)
	require.NotNil(t, detailed.ExitCode)
	assert.Equal(t, 2, *detailed.ExitCode)
	assert.Contains(t, detailed.Summary, "stale or unauthorized")
	assert.NotContains(t, result.Stdout, "change(s) applied")
}

// Test_PushCommand_apiError401DryRunPrintsNoPlan is the same failure under --dry-run, where the
// zero result rendered as a bare plan-table header above the error - a table promising to describe
// a push that was never planned.
func Test_PushCommand_apiError401DryRunPrintsNoPlan(t *testing.T) {
	_, configFile, dir := setupPush(t, map[string]pullStub{
		pushRoute(http.MethodGet, groupPath("folder-uid-1", "group-a")): {
			status: http.StatusUnauthorized,
			body:   `{"message":"Unauthorized"}`,
		},
	})

	result := runPush(t, configFile, dir, "--dry-run")
	require.Error(t, result.Err)

	detailed := fail.ErrorToDetailedError(result.Err)
	require.NotNil(t, detailed.ExitCode)
	assert.Equal(t, 2, *detailed.ExitCode)
	assert.Empty(t, result.Stdout)
}

// Test_PushCommand_planErrorReportsNothing is the other way a push ends before it applies
// anything: Plan itself refuses the bundle. The refusal is the whole message - a summary claiming
// zero changes would only invite the user to read it as "nothing to do".
func Test_PushCommand_planErrorReportsNothing(t *testing.T) {
	fixture := newPushFixture(t, map[string]pullStub{
		pushRoute(http.MethodGet, contactPointsPath): okStub(`[
		  {"uid":"cp-uid-1","name":"probe-slack","type":"slack","settings":{"recipient":"#probe"}},
		  {"uid":"cp-uid-2","name":"probe-slack","type":"slack","settings":{"recipient":"#other"}}
		]`),
		pushRoute(http.MethodGet, policiesPath): okStub(pushRemotePolicies),
	})

	dir := t.TempDir()
	points := "- name: probe-slack\n  type: slack\n  settings:\n    recipient: \"#probe\"\n"
	require.NoError(t, os.WriteFile(filepath.Join(dir, "contact-points.yaml"), []byte(points), 0o600))

	result := runPush(t, newTestConfig(t, fixture.Server), dir)
	require.Error(t, result.Err)
	assert.Contains(t, result.Err.Error(), "set its uid")
	assert.Empty(t, result.Stdout)
}

// Test_PushCommand_invalidFlags pins the pre-network validation: both checks fail before a config
// load, so an invalid flag costs neither a Keychain read nor a request.
func Test_PushCommand_invalidFlags(t *testing.T) {
	cases := map[string]struct {
		args []string
		want string
	}{
		"empty path":        {args: []string{"-p", ""}, want: "--path is required"},
		"zero concurrency":  {args: []string{"--max-concurrent", "0"}, want: "max-concurrent must be greater than zero"},
		"negative parallel": {args: []string{"--max-concurrent", "-2"}, want: "max-concurrent must be greater than zero"},
	}

	for name, testCase := range cases {
		t.Run(name, func(t *testing.T) {
			_, configFile, dir := setupPush(t, nil)

			result := runPush(t, configFile, dir, testCase.args...)
			require.Error(t, result.Err)
			assert.Contains(t, result.Err.Error(), testCase.want)
		})
	}
}

// Test_PushCommand_missingBundle pins that an unreadable bundle is an error rather than an empty
// push: reading the whole bundle before writing anything is what keeps a typo in --path from
// being indistinguishable from "nothing to do".
func Test_PushCommand_missingBundle(t *testing.T) {
	fixture, configFile, dir := setupPush(t, nil)

	result := runPush(t, configFile, filepath.Join(dir, "nope"))
	require.Error(t, result.Err)
	assert.Contains(t, result.Err.Error(), "read bundle")
	assert.Empty(t, fixture.writes(), "a bundle that could not be read must not be half-pushed")
}

// Test_PushCommand_onErrorAbort is the end-to-end abort path, and the regression test for what a
// cut-short push must still print: the changes it had already made, and the summary closing them.
// Swallowing them would leave the user with an error and no idea what is now in Grafana.
func Test_PushCommand_onErrorAbort(t *testing.T) {
	fixture, configFile, dir := setupPush(t, map[string]pullStub{
		pushRoute(http.MethodPut, policiesPath): {
			status: http.StatusBadRequest,
			body:   `{"message":"invalid policy tree"}`,
		},
	})

	result := runPush(t, configFile, dir, "--on-error", "abort")
	require.Error(t, result.Err)
	// Under abort the push's own error wins over the report's aggregate, exactly as it does for a
	// 401 or a 403: it is the failure that stopped the run, so it is the one that must render.
	assert.Equal(t, "alerts: push policy tree policies: invalid policy tree", result.Err.Error())

	assert.Contains(t, result.Stdout, "✔ Update ContactPoint cp-uid-1", "what was already written must still be reported")
	assert.Contains(t, result.Stdout, "✘ Update PolicyTree policies")
	assert.Contains(t, result.Stdout, "1 change(s) applied, 1 error(s)")

	assert.True(t, fixture.saw(http.MethodPut, pointPath))
	assert.False(t, fixture.saw(http.MethodPut, groupPath("folder-uid-1", "group-a")),
		"nothing after the first failure may be written")
}

// Test_PushCommand_everyChangeFailing pins the third summary rendering. A push with some failures
// warns; one where nothing at all landed is an error line, so a wall of ✘ does not end on a ✔.
func Test_PushCommand_everyChangeFailing(t *testing.T) {
	refused := pullStub{status: http.StatusBadRequest, body: `{"message":"nope"}`}

	_, configFile, dir := setupPush(t, map[string]pullStub{
		pushRoute(http.MethodPut, pointPath):                            refused,
		pushRoute(http.MethodPut, policiesPath):                         refused,
		pushRoute(http.MethodPut, groupPath("folder-uid-1", "group-a")): refused,
	})

	result := runPush(t, configFile, dir)
	require.Error(t, result.Err)

	assert.Contains(t, result.Stdout, "✘ 0 change(s) applied, 3 error(s)")
	assert.NotContains(t, result.Stdout, "✔")
}

// Test_PushCommand_apiError401ReportsWhatItDid is the same promise for the other way a push ends
// early. The exit-2 rendering is unchanged; what is pinned here is that the contact point written
// before the session died is still on screen.
func Test_PushCommand_apiError401ReportsWhatItDid(t *testing.T) {
	_, configFile, dir := setupPush(t, map[string]pullStub{
		pushRoute(http.MethodPut, policiesPath): {
			status: http.StatusUnauthorized,
			body:   `{"message":"Unauthorized"}`,
		},
	})

	result := runPush(t, configFile, dir)
	require.Error(t, result.Err)

	detailed := fail.ErrorToDetailedError(result.Err)
	require.NotNil(t, detailed.ExitCode)
	assert.Equal(t, 2, *detailed.ExitCode)

	assert.Contains(t, result.Stdout, "✔ Update ContactPoint cp-uid-1")
	assert.Contains(t, result.Stdout, "1 change(s) applied, 1 error(s)")
}

// Test_PushCommand_allUnchangedExitsZero is the recorded Viewer behaviour: a bundle that already
// matches the instance issues no write, so there is nothing for Grafana to refuse and the command
// exits 0 even for a user who could not have written anything.
func Test_PushCommand_allUnchangedExitsZero(t *testing.T) {
	fixture, configFile, dir := setupPush(t, map[string]pullStub{
		pushRoute(http.MethodGet, groupPath("folder-uid-1", "group-a")): okStub(pushUnchangedGroup),
		pushRoute(http.MethodGet, contactPointsPath):                    okStub(pushUnchangedPoints),
		pushRoute(http.MethodGet, policiesPath):                         okStub(pushUnchangedPolicies),
	})

	result := runPush(t, configFile, dir)
	require.NoError(t, result.Err)

	assert.Contains(t, result.Stdout, "✔ Unchanged ContactPoint cp-uid-1")
	assert.Contains(t, result.Stdout, "✔ Unchanged PolicyTree policies")
	assert.Contains(t, result.Stdout, "✔ Unchanged RuleGroup folder-uid-1/group-a")
	assert.Contains(t, result.Stdout, "3 change(s) applied, 0 error(s)")
	assert.Empty(t, fixture.writes(), "an unchanged bundle writes nothing, so a Viewer can run it")
}

// Test_PushCommand_dryRunReportsRefusals pins what a dry run does with an entry the apply would
// refuse: it says so, below the plan, and still exits 0 - it changed nothing, and showing the
// refusal before the apply hits it is the point.
func Test_PushCommand_dryRunReportsRefusals(t *testing.T) {
	foreign := `{"folderUid":"folder-uid-1","title":"group-a","interval":60,"rules":[
	  {"uid":"rule-uid-1","title":"first","folderUID":"folder-uid-1","ruleGroup":"group-a",
	   "condition":"C","for":"1m","execErrState":"Error","noDataState":"NoData","orgID":1,
	   "provenance":"file","data":[{"refId":"A","datasourceUid":"ds-uid-1","model":{"refId":"A"}}]}
	]}`

	fixture, configFile, dir := setupPush(t, map[string]pullStub{
		pushRoute(http.MethodGet, groupPath("folder-uid-1", "group-a")): okStub(foreign),
	})

	result := runPush(t, configFile, dir, "--dry-run")
	require.NoError(t, result.Err, "a dry run exits 0 even when it found refusals")

	assert.Contains(t, result.Stdout, "Update  ContactPoint  cp-uid-1")
	assert.NotContains(t, result.Stdout, "Update     RuleGroup", "a refused entry is not planned work")
	assert.Contains(t, result.Stdout, "0 to create, 2 to update, 0 unchanged")
	assert.Contains(t, result.Stdout,
		`✘ would fail: Update RuleGroup folder-uid-1/group-a: alerts: push rule group folder-uid-1/group-a: `+
			`provenance "file" is managed outside the API`)

	assert.Empty(t, fixture.writes(), "a dry run must not write")
}

// Test_PushCommand_scrubsForgedLines pins that a result line is scrubbed as a WHOLE line rather
// than field by field.
//
// A contact point name is arbitrary text a bundle carries, and it is re-emitted twice on the same
// line: once as the plan ID and once inside the error, which quotes it back. Scrubbing only the ID
// would leave a name holding a newline free to open a line of its own - "✔ Update RuleGroup ..." -
// in the output a human reads before approving an apply, and a right-to-left override free to
// re-order everything printed after it.
func Test_PushCommand_scrubsForgedLines(t *testing.T) {
	// The name below is `probe`, a newline, a forged success line, a right-to-left override and
	// `slack`; every one of those characters must come out as a space. The two control characters
	// are written as YAML escapes rather than as themselves, so this file stays free of the very
	// bytes bidichk and gosec's G116 refuse to have in a Go source file.
	const forgedPointsFile = `- name: "probe\n✔ Update RuleGroup forged\u202Eslack"
  type: slack
  settings:
    recipient: "#probe"
    url: "[REDACTED]"
`

	const scrubbedName = "probe ✔ Update RuleGroup forged slack"

	cases := map[string]struct {
		args []string
		want string
	}{
		"apply": {want: "✘ Create ContactPoint " + scrubbedName +
			": alerts: push contact point " + scrubbedName + ": secret field(s) settings.url are redacted"},
		"dry run": {args: []string{"--dry-run"}, want: "✘ would fail: Create ContactPoint " + scrubbedName +
			": alerts: push contact point " + scrubbedName + ": secret field(s) settings.url are redacted"},
	}

	for name, testCase := range cases {
		t.Run(name, func(t *testing.T) {
			_, configFile, dir := setupPush(t, nil)
			require.NoError(t, os.WriteFile(
				filepath.Join(dir, "contact-points.yaml"), []byte(forgedPointsFile), 0o600))

			result := runPush(t, configFile, dir, testCase.args...)

			assert.Contains(t, result.Stdout, testCase.want)
			assert.NotContains(t, result.Stdout, "\n✔ Update RuleGroup forged",
				"a newline in a name must not be able to open a line of its own")
			assert.NotContains(t, result.Stdout, "\u202e", "a bidirectional override must not survive")
			assert.Equal(t, 1, strings.Count(result.Stdout, "ContactPoint "+scrubbedName+":"),
				"the refusal is one line, printed once")
		})
	}
}

// Test_PushCommand_flagsReachThePush walks the three flags nothing else asserts the effect of.
// They are translated into alerts.PushOptions by pushOptions(), which is the one place a wrong
// field would be invisible in the output and wrong in Grafana.
func Test_PushCommand_flagsReachThePush(t *testing.T) {
	created := map[string]pullStub{
		pushRoute(http.MethodGet, groupPath("folder-uid-1", "group-a")): {status: http.StatusNotFound, body: `{}`},
	}

	t.Run("--ui-editable leaves a created group editable in the UI", func(t *testing.T) {
		fixture, configFile, dir := setupPush(t, created)

		result := runPush(t, configFile, dir, "--ui-editable")
		require.NoError(t, result.Err)

		written := fixture.requestsFor(http.MethodPut, groupPath("folder-uid-1", "group-a"))
		require.Len(t, written, 1)
		require.NotNil(t, written[0].header)
		assert.Equal(t, "true", *written[0].header)
	})

	t.Run("a created rule claims the context's org", func(t *testing.T) {
		fixture := newPushFixture(t, pushRoutes(created))
		configFile := newTestConfigInOrg(t, fixture.Server, 7)

		result := runPush(t, configFile, pushBundleDir(t))
		require.NoError(t, result.Err)

		written := fixture.requestsFor(http.MethodPut, groupPath("folder-uid-1", "group-a"))
		require.Len(t, written, 1)
		assert.Contains(t, written[0].body, `"orgID":7`,
			"the file says orgID 1; the context is what a created group takes its org from")
	})

	t.Run("--max-concurrent is accepted and still applies everything", func(t *testing.T) {
		fixture, configFile, dir := setupPush(t, nil)

		result := runPush(t, configFile, dir, "--max-concurrent", "1")
		require.NoError(t, result.Err)
		assert.Contains(t, result.Stdout, "3 change(s) applied, 0 error(s)")
		assert.Len(t, fixture.writes(), 3)
	})
}

// Test_PushCommand_loadConfigFailure pins that a missing config file fails before anything else:
// the fake keychain store is installed so the failure cannot come from a real-Keychain prompt.
func Test_PushCommand_loadConfigFailure(t *testing.T) {
	t.Cleanup(config.SetKeychainStore(testutils.NewFakeKeychainStore()))

	result := runPush(t, "/nonexistent/path/config.yaml", pushBundleDir(t))
	require.Error(t, result.Err)
	assert.Contains(t, result.Err.Error(), "/nonexistent/path/config.yaml")
	assert.Nil(t, fail.ErrorToDetailedError(result.Err).ExitCode, "a config error must not claim the auth exit code")
}
