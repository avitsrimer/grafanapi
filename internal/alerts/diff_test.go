package alerts_test

import (
	"testing"
	"time"

	"github.com/go-openapi/strfmt"
	"github.com/grafana/grafana-openapi-client-go/models"
	"github.com/grafana/grafanapi/internal/alerts"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestPlan_EmptyLocalPlansNothing pins the no-prune contract: a bundle that does not mention an
// object says nothing about it, so a push from an empty directory is a no-op rather than a wipe.
func TestPlan_EmptyLocalPlansNothing(t *testing.T) {
	changes, err := alerts.Plan(alerts.Bundle{}, testBundle())
	require.NoError(t, err)
	assert.Empty(t, changes)

	changes, err = alerts.Plan(alerts.Bundle{}, alerts.Bundle{})
	require.NoError(t, err)
	assert.Empty(t, changes)
}

// TestPlan_RuleGroups is the rule-group table: which action each case produces and, for an
// update, exactly which lines describe it. The details are the part users act on - a "-" line is
// the only warning that a group PUT is about to delete a rule the file no longer lists.
func TestPlan_RuleGroups(t *testing.T) {
	tests := []struct {
		name   string
		local  *models.AlertRuleGroup
		remote *models.AlertRuleGroup
		want   alerts.Change
	}{
		{
			name:  "a group with no remote counterpart is created with every rule listed",
			local: testGroup("folder-uid-1", "probe-group", diffFileRule("rule-uid-1", "rule one")),
			want: alerts.Change{
				Action: alerts.ActionCreate,
				Kind:   alerts.KindRuleGroup,
				ID:     "folder-uid-1/probe-group",
				Detail: []string{"+ rule one (rule-uid-1)"},
			},
		},
		{
			name:   "a group equal to its remote counterpart is unchanged, read-only fields aside",
			local:  testGroup("folder-uid-1", "probe-group", diffFileRule("rule-uid-1", "rule one")),
			remote: testGroup("folder-uid-1", "probe-group", testRule("folder-uid-1", "probe-group", "rule-uid-1", "rule one")),
			want: alerts.Change{
				Action: alerts.ActionUnchanged,
				Kind:   alerts.KindRuleGroup,
				ID:     "folder-uid-1/probe-group",
			},
		},
		{
			name:   "a changed interval is reported with both values",
			local:  diffInterval(testGroup("folder-uid-1", "probe-group"), 120),
			remote: testGroup("folder-uid-1", "probe-group"),
			want: alerts.Change{
				Action: alerts.ActionUpdate,
				Kind:   alerts.KindRuleGroup,
				ID:     "folder-uid-1/probe-group",
				Detail: []string{"~ interval 60s→120s"},
			},
		},
		{
			name: "a rule the file adds is a + line",
			local: testGroup("folder-uid-1", "probe-group",
				diffFileRule("rule-uid-1", "rule one"),
				diffFileRule("rule-uid-2", "rule two"),
			),
			remote: testGroup("folder-uid-1", "probe-group", diffFileRule("rule-uid-1", "rule one")),
			want: alerts.Change{
				Action: alerts.ActionUpdate,
				Kind:   alerts.KindRuleGroup,
				ID:     "folder-uid-1/probe-group",
				Detail: []string{"+ rule two (rule-uid-2)"},
			},
		},
		{
			name: "a rule with no uid is new and is named by its title alone",
			local: testGroup("folder-uid-1", "probe-group",
				diffFileRule("rule-uid-1", "rule one"),
				diffFileRule("", "rule two"),
			),
			remote: testGroup("folder-uid-1", "probe-group", diffFileRule("rule-uid-1", "rule one")),
			want: alerts.Change{
				Action: alerts.ActionUpdate,
				Kind:   alerts.KindRuleGroup,
				ID:     "folder-uid-1/probe-group",
				Detail: []string{"+ rule two"},
			},
		},
		{
			name:  "a rule the file drops is a - line, because the group PUT replaces the group",
			local: testGroup("folder-uid-1", "probe-group", diffFileRule("rule-uid-1", "rule one")),
			remote: testGroup("folder-uid-1", "probe-group",
				diffFileRule("rule-uid-1", "rule one"),
				diffFileRule("rule-uid-2", "rule two"),
			),
			want: alerts.Change{
				Action: alerts.ActionUpdate,
				Kind:   alerts.KindRuleGroup,
				ID:     "folder-uid-1/probe-group",
				Detail: []string{"- rule two (rule-uid-2)"},
			},
		},
		{
			name:   "a rule changed in place is a ~ line",
			local:  testGroup("folder-uid-1", "probe-group", diffPausedRule(false)),
			remote: testGroup("folder-uid-1", "probe-group", diffPausedRule(true)),
			want: alerts.Change{
				Action: alerts.ActionUpdate,
				Kind:   alerts.KindRuleGroup,
				ID:     "folder-uid-1/probe-group",
				Detail: []string{"~ rule one (rule-uid-1)"},
			},
		},
		{
			name: "re-ordering the rules is a change, because Grafana evaluates them in body order",
			local: testGroup("folder-uid-1", "probe-group",
				diffFileRule("rule-uid-2", "rule two"),
				diffFileRule("rule-uid-1", "rule one"),
			),
			remote: testGroup("folder-uid-1", "probe-group",
				diffFileRule("rule-uid-1", "rule one"),
				diffFileRule("rule-uid-2", "rule two"),
			),
			want: alerts.Change{
				Action: alerts.ActionUpdate,
				Kind:   alerts.KindRuleGroup,
				ID:     "folder-uid-1/probe-group",
				Detail: []string{"~ rule order"},
			},
		},
		{
			name: "a re-order and an added rule are both reported",
			local: testGroup("folder-uid-1", "probe-group",
				diffFileRule("rule-uid-2", "rule two"),
				diffFileRule("", "rule three"),
				diffFileRule("rule-uid-1", "rule one"),
			),
			remote: testGroup("folder-uid-1", "probe-group",
				diffFileRule("rule-uid-1", "rule one"),
				diffFileRule("rule-uid-2", "rule two"),
			),
			want: alerts.Change{
				Action: alerts.ActionUpdate,
				Kind:   alerts.KindRuleGroup,
				ID:     "folder-uid-1/probe-group",
				Detail: []string{"~ rule order", "+ rule three"},
			},
		},
		{
			name: "inserting a rule between two others is not a re-order of the rules they share",
			local: testGroup("folder-uid-1", "probe-group",
				diffFileRule("rule-uid-1", "rule one"),
				diffFileRule("", "rule three"),
				diffFileRule("rule-uid-2", "rule two"),
			),
			remote: testGroup("folder-uid-1", "probe-group",
				diffFileRule("rule-uid-1", "rule one"),
				diffFileRule("rule-uid-2", "rule two"),
			),
			want: alerts.Change{
				Action: alerts.ActionUpdate,
				Kind:   alerts.KindRuleGroup,
				ID:     "folder-uid-1/probe-group",
				Detail: []string{"+ rule three"},
			},
		},
		{
			name:   "a rule leaving out the fields push fills in is unchanged, not a permanent diff",
			local:  testGroup("folder-uid-1", "probe-group", diffMinimalRule("rule-uid-1", "rule one")),
			remote: testGroup("folder-uid-1", "probe-group", testRule("folder-uid-1", "probe-group", "rule-uid-1", "rule one")),
			want: alerts.Change{
				Action: alerts.ActionUnchanged,
				Kind:   alerts.KindRuleGroup,
				ID:     "folder-uid-1/probe-group",
			},
		},
		{
			name:   "leaving those fields out hides nothing else: another field still reports",
			local:  testGroup("folder-uid-1", "probe-group", diffPendingRule(10*time.Minute)),
			remote: testGroup("folder-uid-1", "probe-group", testRule("folder-uid-1", "probe-group", "rule-uid-1", "rule one")),
			want: alerts.Change{
				Action: alerts.ActionUpdate,
				Kind:   alerts.KindRuleGroup,
				ID:     "folder-uid-1/probe-group",
				Detail: []string{"~ rule one (rule-uid-1)"},
			},
		},
		{
			name:   "a rule claiming another folder and group is unchanged, because push overwrites both",
			local:  testGroup("folder-uid-1", "probe-group", diffMovedRule()),
			remote: testGroup("folder-uid-1", "probe-group", diffFileRule("rule-uid-1", "rule one")),
			want: alerts.Change{
				Action: alerts.ActionUnchanged,
				Kind:   alerts.KindRuleGroup,
				ID:     "folder-uid-1/probe-group",
			},
		},
		{
			name:   "a rule claiming another organization is unchanged, because push resolves orgID",
			local:  testGroup("folder-uid-1", "probe-group", diffOrgRule(2)),
			remote: testGroup("folder-uid-1", "probe-group", diffFileRule("rule-uid-1", "rule one")),
			want: alerts.Change{
				Action: alerts.ActionUnchanged,
				Kind:   alerts.KindRuleGroup,
				ID:     "folder-uid-1/probe-group",
			},
		},
		{
			name: "every kind of line at once, interval first and the rules sorted",
			local: diffInterval(testGroup("folder-uid-1", "probe-group",
				diffPausedRule(false),
				diffFileRule("rule-uid-3", "rule three"),
			), 120),
			remote: testGroup("folder-uid-1", "probe-group",
				diffPausedRule(true),
				diffFileRule("rule-uid-2", "rule two"),
			),
			want: alerts.Change{
				Action: alerts.ActionUpdate,
				Kind:   alerts.KindRuleGroup,
				ID:     "folder-uid-1/probe-group",
				Detail: []string{
					"~ interval 60s→120s",
					"+ rule three (rule-uid-3)",
					"- rule two (rule-uid-2)",
					"~ rule one (rule-uid-1)",
				},
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			local := alerts.Bundle{Groups: []*models.AlertRuleGroup{test.local}}
			remote := alerts.Bundle{}

			if test.remote != nil {
				remote.Groups = []*models.AlertRuleGroup{test.remote}
			}

			changes, err := alerts.Plan(local, remote)
			require.NoError(t, err)
			require.Len(t, changes, 1)
			assert.Equal(t, test.want, changes[0])
		})
	}
}

// TestPlan_RuleGroupNormalizesEncodings covers the reason both sides go through the bundle's own
// encoding before they are compared: a file and a freshly fetched model spell the same value
// differently, and every one of these differences would otherwise show as an update that no push
// could ever close.
// TestPlan_UidLessRuleReplacingItsRemoteTwin pins the guard on the one plan that reads wrong
// without it: pushing the same file twice after it introduced a rule.
//
// The first push makes Grafana assign a UID the file still does not carry, so the second plan
// matches nothing by uid and reports one rule as both a `+` and a `-`. The write really does
// replace it - the rule loses its UID, its alert state and its silence history - so the plan says
// so on a line of its own. It stays a guard, not a match: matching by title would silently update
// the wrong rule the moment someone renames one.
func TestPlan_UidLessRuleReplacingItsRemoteTwin(t *testing.T) {
	tests := []struct {
		name   string
		local  *models.AlertRuleGroup
		remote *models.AlertRuleGroup
		want   []string
	}{
		{
			name:   "the warning heads the +/- pair it explains",
			local:  testGroup("folder-uid-1", "probe-group", diffFileRule("", "rule one")),
			remote: testGroup("folder-uid-1", "probe-group", testRule("folder-uid-1", "probe-group", "rule-uid-1", "rule one")),
			want: []string{
				"! rule one (rule-uid-1) would be replaced, not updated; run alerts pull first",
				"+ rule one",
				"- rule one (rule-uid-1)",
			},
		},
		{
			name: "a remote rule another local rule claims by uid is not at risk",
			local: testGroup("folder-uid-1", "probe-group",
				diffFileRule("rule-uid-1", "rule one"),
				diffFileRule("", "rule one"),
			),
			remote: testGroup("folder-uid-1", "probe-group", testRule("folder-uid-1", "probe-group", "rule-uid-1", "rule one")),
			want:   []string{"+ rule one"},
		},
		{
			name:   "a uid-less rule with no remote twin is a plain addition",
			local:  testGroup("folder-uid-1", "probe-group", diffFileRule("", "rule two")),
			remote: testGroup("folder-uid-1", "probe-group", testRule("folder-uid-1", "probe-group", "rule-uid-1", "rule one")),
			want:   []string{"+ rule two", "- rule one (rule-uid-1)"},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			changes, err := alerts.Plan(
				alerts.Bundle{Groups: []*models.AlertRuleGroup{test.local}},
				alerts.Bundle{Groups: []*models.AlertRuleGroup{test.remote}},
			)
			require.NoError(t, err)
			require.Len(t, changes, 1)
			assert.Equal(t, alerts.ActionUpdate, changes[0].Action)
			assert.Equal(t, test.want, changes[0].Detail)
		})
	}
}

func TestPlan_RuleGroupNormalizesEncodings(t *testing.T) {
	tests := []struct {
		name   string
		local  *models.AlertRuleGroup
		remote *models.AlertRuleGroup
	}{
		{
			name:   "a nil rule list and an empty one",
			local:  testGroup("folder-uid-1", "probe-group"),
			remote: diffEmptyRules(testGroup("folder-uid-1", "probe-group")),
		},
		{
			name:   "a float and an integer holding the same number",
			local:  testGroup("folder-uid-1", "probe-group", diffModelRule(float64(43200))),
			remote: testGroup("folder-uid-1", "probe-group", diffModelRule(int64(43200))),
		},
		{
			name:   "a nil annotation map and an empty one",
			local:  testGroup("folder-uid-1", "probe-group", diffAnnotatedRule(nil)),
			remote: testGroup("folder-uid-1", "probe-group", diffAnnotatedRule(map[string]string{})),
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			changes, err := alerts.Plan(
				alerts.Bundle{Groups: []*models.AlertRuleGroup{test.local}},
				alerts.Bundle{Groups: []*models.AlertRuleGroup{test.remote}},
			)
			require.NoError(t, err)
			require.Len(t, changes, 1)
			assert.Equal(t, alerts.ActionUnchanged, changes[0].Action, "detail: %v", changes[0].Detail)
		})
	}
}

// TestPlan_ContactPoints is the contact-point table. The redaction cases are the load-bearing
// ones: a pulled bundle never holds a secret, so if "[REDACTED]" did not compare as unchanged then
// every pull -> push round trip would report - and rewrite - every contact point that has one.
func TestPlan_ContactPoints(t *testing.T) {
	tests := []struct {
		name   string
		local  *models.EmbeddedContactPoint
		remote []*models.EmbeddedContactPoint
		want   alerts.Change
	}{
		{
			name:  "a uid with no remote counterpart is created",
			local: diffPoint("cp-uid-1", "probe-slack", map[string]any{"recipient": "#probe"}),
			want:  alerts.Change{Action: alerts.ActionCreate, Kind: alerts.KindContactPoint, ID: "cp-uid-1"},
		},
		{
			name:  "a name with no remote counterpart is created under the name",
			local: diffPoint("", "probe-slack", map[string]any{"recipient": "#probe"}),
			want:  alerts.Change{Action: alerts.ActionCreate, Kind: alerts.KindContactPoint, ID: "probe-slack"},
		},
		{
			name:  "an equal point matched by uid is unchanged",
			local: diffPoint("cp-uid-1", "probe-slack", map[string]any{"recipient": "#probe"}),
			remote: []*models.EmbeddedContactPoint{
				diffPoint("cp-uid-1", "probe-slack", map[string]any{"recipient": "#probe"}),
			},
			want: alerts.Change{Action: alerts.ActionUnchanged, Kind: alerts.KindContactPoint, ID: "cp-uid-1"},
		},
		{
			name:  "a uid match wins over a same-named point",
			local: diffPoint("cp-uid-1", "probe-slack", map[string]any{"recipient": "#probe"}),
			remote: []*models.EmbeddedContactPoint{
				diffPoint("cp-uid-2", "probe-slack", map[string]any{"recipient": "#other"}),
				diffPoint("cp-uid-1", "probe-slack", map[string]any{"recipient": "#probe"}),
			},
			want: alerts.Change{Action: alerts.ActionUnchanged, Kind: alerts.KindContactPoint, ID: "cp-uid-1"},
		},
		{
			name:  "a uid-less point is matched by its unique name",
			local: diffPoint("", "probe-slack", map[string]any{"recipient": "#changed"}),
			remote: []*models.EmbeddedContactPoint{
				diffPoint("cp-uid-1", "probe-slack", map[string]any{"recipient": "#probe"}),
			},
			want: alerts.Change{Action: alerts.ActionUpdate, Kind: alerts.KindContactPoint, ID: "probe-slack"},
		},
		{
			// The comparison is against the body the push would send - the local point under the
			// remote's uid - not against the file as written. Comparing the file directly finds one
			// key fewer than the remote object carries, which would plan an Update for an identical
			// contact point and re-PUT it on every push.
			name:  "an equal uid-less point matched by name is unchanged",
			local: diffPoint("", "probe-slack", map[string]any{"recipient": "#probe"}),
			remote: []*models.EmbeddedContactPoint{
				diffPoint("cp-uid-1", "probe-slack", map[string]any{"recipient": "#probe"}),
			},
			want: alerts.Change{Action: alerts.ActionUnchanged, Kind: alerts.KindContactPoint, ID: "probe-slack"},
		},
		{
			name:  "an equal uid-less point whose secret is still redacted is unchanged",
			local: diffPoint("", "probe-slack", map[string]any{"recipient": "#probe", "url": "[REDACTED]"}),
			remote: []*models.EmbeddedContactPoint{
				diffPoint("cp-uid-1", "probe-slack", map[string]any{"recipient": "#probe", "url": "https://hooks.example/abc"}),
			},
			want: alerts.Change{Action: alerts.ActionUnchanged, Kind: alerts.KindContactPoint, ID: "probe-slack"},
		},
		{
			name:  "a changed setting is an update",
			local: diffPoint("cp-uid-1", "probe-slack", map[string]any{"recipient": "#changed"}),
			remote: []*models.EmbeddedContactPoint{
				diffPoint("cp-uid-1", "probe-slack", map[string]any{"recipient": "#probe"}),
			},
			want: alerts.Change{Action: alerts.ActionUpdate, Kind: alerts.KindContactPoint, ID: "cp-uid-1"},
		},
		{
			name:  "a redacted top-level setting compares as unchanged",
			local: diffPoint("cp-uid-1", "probe-slack", map[string]any{"recipient": "#probe", "url": "[REDACTED]"}),
			remote: []*models.EmbeddedContactPoint{
				diffPoint("cp-uid-1", "probe-slack", map[string]any{"recipient": "#probe", "url": "https://hooks.example/abc"}),
			},
			want: alerts.Change{Action: alerts.ActionUnchanged, Kind: alerts.KindContactPoint, ID: "cp-uid-1"},
		},
		{
			name: "a redacted nested setting compares as unchanged",
			local: diffPoint("cp-uid-1", "probe-webhook", map[string]any{
				"http_config": map[string]any{"basic_auth": map[string]any{"user": "probe", "password": "[REDACTED]"}},
			}),
			remote: []*models.EmbeddedContactPoint{
				diffPoint("cp-uid-1", "probe-webhook", map[string]any{
					"http_config": map[string]any{"basic_auth": map[string]any{"user": "probe", "password": "s3cret"}},
				}),
			},
			want: alerts.Change{Action: alerts.ActionUnchanged, Kind: alerts.KindContactPoint, ID: "cp-uid-1"},
		},
		{
			name: "a redaction marker does not hide a sibling change",
			local: diffPoint("cp-uid-1", "probe-webhook", map[string]any{
				"http_config": map[string]any{"basic_auth": map[string]any{"user": "changed", "password": "[REDACTED]"}},
			}),
			remote: []*models.EmbeddedContactPoint{
				diffPoint("cp-uid-1", "probe-webhook", map[string]any{
					"http_config": map[string]any{"basic_auth": map[string]any{"user": "probe", "password": "s3cret"}},
				}),
			},
			want: alerts.Change{Action: alerts.ActionUpdate, Kind: alerts.KindContactPoint, ID: "cp-uid-1"},
		},
		{
			name:  "provenance is not a difference",
			local: diffPoint("cp-uid-1", "probe-slack", map[string]any{"recipient": "#probe"}),
			remote: []*models.EmbeddedContactPoint{
				diffProvenance(diffPoint("cp-uid-1", "probe-slack", map[string]any{"recipient": "#probe"}), "api"),
			},
			want: alerts.Change{Action: alerts.ActionUnchanged, Kind: alerts.KindContactPoint, ID: "cp-uid-1"},
		},
		// The four cases below walk the array arm of the redaction comparison. A webhook's custom
		// headers are the real shape: an array of objects, one of whose values Grafana redacts.
		// Without the arm a contact point with any array in its settings would be rewritten on
		// every push, or - worse - an edit next to the marker would be masked.
		{
			name: "a redacted setting inside an array compares as unchanged",
			local: diffPoint("cp-uid-1", "probe-webhook", map[string]any{
				"headers": []any{map[string]any{"name": "Authorization", "value": "[REDACTED]"}},
			}),
			remote: []*models.EmbeddedContactPoint{
				diffPoint("cp-uid-1", "probe-webhook", map[string]any{
					"headers": []any{map[string]any{"name": "Authorization", "value": "Bearer s3cret"}},
				}),
			},
			want: alerts.Change{Action: alerts.ActionUnchanged, Kind: alerts.KindContactPoint, ID: "cp-uid-1"},
		},
		{
			name: "a change beside a redacted array element is an update",
			local: diffPoint("cp-uid-1", "probe-webhook", map[string]any{
				"headers": []any{map[string]any{"name": "X-Probe", "value": "[REDACTED]"}},
			}),
			remote: []*models.EmbeddedContactPoint{
				diffPoint("cp-uid-1", "probe-webhook", map[string]any{
					"headers": []any{map[string]any{"name": "Authorization", "value": "Bearer s3cret"}},
				}),
			},
			want: alerts.Change{Action: alerts.ActionUpdate, Kind: alerts.KindContactPoint, ID: "cp-uid-1"},
		},
		{
			name: "an array of a different length is an update",
			local: diffPoint("cp-uid-1", "probe-webhook", map[string]any{
				"headers": []any{map[string]any{"value": "[REDACTED]"}, map[string]any{"value": "[REDACTED]"}},
			}),
			remote: []*models.EmbeddedContactPoint{
				diffPoint("cp-uid-1", "probe-webhook", map[string]any{
					"headers": []any{map[string]any{"value": "Bearer s3cret"}},
				}),
			},
			want: alerts.Change{Action: alerts.ActionUpdate, Kind: alerts.KindContactPoint, ID: "cp-uid-1"},
		},
		{
			name: "an array against a scalar is an update",
			local: diffPoint("cp-uid-1", "probe-webhook", map[string]any{
				"headers": []any{map[string]any{"value": "[REDACTED]"}},
			}),
			remote: []*models.EmbeddedContactPoint{
				diffPoint("cp-uid-1", "probe-webhook", map[string]any{"headers": "Bearer s3cret"}),
			},
			want: alerts.Change{Action: alerts.ActionUpdate, Kind: alerts.KindContactPoint, ID: "cp-uid-1"},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			changes, err := alerts.Plan(
				alerts.Bundle{ContactPoints: []*models.EmbeddedContactPoint{test.local}},
				alerts.Bundle{ContactPoints: test.remote},
			)
			require.NoError(t, err)
			require.Len(t, changes, 1)
			assert.Equal(t, test.want, changes[0])
		})
	}
}

// TestPlan_ContactPointAmbiguousName covers the one case a plan cannot decide: Grafana allows
// several contact points to share a name, so a uid-less entry matching more than one names no
// single object and pushing it would overwrite an arbitrary one of them.
func TestPlan_ContactPointAmbiguousName(t *testing.T) {
	local := alerts.Bundle{ContactPoints: []*models.EmbeddedContactPoint{
		diffPoint("", "probe-slack", map[string]any{"recipient": "#probe"}),
	}}
	remote := alerts.Bundle{ContactPoints: []*models.EmbeddedContactPoint{
		diffPoint("cp-uid-1", "probe-slack", map[string]any{"recipient": "#probe"}),
		diffPoint("cp-uid-2", "probe-slack", map[string]any{"recipient": "#other"}),
	}}

	_, err := alerts.Plan(local, remote)
	require.Error(t, err)
	assert.Contains(t, err.Error(), `"probe-slack"`)
	assert.Contains(t, err.Error(), "set its uid")
}

// TestPlan_ContactPointsAddressingOneRemote pins the uniqueness ReadBundle cannot check. Two local
// entries with different plan IDs - one carrying the uid, one matched by name alone - resolve to
// the same remote contact point, and applying both would PUT that uid twice with no ordering
// between them, so the last body silently wins.
func TestPlan_ContactPointsAddressingOneRemote(t *testing.T) {
	local := alerts.Bundle{ContactPoints: []*models.EmbeddedContactPoint{
		diffPoint("cp-uid-1", "probe-slack", map[string]any{"recipient": "#probe"}),
		diffPoint("", "probe-slack", map[string]any{"recipient": "#other"}),
	}}
	remote := alerts.Bundle{ContactPoints: []*models.EmbeddedContactPoint{
		diffPoint("cp-uid-1", "probe-slack", map[string]any{"recipient": "#probe"}),
	}}

	_, err := alerts.Plan(local, remote)
	require.Error(t, err)
	assert.Contains(t, err.Error(), `"cp-uid-1"`, "the error names both local entries")
	assert.Contains(t, err.Error(), `"probe-slack"`)
	assert.Contains(t, err.Error(), "keep one")
}

// TestPlan_PolicyTree covers the single-object kind, including the recursion that matters: Grafana
// stamps provenance on every node of the tree, so ignoring it at the root alone would report a
// change on every pull -> push round trip.
func TestPlan_PolicyTree(t *testing.T) {
	tests := []struct {
		name   string
		local  *models.Route
		remote *models.Route
		want   []alerts.Change
	}{
		{
			name:   "no local tree plans nothing",
			remote: &models.Route{Receiver: "grafana-default-email"},
			want:   []alerts.Change{},
		},
		{
			name:  "no remote tree is a create",
			local: &models.Route{Receiver: "grafana-default-email"},
			want:  []alerts.Change{{Action: alerts.ActionCreate, Kind: alerts.KindPolicyTree, ID: "policies"}},
		},
		{
			name: "nested provenance is ignored",
			local: &models.Route{
				Receiver: "grafana-default-email",
				Routes:   []*models.Route{{Receiver: "probe-slack"}},
			},
			remote: &models.Route{
				Receiver:   "grafana-default-email",
				Provenance: "api",
				Routes:     []*models.Route{{Receiver: "probe-slack", Provenance: "api"}},
			},
			want: []alerts.Change{{Action: alerts.ActionUnchanged, Kind: alerts.KindPolicyTree, ID: "policies"}},
		},
		{
			name: "an empty list and an absent one are the same tree",
			local: &models.Route{
				Receiver:            "grafana-default-email",
				GroupBy:             []string{},
				ActiveTimeIntervals: []string{},
				Routes:              []*models.Route{},
			},
			remote: &models.Route{Receiver: "grafana-default-email", Provenance: "api"},
			want:   []alerts.Change{{Action: alerts.ActionUnchanged, Kind: alerts.KindPolicyTree, ID: "policies"}},
		},
		{
			name: "a changed child route is an update",
			local: &models.Route{
				Receiver: "grafana-default-email",
				Routes:   []*models.Route{{Receiver: "probe-pagerduty"}},
			},
			remote: &models.Route{
				Receiver:   "grafana-default-email",
				Provenance: "api",
				Routes:     []*models.Route{{Receiver: "probe-slack", Provenance: "api"}},
			},
			want: []alerts.Change{{Action: alerts.ActionUpdate, Kind: alerts.KindPolicyTree, ID: "policies"}},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			changes, err := alerts.Plan(
				alerts.Bundle{Policies: test.local},
				alerts.Bundle{Policies: test.remote},
			)
			require.NoError(t, err)
			assert.Equal(t, test.want, changes)
		})
	}
}

// TestPlan_SortedAndDeterministic pins the order push applies a plan in - contact points, then the
// policy tree, then rule groups - and that the order comes from the objects rather than from the
// order the bundle happened to list them in. A dry run that does not match the apply that follows
// is worse than no dry run at all.
func TestPlan_SortedAndDeterministic(t *testing.T) {
	local := alerts.Bundle{
		Groups: []*models.AlertRuleGroup{
			testGroup("folder-uid-2", "probe-group"),
			testGroup("folder-uid-1", "second-group"),
			testGroup("folder-uid-1", "first-group"),
		},
		ContactPoints: []*models.EmbeddedContactPoint{
			diffPoint("cp-uid-2", "second", map[string]any{"recipient": "#b"}),
			diffPoint("cp-uid-1", "first", map[string]any{"recipient": "#a"}),
		},
		Policies: &models.Route{Receiver: "grafana-default-email"},
	}

	changes, err := alerts.Plan(local, alerts.Bundle{})
	require.NoError(t, err)

	ordered := make([]string, 0, len(changes))
	for _, change := range changes {
		ordered = append(ordered, string(change.Kind)+" "+change.ID)
	}

	assert.Equal(t, []string{
		"ContactPoint cp-uid-1",
		"ContactPoint cp-uid-2",
		"PolicyTree policies",
		"RuleGroup folder-uid-1/first-group",
		"RuleGroup folder-uid-1/second-group",
		"RuleGroup folder-uid-2/probe-group",
	}, ordered)

	again, err := alerts.Plan(local, alerts.Bundle{})
	require.NoError(t, err)
	assert.Equal(t, changes, again)
}

// TestPlan_NilEntriesSkipped guards the payload shape: every bundle slice holds pointers, so a
// `null` element decoded from a hand-written file must be skipped rather than panic a plan.
func TestPlan_NilEntriesSkipped(t *testing.T) {
	local := alerts.Bundle{
		Groups:        []*models.AlertRuleGroup{nil},
		ContactPoints: []*models.EmbeddedContactPoint{nil},
	}

	changes, err := alerts.Plan(local, alerts.Bundle{})
	require.NoError(t, err)
	assert.Empty(t, changes)
}

// diffAnnotatedRule builds a rule carrying the given annotation map, which is how the nil-versus-
// empty normalization is exercised on a map rather than on a slice.
func diffAnnotatedRule(annotations map[string]string) *models.ProvisionedAlertRule {
	rule := diffFileRule("rule-uid-1", "rule one")
	rule.Annotations = annotations

	return rule
}

// diffEmptyRules gives a group an empty - rather than nil - rule slice, which is what Grafana
// returns for a group whose rules were all removed.
func diffEmptyRules(group *models.AlertRuleGroup) *models.AlertRuleGroup {
	group.Rules = []*models.ProvisionedAlertRule{}

	return group
}

// diffFileRule builds a rule as it comes back from a bundle file: everything a write needs, none
// of the three fields Grafana owns and strips.
func diffFileRule(uid, title string) *models.ProvisionedAlertRule {
	return diffStripped(testRule("folder-uid-1", "probe-group", uid, title))
}

// diffInterval sets a group's evaluation interval, in seconds.
func diffInterval(group *models.AlertRuleGroup, interval int64) *models.AlertRuleGroup {
	group.Interval = interval

	return group
}

// diffMinimalRule builds the rule a hand-written file is allowed to carry: identified by uid and
// title, with the three fields push fills in from the group it is pushing - folderUID, ruleGroup
// and orgID - left out entirely.
func diffMinimalRule(uid, title string) *models.ProvisionedAlertRule {
	rule := diffFileRule(uid, title)
	rule.FolderUID = nil
	rule.RuleGroup = nil
	rule.OrgID = nil

	return rule
}

// diffModelRule builds a rule whose query model carries one number, so the two ways JSON can
// spell the same number can be compared.
func diffModelRule(maxDataPoints any) *models.ProvisionedAlertRule {
	rule := diffFileRule("rule-uid-1", "rule one")
	rule.Data[0].Model = map[string]any{"maxDataPoints": maxDataPoints}

	return rule
}

// diffMovedRule builds the same rule claiming another folder and group, both of which push
// overwrites from the group it is pushing.
func diffMovedRule() *models.ProvisionedAlertRule {
	return diffStripped(testRule("folder-uid-2", "other-group", "rule-uid-1", "rule one"))
}

// diffOrgRule builds the same rule claiming another organization, which push overwrites with the
// one the remote group is in.
func diffOrgRule(orgID int64) *models.ProvisionedAlertRule {
	rule := diffFileRule("rule-uid-1", "rule one")
	rule.OrgID = &orgID

	return rule
}

// diffPausedRule builds a rule differing from its twin in one write-relevant field only.
func diffPausedRule(paused bool) *models.ProvisionedAlertRule {
	rule := diffFileRule("rule-uid-1", "rule one")
	rule.IsPaused = paused

	return rule
}

// diffPendingRule builds a minimal rule whose pending period differs from its twin's: the field
// that has to keep being compared once the ones push pins are projected away.
func diffPendingRule(pending time.Duration) *models.ProvisionedAlertRule {
	rule := diffMinimalRule("rule-uid-1", "rule one")
	period := strfmt.Duration(pending)
	rule.For = &period

	return rule
}

// diffPoint builds a contact point with the settings the caller wants to compare.
func diffPoint(uid, name string, settings map[string]any) *models.EmbeddedContactPoint {
	pointType := models.EmbeddedContactPointTypeSlack

	return &models.EmbeddedContactPoint{
		UID:      uid,
		Name:     name,
		Type:     &pointType,
		Settings: settings,
	}
}

// diffProvenance stamps a contact point the way Grafana stamps one it has claimed.
func diffProvenance(point *models.EmbeddedContactPoint, provenance string) *models.EmbeddedContactPoint {
	point.Provenance = provenance

	return point
}

// diffStripped clears the three fields Grafana owns, which is what a rule read back from a bundle
// file no longer carries.
func diffStripped(rule *models.ProvisionedAlertRule) *models.ProvisionedAlertRule {
	rule.ID = 0
	rule.Provenance = ""
	rule.Updated = strfmt.DateTime{}

	return rule
}
