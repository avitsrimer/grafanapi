package alerts_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-openapi/runtime"
	goapi "github.com/grafana/grafana-openapi-client-go/client"
	"github.com/grafana/grafanapi/internal/alerts"
	"github.com/grafana/grafanapi/internal/config"
	"github.com/grafana/grafanapi/internal/grafana"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// policiesPath is the third endpoint a pull always reads; the other two (alertRulesPath,
// contactPointsPath) are declared in rules_test.go. Spelled out rather than derived so a change
// to the generated client's path patterns fails here instead of silently querying nothing.
const policiesPath = "/api/v1/provisioning/policies"

// groupPathPrefix is what every rule-group address starts with, which is how the concurrency test
// tells the group fetches apart from the two whole-instance reads that follow them.
const groupPathPrefix = "/api/v1/provisioning/folder/"

// pullRuleList is the discovery fixture: four rules across three groups in two folders. Two of
// them share a group, which is what pins that the group is fetched once rather than once per
// rule.
const pullRuleList = `[
  {"uid":"rule-uid-1","title":"first","folderUID":"folder-uid-1","ruleGroup":"group-b"},
  {"uid":"rule-uid-2","title":"second","folderUID":"folder-uid-1","ruleGroup":"group-a"},
  {"uid":"rule-uid-3","title":"third","folderUID":"folder-uid-1","ruleGroup":"group-a"},
  {"uid":"rule-uid-4","title":"fourth","folderUID":"folder-uid-2","ruleGroup":"group-c"}
]`

// pullContactPoints and pullPolicyTree are the two whole-instance objects every pull reads. The
// redacted setting and the per-node provenance are both carried through verbatim: what a pull
// writes to disk is decided by WriteBundle, not here.
const (
	pullContactPoints = `[{"uid":"cp-uid-1","name":"probe-slack","type":"slack","settings":{"url":"[REDACTED]"}}]`
	pullPolicyTree    = `{"receiver":"probe-slack","provenance":"api","routes":[{"receiver":"probe-slack"}]}`
)

// folderPath is the folder-existence check `alerts pull --folder-uid` performs before listing.
func folderPath(uid string) string {
	return "/api/folders/" + uid
}

// groupPath is the provisioning address of one rule group: folder UID first, group title second.
func groupPath(folderUID, title string) string {
	return groupPathPrefix + folderUID + "/rule-groups/" + title
}

// groupResponse builds one rule-group payload with the given rules, enough for Pull to return it
// and for the assertions to tell the groups apart.
func groupResponse(folderUID, title string, interval int, ruleUIDs ...string) stubResponse {
	rules := make([]string, 0, len(ruleUIDs))

	for _, uid := range ruleUIDs {
		rules = append(rules, fmt.Sprintf(
			`{"uid":%q,"title":%q,"folderUID":%q,"ruleGroup":%q,"provenance":"api"}`,
			uid, uid+"-title", folderUID, title,
		))
	}

	return okResponse(fmt.Sprintf(
		`{"folderUid":%q,"title":%q,"interval":%d,"rules":[%s]}`,
		folderUID, title, interval, strings.Join(rules, ","),
	))
}

// pullRoutes is the full happy-path route table: the rule list, the three groups it implies, the
// contact points and the policy tree.
func pullRoutes() map[string]stubResponse {
	return map[string]stubResponse{
		alertRulesPath:                       okResponse(pullRuleList),
		groupPath("folder-uid-1", "group-a"): groupResponse("folder-uid-1", "group-a", 60, "rule-uid-2", "rule-uid-3"),
		groupPath("folder-uid-1", "group-b"): groupResponse("folder-uid-1", "group-b", 120, "rule-uid-1"),
		groupPath("folder-uid-2", "group-c"): groupResponse("folder-uid-2", "group-c", 300, "rule-uid-4"),
		contactPointsPath:                    okResponse(pullContactPoints),
		policiesPath:                         okResponse(pullPolicyTree),
	}
}

// TestPull_ConcurrencyBoundHonoured pins that MaxConcurrent actually bounds the group fetches:
// without a bound a pull of a large instance would open one connection per group at once.
//
// The fetches are released in batches of exactly the limit rather than held for a wall-clock
// delay, which pins the bound in both directions with nothing timing-dependent about it: a run
// that never reaches the limit never opens the barrier (and fails on its timeout), and a run that
// exceeds it fails on the peak.
func TestPull_ConcurrencyBoundHonoured(t *testing.T) {
	const (
		groupCount = 6
		limit      = 2
	)

	routes := map[string]stubResponse{
		contactPointsPath: okResponse(`[]`),
		policiesPath:      okResponse(`{"receiver":"probe-slack"}`),
	}

	refs := make([]alerts.GroupRef, 0, groupCount)

	for i := range groupCount {
		title := fmt.Sprintf("group-%d", i)
		refs = append(refs, alerts.GroupRef{FolderUID: "folder-uid-1", Title: title})
		routes[groupPath("folder-uid-1", title)] = groupResponse("folder-uid-1", title, 60)
	}

	gate := &batchBarrier{limit: limit}

	// Only the group fetches are held. The contact-point and policy reads run after them, one at a
	// time, so holding those would simply deadlock.
	fixture := newHoldingPullFixture(t, routes, func(path string) {
		if strings.HasPrefix(path, groupPathPrefix) {
			gate.wait(t)
		}
	})

	bundle, err := alerts.Pull(t.Context(), fixture.client, alerts.PullOptions{Groups: refs, MaxConcurrent: limit})
	require.NoError(t, err)
	assert.Len(t, bundle.Groups, groupCount)

	assert.Equal(t, limit, fixture.peakInFlight(),
		"the group fetches must run concurrently up to --max-concurrent, and never past it")
}

// batchBarrier releases its waiters in batches of exactly limit. It is what lets the concurrency
// test observe the bound without sleeping: the batch only opens once the bound is saturated.
type batchBarrier struct {
	mu      sync.Mutex
	limit   int
	waiting int
	gate    chan struct{}
}

// wait blocks until limit callers (this one included) are waiting together, failing the test
// rather than hanging forever if that never happens.
func (b *batchBarrier) wait(t *testing.T) {
	t.Helper()

	b.mu.Lock()

	if b.gate == nil {
		b.gate = make(chan struct{})
	}

	gate := b.gate
	b.waiting++

	if b.waiting == b.limit {
		close(gate)
		b.waiting, b.gate = 0, nil
	}

	b.mu.Unlock()

	select {
	case <-gate:
	case <-time.After(5 * time.Second):
		assert.Fail(t, "the group fetches never reached the concurrency bound")
	}
}

// TestPull_DiscoversGroupsAcrossFolders is the happy path: with no selection at all, every rule
// group Grafana lists is fetched once and returned in the bundle's on-disk order, alongside the
// two whole-instance objects.
func TestPull_DiscoversGroupsAcrossFolders(t *testing.T) {
	fixture := newPullFixture(t, pullRoutes())

	bundle, err := alerts.Pull(t.Context(), fixture.client, alerts.PullOptions{})
	require.NoError(t, err)

	require.Len(t, bundle.Groups, 3)
	assert.Equal(t, "folder-uid-1", bundle.Groups[0].FolderUID)
	assert.Equal(t, "group-a", bundle.Groups[0].Title)
	assert.Equal(t, "group-b", bundle.Groups[1].Title)
	assert.Equal(t, "folder-uid-2", bundle.Groups[2].FolderUID)
	assert.Equal(t, "group-c", bundle.Groups[2].Title)
	assert.Equal(t, int64(60), bundle.Groups[0].Interval)
	assert.Len(t, bundle.Groups[0].Rules, 2)

	require.Len(t, bundle.ContactPoints, 1)
	assert.Equal(t, "cp-uid-1", bundle.ContactPoints[0].UID)
	require.NotNil(t, bundle.Policies)
	assert.Equal(t, "probe-slack", bundle.Policies.Receiver)
	require.Len(t, bundle.Policies.Routes, 1)

	assert.Equal(t, 1, fixture.requestCount(alertRulesPath))
	assert.Equal(t, 1, fixture.requestCount(groupPath("folder-uid-1", "group-a")),
		"a group holding two rules must still be fetched once")
	assert.Equal(t, 1, fixture.requestCount(policiesPath))
}

// TestPull_EmptyFolderIsNotAnError is the other half of the folder check: a folder that exists
// but holds no rules is an empty result, not a failure. Without the existence check this case
// would be indistinguishable from a typo, which is exactly why the check exists.
func TestPull_EmptyFolderIsNotAnError(t *testing.T) {
	fixture := newPullFixture(t, map[string]stubResponse{
		folderPath("folder-uid-empty"): okResponse(`{"uid":"folder-uid-empty","title":"Empty"}`),
		alertRulesPath:                 okResponse(pullRuleList),
		contactPointsPath:              okResponse(pullContactPoints),
		policiesPath:                   okResponse(pullPolicyTree),
	})

	bundle, err := alerts.Pull(t.Context(), fixture.client, alerts.PullOptions{FolderUIDs: []string{"folder-uid-empty"}})
	require.NoError(t, err)
	assert.NotNil(t, bundle.Groups, "an empty selection must still produce an empty rules/ tree, not a nil one")
	assert.Empty(t, bundle.Groups)
}

// TestPull_FolderUIDsFilter pins both halves of a filtered pull: the folder is verified to exist
// before anything is listed, and only its groups are fetched. The fixture's route table is the
// assertion for the second half - a request for folder-uid-2's group would fail the test.
func TestPull_FolderUIDsFilter(t *testing.T) {
	fixture := newPullFixture(t, map[string]stubResponse{
		folderPath("folder-uid-1"):           okResponse(`{"uid":"folder-uid-1","title":"Probe"}`),
		alertRulesPath:                       okResponse(pullRuleList),
		groupPath("folder-uid-1", "group-a"): groupResponse("folder-uid-1", "group-a", 60, "rule-uid-2", "rule-uid-3"),
		groupPath("folder-uid-1", "group-b"): groupResponse("folder-uid-1", "group-b", 120, "rule-uid-1"),
		contactPointsPath:                    okResponse(pullContactPoints),
		policiesPath:                         okResponse(pullPolicyTree),
	})

	bundle, err := alerts.Pull(t.Context(), fixture.client, alerts.PullOptions{FolderUIDs: []string{"folder-uid-1"}})
	require.NoError(t, err)

	require.Len(t, bundle.Groups, 2)
	assert.Equal(t, "group-a", bundle.Groups[0].Title)
	assert.Equal(t, "group-b", bundle.Groups[1].Title)
	assert.Equal(t, 1, fixture.requestCount(folderPath("folder-uid-1")), "each --folder-uid must be verified to exist")
}

// TestPull_GroupSelectionSkipsTheRuleList covers the mode push uses: the caller already knows
// which groups it cares about, so exactly those are fetched and the whole-instance rule listing
// is never issued. The route table omits it, so a listing would fail the test outright.
func TestPull_GroupSelectionSkipsTheRuleList(t *testing.T) {
	fixture := newPullFixture(t, map[string]stubResponse{
		groupPath("folder-uid-1", "group-a"): groupResponse("folder-uid-1", "group-a", 60, "rule-uid-2"),
		groupPath("folder-uid-2", "group-c"): groupResponse("folder-uid-2", "group-c", 300, "rule-uid-4"),
		contactPointsPath:                    okResponse(pullContactPoints),
		policiesPath:                         okResponse(pullPolicyTree),
	})

	bundle, err := alerts.Pull(t.Context(), fixture.client, alerts.PullOptions{
		Groups: []alerts.GroupRef{
			{FolderUID: "folder-uid-2", Title: "group-c"},
			{FolderUID: "folder-uid-1", Title: "group-a"},
			// Repeated on purpose: a local bundle naming the same group twice must not double
			// the requests.
			{FolderUID: "folder-uid-1", Title: "group-a"},
		},
	})
	require.NoError(t, err)

	require.Len(t, bundle.Groups, 2)
	assert.Equal(t, "group-a", bundle.Groups[0].Title)
	assert.Equal(t, "group-c", bundle.Groups[1].Title)

	assert.Equal(t, 0, fixture.requestCount(alertRulesPath), "a group selection must not list every rule")
	assert.Equal(t, 1, fixture.requestCount(groupPath("folder-uid-1", "group-a")))
	assert.Equal(t, 1, fixture.requestCount(groupPath("folder-uid-2", "group-c")))
}

// TestPull_MissingGroupIsAbsent pins that a 404 on a selected group is absence, not failure: it
// is the normal answer for a group a push is about to create, and for one deleted between the
// listing and its fetch.
func TestPull_MissingGroupIsAbsent(t *testing.T) {
	fixture := newPullFixture(t, map[string]stubResponse{
		groupPath("folder-uid-1", "group-a"): groupResponse("folder-uid-1", "group-a", 60, "rule-uid-2"),
		groupPath("folder-uid-1", "group-new"): {
			status: http.StatusNotFound,
			body:   `{"message":"rule group not found"}`,
		},
		contactPointsPath: okResponse(pullContactPoints),
		policiesPath:      okResponse(pullPolicyTree),
	})

	bundle, err := alerts.Pull(t.Context(), fixture.client, alerts.PullOptions{
		Groups: []alerts.GroupRef{
			{FolderUID: "folder-uid-1", Title: "group-a"},
			{FolderUID: "folder-uid-1", Title: "group-new"},
		},
	})
	require.NoError(t, err)

	require.Len(t, bundle.Groups, 1)
	assert.Equal(t, "group-a", bundle.Groups[0].Title)
}

// TestPull_NilClientIsAnError guards the caller contract the way the read path does: a nil client
// must be a clear error rather than a nil dereference.
func TestPull_NilClientIsAnError(t *testing.T) {
	_, err := alerts.Pull(t.Context(), nil, alerts.PullOptions{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no grafana client configured")
}

// TestPull_ServerErrorIsWrapped pins that a failure that is not a 401 names the group it happened
// on - a pull touches one request per group, so the bare transport error would say nothing about
// which - while still carrying the status for the converter.
func TestPull_ServerErrorIsWrapped(t *testing.T) {
	fixture := newPullFixture(t, map[string]stubResponse{
		groupPath("folder-uid-1", "group-a"): {status: http.StatusInternalServerError, body: `{"message":"boom"}`},
		contactPointsPath:                    okResponse(pullContactPoints),
		policiesPath:                         okResponse(pullPolicyTree),
	})

	_, err := alerts.Pull(t.Context(), fixture.client, alerts.PullOptions{
		Groups: []alerts.GroupRef{{FolderUID: "folder-uid-1", Title: "group-a"}},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "alerts: pull rule group folder-uid-1/group-a")

	var apiErr *runtime.APIError
	require.ErrorAs(t, err, &apiErr)
	assert.Equal(t, http.StatusInternalServerError, apiErr.Code)
}

// TestPull_UnknownFolderIsAnError covers the reason the folder check exists: GetAlertRules has no
// folder filter, so without it a misspelled --folder-uid would pull an empty bundle and the next
// push would read that as "every group in this folder was deleted".
func TestPull_UnknownFolderIsAnError(t *testing.T) {
	fixture := newPullFixture(t, map[string]stubResponse{
		folderPath("folder-uid-missing"): {status: http.StatusNotFound, body: `{"message":"folder not found"}`},
	})

	_, err := alerts.Pull(t.Context(), fixture.client, alerts.PullOptions{FolderUIDs: []string{"folder-uid-missing"}})
	require.Error(t, err)
	assert.Equal(t, "alerts: pull: folder folder-uid-missing not found", err.Error())
	assert.Equal(t, 0, fixture.requestCount(alertRulesPath), "an unknown folder must fail before anything is listed")
}

// TestPull_Unauthorized is the exit-2 regression test, run against both transports a pull uses.
//
// The folder case is the one that can regress silently: unlike the provisioning operations, the
// generated folders client declares a *typed* 401 response, so the error it returns is not a
// *runtime.APIError at all. Pull converts it, and without that conversion a dead session during
// `alerts pull --folder-uid` would exit 1 with an opaque message instead of exiting 2 with the
// "run login update" suggestion.
func TestPull_Unauthorized(t *testing.T) {
	unauthorized := stubResponse{status: http.StatusUnauthorized, body: `{"message":"Unauthorized"}`}

	cases := []struct {
		name   string
		routes map[string]stubResponse
		opts   alerts.PullOptions
	}{
		{
			name:   "from the provisioning rule listing",
			routes: map[string]stubResponse{alertRulesPath: unauthorized},
		},
		{
			name:   "from the typed folder check",
			routes: map[string]stubResponse{folderPath("folder-uid-1"): unauthorized},
			opts:   alerts.PullOptions{FolderUIDs: []string{"folder-uid-1"}},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newPullFixture(t, tc.routes)

			_, err := alerts.Pull(t.Context(), fixture.client, tc.opts)
			require.Error(t, err)

			var apiErr *runtime.APIError
			require.ErrorAs(t, err, &apiErr)
			assert.Equal(t, http.StatusUnauthorized, apiErr.Code)
		})
	}
}

// pullFixture is a generated client wired against a scripted server that also records what was
// requested and how much of it ran at once - the two things a pull's behaviour is defined by
// beyond the bundle it returns.
type pullFixture struct {
	client *goapi.GrafanaHTTPAPI

	mu       sync.Mutex
	counts   map[string]int
	inFlight int
	peak     int
}

// newHoldingPullFixture is newPullFixture with a hook that may hold a request before it is
// answered, which is what makes the concurrency bound observable without timing anything.
func newHoldingPullFixture(t *testing.T, routes map[string]stubResponse, hold func(path string)) *pullFixture {
	t.Helper()

	fixture := &pullFixture{counts: map[string]int{}}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fixture.enter(r.URL.Path)
		defer fixture.leave()

		route, ok := routes[r.URL.Path]
		if !ok {
			assert.Fail(t, "unexpected request path", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)

			return
		}

		if hold != nil {
			hold(r.URL.Path)
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(route.status)

		_, err := w.Write([]byte(route.body))
		assert.NoError(t, err)
	}))
	t.Cleanup(server.Close)

	gClient, err := grafana.ClientFromContext(&config.Context{
		Grafana: &config.GrafanaConfig{Server: server.URL, SessionCookie: "abc123"},
	})
	require.NoError(t, err)

	fixture.client = gClient

	return fixture
}

// newPullFixture serves routes (path -> response) as JSON, failing the test on any request to a
// path the table does not cover.
func newPullFixture(t *testing.T, routes map[string]stubResponse) *pullFixture {
	t.Helper()

	return newHoldingPullFixture(t, routes, nil)
}

// enter records one request and updates the peak number of concurrent ones.
func (f *pullFixture) enter(path string) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.counts[path]++
	f.inFlight++

	if f.inFlight > f.peak {
		f.peak = f.inFlight
	}
}

// leave marks one request as finished.
func (f *pullFixture) leave() {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.inFlight--
}

// peakInFlight returns the highest number of requests that were in flight at once.
func (f *pullFixture) peakInFlight() int {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.peak
}

// requestCount returns how many times path was requested.
func (f *pullFixture) requestCount(path string) int {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.counts[path]
}
