package alerts_test

import (
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/go-openapi/runtime"
	"github.com/grafana/grafana-openapi-client-go/models"
	"github.com/grafana/grafanapi/internal/alerts"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	scopedFolder = "folder-uid-1"
	scopedGroup  = "probe-group"
)

// TestPushRuleUIDs_KeepsTheGroupsOtherRulesFromGrafana is the data-loss test of the rule-scoped
// push: whatever the file says about the siblings of the named rule - dropped, renamed, re-ordered,
// a rule added, another interval - the group PUT carries Grafana's own.
func TestPushRuleUIDs_KeepsTheGroupsOtherRulesFromGrafana(t *testing.T) {
	remote := scopedRemoteGroup("api", "rule-uid-1", "rule-uid-2", "rule-uid-3")
	fixture := newPushFixture(t, scopedRoutes(t, remote))

	edited := scopedFileRule("rule-uid-1")
	edited.Data[0].Model = map[string]any{"type": "math", "expression": "$A > 5"}

	renamed := scopedFileRule("rule-uid-3")
	renamed.Title = new("renamed in the file")

	uidless := scopedFileRule("")
	uidless.Title = new("added in the file")

	// Dropped rule 2, renamed rule 3, a uid-less rule, re-ordered, another interval.
	local := alerts.Bundle{Groups: []*models.AlertRuleGroup{{
		FolderUID: scopedFolder,
		Title:     scopedGroup,
		Interval:  120,
		Rules:     []*models.ProvisionedAlertRule{renamed, uidless, edited},
	}}}

	ctx, logs := captureLogs(t)

	result, err := alerts.Push(ctx, fixture.client, local, alerts.PushOptions{RuleUIDs: []string{"rule-uid-1"}})
	require.NoError(t, err)

	written := fixture.requests(http.MethodPut, groupPath(scopedFolder, scopedGroup))
	require.Len(t, written, 1)
	require.Len(t, fixture.writes(), 1)

	body := decodeGroupBody(t, written[0].body)
	assert.EqualValues(t, 60, body.Interval, "the interval is Grafana's")
	assert.Equal(t, []string{"rule-uid-1", "rule-uid-2", "rule-uid-3"}, ruleUIDs(body), "the order is Grafana's")

	assertRuleEqualsRemote(t, body.Rules[1], remote.Rules[1])
	assertRuleEqualsRemote(t, body.Rules[2], remote.Rules[2])
	assert.Equal(t, "$A > 5", modelExpression(t, body.Rules[0]), "the named rule is the file's")

	assert.Equal(t, []alerts.Change{{
		Action: alerts.ActionUpdate,
		Kind:   alerts.KindRuleGroup,
		ID:     "folder-uid-1/probe-group",
		Detail: []string{"~ rule-uid-1 title (rule-uid-1)"},
	}}, result.Applied)
	assert.Empty(t, result.Failed)

	assert.Equal(t, 1, strings.Count(logs.String(), "not pushed by --rule-uid"), logs.String())
	assert.Contains(t, logs.String(), "4 other difference(s)")
	assertNoWholeInstanceReads(t, fixture)
}

// TestPushRuleUIDs_SiblingsComeFromGrafana pins the two cases a plan built from the file would turn
// into a deletion: a sibling Grafana gained since the pull must be kept, and one it lost must not be
// brought back.
func TestPushRuleUIDs_SiblingsComeFromGrafana(t *testing.T) {
	tests := []struct {
		name   string
		remote []string
		want   []string
	}{
		{
			name:   "a sibling added in Grafana after the pull is kept",
			remote: []string{"rule-uid-1", "rule-uid-2", "rule-uid-3", "rule-uid-4"},
			want:   []string{"rule-uid-1", "rule-uid-2", "rule-uid-3", "rule-uid-4"},
		},
		{
			name:   "a sibling deleted in Grafana is not resurrected",
			remote: []string{"rule-uid-1", "rule-uid-3"},
			want:   []string{"rule-uid-1", "rule-uid-3"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newPushFixture(t, scopedRoutes(t, scopedRemoteGroup("api", tc.remote...)))

			edited := scopedFileRule("rule-uid-1")
			edited.Data[0].Model = map[string]any{"type": "math", "expression": "$A > 5"}

			local := alerts.Bundle{Groups: []*models.AlertRuleGroup{
				pushGroup(scopedGroup, edited, scopedFileRule("rule-uid-2"), scopedFileRule("rule-uid-3")),
			}}

			result, err := alerts.Push(t.Context(), fixture.client, local, alerts.PushOptions{RuleUIDs: []string{"rule-uid-1"}})
			require.NoError(t, err)

			written := fixture.requests(http.MethodPut, groupPath(scopedFolder, scopedGroup))
			require.Len(t, written, 1)
			assert.Equal(t, tc.want, ruleUIDs(decodeGroupBody(t, written[0].body)))

			require.Len(t, result.Applied, 1)
			assert.Equal(t, []string{"~ rule-uid-1 title (rule-uid-1)"}, result.Applied[0].Detail)
		})
	}
}

// TestPushRuleUIDs_NilRemoteRuleIsKept pins that a null element in the fetched group's rules is
// never dereferenced: it stays where it is in the synthetic group, and the body drops it as every
// group body does.
func TestPushRuleUIDs_NilRemoteRuleIsKept(t *testing.T) {
	rule1, rule2 := scopedRemoteRule("rule-uid-1", "api"), scopedRemoteRule("rule-uid-2", "api")
	routes := scopedRoutes(t, scopedRemoteGroup("api", "rule-uid-1", "rule-uid-2"))
	routes[pushRoute(http.MethodGet, groupPath(scopedFolder, scopedGroup))] = okResponse(
		`{"folderUid":"folder-uid-1","title":"probe-group","interval":60,"rules":[null,` +
			marshalJSON(t, rule1) + `,` + marshalJSON(t, rule2) + `]}`,
	)

	fixture := newPushFixture(t, routes)

	edited := scopedFileRule("rule-uid-1")
	edited.Data[0].Model = map[string]any{"type": "math", "expression": "$A > 5"}

	local := alerts.Bundle{Groups: []*models.AlertRuleGroup{pushGroup(scopedGroup, edited, scopedFileRule("rule-uid-2"))}}

	_, err := alerts.Push(t.Context(), fixture.client, local, alerts.PushOptions{RuleUIDs: []string{"rule-uid-1"}})
	require.NoError(t, err)

	written := fixture.requests(http.MethodPut, groupPath(scopedFolder, scopedGroup))
	require.Len(t, written, 1)
	assert.Equal(t, []string{"rule-uid-1", "rule-uid-2"}, ruleUIDs(decodeGroupBody(t, written[0].body)))

	remote := alerts.Bundle{Groups: []*models.AlertRuleGroup{{
		FolderUID: scopedFolder, Title: scopedGroup, Interval: 60,
		Rules: []*models.ProvisionedAlertRule{nil, rule1, rule2},
	}}}

	synthetic, _, err := alerts.ScopeBundlesForTest(local, remote, []string{"rule-uid-1"})
	require.NoError(t, err)
	require.Len(t, synthetic.Groups, 1)
	require.Len(t, synthetic.Groups[0].Rules, 3)
	assert.Nil(t, synthetic.Groups[0].Rules[0], "the null stays in place")
	assert.Same(t, edited, synthetic.Groups[0].Rules[1], "the named rule is swapped in")
	assert.Same(t, rule2, synthetic.Groups[0].Rules[2], "a sibling is Grafana's")
}

// TestPushRuleUIDs_GroupsAreWrittenOnce pins the grouping: named rules in one group are one PUT
// carrying every edit, in two groups they are two.
func TestPushRuleUIDs_GroupsAreWrittenOnce(t *testing.T) {
	t.Run("two rules of one group are one write", func(t *testing.T) {
		fixture := newPushFixture(t, scopedRoutes(t, scopedRemoteGroup("api", "rule-uid-1", "rule-uid-2", "rule-uid-3")))

		one, two := scopedFileRule("rule-uid-1"), scopedFileRule("rule-uid-2")
		one.Data[0].Model = map[string]any{"type": "math", "expression": "one"}
		two.Data[0].Model = map[string]any{"type": "math", "expression": "two"}

		local := alerts.Bundle{Groups: []*models.AlertRuleGroup{pushGroup(scopedGroup, one, two, scopedFileRule("rule-uid-3"))}}

		result, err := alerts.Push(
			t.Context(), fixture.client, local, alerts.PushOptions{RuleUIDs: []string{"rule-uid-2", "rule-uid-1"}},
		)
		require.NoError(t, err)

		written := fixture.requests(http.MethodPut, groupPath(scopedFolder, scopedGroup))
		require.Len(t, written, 1)
		require.Len(t, fixture.writes(), 1)

		body := decodeGroupBody(t, written[0].body)
		assert.Equal(t, "one", modelExpression(t, body.Rules[0]))
		assert.Equal(t, "two", modelExpression(t, body.Rules[1]))

		require.Len(t, result.Applied, 1)
		assert.Equal(t, []string{"~ rule-uid-1 title (rule-uid-1)", "~ rule-uid-2 title (rule-uid-2)"}, result.Applied[0].Detail)
	})

	t.Run("rules of two groups are two writes", func(t *testing.T) {
		other := scopedGroupOf("folder-uid-2", "other-group", "api", "rule-uid-9")
		routes := scopedRoutes(t, scopedRemoteGroup("api", "rule-uid-1"), other)
		fixture := newPushFixture(t, routes)

		one, nine := scopedFileRule("rule-uid-1"), scopedFileRuleIn("folder-uid-2", "other-group", "rule-uid-9")
		one.Data[0].Model = map[string]any{"type": "math", "expression": "one"}
		nine.Data[0].Model = map[string]any{"type": "math", "expression": "nine"}

		local := alerts.Bundle{Groups: []*models.AlertRuleGroup{
			pushGroup(scopedGroup, one),
			testGroup("folder-uid-2", "other-group", nine),
		}}

		_, err := alerts.Push(
			t.Context(), fixture.client, local, alerts.PushOptions{RuleUIDs: []string{"rule-uid-1", "rule-uid-9"}},
		)
		require.NoError(t, err)

		assert.Len(t, fixture.requests(http.MethodPut, groupPath(scopedFolder, scopedGroup)), 1)
		assert.Len(t, fixture.requests(http.MethodPut, groupPath("folder-uid-2", "other-group")), 1)
		assert.Len(t, fixture.writes(), 2)
	})
}

// TestPushRuleUIDs_UnchangedIsNotWritten pins that a file equal to Grafana is no write and no
// warning.
func TestPushRuleUIDs_UnchangedIsNotWritten(t *testing.T) {
	fixture := newPushFixture(t, scopedRoutes(t, scopedRemoteGroup("api", "rule-uid-1", "rule-uid-2")))
	local := alerts.Bundle{Groups: []*models.AlertRuleGroup{
		pushGroup(scopedGroup, scopedFileRule("rule-uid-1"), scopedFileRule("rule-uid-2")),
	}}

	ctx, logs := captureLogs(t)

	result, err := alerts.Push(ctx, fixture.client, local, alerts.PushOptions{RuleUIDs: []string{"rule-uid-1"}})
	require.NoError(t, err)

	assert.Equal(t, []alerts.Change{{
		Action: alerts.ActionUnchanged,
		Kind:   alerts.KindRuleGroup,
		ID:     "folder-uid-1/probe-group",
	}}, result.Applied)
	assert.Empty(t, fixture.writes())
	assert.Empty(t, logs.String())
}

// TestPushRuleUIDs_ProvenanceMirrorsTheStoredValue pins that the header of a scoped write is decided
// by the group Grafana holds, exactly as a whole push decides it.
func TestPushRuleUIDs_ProvenanceMirrorsTheStoredValue(t *testing.T) {
	tests := []struct {
		name       string
		stored     string
		wantHeader *string
	}{
		{name: "unclaimed is sent with the header", stored: "", wantHeader: new("true")},
		{name: "claimed by the API is sent without it", stored: "api", wantHeader: nil},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newPushFixture(t, scopedRoutes(t, scopedRemoteGroup(tc.stored, "rule-uid-1")))

			edited := scopedFileRule("rule-uid-1")
			edited.Data[0].Model = map[string]any{"type": "math", "expression": "$A > 5"}

			local := alerts.Bundle{Groups: []*models.AlertRuleGroup{pushGroup(scopedGroup, edited)}}

			_, err := alerts.Push(t.Context(), fixture.client, local, alerts.PushOptions{RuleUIDs: []string{"rule-uid-1"}})
			require.NoError(t, err)

			written := fixture.requests(http.MethodPut, groupPath(scopedFolder, scopedGroup))
			require.Len(t, written, 1)
			assert.Equal(t, tc.wantHeader, written[0].provenance)
		})
	}
}

// TestPushRuleUIDs_RefusesBeforeAnyWrite covers the failures that must stop the whole command: they
// are not per-object failures, so opts.StopOnError has no say.
func TestPushRuleUIDs_RefusesBeforeAnyWrite(t *testing.T) {
	t.Run("a uid the bundle does not hold makes no request", func(t *testing.T) {
		fixture := newPushFixture(t, scopedRoutes(t, scopedRemoteGroup("api", "rule-uid-1")))
		local := alerts.Bundle{Groups: []*models.AlertRuleGroup{pushGroup(scopedGroup, scopedFileRule("rule-uid-1"))}}

		_, err := alerts.Push(t.Context(), fixture.client, local, alerts.PushOptions{RuleUIDs: []string{"rule-uid-7"}})
		require.Error(t, err)
		require.NotErrorIs(t, err, alerts.ErrRuleNotFound)
		assert.Contains(t, err.Error(), "alerts: push rule rule-uid-7: not in the bundle")
		assert.Empty(t, fixture.recorded, "a mistake in the arguments must not cost a request")
	})

	t.Run("a uid Grafana does not know is not found", func(t *testing.T) {
		routes := scopedRoutes(t, scopedRemoteGroup("api", "rule-uid-1"))
		routes[pushRoute(http.MethodGet, rulePath("rule-uid-9"))] = notFoundResponse()
		fixture := newPushFixture(t, routes)

		local := alerts.Bundle{Groups: []*models.AlertRuleGroup{
			pushGroup(scopedGroup, scopedFileRule("rule-uid-1"), scopedFileRule("rule-uid-9")),
		}}

		result, err := alerts.Push(
			t.Context(), fixture.client, local, alerts.PushOptions{RuleUIDs: []string{"rule-uid-9"}, StopOnError: false},
		)
		require.ErrorIs(t, err, alerts.ErrRuleNotFound)
		require.EqualError(t, err, "alerts: rule rule-uid-9 not found")
		assert.Equal(t, alerts.Result{}, result)
		assert.Empty(t, fixture.writes())
	})

	t.Run("a rule Grafana holds in another group is a move", func(t *testing.T) {
		elsewhere := scopedGroupOf(scopedFolder, "other-group", "api", "rule-uid-1")
		fixture := newPushFixture(t, scopedRoutes(t, elsewhere))

		local := alerts.Bundle{Groups: []*models.AlertRuleGroup{pushGroup(scopedGroup, scopedFileRule("rule-uid-1"))}}

		result, err := alerts.Push(t.Context(), fixture.client, local, alerts.PushOptions{RuleUIDs: []string{"rule-uid-1"}})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "Grafana holds it in rule group folder-uid-1/other-group, the bundle in folder-uid-1/probe-group")
		assert.Contains(t, err.Error(), "whole-group push")
		assert.Equal(t, alerts.Result{}, result)
		assert.Empty(t, fixture.writes())
	})

	t.Run("a dead session on the rule lookup is a 401", func(t *testing.T) {
		routes := scopedRoutes(t, scopedRemoteGroup("api", "rule-uid-1"))
		routes[pushRoute(http.MethodGet, rulePath("rule-uid-1"))] = stubResponse{status: http.StatusUnauthorized, body: `{"message":"unauthorized"}`}
		fixture := newPushFixture(t, routes)

		local := alerts.Bundle{Groups: []*models.AlertRuleGroup{pushGroup(scopedGroup, scopedFileRule("rule-uid-1"))}}

		_, err := alerts.Push(t.Context(), fixture.client, local, alerts.PushOptions{RuleUIDs: []string{"rule-uid-1"}})
		require.Error(t, err)

		var apiErr *runtime.APIError

		require.ErrorAs(t, err, &apiErr)
		assert.Equal(t, http.StatusUnauthorized, apiErr.Code)
		assert.Empty(t, fixture.writes())
	})
}

// TestPlanScoped_RefusesWhatReachesBeyondTheNamedRules feeds the guard plans no real scope output
// produces, built by doctoring the synthetic group, and pins that each is refused while the plan
// scopeBundles really builds passes.
func TestPlanScoped_RefusesWhatReachesBeyondTheNamedRules(t *testing.T) {
	remote := alerts.Bundle{Groups: []*models.AlertRuleGroup{scopedRemoteGroup("api", "rule-uid-1", "rule-uid-2", "rule-uid-3")}}
	uids := []string{"rule-uid-1"}
	editedLine := "~ rule-uid-1 title (rule-uid-1)"

	// synthetic is the local side of a genuine rule-scoped plan: rule 1 edited, nothing else.
	synthetic := func(t *testing.T) *models.AlertRuleGroup {
		t.Helper()

		edited := scopedFileRule("rule-uid-1")
		edited.Data[0].Model = map[string]any{"type": "math", "expression": "$A > 5"}

		local := alerts.Bundle{Groups: []*models.AlertRuleGroup{pushGroup(scopedGroup, edited)}}

		built, _, err := alerts.ScopeBundlesForTest(local, remote, uids)
		require.NoError(t, err)
		require.Len(t, built.Groups, 1)

		group := *built.Groups[0]
		group.Rules = slices.Clone(group.Rules)

		return &group
	}

	otherRemote := scopedGroupOf("folder-uid-2", "other-group", "api", "rule-uid-1", "rule-uid-9")
	otherLocal := func(expression string) *models.AlertRuleGroup {
		edited := scopedFileRuleIn("folder-uid-2", "other-group", "rule-uid-1")
		edited.Data[0].Model = map[string]any{"type": "math", "expression": expression}

		return testGroup("folder-uid-2", "other-group", edited, scopedFileRuleIn("folder-uid-2", "other-group", "rule-uid-9"))
	}

	tests := []struct {
		name    string
		local   func(group *models.AlertRuleGroup) alerts.Bundle
		remote  alerts.Bundle
		uids    []string
		wantErr string
	}{
		{
			name: "a dropped sibling",
			local: func(group *models.AlertRuleGroup) alerts.Bundle {
				group.Rules = []*models.ProvisionedAlertRule{group.Rules[0], group.Rules[2]}

				return alerts.Bundle{Groups: []*models.AlertRuleGroup{group}}
			},
			remote:  remote,
			uids:    uids,
			wantErr: "(- rule-uid-2 title (rule-uid-2))",
		},
		{
			name: "another interval",
			local: func(group *models.AlertRuleGroup) alerts.Bundle {
				group.Interval = 120

				return alerts.Bundle{Groups: []*models.AlertRuleGroup{group}}
			},
			remote:  remote,
			uids:    uids,
			wantErr: "(~ interval 60s→120s)",
		},
		{
			name: "another order",
			local: func(group *models.AlertRuleGroup) alerts.Bundle {
				group.Rules[0], group.Rules[2] = group.Rules[2], group.Rules[0]

				return alerts.Bundle{Groups: []*models.AlertRuleGroup{group}}
			},
			remote:  remote,
			uids:    uids,
			wantErr: "(~ rule order)",
		},
		{
			name: "a changed sibling",
			local: func(group *models.AlertRuleGroup) alerts.Bundle {
				edited := *group.Rules[1]
				edited.Data = slices.Clone(edited.Data)
				edited.Data[0] = &models.AlertQuery{
					RefID: edited.Data[0].RefID, DatasourceUID: edited.Data[0].DatasourceUID,
					Model: map[string]any{"type": "math", "expression": "tampered"},
				}
				group.Rules[1] = &edited

				return alerts.Bundle{Groups: []*models.AlertRuleGroup{group}}
			},
			remote:  remote,
			uids:    uids,
			wantErr: "(~ rule-uid-2 title (rule-uid-2))",
		},
		{
			name: "a group Grafana does not hold",
			local: func(group *models.AlertRuleGroup) alerts.Bundle {
				group.Title = "ghost"

				return alerts.Bundle{Groups: []*models.AlertRuleGroup{group}}
			},
			remote:  remote,
			uids:    uids,
			wantErr: "RuleGroup folder-uid-1/ghost: the plan would change more than the named rules (action Create)",
		},
		{
			name: "a group holding none of the named rules",
			local: func(group *models.AlertRuleGroup) alerts.Bundle {
				return alerts.Bundle{Groups: []*models.AlertRuleGroup{group, otherLocal("changed")}}
			},
			remote: alerts.Bundle{Groups: []*models.AlertRuleGroup{
				remote.Groups[0], scopedGroupOf("folder-uid-2", "other-group", "api", "rule-uid-9"),
			}},
			uids:    uids,
			wantErr: "RuleGroup folder-uid-2/other-group: the plan would change more than the named rules (a rule group holding none",
		},
		{
			name: "a named rule's change under another group's ID",
			local: func(group *models.AlertRuleGroup) alerts.Bundle {
				return alerts.Bundle{Groups: []*models.AlertRuleGroup{group, otherLocal("changed")}}
			},
			remote:  alerts.Bundle{Groups: []*models.AlertRuleGroup{remote.Groups[0], otherRemote}},
			uids:    []string{"rule-uid-1", "rule-uid-9"},
			wantErr: "RuleGroup folder-uid-2/other-group: the plan would change more than the named rules (" + editedLine + ")",
		},
		{
			name: "a contact point",
			local: func(group *models.AlertRuleGroup) alerts.Bundle {
				return alerts.Bundle{
					Groups:        []*models.AlertRuleGroup{group},
					ContactPoints: []*models.EmbeddedContactPoint{diffPoint("cp-uid-1", "probe-slack", map[string]any{"recipient": "#probe"})},
				}
			},
			remote:  remote,
			uids:    uids,
			wantErr: "push of ContactPoint cp-uid-1: the plan would change more than the named rules (not a rule group)",
		},
		{
			name: "a policy tree",
			local: func(group *models.AlertRuleGroup) alerts.Bundle {
				return alerts.Bundle{Groups: []*models.AlertRuleGroup{group}, Policies: &models.Route{Receiver: "probe"}}
			},
			remote:  remote,
			uids:    uids,
			wantErr: "push of PolicyTree",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			changes, err := alerts.PlanScopedForTest(tc.local(synthetic(t)), tc.remote, tc.uids)
			require.Error(t, err)
			assert.Nil(t, changes)
			assert.Contains(t, err.Error(), "alerts: push: refusing a rule-scoped push of ")
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}

	t.Run("the plan scopeBundles builds passes", func(t *testing.T) {
		changes, err := alerts.PlanScopedForTest(alerts.Bundle{Groups: []*models.AlertRuleGroup{synthetic(t)}}, remote, uids)
		require.NoError(t, err)
		assert.Equal(t, []alerts.Change{{
			Action: alerts.ActionUpdate, Kind: alerts.KindRuleGroup, ID: "folder-uid-1/probe-group", Detail: []string{editedLine},
		}}, changes)
	})

	t.Run("an identical pair is one Unchanged and passes", func(t *testing.T) {
		identical := alerts.Bundle{Groups: []*models.AlertRuleGroup{
			pushGroup(scopedGroup, scopedFileRule("rule-uid-1"), scopedFileRule("rule-uid-2"), scopedFileRule("rule-uid-3")),
		}}

		changes, err := alerts.PlanScopedForTest(identical, remote, uids)
		require.NoError(t, err)
		assert.Equal(t, []alerts.Change{{
			Action: alerts.ActionUnchanged, Kind: alerts.KindRuleGroup, ID: "folder-uid-1/probe-group",
		}}, changes)
	})
}

// TestPushRuleUIDs_GuardStandsBetweenScopeAndWrite hands push doctored scope output - which no
// scopeBundles produces - and pins that Push itself refuses it, in apply and in dry run, before a
// write and with an empty Result. It fails if push plans without planScoped.
func TestPushRuleUIDs_GuardStandsBetweenScopeAndWrite(t *testing.T) {
	doctors := map[string]func(*models.AlertRuleGroup) []*models.AlertRuleGroup{
		"an extra sibling": func(group *models.AlertRuleGroup) []*models.AlertRuleGroup {
			extra := scopedFileRule("rule-uid-8")
			group.Rules = append(group.Rules, extra)

			return []*models.AlertRuleGroup{group}
		},
		"another interval": func(group *models.AlertRuleGroup) []*models.AlertRuleGroup {
			group.Interval = 120

			return []*models.AlertRuleGroup{group}
		},
		"a group Grafana does not hold": func(group *models.AlertRuleGroup) []*models.AlertRuleGroup {
			return []*models.AlertRuleGroup{group, scopedGroupOf(scopedFolder, "ghost", "", "rule-uid-8")}
		},
	}

	for name, doctor := range doctors {
		for _, dryRun := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s dryRun=%t", name, dryRun), func(t *testing.T) {
				fixture := newPushFixture(t, scopedRoutes(t, scopedRemoteGroup("api", "rule-uid-1", "rule-uid-2")))

				edited := scopedFileRule("rule-uid-1")
				edited.Data[0].Model = map[string]any{"type": "math", "expression": "$A > 5"}

				local := alerts.Bundle{Groups: []*models.AlertRuleGroup{pushGroup(scopedGroup, edited, scopedFileRule("rule-uid-2"))}}

				scope := func(local, remote alerts.Bundle, uids []string) (alerts.Bundle, alerts.Bundle, error) {
					built, held, err := alerts.ScopeBundlesForTest(local, remote, uids)
					if err != nil {
						return built, held, err
					}

					group := *built.Groups[0]
					group.Rules = slices.Clone(group.Rules)

					return alerts.Bundle{Groups: doctor(&group)}, held, nil
				}

				result, err := alerts.PushWithScopeForTest(
					t.Context(), fixture.client, local, alerts.PushOptions{RuleUIDs: []string{"rule-uid-1"}, DryRun: dryRun}, scope,
				)
				require.Error(t, err)
				assert.Contains(t, err.Error(), "refusing a rule-scoped push")
				assert.Equal(t, alerts.Result{}, result)
				assert.Empty(t, fixture.writes())
			})
		}
	}
}

// TestPushRuleUIDs_ProvenanceRefusals pins that a scoped write is refused exactly as a whole push
// refuses it, per item and in dry run too.
func TestPushRuleUIDs_ProvenanceRefusals(t *testing.T) {
	mixed := scopedRemoteGroup("", "rule-uid-1", "rule-uid-2")
	mixed.Rules[1].Provenance = "api"

	tests := []struct {
		name    string
		remote  *models.AlertRuleGroup
		wantErr string
	}{
		{name: "mixed provenance", remote: mixed, wantErr: "mixed provenance"},
		{name: "foreign provenance", remote: scopedRemoteGroup("file", "rule-uid-1", "rule-uid-2"), wantErr: `provenance "file" is managed outside the API`},
	}

	for _, tc := range tests {
		for _, dryRun := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s dryRun=%t", tc.name, dryRun), func(t *testing.T) {
				fixture := newPushFixture(t, scopedRoutes(t, tc.remote))

				edited := scopedFileRule("rule-uid-1")
				edited.Data[0].Model = map[string]any{"type": "math", "expression": "$A > 5"}

				local := alerts.Bundle{Groups: []*models.AlertRuleGroup{pushGroup(scopedGroup, edited, scopedFileRule("rule-uid-2"))}}

				result, err := alerts.Push(
					t.Context(), fixture.client, local, alerts.PushOptions{RuleUIDs: []string{"rule-uid-1"}, DryRun: dryRun},
				)
				require.NoError(t, err)
				assert.Empty(t, result.Applied)
				require.Len(t, result.Failed, 1)
				assert.Equal(t, "folder-uid-1/probe-group", result.Failed[0].Change.ID)
				assert.Contains(t, result.Failed[0].Err.Error(), tc.wantErr)
				assert.Empty(t, fixture.writes())
			})
		}
	}
}

// TestPushRuleUIDs_ForbiddenWrite pins that a 403 on the group PUT is a per-item failure keeping
// the *runtime.APIError reachable, so it renders as permission denied.
func TestPushRuleUIDs_ForbiddenWrite(t *testing.T) {
	routes := scopedRoutes(t, scopedRemoteGroup("api", "rule-uid-1"))
	routes[pushRoute(http.MethodPut, groupPath(scopedFolder, scopedGroup))] = stubResponse{
		status: http.StatusForbidden, body: `{"message":"denied"}`,
	}

	fixture := newPushFixture(t, routes)

	edited := scopedFileRule("rule-uid-1")
	edited.Data[0].Model = map[string]any{"type": "math", "expression": "$A > 5"}

	local := alerts.Bundle{Groups: []*models.AlertRuleGroup{pushGroup(scopedGroup, edited)}}

	result, err := alerts.Push(t.Context(), fixture.client, local, alerts.PushOptions{RuleUIDs: []string{"rule-uid-1"}})
	require.NoError(t, err)
	require.Len(t, result.Failed, 1)

	var apiErr *runtime.APIError

	require.ErrorAs(t, result.Failed[0].Err, &apiErr)
	assert.Equal(t, http.StatusForbidden, apiErr.Code)
}

// TestPushRuleUIDs_StopOnErrorStopsAtTheFirstGroup pins that with StopOnError the second group is
// never written once the first group's PUT failed.
func TestPushRuleUIDs_StopOnErrorStopsAtTheFirstGroup(t *testing.T) {
	other := scopedGroupOf("folder-uid-2", "other-group", "api", "rule-uid-9")
	routes := scopedRoutes(t, scopedRemoteGroup("api", "rule-uid-1"), other)
	routes[pushRoute(http.MethodPut, groupPath(scopedFolder, scopedGroup))] = stubResponse{
		status: http.StatusForbidden, body: `{"message":"denied"}`,
	}

	fixture := newPushFixture(t, routes)

	one, nine := scopedFileRule("rule-uid-1"), scopedFileRuleIn("folder-uid-2", "other-group", "rule-uid-9")
	one.Data[0].Model = map[string]any{"type": "math", "expression": "one"}
	nine.Data[0].Model = map[string]any{"type": "math", "expression": "nine"}

	local := alerts.Bundle{Groups: []*models.AlertRuleGroup{
		pushGroup(scopedGroup, one),
		testGroup("folder-uid-2", "other-group", nine),
	}}

	result, err := alerts.Push(
		t.Context(), fixture.client, local,
		alerts.PushOptions{RuleUIDs: []string{"rule-uid-1", "rule-uid-9"}, StopOnError: true},
	)
	require.Error(t, err)

	var apiErr *runtime.APIError

	require.ErrorAs(t, err, &apiErr)
	assert.Equal(t, http.StatusForbidden, apiErr.Code)

	require.Len(t, result.Failed, 1)
	assert.Equal(t, "folder-uid-1/probe-group", result.Failed[0].Change.ID)
	assert.Len(t, fixture.writes(), 1, "only the first group's PUT is attempted")
	assert.Empty(t, fixture.requests(http.MethodPut, groupPath("folder-uid-2", "other-group")))
}

// TestPushRuleUIDs_DryRunMatchesApply pins that a dry run reports the plan an apply then performs,
// with no write.
func TestPushRuleUIDs_DryRunMatchesApply(t *testing.T) {
	run := func(t *testing.T, dryRun bool) (alerts.Result, *pushFixture) {
		t.Helper()

		fixture := newPushFixture(t, scopedRoutes(t, scopedRemoteGroup("api", "rule-uid-1", "rule-uid-2", "rule-uid-3")))

		edited := scopedFileRule("rule-uid-1")
		edited.Data[0].Model = map[string]any{"type": "math", "expression": "$A > 5"}

		local := alerts.Bundle{Groups: []*models.AlertRuleGroup{{
			FolderUID: scopedFolder, Title: scopedGroup, Interval: 120,
			Rules: []*models.ProvisionedAlertRule{scopedFileRule("rule-uid-3"), edited},
		}}}

		result, err := alerts.Push(
			t.Context(), fixture.client, local, alerts.PushOptions{RuleUIDs: []string{"rule-uid-1"}, DryRun: dryRun},
		)
		require.NoError(t, err)

		return result, fixture
	}

	applied, _ := run(t, false)
	planned, fixture := run(t, true)

	assert.Equal(t, applied.Applied, planned.Applied)
	assert.Equal(t, applied.Failed, planned.Failed)
	assert.Empty(t, fixture.writes())
}

// scopedFileRule is a rule as a bundle file holds it, addressing the probe group.
func scopedFileRule(uid string) *models.ProvisionedAlertRule {
	return scopedFileRuleIn(scopedFolder, scopedGroup, uid)
}

// scopedFileRuleIn is scopedFileRule in any group. Its title is the one scopedRemoteRule gives the
// remote rule of the same uid, so an unedited file rule compares equal to it.
func scopedFileRuleIn(folderUID, group, uid string) *models.ProvisionedAlertRule {
	return diffStripped(testRule(folderUID, group, uid, uid+" title"))
}

// scopedGroupOf is a rule group as Grafana reports it: interval 60, every rule carrying the given
// provenance and the read-only fields.
func scopedGroupOf(folderUID, title, provenance string, uids ...string) *models.AlertRuleGroup {
	group := testGroup(folderUID, title)

	for _, uid := range uids {
		rule := testRule(folderUID, title, uid, uid+" title")
		rule.Provenance = models.Provenance(provenance)
		group.Rules = append(group.Rules, rule)
	}

	return group
}

// scopedRemoteGroup is scopedGroupOf for the probe group.
func scopedRemoteGroup(provenance string, uids ...string) *models.AlertRuleGroup {
	return scopedGroupOf(scopedFolder, scopedGroup, provenance, uids...)
}

// scopedRemoteRule is one rule as Grafana reports it in the probe group.
func scopedRemoteRule(uid, provenance string) *models.ProvisionedAlertRule {
	return scopedRemoteGroup(provenance, uid).Rules[0]
}

// scopedRoutes scripts everything a rule-scoped push of the given groups reads or writes: the rule
// lookup of each of their rules, the folder check, the group GET and the group PUT. Contact points
// and policies are deliberately not scripted - a request for them fails the test.
func scopedRoutes(t *testing.T, groups ...*models.AlertRuleGroup) map[string]stubResponse {
	t.Helper()

	routes := map[string]stubResponse{}

	for _, group := range groups {
		routes[pushRoute(http.MethodGet, folderPath(group.FolderUID))] = okResponse(`{"uid":"` + group.FolderUID + `"}`)
		routes[pushRoute(http.MethodGet, groupPath(group.FolderUID, group.Title))] = okResponse(marshalJSON(t, group))
		routes[pushRoute(http.MethodPut, groupPath(group.FolderUID, group.Title))] = okResponse(pushedGroupBody)

		for _, rule := range group.Rules {
			routes[pushRoute(http.MethodGet, rulePath(rule.UID))] = okResponse(marshalJSON(t, rule))
		}
	}

	return maps.Clone(routes)
}

// marshalJSON renders a model as the JSON a stubbed Grafana answers with.
func marshalJSON(t *testing.T, value any) string {
	t.Helper()

	encoded, err := json.Marshal(value)
	require.NoError(t, err)

	return string(encoded)
}

// assertNoWholeInstanceReads pins what a rule-scoped push never reads: the rule listing, the
// contact points and the policy tree.
func assertNoWholeInstanceReads(t *testing.T, fixture *pushFixture) {
	t.Helper()

	assert.Empty(t, fixture.requests(http.MethodGet, alertRulesPath), "no rule listing")
	assert.Empty(t, fixture.requests(http.MethodGet, contactPointsPath), "no contact points")
	assert.Empty(t, fixture.requests(http.MethodGet, policiesPath), "no policy tree")
}

// assertRuleEqualsRemote compares a rule of a written body with the rule Grafana reported, ignoring
// the three fields a body never carries.
func assertRuleEqualsRemote(t *testing.T, got, remote *models.ProvisionedAlertRule) {
	t.Helper()

	want := *remote
	want.ID = got.ID
	want.Provenance = got.Provenance
	want.Updated = got.Updated

	assert.JSONEq(t, marshalJSON(t, &want), marshalJSON(t, got))
}

// ruleUIDs lists a group's rule uids in order.
func ruleUIDs(group *models.AlertRuleGroup) []string {
	uids := make([]string, 0, len(group.Rules))

	for _, rule := range group.Rules {
		uids = append(uids, rule.UID)
	}

	return uids
}

// modelExpression reads the "expression" a test put into a rule's first query model.
func modelExpression(t *testing.T, rule *models.ProvisionedAlertRule) any {
	t.Helper()

	model, ok := rule.Data[0].Model.(map[string]any)
	require.True(t, ok, "the query model decodes as an object")

	return model["expression"]
}
