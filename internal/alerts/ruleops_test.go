package alerts_test

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/go-openapi/runtime"
	"github.com/grafana/grafanapi/internal/alerts"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestDeleteRules_DryRunWritesNothing pins the promise --dry-run makes on a destructive command:
// the report says what would go, and nothing does.
func TestDeleteRules_DryRunWritesNothing(t *testing.T) {
	fixture := newPushFixture(t, map[string]stubResponse{
		http.MethodGet + " " + rulePath("rule-uid-1"): okResponse(opRule("rule-uid-1", "probe-group", "api", false)),
	})

	results, err := alerts.DeleteRules(t.Context(), fixture.client, []string{"rule-uid-1"}, alerts.RuleOpOptions{DryRun: true})
	require.NoError(t, err)

	require.Len(t, results, 1)
	require.NoError(t, results[0].Err)
	assert.True(t, results[0].Changed, "a dry run still reports what it would delete")
	assert.Empty(t, fixture.writes(), "a dry run must not delete")
}

// TestDeleteRules_ForeignProvenanceIsARefusal is decision 3 on the delete path: a rule another
// provisioner owns would be re-created by that provisioner's next run, so deleting it here is
// refused rather than attempted.
func TestDeleteRules_ForeignProvenanceIsARefusal(t *testing.T) {
	fixture := newPushFixture(t, map[string]stubResponse{
		http.MethodGet + " " + rulePath("rule-uid-1"): okResponse(opRule("rule-uid-1", "probe-group", "file", false)),
	})

	results, err := alerts.DeleteRules(t.Context(), fixture.client, []string{"rule-uid-1"}, alerts.RuleOpOptions{})
	require.NoError(t, err)

	require.Len(t, results, 1)
	require.Error(t, results[0].Err)
	assert.Equal(t,
		`alerts: delete rule rule-uid-1: provenance "file" is managed outside the API`,
		results[0].Err.Error(),
	)
	assert.False(t, results[0].Changed)
	assert.Empty(t, fixture.writes(), "a refused rule must not be deleted at all")
}

// TestDeleteRules_Forbidden pins where a 403 lands: on the rule that was refused, with the
// *runtime.APIError still reachable for the permission-denied rendering.
func TestDeleteRules_Forbidden(t *testing.T) {
	fixture := newPushFixture(t, map[string]stubResponse{
		http.MethodGet + " " + rulePath("rule-uid-1"): okResponse(opRule("rule-uid-1", "probe-group", "api", false)),
		http.MethodDelete + " " + rulePath("rule-uid-1"): {
			status: http.StatusForbidden,
			body:   `{"message":"You'll need additional permissions"}`,
		},
	})

	results, err := alerts.DeleteRules(t.Context(), fixture.client, []string{"rule-uid-1"}, alerts.RuleOpOptions{})
	require.NoError(t, err)
	require.Len(t, results, 1)

	var apiErr *runtime.APIError
	require.ErrorAs(t, results[0].Err, &apiErr)
	assert.Equal(t, http.StatusForbidden, apiErr.Code)
}

// TestDeleteRules_HeaderMirrorsTheStoredProvenance is the delete half of decision 3. Sending
// X-Disable-Provenance for a rule Grafana has claimed is a 409, and omitting it is the only way to
// delete one - so the header is read off the rule rather than chosen.
func TestDeleteRules_HeaderMirrorsTheStoredProvenance(t *testing.T) {
	tests := []struct {
		name   string
		stored string
		want   *string
	}{
		{name: "an unclaimed rule is deleted with the header", stored: "", want: new("true")},
		{name: "a rule the API owns is deleted without it", stored: "api"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newPushFixture(t, map[string]stubResponse{
				http.MethodGet + " " + rulePath("rule-uid-1"):    okResponse(opRule("rule-uid-1", "probe-group", tc.stored, false)),
				http.MethodDelete + " " + rulePath("rule-uid-1"): {status: http.StatusNoContent},
			})

			results, err := alerts.DeleteRules(t.Context(), fixture.client, []string{"rule-uid-1"}, alerts.RuleOpOptions{})
			require.NoError(t, err)

			require.Len(t, results, 1)
			require.NoError(t, results[0].Err)
			assert.True(t, results[0].Changed)
			assert.Equal(t, "rule-uid-1-title", results[0].Title)
			assert.Equal(t, "probe-group", results[0].Group)

			deleted := fixture.requests(http.MethodDelete, rulePath("rule-uid-1"))
			require.Len(t, deleted, 1)
			assert.Equal(t, tc.want, deleted[0].provenance)
		})
	}
}

// TestDeleteRules_NilClientIsAnError guards the caller contract the way the read path does.
func TestDeleteRules_NilClientIsAnError(t *testing.T) {
	_, err := alerts.DeleteRules(t.Context(), nil, []string{"rule-uid-1"}, alerts.RuleOpOptions{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no grafana client configured")
}

// TestDeleteRules_UnauthorizedStopsTheRun is the exit-2 regression test for the delete path, from
// both requests it makes.
func TestDeleteRules_UnauthorizedStopsTheRun(t *testing.T) {
	unauthorized := stubResponse{status: http.StatusUnauthorized, body: `{"message":"Unauthorized"}`}

	tests := []struct {
		name   string
		routes map[string]stubResponse
	}{
		{
			name:   "from the lookup",
			routes: map[string]stubResponse{http.MethodGet + " " + rulePath("rule-uid-1"): unauthorized},
		},
		{
			name: "from the delete",
			routes: map[string]stubResponse{
				http.MethodGet + " " + rulePath("rule-uid-1"):    okResponse(opRule("rule-uid-1", "probe-group", "api", false)),
				http.MethodDelete + " " + rulePath("rule-uid-1"): unauthorized,
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newPushFixture(t, tc.routes)

			_, err := alerts.DeleteRules(t.Context(), fixture.client, []string{"rule-uid-1"}, alerts.RuleOpOptions{})
			require.Error(t, err)

			var apiErr *runtime.APIError
			require.ErrorAs(t, err, &apiErr)
			assert.Equal(t, http.StatusUnauthorized, apiErr.Code)
		})
	}
}

// TestDeleteRules_UnknownUIDIsNotAnOutage is why every rule is looked up first: Grafana answers a
// DELETE for a uid it does not know with a 500, which would otherwise be reported as a broken
// instance instead of a typo - and would say nothing about which uid was wrong.
func TestDeleteRules_UnknownUIDIsNotAnOutage(t *testing.T) {
	fixture := newPushFixture(t, map[string]stubResponse{
		http.MethodGet + " " + rulePath("rule-uid-gone"): notFoundResponse(),
		http.MethodGet + " " + rulePath("rule-uid-1"):    okResponse(opRule("rule-uid-1", "probe-group", "api", false)),
		http.MethodDelete + " " + rulePath("rule-uid-1"): {status: http.StatusNoContent},
	})

	results, err := alerts.DeleteRules(t.Context(), fixture.client, []string{"rule-uid-gone", "rule-uid-1"}, alerts.RuleOpOptions{})
	require.NoError(t, err)

	require.Len(t, results, 2)
	require.Error(t, results[0].Err)
	assert.Equal(t, "alerts: rule rule-uid-gone not found", results[0].Err.Error())

	require.NoError(t, results[1].Err)
	assert.True(t, results[1].Changed, "the rules that do exist are still deleted")
	assert.Empty(t, fixture.requests(http.MethodDelete, rulePath("rule-uid-gone")))
}

// TestSetRulesPaused_AlreadyInTheRequestedStateWritesNothing keeps pause idempotent. A no-op PUT
// would still be a full replacement of the group, which is not something a command that changes
// nothing should be doing.
func TestSetRulesPaused_AlreadyInTheRequestedStateWritesNothing(t *testing.T) {
	fixture := newPushFixture(t, map[string]stubResponse{
		http.MethodGet + " " + rulePath("rule-uid-1"): okResponse(opRule("rule-uid-1", "probe-group", "api", true)),
		http.MethodGet + " " + groupPath("folder-uid-1", "probe-group"): okResponse(
			opGroup("probe-group", opRule("rule-uid-1", "probe-group", "api", true)),
		),
	})

	results, err := alerts.SetRulesPaused(
		t.Context(), fixture.client, []string{"rule-uid-1"}, alerts.RuleOpOptions{Paused: true},
	)
	require.NoError(t, err)

	require.Len(t, results, 1)
	require.NoError(t, results[0].Err)
	assert.False(t, results[0].Changed, "a rule already paused is reported as unchanged")
	assert.Empty(t, fixture.writes(), "a group with nothing to flip must not be written")
}

// TestSetRulesPaused_DryRunWritesNothing pins what --dry-run reports: the same per-rule verdict the
// real run would produce, with no request that could change anything.
func TestSetRulesPaused_DryRunWritesNothing(t *testing.T) {
	fixture := newPushFixture(t, map[string]stubResponse{
		http.MethodGet + " " + rulePath("rule-uid-1"): okResponse(opRule("rule-uid-1", "probe-group", "api", false)),
		http.MethodGet + " " + groupPath("folder-uid-1", "probe-group"): okResponse(
			opGroup("probe-group", opRule("rule-uid-1", "probe-group", "api", false)),
		),
	})

	results, err := alerts.SetRulesPaused(
		t.Context(), fixture.client, []string{"rule-uid-1"}, alerts.RuleOpOptions{Paused: true, DryRun: true},
	)
	require.NoError(t, err)

	require.Len(t, results, 1)
	require.NoError(t, results[0].Err)
	assert.True(t, results[0].Changed)
	assert.Empty(t, fixture.writes(), "a dry run must not write")
}

// TestSetRulesPaused_Forbidden pins where a 403 lands: on every uid the refused group write was
// made for, with the *runtime.APIError still reachable.
func TestSetRulesPaused_Forbidden(t *testing.T) {
	fixture := newPushFixture(t, map[string]stubResponse{
		http.MethodGet + " " + rulePath("rule-uid-1"): okResponse(opRule("rule-uid-1", "probe-group", "api", false)),
		http.MethodGet + " " + groupPath("folder-uid-1", "probe-group"): okResponse(
			opGroup("probe-group", opRule("rule-uid-1", "probe-group", "api", false)),
		),
		http.MethodPut + " " + groupPath("folder-uid-1", "probe-group"): {
			status: http.StatusForbidden,
			body:   `{"message":"You'll need additional permissions"}`,
		},
	})

	results, err := alerts.SetRulesPaused(
		t.Context(), fixture.client, []string{"rule-uid-1"}, alerts.RuleOpOptions{Paused: true},
	)
	require.NoError(t, err)
	require.Len(t, results, 1)

	var apiErr *runtime.APIError
	require.ErrorAs(t, results[0].Err, &apiErr)
	assert.Equal(t, http.StatusForbidden, apiErr.Code)
}

// TestSetRulesPaused_GroupsTheNamedRules is decision 7's core: the flip travels in a group PUT,
// several rules of one group are one write, naming a rule twice is one rule, and two groups are
// two writes.
func TestSetRulesPaused_GroupsTheNamedRules(t *testing.T) {
	fixture := newPushFixture(t, map[string]stubResponse{
		http.MethodGet + " " + rulePath("rule-uid-1"): okResponse(opRule("rule-uid-1", "probe-group", "api", false)),
		http.MethodGet + " " + rulePath("rule-uid-2"): okResponse(opRule("rule-uid-2", "probe-group", "api", false)),
		http.MethodGet + " " + rulePath("rule-uid-3"): okResponse(opRule("rule-uid-3", "other-group", "api", false)),
		http.MethodGet + " " + groupPath("folder-uid-1", "probe-group"): okResponse(opGroup(
			"probe-group",
			opRule("rule-uid-1", "probe-group", "api", false),
			opRule("rule-uid-2", "probe-group", "api", false),
		)),
		http.MethodGet + " " + groupPath("folder-uid-1", "other-group"): okResponse(
			opGroup("other-group", opRule("rule-uid-3", "other-group", "api", false)),
		),
		http.MethodPut + " " + groupPath("folder-uid-1", "probe-group"): okResponse(pushedGroupBody),
		http.MethodPut + " " + groupPath("folder-uid-1", "other-group"): okResponse(pushedGroupBody),
	})

	uids := []string{"rule-uid-1", "rule-uid-2", "rule-uid-3", "rule-uid-1"}

	results, err := alerts.SetRulesPaused(t.Context(), fixture.client, uids, alerts.RuleOpOptions{Paused: true})
	require.NoError(t, err)

	require.Len(t, results, 3, "naming a rule twice is one rule")

	for _, result := range results {
		require.NoError(t, result.Err)
		assert.True(t, result.Changed, result.UID)
	}

	assert.Equal(t, []string{"rule-uid-1", "rule-uid-2", "rule-uid-3"},
		[]string{results[0].UID, results[1].UID, results[2].UID},
		"results are reported in the order the user named the rules")

	assert.Len(t, fixture.requests(http.MethodGet, rulePath("rule-uid-1")), 1)
	assert.Len(t, fixture.requests(http.MethodGet, groupPath("folder-uid-1", "probe-group")), 1)

	written := fixture.requests(http.MethodPut, groupPath("folder-uid-1", "probe-group"))
	require.Len(t, written, 1, "two rules of one group are one write, not two replacements of it")

	body := decodeGroupBody(t, written[0].body)
	require.Len(t, body.Rules, 2)
	assert.True(t, body.Rules[0].IsPaused)
	assert.True(t, body.Rules[1].IsPaused)
	assert.Nil(t, written[0].provenance, "a group the API owns is written without the header")
	assert.NotContains(t, written[0].body, `"provenance"`)

	assert.Len(t, fixture.requests(http.MethodPut, groupPath("folder-uid-1", "other-group")), 1)
}

// TestSetRulesPaused_MixedProvenanceIsARefusal covers the group half of decision 3: the PUT
// carries one header for every rule in the group, so a group whose rules disagree cannot be
// written without changing the ownership of some of them.
func TestSetRulesPaused_MixedProvenanceIsARefusal(t *testing.T) {
	fixture := newPushFixture(t, map[string]stubResponse{
		http.MethodGet + " " + rulePath("rule-uid-1"): okResponse(opRule("rule-uid-1", "probe-group", "api", false)),
		http.MethodGet + " " + groupPath("folder-uid-1", "probe-group"): okResponse(opGroup(
			"probe-group",
			opRule("rule-uid-1", "probe-group", "api", false),
			opRule("rule-uid-2", "probe-group", "", false),
		)),
	})

	results, err := alerts.SetRulesPaused(
		t.Context(), fixture.client, []string{"rule-uid-1"}, alerts.RuleOpOptions{Paused: true},
	)
	require.NoError(t, err)

	require.Len(t, results, 1)
	require.Error(t, results[0].Err)
	assert.Equal(t,
		`alerts: pause rule group folder-uid-1/probe-group: rules carry mixed provenance ("api" and "")`,
		results[0].Err.Error(),
	)
	assert.Empty(t, fixture.writes(), "a group that cannot be claimed correctly must not be written")
}

// TestSetRulesPaused_NilClientIsAnError guards the caller contract the way the read path does.
func TestSetRulesPaused_NilClientIsAnError(t *testing.T) {
	_, err := alerts.SetRulesPaused(t.Context(), nil, []string{"rule-uid-1"}, alerts.RuleOpOptions{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no grafana client configured")
}

// TestSetRulesPaused_ResumeWritesTheWholeGroup is the resume direction, on an unclaimed group: the
// PUT carries every rule the group holds - not only the named one - and the header that keeps the
// UI's hands on all of them.
func TestSetRulesPaused_ResumeWritesTheWholeGroup(t *testing.T) {
	fixture := newPushFixture(t, map[string]stubResponse{
		http.MethodGet + " " + rulePath("rule-uid-1"): okResponse(opRule("rule-uid-1", "probe-group", "", true)),
		http.MethodGet + " " + groupPath("folder-uid-1", "probe-group"): okResponse(opGroup(
			"probe-group",
			opRule("rule-uid-1", "probe-group", "", true),
			opRule("rule-uid-2", "probe-group", "", true),
		)),
		http.MethodPut + " " + groupPath("folder-uid-1", "probe-group"): okResponse(pushedGroupBody),
	})

	results, err := alerts.SetRulesPaused(
		t.Context(), fixture.client, []string{"rule-uid-1"}, alerts.RuleOpOptions{},
	)
	require.NoError(t, err)

	require.Len(t, results, 1)
	require.NoError(t, results[0].Err)
	assert.True(t, results[0].Changed)

	written := fixture.requests(http.MethodPut, groupPath("folder-uid-1", "probe-group"))
	require.Len(t, written, 1)
	require.NotNil(t, written[0].provenance)
	assert.Equal(t, "true", *written[0].provenance)

	body := decodeGroupBody(t, written[0].body)
	require.Len(t, body.Rules, 2, "a group PUT replaces the group, so every rule it holds must be in the body")
	assert.False(t, body.Rules[0].IsPaused, "the named rule is resumed")
	assert.True(t, body.Rules[1].IsPaused, "a rule nobody named keeps its state")
}

// TestSetRulesPaused_UnauthorizedStopsTheRun is the exit-2 regression test for the pause path,
// from each of the three requests it makes.
func TestSetRulesPaused_UnauthorizedStopsTheRun(t *testing.T) {
	unauthorized := stubResponse{status: http.StatusUnauthorized, body: `{"message":"Unauthorized"}`}
	rule := okResponse(opRule("rule-uid-1", "probe-group", "api", false))
	group := okResponse(opGroup("probe-group", opRule("rule-uid-1", "probe-group", "api", false)))

	tests := []struct {
		name   string
		routes map[string]stubResponse
	}{
		{
			name:   "from the rule lookup",
			routes: map[string]stubResponse{http.MethodGet + " " + rulePath("rule-uid-1"): unauthorized},
		},
		{
			name: "from the group fetch",
			routes: map[string]stubResponse{
				http.MethodGet + " " + rulePath("rule-uid-1"):                   rule,
				http.MethodGet + " " + groupPath("folder-uid-1", "probe-group"): unauthorized,
			},
		},
		{
			name: "from the group write",
			routes: map[string]stubResponse{
				http.MethodGet + " " + rulePath("rule-uid-1"):                   rule,
				http.MethodGet + " " + groupPath("folder-uid-1", "probe-group"): group,
				http.MethodPut + " " + groupPath("folder-uid-1", "probe-group"): unauthorized,
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newPushFixture(t, tc.routes)

			_, err := alerts.SetRulesPaused(
				t.Context(), fixture.client, []string{"rule-uid-1"}, alerts.RuleOpOptions{Paused: true},
			)
			require.Error(t, err)

			var apiErr *runtime.APIError
			require.ErrorAs(t, err, &apiErr)
			assert.Equal(t, http.StatusUnauthorized, apiErr.Code)
		})
	}
}

// TestSetRulesPaused_UnknownUIDChangesNothing pins that the lookup phase runs to completion before
// any write: a typo among the uids is reported as such, the group it would have belonged to is
// never fetched, and the rules that do exist are still flipped.
func TestSetRulesPaused_UnknownUIDChangesNothing(t *testing.T) {
	fixture := newPushFixture(t, map[string]stubResponse{
		http.MethodGet + " " + rulePath("rule-uid-gone"): notFoundResponse(),
		http.MethodGet + " " + rulePath("rule-uid-1"):    okResponse(opRule("rule-uid-1", "probe-group", "api", false)),
		http.MethodGet + " " + groupPath("folder-uid-1", "probe-group"): okResponse(
			opGroup("probe-group", opRule("rule-uid-1", "probe-group", "api", false)),
		),
		http.MethodPut + " " + groupPath("folder-uid-1", "probe-group"): okResponse(pushedGroupBody),
	})

	results, err := alerts.SetRulesPaused(
		t.Context(), fixture.client, []string{"rule-uid-gone", "rule-uid-1"}, alerts.RuleOpOptions{Paused: true},
	)
	require.NoError(t, err)

	require.Len(t, results, 2)
	require.Error(t, results[0].Err)
	assert.Equal(t, "alerts: rule rule-uid-gone not found", results[0].Err.Error())
	assert.False(t, results[0].Changed)
	assert.Empty(t, results[0].Group, "a uid that could not be looked up names nothing")

	require.NoError(t, results[1].Err)
	assert.True(t, results[1].Changed)
	assert.Len(t, fixture.requests(http.MethodPut, groupPath("folder-uid-1", "probe-group")), 1)
}

// TestSetRulesPaused_GroupRacesAreReportedPerRule covers the two windows between the per-rule
// lookup and the group write. Nothing in this package can close them - Grafana has no optimistic
// concurrency here - so what is pinned is that they are reported against the uids that asked for
// them, rather than crashing or silently writing the wrong group.
func TestSetRulesPaused_GroupRacesAreReportedPerRule(t *testing.T) {
	t.Run("the group is gone by the time it is fetched", func(t *testing.T) {
		fixture := newPushFixture(t, map[string]stubResponse{
			http.MethodGet + " " + rulePath("rule-uid-1"):                   okResponse(opRule("rule-uid-1", "probe-group", "api", false)),
			http.MethodGet + " " + groupPath("folder-uid-1", "probe-group"): notFoundResponse(),
		})

		results, err := alerts.SetRulesPaused(
			t.Context(), fixture.client, []string{"rule-uid-1"}, alerts.RuleOpOptions{Paused: true},
		)
		require.NoError(t, err)

		require.Len(t, results, 1)
		require.Error(t, results[0].Err)
		assert.Equal(t, "alerts: pause rule group folder-uid-1/probe-group: not found", results[0].Err.Error())
		assert.False(t, results[0].Changed)
		assert.Empty(t, fixture.writes())
	})

	t.Run("the rule is no longer in the group that was fetched", func(t *testing.T) {
		fixture := newPushFixture(t, map[string]stubResponse{
			http.MethodGet + " " + rulePath("rule-uid-1"): okResponse(opRule("rule-uid-1", "probe-group", "api", false)),
			http.MethodGet + " " + rulePath("rule-uid-2"): okResponse(opRule("rule-uid-2", "probe-group", "api", false)),
			// The group no longer holds rule-uid-1: it was moved or deleted between the two reads.
			http.MethodGet + " " + groupPath("folder-uid-1", "probe-group"): okResponse(
				opGroup("probe-group", opRule("rule-uid-2", "probe-group", "api", false)),
			),
			http.MethodPut + " " + groupPath("folder-uid-1", "probe-group"): okResponse(pushedGroupBody),
		})

		results, err := alerts.SetRulesPaused(
			t.Context(), fixture.client, []string{"rule-uid-1", "rule-uid-2"}, alerts.RuleOpOptions{Paused: true},
		)
		require.NoError(t, err)

		require.Len(t, results, 2)
		require.Error(t, results[0].Err)
		assert.Equal(t,
			"alerts: pause rule rule-uid-1: rule group folder-uid-1/probe-group no longer holds it",
			results[0].Err.Error(),
		)

		require.NoError(t, results[1].Err, "the group's other named rule is still flipped")
		assert.True(t, results[1].Changed)
		assert.Len(t, fixture.requests(http.MethodPut, groupPath("folder-uid-1", "probe-group")), 1)
	})
}

// TestDeleteRules_TolerantOfAThinPayload covers the guards a rule operation needs against a server
// that answers with less than the model requires.
//
// Almost every field of models.ProvisionedAlertRule is a pointer - including the ones the API
// marks required - so a response omitting the title must leave a report naming the uid alone,
// never a panic. A 200 carrying no rule at all is read as absence, which is the only other thing
// there is to do with it.
func TestDeleteRules_TolerantOfAThinPayload(t *testing.T) {
	t.Run("a rule with no title or group", func(t *testing.T) {
		fixture := newPushFixture(t, map[string]stubResponse{
			http.MethodGet + " " + rulePath("rule-uid-1"):    okResponse(`{"uid":"rule-uid-1"}`),
			http.MethodDelete + " " + rulePath("rule-uid-1"): {status: http.StatusNoContent},
		})

		results, err := alerts.DeleteRules(t.Context(), fixture.client, []string{"rule-uid-1"}, alerts.RuleOpOptions{})
		require.NoError(t, err)

		require.Len(t, results, 1)
		require.NoError(t, results[0].Err)
		assert.Equal(t, "rule-uid-1", results[0].UID)
		assert.Empty(t, results[0].Title)
		assert.Empty(t, results[0].Group)
	})

	// There is deliberately no case for a 200 carrying no rule at all: the generated reader
	// allocates the payload before decoding into it, so resp.Payload is never nil through this
	// client. fetchRule's nil check stays as a guard against a nil dereference in code this
	// repository does not own, not as a branch a test can drive.

	t.Run("an empty uid is absence without a request", func(t *testing.T) {
		// GET /alert-rules/ is the *list* endpoint, which would answer 200 with every rule: an
		// empty uid must never reach it.
		fixture := newPushFixture(t, map[string]stubResponse{})

		results, err := alerts.DeleteRules(t.Context(), fixture.client, []string{""}, alerts.RuleOpOptions{})
		require.NoError(t, err)

		require.Len(t, results, 1)
		require.ErrorIs(t, results[0].Err, alerts.ErrRuleNotFound)
		assert.Empty(t, fixture.requests(http.MethodGet, alertRulesPath))
	})
}

// opGroup builds one rule-group payload holding the given rules.
func opGroup(title string, rules ...string) string {
	return fmt.Sprintf(
		`{"folderUid":"folder-uid-1","title":%q,"interval":60,"rules":[%s]}`,
		title, strings.Join(rules, ","),
	)
}

// opRule builds one provisioned rule carrying what a rule operation reads off it: the group it
// lives in, the provenance that decides the header, and the state a pause or resume flips.
func opRule(uid, group, provenance string, paused bool) string {
	return fmt.Sprintf(
		`{"uid":%q,"title":%q,"folderUID":"folder-uid-1","ruleGroup":%q,"orgID":1,"isPaused":%t,"provenance":%q}`,
		uid, uid+"-title", group, paused, provenance,
	)
}

// rulePath is the provisioning address of one alert rule, which both the lookup and the delete
// use.
func rulePath(uid string) string {
	return alertRulesPath + "/" + uid
}
