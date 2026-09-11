package alerts_test

import (
	"encoding/json"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-openapi/runtime"
	goapi "github.com/grafana/grafana-openapi-client-go/client"
	"github.com/grafana/grafana-openapi-client-go/models"
	"github.com/grafana/grafanapi/internal/alerts"
	"github.com/grafana/grafanapi/internal/config"
	"github.com/grafana/grafanapi/internal/grafana"
	"github.com/grafana/grafanapi/internal/testutils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// disableProvenanceHeader is the header every write is judged by: present and "true" leaves the
// object editable in the UI, absent claims it for the API. Getting it wrong is a 500, a 409 or a
// silent change of ownership, which is why these tests assert its presence rather than its effect.
const disableProvenanceHeader = "X-Disable-Provenance"

// The three write responses Grafana actually returns. The generated readers accept exactly these
// codes and turn anything else into an error, so a stub returning 200 for a contact point would
// fail for the wrong reason.
const (
	acceptedResponseBody = `{"message":"accepted"}`
	pushedGroupBody      = `{"folderUid":"folder-uid-1","title":"probe-group","interval":60,"rules":[]}`
)

// pushRemoteGroup is the remote counterpart most tests reconcile against: the same group the local
// bundle holds, claimed by the API, so the write mirrors "api" and the header is omitted.
const pushRemoteGroup = `{
  "folderUid": "folder-uid-1",
  "title": "probe-group",
  "interval": 60,
  "rules": [{
    "uid": "rule-uid-1",
    "title": "rule one",
    "folderUID": "folder-uid-1",
    "ruleGroup": "probe-group",
    "orgID": 1,
    "provenance": "api"
  }]
}`

// TestPush_CreatesEveryKind is the happy create path: a bundle whose objects do not exist remotely
// is POSTed (contact point) or PUT whole (policy tree, rule group), each with the header its
// provenance implies.
func TestPush_CreatesEveryKind(t *testing.T) {
	fixture := newPushFixture(t, pushRoutes(map[string]stubResponse{
		pushRoute(http.MethodGet, groupPath("folder-uid-1", "probe-group")): notFoundResponse(),
		pushRoute(http.MethodGet, folderPath("folder-uid-1")):               okResponse(`{"uid":"folder-uid-1"}`),
		pushRoute(http.MethodPost, contactPointsPath):                       acceptedResponse(),
		pushRoute(http.MethodPut, policiesPath):                             acceptedResponse(),
		pushRoute(http.MethodPut, groupPath("folder-uid-1", "probe-group")): okResponse(pushedGroupBody),
	}))

	result, err := alerts.Push(t.Context(), fixture.client, pushLocalBundle(), alerts.PushOptions{})
	require.NoError(t, err)
	assert.Empty(t, result.Failed)
	require.Len(t, result.Applied, 3)

	created := fixture.requests(http.MethodPost, contactPointsPath)
	require.Len(t, created, 1)
	assert.Nil(t, created[0].provenance, "a create claims the object for the API unless --ui-editable says otherwise")
	assert.Contains(t, created[0].body, `"uid":"cp-uid-1"`)
	assert.NotContains(t, created[0].body, `"provenance"`)

	// The policy tree always exists in Grafana, so its entry is an update even here; the remote
	// tree is unclaimed, which is what the header says.
	tree := fixture.requests(http.MethodPut, policiesPath)
	require.Len(t, tree, 1)
	require.NotNil(t, tree[0].provenance)
	assert.Equal(t, "true", *tree[0].provenance)

	written := fixture.requests(http.MethodPut, groupPath("folder-uid-1", "probe-group"))
	require.Len(t, written, 1)
	assert.Nil(t, written[0].provenance)
}

// TestPush_DryRunWritesNothing pins the promise `--dry-run` makes: the whole plan, including the
// entries that need no work, and not one request that could change anything.
func TestPush_DryRunWritesNothing(t *testing.T) {
	fixture := newPushFixture(t, pushRoutes(map[string]stubResponse{
		pushRoute(http.MethodGet, folderPath("folder-uid-1")):               okResponse(`{"uid":"folder-uid-1"}`),
		pushRoute(http.MethodGet, groupPath("folder-uid-1", "probe-group")): okResponse(pushRemoteGroup),
	}))

	result, err := alerts.Push(t.Context(), fixture.client, pushLocalBundle(), alerts.PushOptions{DryRun: true})
	require.NoError(t, err)
	assert.Empty(t, result.Failed)
	assert.Len(t, result.Applied, 3, "a dry run reports the whole plan, unchanged entries included")
	assert.Empty(t, fixture.writes(), "a dry run must not write")
}

// TestPush_DryRunReportsWhatAnApplyWouldRefuse is the other half of what a dry run is for: the
// checks that decide *whether* a write is allowed are read-only, so running them here is what keeps
// the plan from promising a `Create RuleGroup` the next apply deterministically refuses.
//
// Every refusal reaches Result.Failed, and none of them makes a write.
func TestPush_DryRunReportsWhatAnApplyWouldRefuse(t *testing.T) {
	foreign := `{"folderUid":"folder-uid-1","title":"probe-group","interval":60,
	  "rules":[{"uid":"rule-uid-1","title":"rule one","folderUID":"folder-uid-1",
	  "ruleGroup":"probe-group","orgID":1,"provenance":"file"}]}`

	tests := []struct {
		name   string
		local  alerts.Bundle
		routes map[string]stubResponse
		want   string
	}{
		{
			name:  "a missing folder",
			local: alerts.Bundle{Groups: pushLocalBundle().Groups},
			routes: map[string]stubResponse{
				pushRoute(http.MethodGet, groupPath("folder-uid-1", "probe-group")): notFoundResponse(),
				pushRoute(http.MethodGet, folderPath("folder-uid-1")):               notFoundResponse(),
			},
			want: "alerts: push rule group folder-uid-1/probe-group: " +
				"folder folder-uid-1 does not exist; push folders first (grafanapi resources push)",
		},
		{
			name:  "a rule group another provisioner owns",
			local: alerts.Bundle{Groups: pushLocalBundle().Groups},
			routes: map[string]stubResponse{
				pushRoute(http.MethodGet, groupPath("folder-uid-1", "probe-group")): okResponse(foreign),
				pushRoute(http.MethodGet, folderPath("folder-uid-1")):               okResponse(`{"uid":"folder-uid-1"}`),
			},
			want: `alerts: push rule group folder-uid-1/probe-group: provenance "file" is managed outside the API`,
		},
		{
			name: "a contact point whose secret is still redacted",
			local: alerts.Bundle{ContactPoints: []*models.EmbeddedContactPoint{
				diffPoint("cp-uid-1", "probe-slack", map[string]any{"url": "[REDACTED]"}),
			}},
			routes: map[string]stubResponse{},
			want: "alerts: push contact point cp-uid-1: secret field(s) settings.url are redacted; " +
				"set the real value(s) in the file before creating",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newPushFixture(t, pushRoutes(tc.routes))

			result, err := alerts.Push(t.Context(), fixture.client, tc.local, alerts.PushOptions{DryRun: true})
			require.NoError(t, err)

			require.Len(t, result.Failed, 1)
			assert.Equal(t, tc.want, result.Failed[0].Err.Error())
			assert.Empty(t, result.Applied, "a refused entry is not also reported as planned work")
			assert.Empty(t, fixture.writes(), "a dry run must not write, refusal or not")
		})
	}
}

// TestPush_DryRunIgnoresStopOnError pins that --on-error has no say over a dry run: stopping at the
// first refusal would hide the rest of the plan, which is the whole thing the user asked for.
func TestPush_DryRunIgnoresStopOnError(t *testing.T) {
	fixture := newPushFixture(t, pushRoutes(map[string]stubResponse{
		pushRoute(http.MethodGet, groupPath("folder-uid-1", "group-a")): notFoundResponse(),
		pushRoute(http.MethodGet, groupPath("folder-uid-1", "group-b")): notFoundResponse(),
		pushRoute(http.MethodGet, folderPath("folder-uid-1")):           notFoundResponse(),
	}))

	local := alerts.Bundle{Groups: []*models.AlertRuleGroup{
		pushGroup("group-a", diffFileRule("rule-uid-2", "rule two")),
		pushGroup("group-b", diffFileRule("rule-uid-3", "rule three")),
	}}

	result, err := alerts.Push(t.Context(), fixture.client, local,
		alerts.PushOptions{DryRun: true, StopOnError: true})
	require.NoError(t, err)
	assert.Len(t, result.Failed, 2, "both groups' refusals are reported, not just the first")
}

// TestPush_FetchesOnlyTheGroupsItPushes covers decision 6: a push reads the remote counterparts of
// the groups in the bundle, one GET each, and never lists the instance's rules - including when
// the bundle holds no rule groups at all, which is the case a "fetch what we need" rule most
// easily degrades into "fetch everything".
func TestPush_FetchesOnlyTheGroupsItPushes(t *testing.T) {
	t.Run("one group", func(t *testing.T) {
		fixture := newPushFixture(t, pushRoutes(map[string]stubResponse{
			pushRoute(http.MethodGet, folderPath("folder-uid-1")):               okResponse(`{"uid":"folder-uid-1"}`),
			pushRoute(http.MethodGet, groupPath("folder-uid-1", "probe-group")): okResponse(pushRemoteGroup),
		}))

		_, err := alerts.Push(t.Context(), fixture.client, pushLocalBundle(), alerts.PushOptions{DryRun: true})
		require.NoError(t, err)
		assert.Empty(t, fixture.requests(http.MethodGet, alertRulesPath), "a push must not list every rule")
		assert.Len(t, fixture.requests(http.MethodGet, groupPath("folder-uid-1", "probe-group")), 1)
	})

	t.Run("no groups at all", func(t *testing.T) {
		fixture := newPushFixture(t, pushRoutes(nil))

		local := alerts.Bundle{ContactPoints: pushLocalBundle().ContactPoints}

		_, err := alerts.Push(t.Context(), fixture.client, local, alerts.PushOptions{DryRun: true})
		require.NoError(t, err)
		assert.Empty(t, fixture.requests(http.MethodGet, alertRulesPath),
			"a bundle without rule groups must not fall back to discovering them")
	})
}

// TestPush_ForeignProvenanceIsARefusal is decision 3's refusal half: an object another provisioner
// owns is not written, and what that does to the rest of the push is what --on-error decides.
func TestPush_ForeignProvenanceIsARefusal(t *testing.T) {
	foreign := `{"folderUid":"folder-uid-1","title":"group-a","interval":60,
	  "rules":[{"uid":"rule-uid-1","title":"rule one","folderUID":"folder-uid-1","ruleGroup":"group-a","provenance":"file"}]}`

	local := alerts.Bundle{Groups: []*models.AlertRuleGroup{
		pushGroup("group-a", diffFileRule("rule-uid-2", "rule two")),
		pushGroup("group-b", diffFileRule("rule-uid-3", "rule three")),
	}}

	routes := pushRoutes(map[string]stubResponse{
		pushRoute(http.MethodGet, folderPath("folder-uid-1")):           okResponse(`{"uid":"folder-uid-1"}`),
		pushRoute(http.MethodGet, groupPath("folder-uid-1", "group-a")): okResponse(foreign),
		pushRoute(http.MethodGet, groupPath("folder-uid-1", "group-b")): notFoundResponse(),
		pushRoute(http.MethodPut, groupPath("folder-uid-1", "group-b")): okResponse(pushedGroupBody),
	})

	t.Run("the other groups are still pushed", func(t *testing.T) {
		fixture := newPushFixture(t, routes)

		result, err := alerts.Push(t.Context(), fixture.client, local, alerts.PushOptions{})
		require.NoError(t, err)

		require.Len(t, result.Failed, 1)
		assert.Equal(t, "folder-uid-1/group-a", result.Failed[0].Change.ID)
		assert.Equal(t,
			`alerts: push rule group folder-uid-1/group-a: provenance "file" is managed outside the API`,
			result.Failed[0].Err.Error(),
		)

		require.Len(t, result.Applied, 1)
		assert.Equal(t, "folder-uid-1/group-b", result.Applied[0].ID)
		assert.Empty(t, fixture.requests(http.MethodPut, groupPath("folder-uid-1", "group-a")),
			"a refused group must not be written at all")
	})

	t.Run("StopOnError stops at it", func(t *testing.T) {
		fixture := newPushFixture(t, routes)

		result, err := alerts.Push(t.Context(), fixture.client, local, alerts.PushOptions{StopOnError: true})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "alerts: push rule group folder-uid-1/group-a")

		require.Len(t, result.Failed, 1)
		assert.Empty(t, result.Applied)
		assert.Empty(t, fixture.writes(), "nothing after the first failure may be written")
	})
}

// TestPush_Forbidden pins where a 403 lands: always against the one object - or the one folder -
// it was answered for, never against the run. Grafana grants folders:read per folder, so a bundle
// spanning ten folders routinely gets a 403 for the eight this session does not own, and refusing
// the other two over it would make a partially-permitted bundle unpushable and unplannable.
//
// Every one of those failures must keep the *runtime.APIError reachable, which is what Task 6's
// permission-denied rendering matches on.
func TestPush_Forbidden(t *testing.T) {
	forbidden := stubResponse{status: http.StatusForbidden, body: `{"message":"You'll need additional permissions"}`}

	t.Run("on a write it is a per-object failure", func(t *testing.T) {
		fixture := newPushFixture(t, pushRoutes(map[string]stubResponse{
			pushRoute(http.MethodGet, groupPath("folder-uid-1", "probe-group")): notFoundResponse(),
			pushRoute(http.MethodGet, folderPath("folder-uid-1")):               okResponse(`{"uid":"folder-uid-1"}`),
			pushRoute(http.MethodPost, contactPointsPath):                       acceptedResponse(),
			pushRoute(http.MethodPut, policiesPath):                             acceptedResponse(),
			pushRoute(http.MethodPut, groupPath("folder-uid-1", "probe-group")): forbidden,
		}))

		result, err := alerts.Push(t.Context(), fixture.client, pushLocalBundle(), alerts.PushOptions{})
		require.NoError(t, err)
		require.Len(t, result.Failed, 1)

		var apiErr *runtime.APIError
		require.ErrorAs(t, result.Failed[0].Err, &apiErr)
		assert.Equal(t, http.StatusForbidden, apiErr.Code)
	})

	t.Run("on the folder pre-check it fails only that folder's groups", func(t *testing.T) {
		fixture := newPushFixture(t, twoFolderRoutes(map[string]stubResponse{
			pushRoute(http.MethodGet, folderPath("folder-uid-1")): forbidden,
		}))

		result, err := alerts.Push(t.Context(), fixture.client, twoFolderBundle(), alerts.PushOptions{})
		require.NoError(t, err, "one unreadable folder is not the end of the push")

		require.Len(t, result.Failed, 1)
		assert.Equal(t, "folder-uid-1/group-a", result.Failed[0].Change.ID)
		assert.Equal(t,
			"alerts: push rule group folder-uid-1/group-a: folder folder-uid-1: permission denied",
			result.Failed[0].Err.Error(),
		)

		var apiErr *runtime.APIError
		require.ErrorAs(t, result.Failed[0].Err, &apiErr,
			"the 403 must stay reachable, or the failure renders as something other than permission denied")
		assert.Equal(t, http.StatusForbidden, apiErr.Code)

		require.Len(t, result.Applied, 1)
		assert.Equal(t, "folder-uid-2/group-b", result.Applied[0].ID)
		assert.Empty(t, fixture.requests(http.MethodPut, groupPath("folder-uid-1", "group-a")),
			"a group in a folder this session may not read must not be written")
	})

	t.Run("on the folder pre-check it honours StopOnError", func(t *testing.T) {
		fixture := newPushFixture(t, twoFolderRoutes(map[string]stubResponse{
			pushRoute(http.MethodGet, folderPath("folder-uid-1")): forbidden,
		}))

		result, err := alerts.Push(t.Context(), fixture.client, twoFolderBundle(),
			alerts.PushOptions{StopOnError: true})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "folder folder-uid-1: permission denied")

		assert.Len(t, result.Failed, 1)
		assert.Empty(t, result.Applied)
		assert.Empty(t, fixture.writes(), "nothing after the first failure may be written")
	})

	t.Run("a dry run shows the refusal and the rest of the plan", func(t *testing.T) {
		fixture := newPushFixture(t, twoFolderRoutes(map[string]stubResponse{
			pushRoute(http.MethodGet, folderPath("folder-uid-1")): forbidden,
		}))

		result, err := alerts.Push(t.Context(), fixture.client, twoFolderBundle(),
			alerts.PushOptions{DryRun: true})
		require.NoError(t, err)

		require.Len(t, result.Failed, 1)
		assert.Equal(t, "folder-uid-1/group-a", result.Failed[0].Change.ID)
		require.Len(t, result.Applied, 1, "the folders this session can read are still planned")
		assert.Equal(t, "folder-uid-2/group-b", result.Applied[0].ID)
		assert.Empty(t, fixture.writes(), "a dry run must not write, refusal or not")
	})
}

// TestPush_FolderPreCheckUnauthorizedIsFatal is the other half of TestPush_Forbidden's folder
// arm: a 401 says the session is over, so unlike a 403 it stops the push and reaches the caller as
// an unwrapped *runtime.APIError, which is what the exit-2 stale-session rendering matches on.
func TestPush_FolderPreCheckUnauthorizedIsFatal(t *testing.T) {
	fixture := newPushFixture(t, twoFolderRoutes(map[string]stubResponse{
		pushRoute(http.MethodGet, folderPath("folder-uid-1")): {
			status: http.StatusUnauthorized,
			body:   `{"message":"Unauthorized"}`,
		},
	}))

	_, err := alerts.Push(t.Context(), fixture.client, twoFolderBundle(), alerts.PushOptions{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "alerts: push folder folder-uid-1")

	var apiErr *runtime.APIError
	require.ErrorAs(t, err, &apiErr)
	assert.Equal(t, http.StatusUnauthorized, apiErr.Code)

	assert.Empty(t, fixture.writes(), "no group may be written once the session is gone")
}

// TestPush_GroupBodyReplacesTheWholeGroup is the other half of decision 2: Grafana replaces a rule
// group on PUT, so the body must carry every rule the group should end up with, each pinned to the
// group being written and stripped of the fields Grafana owns.
func TestPush_GroupBodyReplacesTheWholeGroup(t *testing.T) {
	fixture := newPushFixture(t, pushRoutes(map[string]stubResponse{
		pushRoute(http.MethodGet, folderPath("folder-uid-1")):               okResponse(`{"uid":"folder-uid-1"}`),
		pushRoute(http.MethodGet, groupPath("folder-uid-1", "probe-group")): okResponse(pushRemoteGroup),
		pushRoute(http.MethodPut, groupPath("folder-uid-1", "probe-group")): okResponse(pushedGroupBody),
	}))

	// The second rule claims another folder and group, as a hand-edited file may; the write pins
	// both back to the group the file addresses. It also carries the three read-only fields.
	local := alerts.Bundle{Groups: []*models.AlertRuleGroup{pushGroup(
		"probe-group",
		diffFileRule("rule-uid-1", "rule one"),
		testRule("folder-uid-2", "other-group", "rule-uid-9", "rule nine"),
	)}}

	_, err := alerts.Push(t.Context(), fixture.client, local, alerts.PushOptions{})
	require.NoError(t, err)

	written := fixture.requests(http.MethodPut, groupPath("folder-uid-1", "probe-group"))
	require.Len(t, written, 1)

	body := decodeGroupBody(t, written[0].body)
	require.Len(t, body.Rules, 2, "a group PUT replaces the group, so every local rule must be in the body")
	assert.Equal(t, "rule-uid-1", body.Rules[0].UID)
	assert.Equal(t, "rule-uid-9", body.Rules[1].UID)

	for _, rule := range body.Rules {
		assert.Equal(t, "folder-uid-1", *rule.FolderUID)
		assert.Equal(t, "probe-group", *rule.RuleGroup)
	}

	assert.NotContains(t, written[0].body, `"provenance"`, "provenance travels in the header, never in the body")
	assert.NotContains(t, written[0].body, `"id":7`)
}

// TestPush_MissingFolderFailsItsGroup pins the message decision 2 promises. Grafana's own answer
// is a 400 for an Editor and a 403 for anyone else, neither of which mentions the folder or says
// that grafanapi will not create one.
func TestPush_MissingFolderFailsItsGroup(t *testing.T) {
	fixture := newPushFixture(t, pushRoutes(map[string]stubResponse{
		pushRoute(http.MethodGet, groupPath("folder-uid-1", "probe-group")): notFoundResponse(),
		pushRoute(http.MethodGet, folderPath("folder-uid-1")):               notFoundResponse(),
		pushRoute(http.MethodPost, contactPointsPath):                       acceptedResponse(),
		pushRoute(http.MethodPut, policiesPath):                             acceptedResponse(),
	}))

	result, err := alerts.Push(t.Context(), fixture.client, pushLocalBundle(), alerts.PushOptions{})
	require.NoError(t, err)

	require.Len(t, result.Failed, 1)
	assert.Equal(t,
		"alerts: push rule group folder-uid-1/probe-group: "+
			"folder folder-uid-1 does not exist; push folders first (grafanapi resources push)",
		result.Failed[0].Err.Error(),
	)
	assert.Empty(t, fixture.requests(http.MethodPut, groupPath("folder-uid-1", "probe-group")))
	assert.Len(t, result.Applied, 2, "the kinds that do not need a folder are still pushed")
}

// TestPush_NilClientIsAnError guards the caller contract the way the read path does.
func TestPush_NilClientIsAnError(t *testing.T) {
	_, err := alerts.Push(t.Context(), nil, alerts.Bundle{}, alerts.PushOptions{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no grafana client configured")
}

// TestPush_OrgIDFallback walks decision 1's three sources in order. The org a rule claims is not
// something a user should have to maintain by hand, and a rule written into the wrong one is
// invisible rather than rejected.
func TestPush_OrgIDFallback(t *testing.T) {
	remoteInOrgSeven := `{"folderUid":"folder-uid-1","title":"probe-group","interval":60,
	  "rules":[{"uid":"rule-uid-1","title":"rule one","folderUID":"folder-uid-1","ruleGroup":"probe-group","orgID":7}]}`

	tests := []struct {
		name   string
		remote stubResponse
		opts   alerts.PushOptions
		want   int64
	}{
		{
			name:   "the remote group wins",
			remote: okResponse(remoteInOrgSeven),
			opts:   alerts.PushOptions{OrgID: 5},
			want:   7,
		},
		{
			name:   "a new group takes the context's org",
			remote: notFoundResponse(),
			opts:   alerts.PushOptions{OrgID: 5},
			want:   5,
		},
		{
			name:   "a cloud context, whose org-id is zero, keeps what the file says",
			remote: notFoundResponse(),
			opts:   alerts.PushOptions{},
			want:   1,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newPushFixture(t, pushRoutes(map[string]stubResponse{
				pushRoute(http.MethodGet, folderPath("folder-uid-1")):               okResponse(`{"uid":"folder-uid-1"}`),
				pushRoute(http.MethodGet, groupPath("folder-uid-1", "probe-group")): tc.remote,
				pushRoute(http.MethodPut, groupPath("folder-uid-1", "probe-group")): okResponse(pushedGroupBody),
			}))

			local := alerts.Bundle{Groups: []*models.AlertRuleGroup{
				pushGroup("probe-group", diffFileRule("rule-uid-2", "rule two")),
			}}

			_, err := alerts.Push(t.Context(), fixture.client, local, tc.opts)
			require.NoError(t, err)

			written := fixture.requests(http.MethodPut, groupPath("folder-uid-1", "probe-group"))
			require.Len(t, written, 1)

			body := decodeGroupBody(t, written[0].body)
			require.Len(t, body.Rules, 1)
			require.NotNil(t, body.Rules[0].OrgID)
			assert.Equal(t, tc.want, *body.Rules[0].OrgID)
		})
	}
}

// TestPush_ProvenanceHeaderMirrorsTheStoredValue is decision 3's update half, on the kind where
// getting it wrong is worst: a group PUT without the header re-stamps every rule in the group.
func TestPush_ProvenanceHeaderMirrorsTheStoredValue(t *testing.T) {
	tests := []struct {
		name   string
		stored string
		want   *string
	}{
		{name: "an unclaimed group keeps the UI's hands on it", stored: "", want: new("true")},
		{name: "a group the API owns is written without the header", stored: "api"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			remote := `{"folderUid":"folder-uid-1","title":"probe-group","interval":60,
			  "rules":[{"uid":"rule-uid-1","title":"rule one","folderUID":"folder-uid-1",
			  "ruleGroup":"probe-group","orgID":1,"provenance":"` + tc.stored + `"}]}`

			fixture := newPushFixture(t, pushRoutes(map[string]stubResponse{
				pushRoute(http.MethodGet, folderPath("folder-uid-1")):               okResponse(`{"uid":"folder-uid-1"}`),
				pushRoute(http.MethodGet, groupPath("folder-uid-1", "probe-group")): okResponse(remote),
				pushRoute(http.MethodPut, groupPath("folder-uid-1", "probe-group")): okResponse(pushedGroupBody),
			}))

			local := alerts.Bundle{Groups: []*models.AlertRuleGroup{
				pushGroup("probe-group", diffFileRule("rule-uid-2", "rule two")),
			}}

			_, err := alerts.Push(t.Context(), fixture.client, local, alerts.PushOptions{})
			require.NoError(t, err)

			written := fixture.requests(http.MethodPut, groupPath("folder-uid-1", "probe-group"))
			require.Len(t, written, 1)
			assert.Equal(t, tc.want, written[0].provenance)
		})
	}
}

// TestPush_RedactedSettingsAreSentVerbatim pins decision 9's write half: Grafana reads the marker
// it returned as "keep the stored secret", so pushing it back is what makes pull -> edit -> push
// safe for a contact point whose token nobody ever sees.
func TestPush_RedactedSettingsAreSentVerbatim(t *testing.T) {
	fixture := newPushFixture(t, pushRoutes(map[string]stubResponse{
		pushRoute(http.MethodGet, contactPointsPath): okResponse(
			`[{"uid":"cp-uid-1","name":"probe-slack","type":"slack","settings":{"recipient":"#probe","url":"[REDACTED]"},"provenance":"api"}]`,
		),
		pushRoute(http.MethodPut, contactPointsPath+"/cp-uid-1"): acceptedResponse(),
	}))

	local := alerts.Bundle{ContactPoints: []*models.EmbeddedContactPoint{
		diffPoint("cp-uid-1", "probe-slack", map[string]any{"recipient": "#other", "url": "[REDACTED]"}),
	}}

	result, err := alerts.Push(t.Context(), fixture.client, local, alerts.PushOptions{})
	require.NoError(t, err)
	assert.Empty(t, result.Failed)

	written := fixture.requests(http.MethodPut, contactPointsPath+"/cp-uid-1")
	require.Len(t, written, 1)
	assert.Contains(t, written[0].body, `"url":"[REDACTED]"`)
	assert.Contains(t, written[0].body, `"recipient":"#other"`)
	assert.Nil(t, written[0].provenance, "an update mirrors the stored provenance, which is api here")
}

// TestPush_Unauthorized is the exit-2 regression test: whatever a push was in the middle of, a
// dead session stops it and reaches the caller as a *runtime.APIError, from all three transports a
// push uses - the provisioning reads, the typed folder check and the writes themselves.
func TestPush_Unauthorized(t *testing.T) {
	unauthorized := stubResponse{status: http.StatusUnauthorized, body: `{"message":"Unauthorized"}`}

	tests := []struct {
		name   string
		routes map[string]stubResponse
	}{
		{
			name: "from the pre-fetch",
			routes: map[string]stubResponse{
				pushRoute(http.MethodGet, groupPath("folder-uid-1", "probe-group")): unauthorized,
			},
		},
		{
			name: "from the typed folder check",
			routes: map[string]stubResponse{
				pushRoute(http.MethodGet, groupPath("folder-uid-1", "probe-group")): notFoundResponse(),
				pushRoute(http.MethodGet, folderPath("folder-uid-1")):               unauthorized,
				pushRoute(http.MethodPost, contactPointsPath):                       acceptedResponse(),
				pushRoute(http.MethodPut, policiesPath):                             acceptedResponse(),
			},
		},
		{
			name: "from a write",
			routes: map[string]stubResponse{
				pushRoute(http.MethodGet, groupPath("folder-uid-1", "probe-group")): notFoundResponse(),
				pushRoute(http.MethodGet, folderPath("folder-uid-1")):               okResponse(`{"uid":"folder-uid-1"}`),
				pushRoute(http.MethodPost, contactPointsPath):                       acceptedResponse(),
				pushRoute(http.MethodPut, policiesPath):                             acceptedResponse(),
				pushRoute(http.MethodPut, groupPath("folder-uid-1", "probe-group")): unauthorized,
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newPushFixture(t, pushRoutes(tc.routes))

			// StopOnError is deliberately unset: a dead session stops the push whatever
			// --on-error says, since nothing after it could succeed.
			_, err := alerts.Push(t.Context(), fixture.client, pushLocalBundle(), alerts.PushOptions{})
			require.Error(t, err)

			var apiErr *runtime.APIError
			require.ErrorAs(t, err, &apiErr)
			assert.Equal(t, http.StatusUnauthorized, apiErr.Code)
		})
	}
}

// TestPush_UIEditableAppliesToCreatesOnly pins what the flag's help text promises: it decides the
// provenance of an object being created and has no say over one that already exists, whose stored
// value is the only header Grafana will accept.
func TestPush_UIEditableAppliesToCreatesOnly(t *testing.T) {
	fixture := newPushFixture(t, pushRoutes(map[string]stubResponse{
		pushRoute(http.MethodGet, contactPointsPath): okResponse(
			`[{"uid":"cp-uid-2","name":"probe-existing","type":"slack","settings":{},"provenance":"api"}]`,
		),
		pushRoute(http.MethodPost, contactPointsPath):            acceptedResponse(),
		pushRoute(http.MethodPut, contactPointsPath+"/cp-uid-2"): acceptedResponse(),
	}))

	local := alerts.Bundle{ContactPoints: []*models.EmbeddedContactPoint{
		diffPoint("cp-uid-1", "probe-slack", map[string]any{"recipient": "#probe"}),
		diffPoint("cp-uid-2", "probe-existing", map[string]any{"recipient": "#existing"}),
	}}

	result, err := alerts.Push(t.Context(), fixture.client, local, alerts.PushOptions{UIEditable: true})
	require.NoError(t, err)
	assert.Empty(t, result.Failed)

	created := fixture.requests(http.MethodPost, contactPointsPath)
	require.Len(t, created, 1)
	require.NotNil(t, created[0].provenance)
	assert.Equal(t, "true", *created[0].provenance)

	updated := fixture.requests(http.MethodPut, contactPointsPath+"/cp-uid-2")
	require.Len(t, updated, 1)
	assert.Nil(t, updated[0].provenance, "--ui-editable must not try to release an object Grafana owns")
}

// TestPush_UpdatesEveryKind is the happy update path, and the one that pins the header for an
// object Grafana has claimed: sending X-Disable-Provenance on such a write is a 500 for a rule
// group and a 403 for the policy tree.
func TestPush_UpdatesEveryKind(t *testing.T) {
	fixture := newPushFixture(t, pushRoutes(map[string]stubResponse{
		pushRoute(http.MethodGet, contactPointsPath): okResponse(
			`[{"uid":"cp-uid-1","name":"probe-slack","type":"slack","settings":{"recipient":"#old"},"provenance":"api"}]`,
		),
		pushRoute(http.MethodGet, policiesPath):                             okResponse(`{"receiver":"grafana-default-email","provenance":"api"}`),
		pushRoute(http.MethodGet, folderPath("folder-uid-1")):               okResponse(`{"uid":"folder-uid-1"}`),
		pushRoute(http.MethodGet, groupPath("folder-uid-1", "probe-group")): okResponse(pushRemoteGroup),
		pushRoute(http.MethodPut, contactPointsPath+"/cp-uid-1"):            acceptedResponse(),
		pushRoute(http.MethodPut, policiesPath):                             acceptedResponse(),
		pushRoute(http.MethodPut, groupPath("folder-uid-1", "probe-group")): okResponse(pushedGroupBody),
	}))

	local := pushLocalBundle()
	local.Groups[0].Interval = 120

	result, err := alerts.Push(t.Context(), fixture.client, local, alerts.PushOptions{})
	require.NoError(t, err)
	assert.Empty(t, result.Failed)
	require.Len(t, result.Applied, 3)

	for _, change := range result.Applied {
		assert.Equal(t, alerts.ActionUpdate, change.Action, change.ID)
	}

	for _, written := range fixture.writes() {
		assert.Nil(t, written.provenance, "an object the API owns is updated without the header: "+written.path)
	}

	assert.Len(t, fixture.requests(http.MethodPut, contactPointsPath+"/cp-uid-1"), 1)
	assert.Len(t, fixture.requests(http.MethodPut, policiesPath), 1)
	assert.Len(t, fixture.requests(http.MethodPut, groupPath("folder-uid-1", "probe-group")), 1)
}

// TestPush_ValidationErrorIsReadable pins the 400 mapping: the generated client declares a
// per-operation BadRequest struct for every write, and without the shared accessor the user would
// see its opaque rendering instead of the sentence Grafana wrote.
func TestPush_ValidationErrorIsReadable(t *testing.T) {
	fixture := newPushFixture(t, pushRoutes(map[string]stubResponse{
		pushRoute(http.MethodGet, folderPath("folder-uid-1")):               okResponse(`{"uid":"folder-uid-1"}`),
		pushRoute(http.MethodGet, groupPath("folder-uid-1", "probe-group")): okResponse(pushRemoteGroup),
		pushRoute(http.MethodPut, groupPath("folder-uid-1", "probe-group")): {
			status: http.StatusBadRequest,
			body:   `{"message":"invalid alert rule: data is required"}`,
		},
	}))

	local := alerts.Bundle{Groups: []*models.AlertRuleGroup{
		pushGroup("probe-group", diffFileRule("rule-uid-2", "rule two")),
	}}

	result, err := alerts.Push(t.Context(), fixture.client, local, alerts.PushOptions{})
	require.NoError(t, err)

	require.Len(t, result.Failed, 1)
	assert.Equal(t,
		"alerts: push rule group folder-uid-1/probe-group: invalid alert rule: data is required",
		result.Failed[0].Err.Error(),
	)

	// Substituting Grafana's message must not cost the chain: cmd/grafanapi/fail picks between the
	// 401 and 403 renderings by matching on the response the generated client returned, and record
	// recognises a dead session the same way.
	var response interface {
		GetPayload() *models.ValidationError
	}

	require.ErrorAs(t, result.Failed[0].Err, &response, "the generated response stays reachable through errors.As")
}

// TestPush_BodyReplayOnRotate is the rotation half of the write path: when a 401 is answered by a
// *successful* rotation rather than a dead session, the rotating transport replays the request -
// which only works if the body is rewindable (req.GetBody != nil). The generated client builds its
// request bodies itself, so nothing in internal/alerts can prove this except an end-to-end replay.
//
// Both a PUT (rule group) and a POST (contact point) are covered: they are produced by different
// generated operations.
func TestPush_BodyReplayOnRotate(t *testing.T) {
	const newCookie = "new-cookie"

	rotateServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/api/user/auth-tokens/rotate", r.URL.Path)
		http.SetCookie(w, &http.Cookie{
			Name: config.SessionCookieName, Value: newCookie,
			Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode,
		})
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(rotateServer.Close)

	tests := []struct {
		name   string
		local  alerts.Bundle
		method string
		path   string
		routes map[string]stubResponse
	}{
		{
			name:   "a rule-group PUT",
			local:  alerts.Bundle{Groups: pushLocalBundle().Groups},
			method: http.MethodPut,
			path:   groupPath("folder-uid-1", "probe-group"),
			routes: map[string]stubResponse{
				pushRoute(http.MethodGet, folderPath("folder-uid-1")):               okResponse(`{"uid":"folder-uid-1"}`),
				pushRoute(http.MethodGet, groupPath("folder-uid-1", "probe-group")): okResponse(pushRemoteGroup),
				pushRoute(http.MethodPut, groupPath("folder-uid-1", "probe-group")): okResponse(pushedGroupBody),
			},
		},
		{
			name:   "a contact-point POST",
			local:  alerts.Bundle{ContactPoints: pushLocalBundle().ContactPoints},
			method: http.MethodPost,
			path:   contactPointsPath,
			routes: map[string]stubResponse{
				pushRoute(http.MethodPost, contactPointsPath): acceptedResponse(),
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var rejected atomic.Bool

			fixture := newRotatingPushFixture(t, pushRoutes(tc.routes), rotateServer.URL,
				func(r *http.Request) (int, bool) {
					if r.Method == tc.method && r.URL.Path == tc.path && rejected.CompareAndSwap(false, true) {
						return http.StatusUnauthorized, true
					}

					return 0, false
				})

			result, err := alerts.Push(t.Context(), fixture.client, tc.local, alerts.PushOptions{})
			require.NoError(t, err)
			assert.Empty(t, result.Failed)

			written := fixture.requests(tc.method, tc.path)
			require.Len(t, written, 2, "the rejected write must be retried, not abandoned")
			assert.Equal(t, written[0].body, written[1].body, "the retry must replay the identical body")
			assert.NotEmpty(t, written[1].body)
			assert.Contains(t, written[1].cookie, newCookie, "the retry must carry the rotated cookie")
		})
	}
}

// TestPush_ForeignProvenanceOnContactPointsAndPolicyTree is decision 3's refusal half on the two
// kinds the rule-group tests do not cover. Both are single-object writes, so the refusal is the
// only thing standing between a push and overwriting an object its own provisioner will re-create.
func TestPush_ForeignProvenanceOnContactPointsAndPolicyTree(t *testing.T) {
	t.Run("a contact point", func(t *testing.T) {
		fixture := newPushFixture(t, pushRoutes(map[string]stubResponse{
			pushRoute(http.MethodGet, contactPointsPath): okResponse(
				`[{"uid":"cp-uid-1","name":"probe-slack","type":"slack","settings":{"recipient":"#old"},"provenance":"file"}]`,
			),
		}))

		local := alerts.Bundle{ContactPoints: []*models.EmbeddedContactPoint{
			diffPoint("cp-uid-1", "probe-slack", map[string]any{"recipient": "#probe"}),
		}}

		result, err := alerts.Push(t.Context(), fixture.client, local, alerts.PushOptions{})
		require.NoError(t, err)

		require.Len(t, result.Failed, 1)
		assert.Equal(t,
			`alerts: push contact point cp-uid-1: provenance "file" is managed outside the API`,
			result.Failed[0].Err.Error(),
		)
		assert.Empty(t, fixture.writes(), "a refused contact point must not be written at all")
	})

	t.Run("the policy tree", func(t *testing.T) {
		fixture := newPushFixture(t, pushRoutes(map[string]stubResponse{
			pushRoute(http.MethodGet, policiesPath): okResponse(
				`{"receiver":"grafana-default-email","provenance":"converted_prometheus"}`,
			),
		}))

		local := alerts.Bundle{Policies: &models.Route{Receiver: "probe-slack"}}

		result, err := alerts.Push(t.Context(), fixture.client, local, alerts.PushOptions{})
		require.NoError(t, err)

		require.Len(t, result.Failed, 1)
		assert.Equal(t,
			`alerts: push policy tree policies: provenance "converted_prometheus" is managed outside the API`,
			result.Failed[0].Err.Error(),
		)
		assert.Empty(t, fixture.writes(), "a refused policy tree must not be written at all")
	})
}

// TestPush_HandWrittenRuleGetsTheGroupsIdentity is the push half of the hand-typed rule: a bundle
// read from disk whose rule declares neither folderUID, ruleGroup nor orgID still produces a
// complete body, because the group file is what addresses the group and the context supplies the
// organization. Reading through ReadBundle rather than building the model keeps the two halves
// honest about each other.
func TestPush_HandWrittenRuleGetsTheGroupsIdentity(t *testing.T) {
	dir := t.TempDir()
	folder := filepath.Join(dir, "rules", "folder-uid-1")
	require.NoError(t, os.MkdirAll(folder, bundleDirPerm))
	require.NoError(t, os.WriteFile(
		filepath.Join(folder, "probe-group.yaml"),
		[]byte(testGroupFile("probe-group", testRuleEntry(""))),
		bundleFilePerm,
	))

	local, err := alerts.ReadBundle(dir)
	require.NoError(t, err)

	fixture := newPushFixture(t, pushRoutes(map[string]stubResponse{
		pushRoute(http.MethodGet, folderPath("folder-uid-1")):               okResponse(`{"uid":"folder-uid-1"}`),
		pushRoute(http.MethodGet, groupPath("folder-uid-1", "probe-group")): notFoundResponse(),
		pushRoute(http.MethodPut, groupPath("folder-uid-1", "probe-group")): okResponse(pushedGroupBody),
	}))

	result, err := alerts.Push(t.Context(), fixture.client, local, alerts.PushOptions{OrgID: 5})
	require.NoError(t, err)
	require.Empty(t, result.Failed)

	written := fixture.requests(http.MethodPut, groupPath("folder-uid-1", "probe-group"))
	require.Len(t, written, 1)

	body := decodeGroupBody(t, written[0].body)
	require.Len(t, body.Rules, 1)

	rule := body.Rules[0]
	require.NotNil(t, rule.FolderUID)
	assert.Equal(t, "folder-uid-1", *rule.FolderUID, "the group file's folder")
	require.NotNil(t, rule.RuleGroup)
	assert.Equal(t, "probe-group", *rule.RuleGroup, "the group file's title")
	require.NotNil(t, rule.OrgID)
	assert.Equal(t, int64(5), *rule.OrgID, "the context's org-id")
}

// TestPush_PolicyTreeBodyStripsProvenanceFromEveryNode pins the recursion in routeBody. Grafana
// stamps provenance all the way down a policy tree and rejects a write that claims to set it, so a
// child route carrying the value it was pulled with would fail the whole write.
func TestPush_PolicyTreeBodyStripsProvenanceFromEveryNode(t *testing.T) {
	fixture := newPushFixture(t, pushRoutes(map[string]stubResponse{
		pushRoute(http.MethodGet, policiesPath): okResponse(`{"receiver":"grafana-default-email"}`),
		pushRoute(http.MethodPut, policiesPath): acceptedResponse(),
	}))

	local := alerts.Bundle{Policies: &models.Route{
		Receiver:   "probe-slack",
		Provenance: "api",
		Routes: []*models.Route{
			{
				Receiver:   "probe-child",
				Provenance: "api",
				Routes:     []*models.Route{{Receiver: "probe-grandchild", Provenance: "api"}},
			},
		},
	}}

	_, err := alerts.Push(t.Context(), fixture.client, local, alerts.PushOptions{})
	require.NoError(t, err)

	written := fixture.requests(http.MethodPut, policiesPath)
	require.Len(t, written, 1)

	assert.NotContains(t, written[0].body, `"provenance"`, "provenance travels in the header at every depth")
	assert.Contains(t, written[0].body, `"probe-child"`)
	assert.Contains(t, written[0].body, `"probe-grandchild"`, "the whole tree is sent, not just the root")
}

// TestPush_RedactedSecretOnCreateIsARefusal is the create half of decision 9, and the one case the
// "push the marker back and the secret is kept" rule does not cover: there is no stored secret to
// keep for an object that does not exist yet, so a POST carrying "[REDACTED]" would store the
// literal string. This is exactly what pulling from one instance and pushing into another does.
func TestPush_RedactedSecretOnCreateIsARefusal(t *testing.T) {
	tests := []struct {
		name     string
		settings map[string]any
		want     string
	}{
		{
			name:     "a top-level secure field",
			settings: map[string]any{"recipient": "#probe", "url": "[REDACTED]"},
			want:     "settings.url",
		},
		{
			name:     "a nested one",
			settings: map[string]any{"http_config": map[string]any{"password": "[REDACTED]"}},
			want:     "settings.http_config.password",
		},
		{
			name:     "one inside an array",
			settings: map[string]any{"headers": []any{map[string]any{"value": "[REDACTED]"}}},
			want:     "settings.headers[0].value",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newPushFixture(t, pushRoutes(nil))

			local := alerts.Bundle{ContactPoints: []*models.EmbeddedContactPoint{
				diffPoint("cp-uid-1", "probe-slack", tc.settings),
			}}

			result, err := alerts.Push(t.Context(), fixture.client, local, alerts.PushOptions{})
			require.NoError(t, err)

			require.Len(t, result.Failed, 1)
			assert.Equal(t,
				"alerts: push contact point cp-uid-1: secret field(s) "+tc.want+
					" are redacted; set the real value(s) in the file before creating",
				result.Failed[0].Err.Error(),
			)
			assert.Empty(t, fixture.writes(), "a contact point that cannot carry its secret is not created")
		})
	}

	t.Run("an update is still allowed to keep the stored secret", func(t *testing.T) {
		fixture := newPushFixture(t, pushRoutes(map[string]stubResponse{
			pushRoute(http.MethodGet, contactPointsPath): okResponse(
				`[{"uid":"cp-uid-1","name":"probe-slack","type":"slack","settings":{"recipient":"#old","url":"[REDACTED]"},"provenance":"api"}]`,
			),
			pushRoute(http.MethodPut, contactPointsPath+"/cp-uid-1"): acceptedResponse(),
		}))

		local := alerts.Bundle{ContactPoints: []*models.EmbeddedContactPoint{
			diffPoint("cp-uid-1", "probe-slack", map[string]any{"recipient": "#probe", "url": "[REDACTED]"}),
		}}

		result, err := alerts.Push(t.Context(), fixture.client, local, alerts.PushOptions{})
		require.NoError(t, err)
		assert.Empty(t, result.Failed)
		assert.Len(t, fixture.requests(http.MethodPut, contactPointsPath+"/cp-uid-1"), 1)
	})
}

// TestPush_SortsFailuresInPlanOrder pins that both halves of a result come back in plan order, so
// what a push prints does not depend on which of its concurrent writes failed first.
func TestPush_SortsFailuresInPlanOrder(t *testing.T) {
	forbidden := stubResponse{status: http.StatusForbidden, body: `{"message":"denied"}`}

	fixture := newPushFixture(t, pushRoutes(map[string]stubResponse{
		pushRoute(http.MethodGet, folderPath("folder-uid-1")):           okResponse(`{"uid":"folder-uid-1"}`),
		pushRoute(http.MethodGet, groupPath("folder-uid-1", "group-a")): notFoundResponse(),
		pushRoute(http.MethodGet, groupPath("folder-uid-1", "group-b")): notFoundResponse(),
		pushRoute(http.MethodGet, groupPath("folder-uid-1", "group-c")): notFoundResponse(),
		pushRoute(http.MethodPut, groupPath("folder-uid-1", "group-a")): forbidden,
		pushRoute(http.MethodPut, groupPath("folder-uid-1", "group-b")): forbidden,
		pushRoute(http.MethodPut, groupPath("folder-uid-1", "group-c")): forbidden,
	}))

	// Listed out of order on purpose: the plan sorts them, and so must the failures.
	local := alerts.Bundle{Groups: []*models.AlertRuleGroup{
		pushGroup("group-c", diffFileRule("rule-uid-3", "rule three")),
		pushGroup("group-a", diffFileRule("rule-uid-1", "rule one")),
		pushGroup("group-b", diffFileRule("rule-uid-2", "rule two")),
	}}

	result, err := alerts.Push(t.Context(), fixture.client, local, alerts.PushOptions{})
	require.NoError(t, err)

	require.Len(t, result.Failed, 3)
	assert.Equal(t, []string{"folder-uid-1/group-a", "folder-uid-1/group-b", "folder-uid-1/group-c"},
		[]string{result.Failed[0].Change.ID, result.Failed[1].Change.ID, result.Failed[2].Change.ID})
}

// TestPush_UnchangedEntriesNeedNoRequest pins what an Unchanged entry means in a real apply, not
// only under a dry run: it is reported as applied and no write is made for it. This is also the
// shape a Viewer's push takes when the bundle already matches the instance - nothing is written, so
// nothing is refused, and the command exits 0.
func TestPush_UnchangedEntriesNeedNoRequest(t *testing.T) {
	// The remote group is the local one, marshalled: hand-writing "identical" JSON would pin the
	// encoding rather than the comparison.
	unchanged, err := json.Marshal(pushLocalBundle().Groups[0])
	require.NoError(t, err)

	fixture := newPushFixture(t, pushRoutes(map[string]stubResponse{
		pushRoute(http.MethodGet, contactPointsPath): okResponse(
			`[{"uid":"cp-uid-1","name":"probe-slack","type":"slack","settings":{"recipient":"#probe"},"provenance":"api"}]`,
		),
		pushRoute(http.MethodGet, policiesPath):                             okResponse(`{"receiver":"probe-slack"}`),
		pushRoute(http.MethodGet, groupPath("folder-uid-1", "probe-group")): okResponse(string(unchanged)),
	}))

	result, pushErr := alerts.Push(t.Context(), fixture.client, pushLocalBundle(), alerts.PushOptions{})
	require.NoError(t, pushErr)
	assert.Empty(t, result.Failed)
	require.Len(t, result.Applied, 3)

	for _, change := range result.Applied {
		assert.Equal(t, alerts.ActionUnchanged, change.Action, change.ID)
	}

	assert.Empty(t, fixture.writes(), "an Unchanged entry is reported without a request being made")
	assert.Empty(t, fixture.requests(http.MethodGet, folderPath("folder-uid-1")),
		"a group that needs no write needs no folder pre-check either")
}

// TestPush_UnresolvableOrgIDFailsItsGroup is the one case push cannot fill in for a rule that
// omits orgID: a group that does not exist yet, on a context that addresses a Grafana Cloud stack
// rather than an organization. Since orgID is no longer required on read, this is where it has to
// be reported - per group and by name, rather than sent as a body Grafana answers with a 400.
func TestPush_UnresolvableOrgIDFailsItsGroup(t *testing.T) {
	fixture := newPushFixture(t, pushRoutes(map[string]stubResponse{
		pushRoute(http.MethodGet, folderPath("folder-uid-1")):               okResponse(`{"uid":"folder-uid-1"}`),
		pushRoute(http.MethodGet, groupPath("folder-uid-1", "probe-group")): notFoundResponse(),
		pushRoute(http.MethodPut, groupPath("folder-uid-1", "probe-group")): okResponse(pushedGroupBody),
	}))

	rule := diffFileRule("rule-uid-1", "rule one")
	rule.OrgID = nil

	local := alerts.Bundle{Groups: []*models.AlertRuleGroup{pushGroup("probe-group", rule)}}

	result, err := alerts.Push(t.Context(), fixture.client, local, alerts.PushOptions{})
	require.NoError(t, err)

	require.Len(t, result.Failed, 1)
	assert.Equal(t,
		"alerts: push rule group folder-uid-1/probe-group: cannot resolve orgID: "+
			"the rule group does not exist in Grafana and the context has no org-id; "+
			"set org-id on the context or orgID on every rule in the group",
		result.Failed[0].Err.Error(),
	)
	assert.Empty(t, fixture.requests(http.MethodPut, groupPath("folder-uid-1", "probe-group")),
		"nothing is written for a group whose organization cannot be decided")
}

// TestPush_WritesRuleGroupsConcurrently pins the bound --max-concurrent puts on the rule-group
// phase - and that the writes really do overlap. The server answers neither PUT until both have
// arrived, so a serial implementation fails here rather than merely being slower.
func TestPush_WritesRuleGroupsConcurrently(t *testing.T) {
	const groups = 2

	var arrived atomic.Int32

	both := make(chan struct{})

	fixture := newHookedPushFixture(t, pushRoutes(map[string]stubResponse{
		pushRoute(http.MethodGet, folderPath("folder-uid-1")):           okResponse(`{"uid":"folder-uid-1"}`),
		pushRoute(http.MethodGet, groupPath("folder-uid-1", "group-a")): notFoundResponse(),
		pushRoute(http.MethodGet, groupPath("folder-uid-1", "group-b")): notFoundResponse(),
		pushRoute(http.MethodPut, groupPath("folder-uid-1", "group-a")): okResponse(pushedGroupBody),
		pushRoute(http.MethodPut, groupPath("folder-uid-1", "group-b")): okResponse(pushedGroupBody),
	}), func(r *http.Request) {
		if r.Method != http.MethodPut {
			return
		}

		if arrived.Add(1) == groups {
			close(both)
		}

		select {
		case <-both:
		case <-time.After(5 * time.Second):
			assert.Fail(t, "the rule-group writes never overlapped")
		}
	})

	local := alerts.Bundle{Groups: []*models.AlertRuleGroup{
		pushGroup("group-a", diffFileRule("rule-uid-1", "rule one")),
		pushGroup("group-b", diffFileRule("rule-uid-2", "rule two")),
	}}

	result, err := alerts.Push(t.Context(), fixture.client, local, alerts.PushOptions{MaxConcurrent: groups})
	require.NoError(t, err)
	assert.Empty(t, result.Failed)
	assert.Len(t, result.Applied, groups)
	assert.Len(t, fixture.writes(), groups)
}

// pushFixture is a generated client wired against a scripted server that records the method, path,
// provenance header and body of every request - which together are what a push's behaviour is,
// beyond the Result it returns.
type pushFixture struct {
	client *goapi.GrafanaHTTPAPI

	mu       sync.Mutex
	recorded []recordedRequest
}

// recordedRequest is one request a push made. provenance is nil when the header was absent, which
// is a different instruction to Grafana than sending it.
type recordedRequest struct {
	method     string
	path       string
	provenance *string
	cookie     string
	body       string
}

// acceptedResponse is what the contact-point and policy-tree writes return: 202, not 200. The
// generated readers accept exactly that code and turn anything else into an error.
func acceptedResponse() stubResponse {
	return stubResponse{status: http.StatusAccepted, body: acceptedResponseBody}
}

// decodeGroupBody decodes a recorded rule-group write back into the model, so assertions are about
// the fields a push sets rather than about JSON spelling.
func decodeGroupBody(t *testing.T, body string) *models.AlertRuleGroup {
	t.Helper()

	group := &models.AlertRuleGroup{}
	require.NoError(t, json.Unmarshal([]byte(body), group))

	return group
}

// newHookedPushFixture is newPushFixture with a hook the server runs before it answers, which is
// how the concurrency test holds two writes in flight at once without timing anything.
func newHookedPushFixture(t *testing.T, routes map[string]stubResponse, hook func(*http.Request)) *pushFixture {
	t.Helper()

	return newPushFixtureWith(t, routes, "", hook, nil)
}

// newPushFixture serves routes (method+path -> response), failing the test on any request the
// table does not cover.
func newPushFixture(t *testing.T, routes map[string]stubResponse) *pushFixture {
	t.Helper()

	return newPushFixtureWith(t, routes, "", nil, nil)
}

// newPushFixtureWith is the fixture every constructor above is a shorthand for.
//
// rotateURL, when set, wires a real config.SessionSource (instead of a static cookie) pointed at a
// rotate endpoint of the caller's; override, when set, decides which requests are answered before
// the route table is consulted, which together is what lets a 401-then-rotate-then-replay be
// driven end to end.
func newPushFixtureWith(
	t *testing.T,
	routes map[string]stubResponse,
	rotateURL string,
	hook func(*http.Request),
	override func(*http.Request) (int, bool),
) *pushFixture {
	t.Helper()

	fixture := &pushFixture{}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fixture.record(t, r)

		if hook != nil {
			hook(r)
		}

		if override != nil {
			if status, matched := override(r); matched {
				w.WriteHeader(status)

				return
			}
		}

		route, ok := routes[pushRoute(r.Method, r.URL.Path)]
		if !ok {
			assert.Fail(t, "unexpected request", pushRoute(r.Method, r.URL.Path))
			w.WriteHeader(http.StatusNotFound)

			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(route.status)

		_, err := w.Write([]byte(route.body))
		assert.NoError(t, err)
	}))
	t.Cleanup(server.Close)

	grafanaCfg := &config.GrafanaConfig{Server: server.URL, SessionCookie: "abc123"}
	if rotateURL != "" {
		grafanaCfg = &config.GrafanaConfig{
			Server:  server.URL,
			Session: config.NewSessionSource("old-cookie", rotateURL, nil, testutils.NewFakeKeychainStore(), "acct"),
		}
	}

	gClient, err := grafana.ClientFromContext(&config.Context{Grafana: grafanaCfg})
	require.NoError(t, err)

	fixture.client = gClient

	return fixture
}

// newRotatingPushFixture is newPushFixture with a live session rotation behind it; see
// newPushFixtureWith.
func newRotatingPushFixture(
	t *testing.T,
	routes map[string]stubResponse,
	rotateURL string,
	override func(*http.Request) (int, bool),
) *pushFixture {
	t.Helper()

	return newPushFixtureWith(t, routes, rotateURL, nil, override)
}

// notFoundResponse is how Grafana answers a GET for an object that does not exist, which for a
// push is the normal answer for everything it is about to create.
func notFoundResponse() stubResponse {
	return stubResponse{status: http.StatusNotFound, body: `{"message":"not found"}`}
}

// pushGroup builds a local rule group in the bundle's folder.
func pushGroup(title string, rules ...*models.ProvisionedAlertRule) *models.AlertRuleGroup {
	return testGroup("folder-uid-1", title, rules...)
}

// pushLocalBundle is the bundle most of these tests push: one of each kind, as a bundle read back
// from disk holds them (no read-only fields).
func pushLocalBundle() alerts.Bundle {
	return alerts.Bundle{
		Groups:        []*models.AlertRuleGroup{pushGroup("probe-group", diffFileRule("rule-uid-1", "rule one"))},
		ContactPoints: []*models.EmbeddedContactPoint{diffPoint("cp-uid-1", "probe-slack", map[string]any{"recipient": "#probe"})},
		Policies:      &models.Route{Receiver: "probe-slack"},
	}
}

// pushRoute keys a route table by method and path: a push GETs and writes the same addresses, so
// a path alone would not say which of the two a response belongs to.
func pushRoute(method, path string) string {
	return method + " " + path
}

// pushRoutes merges the caller's routes into the reads every push performs, so each test declares
// only what it is actually about.
func pushRoutes(routes map[string]stubResponse) map[string]stubResponse {
	base := map[string]stubResponse{
		pushRoute(http.MethodGet, contactPointsPath): okResponse(`[]`),
		pushRoute(http.MethodGet, policiesPath):      okResponse(`{"receiver":"grafana-default-email"}`),
	}

	maps.Copy(base, routes)

	return base
}

// twoFolderBundle is the bundle the folder pre-check is tested through: one new group in each of
// two folders, and nothing else, so what a per-folder refusal does to the *other* folder is the
// only thing the result can be about.
func twoFolderBundle() alerts.Bundle {
	return alerts.Bundle{Groups: []*models.AlertRuleGroup{
		testGroup("folder-uid-1", "group-a", diffFileRule("rule-uid-1", "rule one")),
		testGroup("folder-uid-2", "group-b", diffFileRule("rule-uid-2", "rule two")),
	}}
}

// twoFolderRoutes answers every read and write twoFolderBundle's push makes, so a test overriding
// one folder's pre-check asserts about a refusal rather than about a route it forgot to script -
// the write of the refused group included, which is what makes "it was not written" an assertion.
func twoFolderRoutes(routes map[string]stubResponse) map[string]stubResponse {
	base := pushRoutes(map[string]stubResponse{
		pushRoute(http.MethodGet, folderPath("folder-uid-1")):           okResponse(`{"uid":"folder-uid-1"}`),
		pushRoute(http.MethodGet, folderPath("folder-uid-2")):           okResponse(`{"uid":"folder-uid-2"}`),
		pushRoute(http.MethodGet, groupPath("folder-uid-1", "group-a")): notFoundResponse(),
		pushRoute(http.MethodGet, groupPath("folder-uid-2", "group-b")): notFoundResponse(),
		pushRoute(http.MethodPut, groupPath("folder-uid-1", "group-a")): okResponse(pushedGroupBody),
		pushRoute(http.MethodPut, groupPath("folder-uid-2", "group-b")): okResponse(pushedGroupBody),
	})

	maps.Copy(base, routes)

	return base
}

// record files one request, reading its body and provenance header before the handler answers.
//
// The read failure is asserted last rather than returned: this runs on the server's goroutine, so
// it must not call require and end the test from there.
func (f *pushFixture) record(t *testing.T, r *http.Request) {
	t.Helper()

	body, err := io.ReadAll(r.Body)

	var provenance *string
	if values := r.Header.Values(disableProvenanceHeader); len(values) > 0 {
		provenance = &values[0]
	}

	f.store(recordedRequest{
		method:     r.Method,
		path:       r.URL.Path,
		provenance: provenance,
		cookie:     r.Header.Get("Cookie"),
		body:       string(body),
	})

	assert.NoError(t, err, "reading the request body")
}

// requests returns every recorded request to one method and path.
func (f *pushFixture) requests(method, path string) []recordedRequest {
	f.mu.Lock()
	defer f.mu.Unlock()

	matched := make([]recordedRequest, 0, len(f.recorded))

	for _, request := range f.recorded {
		if request.method == method && request.path == path {
			matched = append(matched, request)
		}
	}

	return matched
}

// store files one recorded request.
func (f *pushFixture) store(request recordedRequest) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.recorded = append(f.recorded, request)
}

// writes returns every recorded request that could have changed something.
func (f *pushFixture) writes() []recordedRequest {
	f.mu.Lock()
	defer f.mu.Unlock()

	matched := make([]recordedRequest, 0, len(f.recorded))

	for _, request := range f.recorded {
		if request.method != http.MethodGet {
			matched = append(matched, request)
		}
	}

	return matched
}
