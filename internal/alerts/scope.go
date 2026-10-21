package alerts

import (
	"context"
	"fmt"
	"slices"

	"github.com/grafana/grafana-app-sdk/logging"
	goapi "github.com/grafana/grafana-openapi-client-go/client"
	"github.com/grafana/grafana-openapi-client-go/models"
)

// scopeFunc builds the (synthetic local, remote) pair a rule-scoped push plans. Push always passes
// scopeBundles; the parameter exists so a test can hand push doctored output and prove the plan
// check, not the builder, is what keeps a wrong group body from reaching a write.
type scopeFunc func(local, remote Bundle, uids []string) (Bundle, Bundle, error)

// scopedRule is one named rule as the bundle holds it, with the group file it came from.
type scopedRule struct {
	group *models.AlertRuleGroup
	rule  *models.ProvisionedAlertRule
}

// locateLocalRules finds the group file holding each named rule. It is pure and runs before any
// request: a uid the bundle does not hold is a mistake of the caller, not something Grafana can
// answer.
//
// The error is a plain one rather than ErrRuleNotFound, whose rendering suggests `alerts rules
// list` - the wrong advice for a rule that exists in Grafana and is merely not in the bundle.
func locateLocalRules(local Bundle, uids []string) (map[string]scopedRule, error) {
	located := make(map[string]scopedRule, len(uids))

	for _, uid := range uids {
		for _, group := range local.Groups {
			if group == nil {
				continue
			}

			if rule := findRule(group.Rules, uid); rule != nil {
				located[uid] = scopedRule{group: group, rule: rule}

				break
			}
		}

		if _, found := located[uid]; !found {
			return nil, fmt.Errorf(
				"alerts: push rule %s: not in the bundle; run alerts pull --rule-uid %s first", uid, uid,
			)
		}
	}

	return located, nil
}

// scopeBundles builds what a rule-scoped push plans: for each rule group holding a named rule, a
// synthetic local group that is the group as Grafana holds it (identity, interval, rule order and
// every rule) with only the named rules replaced by the bundle's versions, paired with the remote
// group itself.
//
// Nothing about a rule the caller did not name comes from the bundle, which is what makes it
// impossible for an edit to a sibling in the file - or a sibling deleted, re-ordered or added there -
// to reach the group PUT, and why a sibling Grafana gained since the last pull is kept rather than
// deleted. Neither bundle carries contact points or policies.
//
// It is pure and modifies neither argument: the remote rules are shared with the synthetic group,
// which push only reads (every write goes through ruleGroupBody's copies).
func scopeBundles(local, remote Bundle, uids []string) (Bundle, Bundle, error) {
	located, err := locateLocalRules(local, uids)
	if err != nil {
		return Bundle{}, Bundle{}, err
	}

	synthetic := make([]*models.AlertRuleGroup, 0, len(uids))
	held := make([]*models.AlertRuleGroup, 0, len(uids))
	seen := make(map[string]struct{}, len(uids))

	for _, uid := range uids {
		group := remoteGroupHolding(remote.Groups, uid)
		if group == nil {
			return Bundle{}, Bundle{}, fmt.Errorf("alerts: push rule %s: its rule group no longer holds it", uid)
		}

		if file := located[uid].group; file.FolderUID != group.FolderUID || file.Title != group.Title {
			return Bundle{}, Bundle{}, fmt.Errorf(
				"alerts: push rule %s: Grafana holds it in rule group %s, the bundle in %s; "+
					"moving a rule between groups needs a whole-group push (alerts push without --rule-uid)",
				uid, groupID(group), groupID(file),
			)
		}

		if _, done := seen[groupID(group)]; done {
			continue
		}

		seen[groupID(group)] = struct{}{}

		held = append(held, group)
		synthetic = append(synthetic, syntheticGroup(group, located))
	}

	slices.SortStableFunc(synthetic, compareGroups)
	slices.SortStableFunc(held, compareGroups)

	return Bundle{Groups: synthetic}, Bundle{Groups: held}, nil
}

// scopedBundles is the scoped half of push up to the plan: locate the named rules in the bundle,
// fetch their groups from Grafana now, warn about edits that will not be pushed, and build the pair
// to plan.
func scopedBundles(
	ctx context.Context,
	gClient *goapi.GrafanaHTTPAPI,
	local Bundle,
	opts PushOptions,
	scope scopeFunc,
) (Bundle, Bundle, error) {
	uids := dedupeUIDs(opts.RuleUIDs)

	located, err := locateLocalRules(local, uids)
	if err != nil {
		return Bundle{}, Bundle{}, err
	}

	remote, err := Pull(ctx, gClient, PullOptions{RuleUIDs: uids, MaxConcurrent: opts.MaxConcurrent})
	if err != nil {
		return Bundle{}, Bundle{}, err
	}

	if err := warnOtherDifferences(ctx, located, remote, uids); err != nil {
		return Bundle{}, Bundle{}, err
	}

	return scope(local, remote, uids)
}

// planScoped is Plan plus the check that makes a rule-scoped push unable to reach beyond the rules it
// names: it is the only way the scoped path plans, so what push applies is what this accepted.
//
// scopeBundles builds a local group that equals Grafana's but for the named rules, so on real input
// every entry is an Update or Unchanged of a group holding a named rule, and an Update's detail is
// the `~ <title> (<uid>)` lines of those rules. The check is the tripwire for the day that stops
// being true. Every entry must satisfy all of:
//
//   - it is a rule group, never a contact point or the policy tree;
//   - it is an Update or an Unchanged, never a Create;
//   - its ID is a group holding a named rule;
//   - each of its detail lines is the `~` line of a named rule of that group - a rule belongs to the
//     first group (in plan order) that holds it, so a line cannot be borrowed from another group;
//   - an Unchanged has no detail and an Update has some.
//
// Anything else - an added or removed rule, a changed interval, a re-order, a replaced uid-less rule -
// is an error, whatever stage produced it, and push returns it before any write.
func planScoped(local, remote Bundle, uids []string) ([]Change, error) {
	changes, err := Plan(local, remote)
	if err != nil {
		return nil, err
	}

	allowed := allowedScopedDetails(local.Groups, uids)

	for _, change := range changes {
		if offence := scopedOffence(change, allowed); offence != "" {
			return nil, fmt.Errorf(
				"alerts: push: refusing a rule-scoped push of %s %s: the plan would change more than the named rules (%s)",
				change.Kind, change.ID, offence,
			)
		}
	}

	return changes, nil
}

// allowedScopedDetails maps the ID of every group holding a named rule to the detail lines a plan
// may carry for it: the `~` line of each named rule it holds. A rule is credited to the first group
// that holds it, so the same uid in a second group is allowed nothing there.
func allowedScopedDetails(groups []*models.AlertRuleGroup, uids []string) map[string]map[string]struct{} {
	allowed := make(map[string]map[string]struct{}, len(uids))
	claimed := make(map[string]struct{}, len(uids))

	for _, group := range groups {
		if group == nil {
			continue
		}

		for _, uid := range uids {
			rule := findRule(group.Rules, uid)
			if rule == nil {
				continue
			}

			if _, taken := claimed[uid]; taken {
				continue
			}

			claimed[uid] = struct{}{}

			if allowed[groupID(group)] == nil {
				allowed[groupID(group)] = make(map[string]struct{})
			}

			allowed[groupID(group)][ruleDetail(changedMarker, rule)] = struct{}{}
		}
	}

	return allowed
}

// scopedOffence names what is wrong with one plan entry of a rule-scoped push, or returns "" when it
// is allowed.
func scopedOffence(change Change, allowed map[string]map[string]struct{}) string {
	if change.Kind != KindRuleGroup {
		return "not a rule group"
	}

	if change.Action != ActionUpdate && change.Action != ActionUnchanged {
		return "action " + string(change.Action)
	}

	lines, scoped := allowed[change.ID]
	if !scoped {
		return "a rule group holding none of the named rules"
	}

	if change.Action == ActionUnchanged && len(change.Detail) > 0 {
		return "an unchanged group with changes"
	}

	if change.Action == ActionUpdate && len(change.Detail) == 0 {
		return "an update that changes none of the named rules"
	}

	for _, line := range change.Detail {
		if _, ok := lines[line]; !ok {
			return line
		}
	}

	return ""
}

// remoteGroupHolding returns the group among groups that holds the rule carrying uid, or nil.
func remoteGroupHolding(groups []*models.AlertRuleGroup, uid string) *models.AlertRuleGroup {
	for _, group := range groups {
		if group != nil && findRule(group.Rules, uid) != nil {
			return group
		}
	}

	return nil
}

// syntheticGroup is remote with every rule named in located replaced by the bundle's version, in
// the order and with the interval Grafana reports. A nil element of remote's rules is kept in place,
// never dereferenced (ruleGroupBody drops it when the body is built).
func syntheticGroup(remote *models.AlertRuleGroup, located map[string]scopedRule) *models.AlertRuleGroup {
	synthetic := *remote
	synthetic.Rules = make([]*models.ProvisionedAlertRule, 0, len(remote.Rules))

	for _, rule := range remote.Rules {
		if rule != nil {
			if named, ok := located[rule.UID]; ok {
				synthetic.Rules = append(synthetic.Rules, named.rule)

				continue
			}
		}

		synthetic.Rules = append(synthetic.Rules, rule)
	}

	return &synthetic
}

// warnOtherDifferences says, once per group file, that the bundle differs from Grafana outside the
// named rules and that those differences are not pushed. The exit code is unaffected: ignoring them
// is the point of naming a rule, but doing it silently would let an edit to a sibling look pushed.
//
// A group Grafana does not hold is skipped; scopeBundles refuses that case with its own error.
func warnOtherDifferences(
	ctx context.Context,
	located map[string]scopedRule,
	remote Bundle,
	uids []string,
) error {
	named := make(map[string]struct{}, len(uids))
	for _, uid := range uids {
		named[uid] = struct{}{}
	}

	warned := make(map[string]struct{}, len(located))

	for _, uid := range uids {
		file := located[uid].group

		if _, done := warned[groupID(file)]; done {
			continue
		}

		warned[groupID(file)] = struct{}{}

		held := findGroup(remote.Groups, file.FolderUID, file.Title)
		if held == nil {
			continue
		}

		lines, err := otherDifferences(file, held, named)
		if err != nil {
			return fmt.Errorf("alerts: push rule group %s: %w", groupID(file), err)
		}

		if len(lines) > 0 {
			logging.FromContext(ctx).Warn(fmt.Sprintf(
				"%d other difference(s) in rule group %s are not pushed by --rule-uid; "+
					"run alerts push --dry-run without --rule-uid to see them",
				len(lines), groupID(file),
			))
		}
	}

	return nil
}
