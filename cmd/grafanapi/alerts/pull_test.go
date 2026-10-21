package alerts_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	goyaml "github.com/goccy/go-yaml"
	"github.com/grafana/grafanapi/cmd/grafanapi/alerts"
	"github.com/grafana/grafanapi/cmd/grafanapi/config"
	"github.com/grafana/grafanapi/cmd/grafanapi/fail"
	intalerts "github.com/grafana/grafanapi/internal/alerts"
	"github.com/grafana/grafanapi/internal/testutils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The two endpoints `alerts pull` drives that the read commands do not; the other two
// (alertRulesPath, contactPointsPath) are declared in command_test.go. Spelled out rather than
// derived so a change to the generated client's path patterns fails here instead of silently
// querying nothing.
const policiesPath = "/api/v1/provisioning/policies"

// pullRuleList is the discovery fixture: three rules over two groups in two folders, so a
// --folder-uid filter has something to leave behind.
const pullRuleList = `[
  {"uid":"rule-uid-1","title":"first","folderUID":"folder-uid-1","ruleGroup":"group-a"},
  {"uid":"rule-uid-2","title":"second","folderUID":"folder-uid-1","ruleGroup":"group-a"},
  {"uid":"rule-uid-3","title":"third","folderUID":"folder-uid-2","ruleGroup":"group-b"}
]`

// The two whole-instance objects every pull reads, plus the two rule groups pullRuleList implies.
// Contact points reuse command_test.go's contactPointsBody, which already carries a "[REDACTED]"
// secret and a provenance the bundle writer has to strip.
const (
	pullGroupA = `{"folderUid":"folder-uid-1","title":"group-a","interval":60,"rules":[
	  {"uid":"rule-uid-1","title":"first","folderUID":"folder-uid-1","ruleGroup":"group-a",
	   "condition":"C","for":"1m","execErrState":"Error","noDataState":"NoData","orgID":1,
	   "provenance":"api","updated":"2026-09-08T09:08:07Z",
	   "data":[{"refId":"A","datasourceUid":"ds-uid-1","model":{"refId":"A","maxDataPoints":43200}}]}
	]}`
	pullGroupB = `{"folderUid":"folder-uid-2","title":"group-b","interval":300,"rules":[
	  {"uid":"rule-uid-3","title":"third","folderUID":"folder-uid-2","ruleGroup":"group-b",
	   "condition":"C","for":"5m","execErrState":"Error","noDataState":"NoData","orgID":1,
	   "data":[{"refId":"A","datasourceUid":"ds-uid-1","model":{"refId":"A"}}]}
	]}`
	pullPolicyTree = `{"receiver":"probe-slack","provenance":"api","routes":[{"receiver":"probe-slack"}]}`
)

// pullStub is one scripted response: the status and body a path answers with.
type pullStub struct {
	status int
	body   string
}

// okStub is the 200 case, which most routes are.
func okStub(body string) pullStub {
	return pullStub{status: http.StatusOK, body: body}
}

// pullFixture is a request-recording httptest server driven by a path→response table. It records
// paths (not bodies) because everything `alerts pull` sends is a GET: what the assertions need to
// know is which addresses were read, in particular whether the folder check ran.
type pullFixture struct {
	*httptest.Server

	mu        sync.Mutex
	requested []string
}

// folderPath is the folder-existence check `alerts pull --folder-uid` performs before listing.
func folderPath(uid string) string {
	return "/api/folders/" + uid
}

// groupPath is the provisioning address of one rule group: folder UID first, group title second.
func groupPath(folderUID, title string) string {
	return "/api/v1/provisioning/folder/" + folderUID + "/rule-groups/" + title
}

// pullRoutes is the full happy-path route table: the rule list, the two groups it implies, the
// contact points and the policy tree.
func pullRoutes() map[string]pullStub {
	return map[string]pullStub{
		alertRulesPath:                       okStub(pullRuleList),
		groupPath("folder-uid-1", "group-a"): okStub(pullGroupA),
		groupPath("folder-uid-2", "group-b"): okStub(pullGroupB),
		contactPointsPath:                    okStub(contactPointsBody),
		policiesPath:                         okStub(pullPolicyTree),
	}
}

// newPullFixture serves routes, records every path requested, and fails the test on a request to
// a path the table does not script - the shape a command quietly querying the wrong URL would
// take. Handlers use assert (not require) since they run on the server's own goroutine.
//
// rotatePath is scripted as a 401 like everywhere else in this package: it is where the
// session-rotating transport goes on its own when Grafana answers 401, and a 401 there is what a
// truly dead session looks like, so the original 401 surfaces unchanged.
func newPullFixture(t *testing.T, routes map[string]pullStub) *pullFixture {
	t.Helper()

	fixture := &pullFixture{}

	mux := http.NewServeMux()
	mux.HandleFunc(rotatePath, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		fixture.record(r.URL.Path)

		stub, ok := routes[r.URL.Path]
		if !ok {
			failUnexpectedPath(t)(w, r)

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

func (f *pullFixture) record(path string) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.requested = append(f.requested, path)
}

// sawPath reports whether path was requested at least once.
func (f *pullFixture) sawPath(path string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()

	return slices.Contains(f.requested, path)
}

// runPull executes `alerts pull` into dir with args appended to --config and --path.
func runPull(t *testing.T, configFile, dir string, args ...string) testutils.CommandResult {
	t.Helper()

	var result testutils.CommandResult

	testCase := testutils.CommandTestCase{
		Cmd:     alerts.Command(),
		Command: append([]string{"pull", "--config", configFile, "-p", dir}, args...),
		Assertions: []testutils.CommandAssertion{
			func(_ *testing.T, res testutils.CommandResult) { result = res },
		},
	}
	testCase.Run(t)

	return result
}

// newPullFixtureAndConfig is the happy-path setup: the full route table plus a matching config
// file and a temp destination directory. The tests that need to assert *which* paths were
// requested build their own route table and keep the fixture instead.
func newPullFixtureAndConfig(t *testing.T) (string, string) {
	t.Helper()

	fixture := newPullFixture(t, pullRoutes())
	configFile := newTestConfig(t, fixture.Server)

	return configFile, t.TempDir()
}

// bundleFiles lists every file under dir, relative to it, sorted - which is what lets a test
// assert the layout exactly rather than one path at a time.
func bundleFiles(t *testing.T, dir string) []string {
	t.Helper()

	var found []string

	require.NoError(t, filepath.WalkDir(dir, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if entry.IsDir() {
			return nil
		}

		rel, relErr := filepath.Rel(dir, path)
		if relErr != nil {
			return relErr
		}

		found = append(found, filepath.ToSlash(rel))

		return nil
	}))

	slices.Sort(found)

	return found
}

// Test_PullCommand_writesYAMLLayout is the happy path: the documented layout lands on disk in the
// default format, the summary reports all three kinds, and the read-only keys never reach a file.
func Test_PullCommand_writesYAMLLayout(t *testing.T) {
	configFile, dir := newPullFixtureAndConfig(t)

	result := runPull(t, configFile, dir)
	require.NoError(t, result.Err)

	assert.Equal(t, []string{
		"contact-points.yaml",
		"policies.yaml",
		"rules/folder-uid-1/group-a.yaml",
		"rules/folder-uid-2/group-b.yaml",
	}, bundleFiles(t, dir))

	assert.Contains(t, result.Stdout, "2 rule group(s)")
	assert.Contains(t, result.Stdout, "2 contact point(s)")
	assert.Contains(t, result.Stdout, "1 policy tree(s)")
	assert.Contains(t, result.Stdout, dir)

	raw, err := os.ReadFile(filepath.Join(dir, "rules", "folder-uid-1", "group-a.yaml"))
	require.NoError(t, err)

	var group map[string]any
	require.NoError(t, goyaml.Unmarshal(raw, &group))
	assert.Equal(t, "folder-uid-1", group["folderUid"])
	assert.Equal(t, "group-a", group["title"])
	assert.Equal(t, uint64(60), group["interval"], "an interval must stay an integer, not become 60.0")
	assert.NotContains(t, group, "provenance", "read-only keys never reach a file")

	rules, ok := group["rules"].([]any)
	require.True(t, ok, "a group file must carry its rules")
	require.Len(t, rules, 1)

	rule, ok := rules[0].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "folder-uid-1", rule["folderUID"], "rules spell the folder key folderUID, the group folderUid")
	assert.NotContains(t, rule, "provenance")
	assert.NotContains(t, rule, "updated")

	// The redacted secret is written back verbatim: pushing the placeholder is what keeps the
	// stored secret, so a pull must not drop or rewrite it.
	points, err := os.ReadFile(filepath.Join(dir, "contact-points.yaml"))
	require.NoError(t, err)
	assert.Contains(t, string(points), "[REDACTED]")
}

// Test_PullCommand_jsonFormat pins that -o selects the *file* format, not a rendering: the same
// layout lands with .json extensions and valid JSON inside.
func Test_PullCommand_jsonFormat(t *testing.T) {
	configFile, dir := newPullFixtureAndConfig(t)

	result := runPull(t, configFile, dir, "-o", "json")
	require.NoError(t, result.Err)

	assert.Equal(t, []string{
		"contact-points.json",
		"policies.json",
		"rules/folder-uid-1/group-a.json",
		"rules/folder-uid-2/group-b.json",
	}, bundleFiles(t, dir))

	raw, err := os.ReadFile(filepath.Join(dir, "rules", "folder-uid-2", "group-b.json"))
	require.NoError(t, err)

	var group map[string]any
	require.NoError(t, json.Unmarshal(raw, &group))
	assert.Equal(t, "folder-uid-2", group["folderUid"])
	assert.NotContains(t, group, "updated", "`updated` would otherwise leak into JSON files alone")
}

// Test_PullCommand_unknownOutputFormat pins that the table codec is deliberately NOT registered
// here: a bundle has to be readable back by ReadBundle, and -o table would write nothing it could
// parse.
func Test_PullCommand_unknownOutputFormat(t *testing.T) {
	configFile, dir := newPullFixtureAndConfig(t)

	for _, format := range []string{"table", "toml"} {
		t.Run(format, func(t *testing.T) {
			result := runPull(t, configFile, dir, "-o", format)
			require.Error(t, result.Err)
			assert.Contains(t, result.Err.Error(), "unknown output format")
			assert.Contains(t, result.Err.Error(), "json, yaml")
			assert.Empty(t, bundleFiles(t, dir), "validation runs before any file is written")
		})
	}
}

// Test_PullCommand_folderUIDFilter pins the two halves of --folder-uid: the folder is verified to
// exist before anything is listed, and only its groups are written.
func Test_PullCommand_folderUIDFilter(t *testing.T) {
	routes := pullRoutes()
	routes[folderPath("folder-uid-1")] = okStub(`{"uid":"folder-uid-1","title":"Probe Folder"}`)

	fixture := newPullFixture(t, routes)

	configFile := newTestConfig(t, fixture.Server)

	dir := t.TempDir()

	result := runPull(t, configFile, dir, "--folder-uid", "folder-uid-1")
	require.NoError(t, result.Err)

	assert.True(t, fixture.sawPath(folderPath("folder-uid-1")), "a named folder must be verified to exist")
	assert.False(t, fixture.sawPath(groupPath("folder-uid-2", "group-b")), "an unselected folder's groups are not fetched")

	assert.Equal(t, []string{
		"contact-points.yaml",
		"policies.yaml",
		"rules/folder-uid-1/group-a.yaml",
	}, bundleFiles(t, dir))

	assert.Contains(t, result.Stdout, "1 rule group(s)")
}

// Test_PullCommand_folderUIDKeepsOtherFolders pins the pruning half of a partial pull: a filtered
// pull removes the stale files of the folders it fetched and leaves every other folder's alone.
func Test_PullCommand_folderUIDKeepsOtherFolders(t *testing.T) {
	routes := pullRoutes()
	routes[folderPath("folder-uid-1")] = okStub(`{"uid":"folder-uid-1","title":"Probe Folder"}`)

	fixture := newPullFixture(t, routes)

	configFile := newTestConfig(t, fixture.Server)

	dir := t.TempDir()
	require.NoError(t, runPull(t, configFile, dir).Err)

	// A group that has since disappeared from the selected folder, and one in a folder this pull
	// will not name.
	stale := filepath.Join(dir, "rules", "folder-uid-1", "gone.yaml")
	require.NoError(t, os.WriteFile(stale, []byte("folderUid: folder-uid-1\n"), 0o600))

	result := runPull(t, configFile, dir, "--folder-uid", "folder-uid-1")
	require.NoError(t, result.Err)

	assert.Equal(t, []string{
		"contact-points.yaml",
		"policies.yaml",
		"rules/folder-uid-1/group-a.yaml",
		"rules/folder-uid-2/group-b.yaml",
	}, bundleFiles(t, dir), "the stale file goes, the unselected folder stays")
}

// Test_PullCommand_unknownFolderUID pins that a misspelled UID is an error rather than an empty
// bundle: GetAlertRules has no folder filter, so without the check a typo and a folder holding no
// rules are the same empty result - and the next push would read it as "these groups are gone".
func Test_PullCommand_unknownFolderUID(t *testing.T) {
	routes := pullRoutes()
	routes[folderPath("nope")] = pullStub{status: http.StatusNotFound, body: `{"message":"folder not found"}`}

	fixture := newPullFixture(t, routes)

	configFile := newTestConfig(t, fixture.Server)

	dir := t.TempDir()

	result := runPull(t, configFile, dir, "--folder-uid", "nope")
	require.Error(t, result.Err)
	assert.Contains(t, result.Err.Error(), "folder nope not found")
	assert.Nil(t, fail.ErrorToDetailedError(result.Err).ExitCode, "a missing folder must not claim the auth exit code")

	assert.Empty(t, bundleFiles(t, dir), "nothing is written when the selection could not be resolved")
	assert.False(t, fixture.sawPath(alertRulesPath), "the folder check runs before anything is listed")
}

// Test_PullCommand_apiError401 is the stale-session regression test: a dead session must render
// as exit 2 through this command too, and leave no half-written bundle behind.
func Test_PullCommand_apiError401(t *testing.T) {
	routes := pullRoutes()
	routes[alertRulesPath] = pullStub{status: http.StatusUnauthorized, body: `{"message":"Unauthorized"}`}

	fixture := newPullFixture(t, routes)

	configFile := newTestConfig(t, fixture.Server)

	dir := t.TempDir()

	result := runPull(t, configFile, dir)
	require.Error(t, result.Err)

	detailed := fail.ErrorToDetailedError(result.Err)
	require.NotNil(t, detailed.ExitCode)
	assert.Equal(t, 2, *detailed.ExitCode)
	assert.Contains(t, detailed.Summary, "stale or unauthorized")

	assert.Empty(t, bundleFiles(t, dir))
}

// Test_PullCommand_apiError500 pins the exit-code contract for a non-auth failure: the error
// surfaces with the status code and no explicit exit code, i.e. the default exit 1.
func Test_PullCommand_apiError500(t *testing.T) {
	routes := pullRoutes()
	routes[policiesPath] = pullStub{status: http.StatusInternalServerError, body: `{"message":"boom"}`}

	fixture := newPullFixture(t, routes)

	configFile := newTestConfig(t, fixture.Server)

	dir := t.TempDir()

	result := runPull(t, configFile, dir)
	require.Error(t, result.Err)
	assert.Contains(t, result.Err.Error(), "pull policy tree")
	assert.Contains(t, result.Err.Error(), "status 500")
	assert.Nil(t, fail.ErrorToDetailedError(result.Err).ExitCode, "a 500 must not claim the auth exit code")
	assert.Empty(t, bundleFiles(t, dir), "a bundle is written whole or not at all")
}

// Test_PullCommand_invalidFlags pins the pre-network validation: both checks fail before a config
// load, so an invalid flag costs neither a Keychain read nor a request.
func Test_PullCommand_invalidFlags(t *testing.T) {
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
			configFile, dir := newPullFixtureAndConfig(t)

			result := runPull(t, configFile, dir, testCase.args...)
			require.Error(t, result.Err)
			assert.Contains(t, result.Err.Error(), testCase.want)
		})
	}
}

// Test_PullCommand_sanitisedNameCollisionAbortsThePull pins what happens when two group titles in
// one folder sanitise to the same file name. The mapping is many-to-one by design, so the pull
// refuses the whole bundle rather than letting one group overwrite the other - which the next push
// would apply as "every rule of the overwritten group was deleted".
func Test_PullCommand_sanitisedNameCollisionAbortsThePull(t *testing.T) {
	// "a/b" and "a_b" both sanitise to "a_b.yaml": the separator becomes an underscore.
	rules := `[
	  {"uid":"rule-uid-1","title":"first","folderUID":"folder-uid-1","ruleGroup":"a/b"},
	  {"uid":"rule-uid-2","title":"second","folderUID":"folder-uid-1","ruleGroup":"a_b"}
	]`

	group := func(title string) string {
		return `{"folderUid":"folder-uid-1","title":"` + title + `","interval":60,"rules":[]}`
	}

	fixture := newPullFixture(t, map[string]pullStub{
		alertRulesPath:                   okStub(rules),
		groupPath("folder-uid-1", "a/b"): okStub(group("a/b")),
		groupPath("folder-uid-1", "a_b"): okStub(group("a_b")),
		contactPointsPath:                okStub(contactPointsBody),
		policiesPath:                     okStub(pullPolicyTree),
	})

	configFile := newTestConfig(t, fixture.Server)
	dir := t.TempDir()

	result := runPull(t, configFile, dir)
	require.Error(t, result.Err)
	assert.Contains(t, result.Err.Error(), "both map to")
	assert.Contains(t, result.Err.Error(), "rename one")

	assert.Empty(t, bundleFiles(t, dir), "a bundle that cannot be represented on disk leaves nothing behind")
}

// Test_PullCommand_loadConfigFailure pins that a missing config file fails before anything else:
// the fake keychain store is installed so the failure cannot come from a real-Keychain prompt.
func Test_PullCommand_loadConfigFailure(t *testing.T) {
	t.Cleanup(config.SetKeychainStore(testutils.NewFakeKeychainStore()))

	dir := t.TempDir()

	result := runPull(t, "/nonexistent/path/config.yaml", dir)
	require.Error(t, result.Err)
	assert.Contains(t, result.Err.Error(), "/nonexistent/path/config.yaml")
	assert.Nil(t, fail.ErrorToDetailedError(result.Err).ExitCode, "a config error must not claim the auth exit code")
	assert.Empty(t, bundleFiles(t, dir), "no bundle may be written when the config never loaded")
}

// ruleScopeGroup is a rule group holding two rules, so a rule-scoped pull has a sibling to leave
// alone: rule-uid-1 is the one the tests name, rule-uid-2 the one they edit locally.
func ruleScopeGroup(title string) string {
	rule := func(uid, name string) string {
		return `{"uid":"` + uid + `","title":"` + name + `","folderUID":"folder-uid-1","ruleGroup":"` + title + `",
		  "condition":"C","for":"1m","execErrState":"Error","noDataState":"NoData","orgID":1,
		  "data":[{"refId":"A","datasourceUid":"ds-uid-1","model":{"refId":"A"}}]}`
	}

	return `{"folderUid":"folder-uid-1","title":"` + title + `","interval":60,"rules":[` +
		rule("rule-uid-1", "first") + `,` + rule("rule-uid-2", "second") + `]}`
}

// ruleScopeRoutes serves the two GETs a rule-scoped pull makes, plus the whole-instance routes of
// a full pull so a test can seed a bundle first. A request to contact points or policies during
// the scoped pull is what the tests assert never happens.
func ruleScopeRoutes(group string) map[string]pullStub {
	routes := pullRoutes()
	routes[groupPath("folder-uid-1", group)] = okStub(ruleScopeGroup(group))
	routes[opRulePath("rule-uid-1")] = okStub(
		`{"uid":"rule-uid-1","title":"first","folderUID":"folder-uid-1","ruleGroup":"` + group + `"}`)
	routes[alertRulesPath] = okStub(`[
	  {"uid":"rule-uid-1","title":"first","folderUID":"folder-uid-1","ruleGroup":"` + group + `"},
	  {"uid":"rule-uid-2","title":"second","folderUID":"folder-uid-1","ruleGroup":"` + group + `"},
	  {"uid":"rule-uid-3","title":"third","folderUID":"folder-uid-2","ruleGroup":"group-b"}
	]`)

	return routes
}

// reset forgets the requests seen so far, so a test can seed a bundle with a full pull and then
// assert on the requests of the command under test alone.
func (f *pullFixture) reset() {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.requested = nil
}

// requestedPaths returns a copy of every path requested since the last reset.
func (f *pullFixture) requestedPaths() []string {
	f.mu.Lock()
	defer f.mu.Unlock()

	return slices.Clone(f.requested)
}

func readFile(t *testing.T, path string) string {
	t.Helper()

	raw, err := os.ReadFile(path)
	require.NoError(t, err)

	return string(raw)
}

// seedRuleScopeBundle runs a full pull against group-a holding two rules, leaving a bundle with
// contact points, policies and two group files, and returns the fixture, config and directory.
func seedRuleScopeBundle(t *testing.T) (*pullFixture, string, string) {
	t.Helper()

	fixture := newPullFixture(t, ruleScopeRoutes("group-a"))
	configFile := newTestConfig(t, fixture.Server)
	dir := t.TempDir()

	require.NoError(t, runPull(t, configFile, dir).Err)
	fixture.reset()

	return fixture, configFile, dir
}

// Test_PullCommand_ruleUID pins the happy path: the one group file is rewritten whole, the other
// files stay byte-identical, only the rule and its group are requested, and the summary says that
// contact points and policies were not pulled.
func Test_PullCommand_ruleUID(t *testing.T) {
	fixture, configFile, dir := seedRuleScopeBundle(t)

	groupFile := filepath.Join(dir, "rules", "folder-uid-1", "group-a.yaml")
	want := readFile(t, groupFile)
	others := map[string]string{}

	for _, rel := range []string{"contact-points.yaml", "policies.yaml", "rules/folder-uid-2/group-b.yaml"} {
		others[rel] = readFile(t, filepath.Join(dir, rel))
	}

	// A local edit to the NAMED rule is discarded by design: the file is replaced with Grafana's.
	edited := strings.Replace(want, "first", "edited-locally", 1)
	require.NotEqual(t, want, edited)
	require.NoError(t, os.WriteFile(groupFile, []byte(edited), 0o600))

	result := runPull(t, configFile, dir, "--rule-uid", "rule-uid-1")
	require.NoError(t, result.Err)

	assert.Equal(t, want, readFile(t, groupFile), "the named rule is replaced with Grafana's version")
	assert.Contains(t, want, "rule-uid-2", "the file holds the whole group")

	for rel, content := range others {
		assert.Equal(t, content, readFile(t, filepath.Join(dir, rel)), rel)
	}

	assert.Contains(t, result.Stdout, "1 rule group(s) written to "+dir+"; contact points and policies were not pulled")
	assert.Equal(t, []string{opRulePath("rule-uid-1"), groupPath("folder-uid-1", "group-a")}, fixture.requestedPaths(),
		"no contact points, no policies, no rule listing")
}

// Test_PullCommand_ruleUIDJSONIntoEmptyBundle pins that -o still picks the file format when no file
// exists for the group yet.
func Test_PullCommand_ruleUIDJSONIntoEmptyBundle(t *testing.T) {
	fixture := newPullFixture(t, ruleScopeRoutes("group-a"))
	configFile := newTestConfig(t, fixture.Server)
	dir := t.TempDir()

	result := runPull(t, configFile, dir, "--rule-uid", "rule-uid-1", "-o", "json")
	require.NoError(t, result.Err)

	assert.Equal(t, []string{"rules/folder-uid-1/group-a.json"}, bundleFiles(t, dir))
}

// Test_PullCommand_ruleUIDInvalidFlags pins the pre-network refusals: each one fails before a
// config load, so no request is recorded.
func Test_PullCommand_ruleUIDInvalidFlags(t *testing.T) {
	cases := map[string]struct {
		args []string
		want string
	}{
		"with folder-uid": {
			args: []string{"--rule-uid", "x", "--folder-uid", "y"},
			want: "--rule-uid and --folder-uid cannot be combined",
		},
		"empty uid":                {args: []string{"--rule-uid", ""}, want: "--rule-uid must not be empty"},
		"blank uid":                {args: []string{"--rule-uid", " "}, want: "--rule-uid must not be empty"},
		"overwrite without a rule": {args: []string{"--overwrite"}, want: "--overwrite only applies with --rule-uid"},
	}

	for name, testCase := range cases {
		t.Run(name, func(t *testing.T) {
			fixture := newPullFixture(t, ruleScopeRoutes("group-a"))
			configFile := newTestConfig(t, fixture.Server)
			dir := t.TempDir()

			result := runPull(t, configFile, dir, testCase.args...)
			require.Error(t, result.Err)
			assert.Contains(t, result.Err.Error(), testCase.want)
			assert.Empty(t, fixture.requestedPaths())
			assert.Empty(t, bundleFiles(t, dir))
		})
	}
}

// Test_PullCommand_ruleUIDRefusesTheOtherFormatTwin pins that a file for the group in another
// format is refused, nothing is written or removed, and the bundle still reads back.
func Test_PullCommand_ruleUIDRefusesTheOtherFormatTwin(t *testing.T) {
	_, configFile, dir := seedRuleScopeBundle(t)
	before := bundleFiles(t, dir)

	result := runPull(t, configFile, dir, "--rule-uid", "rule-uid-1", "-o", "json", "--overwrite")
	require.Error(t, result.Err)
	assert.Contains(t, result.Err.Error(), "group-a.yaml")
	assert.Contains(t, result.Err.Error(), "-o")

	assert.Equal(t, before, bundleFiles(t, dir), "nothing is written or removed")

	_, err := intalerts.ReadBundle(dir)
	require.NoError(t, err, "the bundle is not wedged")
}

// Test_PullCommand_ruleUIDSiblingEdit pins the data-loss guard: a file whose other rules differ
// from Grafana is refused (byte-identical afterwards), and --overwrite replaces it.
func Test_PullCommand_ruleUIDSiblingEdit(t *testing.T) {
	_, configFile, dir := seedRuleScopeBundle(t)

	groupFile := filepath.Join(dir, "rules", "folder-uid-1", "group-a.yaml")
	remote := readFile(t, groupFile)
	tampered := strings.Replace(remote, "second", "tampered", 1)
	require.NotEqual(t, remote, tampered)
	require.NoError(t, os.WriteFile(groupFile, []byte(tampered), 0o600))

	result := runPull(t, configFile, dir, "--rule-uid", "rule-uid-1")
	require.Error(t, result.Err)
	assert.Contains(t, result.Err.Error(), "group-a.yaml")
	assert.Contains(t, result.Err.Error(), "--overwrite")
	assert.Nil(t, fail.ErrorToDetailedError(result.Err).ExitCode)
	assert.Equal(t, tampered, readFile(t, groupFile), "a refused pull leaves the file byte-identical")

	result = runPull(t, configFile, dir, "--rule-uid", "rule-uid-1", "--overwrite")
	require.NoError(t, result.Err)
	assert.Equal(t, remote, readFile(t, groupFile), "the sibling equals Grafana's again")
}

// Test_PullCommand_ruleUIDMovedRule pins the fail-closed behaviour for a rule that moved group:
// the new group's file is written, the old file is left, and the bundle then refuses to read,
// naming both files, until the user removes the old one.
func Test_PullCommand_ruleUIDMovedRule(t *testing.T) {
	fixture := newPullFixture(t, ruleScopeRoutes("probe-group"))
	configFile := newTestConfig(t, fixture.Server)
	dir := t.TempDir()

	old := strings.ReplaceAll(ruleScopeGroupYAML(), "probe-group", "old-group")
	oldFile := filepath.Join(dir, "rules", "folder-uid-1", "old-group.yaml")
	require.NoError(t, os.MkdirAll(filepath.Dir(oldFile), 0o750))
	require.NoError(t, os.WriteFile(oldFile, []byte(old), 0o600))

	result := runPull(t, configFile, dir, "--rule-uid", "rule-uid-1")
	require.NoError(t, result.Err)

	assert.Equal(t, []string{
		"rules/folder-uid-1/old-group.yaml",
		"rules/folder-uid-1/probe-group.yaml",
	}, bundleFiles(t, dir))
	assert.Equal(t, old, readFile(t, oldFile), "the old file is left alone")

	_, err := intalerts.ReadBundle(dir)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "old-group.yaml")
	assert.Contains(t, err.Error(), "probe-group.yaml")
}

// ruleScopeGroupYAML is a one-rule group file for rule-uid-1 in "probe-group".
func ruleScopeGroupYAML() string {
	return `folderUid: folder-uid-1
title: probe-group
interval: 60
rules:
  - uid: rule-uid-1
    title: first
    condition: C
    for: 1m
    execErrState: Error
    noDataState: NoData
    data:
      - refId: A
        datasourceUid: ds-uid-1
        model:
          refId: A
`
}

// Test_PullCommand_ruleUIDUnknown pins that an unknown uid renders as "Alert rule not found" and
// stops the pull before anything is written.
func Test_PullCommand_ruleUIDUnknown(t *testing.T) {
	routes := ruleScopeRoutes("group-a")
	routes[opRulePath("rule-uid-gone")] = pullStub{status: http.StatusNotFound, body: `{"message":"not found"}`}

	fixture := newPullFixture(t, routes)
	configFile := newTestConfig(t, fixture.Server)
	dir := t.TempDir()

	result := runPull(t, configFile, dir, "--rule-uid", "rule-uid-gone")
	require.Error(t, result.Err)

	detailed := fail.ErrorToDetailedError(result.Err)
	assert.Equal(t, "Alert rule not found", detailed.Summary)
	assert.Nil(t, detailed.ExitCode)
	assert.Empty(t, bundleFiles(t, dir))
}

// Test_PullCommand_ruleUIDApiError401 pins the stale-session exit code for the rule GET.
func Test_PullCommand_ruleUIDApiError401(t *testing.T) {
	routes := ruleScopeRoutes("group-a")
	routes[opRulePath("rule-uid-1")] = pullStub{status: http.StatusUnauthorized, body: `{"message":"Unauthorized"}`}

	fixture := newPullFixture(t, routes)
	configFile := newTestConfig(t, fixture.Server)
	dir := t.TempDir()

	result := runPull(t, configFile, dir, "--rule-uid", "rule-uid-1")
	require.Error(t, result.Err)

	detailed := fail.ErrorToDetailedError(result.Err)
	require.NotNil(t, detailed.ExitCode)
	assert.Equal(t, 2, *detailed.ExitCode)
	assert.Contains(t, detailed.Summary, "stale or unauthorized")
	assert.Empty(t, bundleFiles(t, dir))
}
