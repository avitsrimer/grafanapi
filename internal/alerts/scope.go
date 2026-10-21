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
