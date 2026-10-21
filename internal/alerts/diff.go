package alerts

import (
	"cmp"
	"fmt"
	"reflect"
	"slices"

	"github.com/grafana/grafana-openapi-client-go/models"
)

// The actions a plan entry can carry. There is deliberately no Delete: a bundle is not required to
// describe a whole Grafana, so an object that exists remotely and not locally is simply not managed
// here rather than scheduled for removal (pruning is future work, behind a flag of its own).
//
// Rules are the exception that proves it. A rule missing from a group file *is* deleted, because
// Grafana replaces a rule group wholesale on PUT - which is why that removal is spelled out as a
// "-" line inside the group's own Update entry instead of being left implicit.
const (
	ActionCreate    Action = "Create"
	ActionUnchanged Action = "Unchanged"
	ActionUpdate    Action = "Update"
)

// The kinds of object a plan entry is about, which is also the order push applies them in: contact
// points first (a policy may route to one), then the policy tree, then the rule groups. The
// constants sort into that order as plain strings, which is what makes sorting a plan by kind
// produce a readable one.
const (
	KindContactPoint Kind = "ContactPoint"
	KindPolicyTree   Kind = "PolicyTree"
	KindRuleGroup    Kind = "RuleGroup"
)

// policyTreeID is the ID every policy-tree entry carries. The tree is a single unnamed object, so
// there is nothing to derive an ID from and one constant keeps the plan's ID column populated.
const policyTreeID = "policies"

// The markers a plan's per-rule detail lines open with. They are compared as bytes, not ranked:
// the lines of one group are sorted, and "!" < "+" < "-" < "~" is what puts a replacement warning
// at the head of the block it explains, then the additions, the deletions and the changes.
const (
	replacedMarker = "!"
	addedMarker    = "+"
	removedMarker  = "-"
	changedMarker  = "~"
)

// ruleOrderDetail is the line a group whose rules are the same but differently ordered reports.
// It names no rule because no single rule moved: the sequence did, and the group PUT is what
// carries it.
const ruleOrderDetail = changedMarker + " rule order"

// redactedValue is what Grafana substitutes for every field a contact-point type declares secure.
// A pulled bundle therefore never holds the real secret, and pushing the marker back keeps the
// stored value - so a local "[REDACTED]" means "leave this as it is", not "set this literal
// string", and comparing it to whatever is stored remotely must report no change.
const redactedValue = "[REDACTED]"

// Plan reports what pushing the local bundle onto the remote one would do, as one entry per local
// object sorted by kind and then ID.
//
// It is pure: no network, no filesystem, no clock. The whole of `push --dry-run` is this function
// plus a renderer, and push itself applies exactly the entries Plan produced, so anything that
// could differ between the two runs would be a lie in the dry run.
//
// Comparison goes through the same projection WriteBundle uses - each model marshalled to its own
// JSON shape, the read-only keys (`id`, `updated`, `provenance`) removed, integers kept integral -
// with empty values dropped on top. That is what makes a hand-edited file and a freshly fetched
// model compare equal when they mean the same thing: `null` and `[]` for an absent rule list, an
// int64 and a float64 for the same number in a query model, a missing key and an empty map.
//
// Objects that exist only remotely are omitted entirely; see the Action constants.
func Plan(local, remote Bundle) ([]Change, error) {
	changes := make([]Change, 0, len(local.ContactPoints)+len(local.Groups)+1)

	points, err := planContactPoints(local.ContactPoints, remote.ContactPoints)
	if err != nil {
		return nil, err
	}

	changes = append(changes, points...)

	policies, err := planPolicyTree(local.Policies, remote.Policies)
	if err != nil {
		return nil, err
	}

	changes = append(changes, policies...)

	groups, err := planRuleGroups(local.Groups, remote.Groups)
	if err != nil {
		return nil, err
	}

	changes = append(changes, groups...)

	slices.SortStableFunc(changes, compareChanges)

	return changes, nil
}

// Action is what pushing one object would do to it.
type Action string

// Change is one entry of a plan: the action, the kind and ID of the object it applies to, and -
// for a rule group - the lines describing what changes inside it.
//
// Detail is empty for an Unchanged entry and for the kinds that have no inner structure worth
// spelling out. For a rule group it holds at most one interval line, `~ interval 60s→120s`, then a
// `~ rule order` line when the rules the two sides share stand in a different sequence, followed by
// one line per rule, `+ <title> (<uid>)` for a rule the push would add, `-` for one it would delete
// (a group PUT replaces the group) and `~` for one it would change.
type Change struct {
	Action Action
	Kind   Kind
	ID     string
	Detail []string
}

// Kind is the kind of object a plan entry is about.
type Kind string

// actionFor turns "the local and remote objects compare equal" into the action that says so.
func actionFor(equal bool) Action {
	if equal {
		return ActionUnchanged
	}

	return ActionUpdate
}

// compareChanges orders two plan entries by kind and then ID, which is the order Plan returns and
// push applies them in.
func compareChanges(a, b Change) int {
	return cmp.Or(
		cmp.Compare(a.Kind, b.Kind),
		cmp.Compare(a.ID, b.ID),
	)
}

// comparableValue projects one model onto the generic value Plan compares: the encoding shape a
// bundle file carries, with every empty value dropped.
func comparableValue(model any, strip func(any)) (any, error) {
	value, err := encodableValue(model, strip)
	if err != nil {
		return nil, fmt.Errorf("alerts: plan: %w", err)
	}

	return dropEmptyValues(value), nil
}

// contactPointID is the ID a contact point appears in a plan under: its UID, or its name when it
// has none, which is the same pair the matching below uses.
func contactPointID(point *models.EmbeddedContactPoint) string {
	if point.UID != "" {
		return point.UID
	}

	return point.Name
}

// createdRuleDetails lists every rule of a group that does not exist remotely, so a Create entry
// says what it would create rather than only that it would create something.
func createdRuleDetails(rules []*models.ProvisionedAlertRule) []string {
	details := make([]string, 0, len(rules))

	for _, rule := range rules {
		if rule != nil {
			details = append(details, ruleDetail(addedMarker, rule))
		}
	}

	slices.Sort(details)

	return details
}

// dropEmptyValues removes every map key whose value carries no information: a null, an empty
// object or an empty array.
//
// The three are interchangeable in the payloads this compares - a model's nil slice marshals to
// `null` where the same slice fetched from Grafana marshals to `[]`, and a field a hand-written
// file omits is an absent key rather than an empty one - so without this a re-pull would show
// changes that no push could ever close. Array elements are recursed into but never dropped:
// position is meaningful there, and an empty element is a real difference.
func dropEmptyValues(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		for key, item := range typed {
			item = dropEmptyValues(item)
			if isEmptyValue(item) {
				delete(typed, key)

				continue
			}

			typed[key] = item
		}

		return typed
	case []any:
		for i, item := range typed {
			typed[i] = dropEmptyValues(item)
		}

		return typed
	default:
		return value
	}
}

// equalContactPoints reports whether pushing local would leave the remote contact point as it
// already is, honouring Grafana's redaction of secure fields (see equalRedacted).
//
// What is compared is the body the push would send - contactPointBody under the remote UID - not
// the local file as written. A file may name a contact point without a uid and be matched to it by
// name, which is a supported way to write a bundle by hand; comparing that file directly would
// find one key fewer than the remote object carries and plan an Update for an identical point,
// re-PUTting it on every single push.
func equalContactPoints(local, remote *models.EmbeddedContactPoint) (bool, error) {
	return equalProjected(contactPointBody(local, remote.UID), remote, stripContactPointFields, equalRedacted)
}

// equalProjected reports whether two models are the same write: both are projected through strip -
// the encoding shape a bundle file carries, which is what makes a hand-edited file and a freshly
// fetched model compare equal when they mean the same thing - and equal then decides what "the
// same" means for that kind, which is plain deep equality everywhere except contact points.
func equalProjected(local, remote any, strip func(any), equal func(local, remote any) bool) (bool, error) {
	localValue, err := comparableValue(local, strip)
	if err != nil {
		return false, err
	}

	remoteValue, err := comparableValue(remote, strip)
	if err != nil {
		return false, err
	}

	return equal(localValue, remoteValue), nil
}

// equalRedacted is deep equality with one asymmetry: a local "[REDACTED]" leaf equals whatever
// stands at the same place remotely.
//
// It has to be a recursive walk rather than a check on the settings object itself, because
// settings is free-form JSON (models.JSON is an interface) and a type's secure fields can sit at
// any depth - a webhook's http_config.password, for instance. Treating the marker as a wildcard is
// what makes pull -> edit -> push a no-op for every field the user did not touch; pushing the
// marker back is what keeps the stored secret (verified against Grafana 12.3).
func equalRedacted(local, remote any) bool {
	if text, ok := local.(string); ok && text == redactedValue {
		return true
	}

	switch typed := local.(type) {
	case map[string]any:
		return equalRedactedMap(typed, remote)
	case []any:
		return equalRedactedSlice(typed, remote)
	default:
		return reflect.DeepEqual(local, remote)
	}
}

// equalRedactedMap compares two objects key by key under the redaction rule.
func equalRedactedMap(local map[string]any, remote any) bool {
	other, ok := remote.(map[string]any)
	if !ok || len(other) != len(local) {
		return false
	}

	for key, item := range local {
		value, found := other[key]
		if !found || !equalRedacted(item, value) {
			return false
		}
	}

	return true
}

// equalRedactedSlice compares two arrays element by element under the redaction rule.
func equalRedactedSlice(local []any, remote any) bool {
	other, ok := remote.([]any)
	if !ok || len(other) != len(local) {
		return false
	}

	for i, item := range local {
		if !equalRedacted(item, other[i]) {
			return false
		}
	}

	return true
}

// equalRoutes reports whether two notification policy trees are the same write. Provenance is
// stripped from every node, not only the root, because Grafana stamps it all the way down.
func equalRoutes(local, remote *models.Route) (bool, error) {
	return equalProjected(local, remote, stripRouteFields, reflect.DeepEqual)
}

// equalRules reports whether two alert rules are the same write, the three fields Grafana owns
// aside.
//
// What is passed as local must already be the rule as the group's write body would carry it - see
// pinRule, and ruleDetails, which is the only caller - not the rule as the file wrote it. folderUID,
// ruleGroup and orgID are compared like any other field here, so the projection is what decides
// them; the push overwrites all three, and a file that omits or disagrees about them would
// otherwise report a change no push could ever close.
func equalRules(local, remote *models.ProvisionedAlertRule) (bool, error) {
	return equalProjected(local, remote, stripRuleFields, reflect.DeepEqual)
}

// findGroup returns the rule group addressed by a folder UID and title, or nil when there is none.
func findGroup(groups []*models.AlertRuleGroup, folderUID, title string) *models.AlertRuleGroup {
	for _, group := range groups {
		if group != nil && group.FolderUID == folderUID && group.Title == title {
			return group
		}
	}

	return nil
}

// findRule returns the rule carrying uid, or nil when there is none.
//
// An empty uid never matches: it means "a rule the user has just written into a group file", which
// is a rule Grafana has not assigned an identity to yet and therefore cannot already hold.
func findRule(rules []*models.ProvisionedAlertRule, uid string) *models.ProvisionedAlertRule {
	if uid == "" {
		return nil
	}

	for _, rule := range rules {
		if rule != nil && rule.UID == uid {
			return rule
		}
	}

	return nil
}

// groupDetail describes what pushing local over remote would change inside one rule group. An
// empty result means the two are the same write: the group carries nothing but its identity, its
// interval, the order of its rules and the rules themselves.
func groupDetail(local, remote *models.AlertRuleGroup) ([]string, error) {
	var detail []string

	if local.Interval != remote.Interval {
		detail = append(detail, fmt.Sprintf("%s interval %ds→%ds", changedMarker, remote.Interval, local.Interval))
	}

	if !sameRuleOrder(local.Rules, remote.Rules) {
		detail = append(detail, ruleOrderDetail)
	}

	rules, err := ruleDetails(local, remote)
	if err != nil {
		return nil, err
	}

	return append(detail, rules...), nil
}

// groupID is the ID a rule group appears in a plan under: the folder UID and title that address
// it, which is also how push pairs a plan entry back up with the group it was planned from.
func groupID(group *models.AlertRuleGroup) string {
	return groupRefID(GroupRef{FolderUID: group.FolderUID, Title: group.Title})
}

// isEmptyValue reports whether a decoded value is one of the three interchangeable forms of
// "nothing"; see dropEmptyValues.
func isEmptyValue(value any) bool {
	switch typed := value.(type) {
	case nil:
		return true
	case map[string]any:
		return len(typed) == 0
	case []any:
		return len(typed) == 0
	default:
		return false
	}
}

// kindLabel is how a kind is written in a message to the user: the plan's column values are
// type-like names, and an error reads as English.
func kindLabel(kind Kind) string {
	switch kind {
	case KindContactPoint:
		return "contact point"
	case KindPolicyTree:
		return "policy tree"
	case KindRuleGroup:
		return "rule group"
	default:
		return string(kind)
	}
}

// matchContactPoint finds the remote contact point a local one addresses: by UID when it has one,
// otherwise by name.
//
// A name is not an identity - Grafana allows several contact points to share one, which is how a
// name ends up addressing more than one object - so a uid-less entry matching several is an error
// rather than a coin toss: pushing it would overwrite an arbitrary one of them.
func matchContactPoint(
	local *models.EmbeddedContactPoint,
	remote []*models.EmbeddedContactPoint,
) (*models.EmbeddedContactPoint, bool, error) {
	if local.UID != "" {
		for _, point := range remote {
			if point != nil && point.UID == local.UID {
				return point, true, nil
			}
		}

		return nil, false, nil
	}

	var matched []*models.EmbeddedContactPoint

	for _, point := range remote {
		if point != nil && local.Name != "" && point.Name == local.Name {
			matched = append(matched, point)
		}
	}

	switch len(matched) {
	case 0:
		return nil, false, nil
	case 1:
		return matched[0], true, nil
	default:
		return nil, false, fmt.Errorf(
			"alerts: plan: contact point %q has no uid and matches %d contact points by name; set its uid",
			local.Name, len(matched),
		)
	}
}

// otherDifferences describes what pushing local over remote would change in one rule group
// *outside* the rules named in named (a set of rule uids): sibling additions, removals and edits,
// the interval and the order the siblings stand in.
//
// The named rules are removed from copies of both groups before groupDetail runs, so the answer is
// structural - no plan line is parsed or filtered, and a title crafted to look like a named rule's
// line cannot hide a difference. Nil rules are dropped from the copies; the arguments are not
// modified.
func otherDifferences(local, remote *models.AlertRuleGroup, named map[string]struct{}) ([]string, error) {
	return groupDetail(withoutRules(local, named), withoutRules(remote, named))
}

// planContactPoints plans the contact-point half of a bundle, one entry per local point.
//
// Uniqueness is enforced against the *resolved* remote object rather than against the local IDs,
// which ReadBundle has already checked: `{uid: cp-1, name: team}` and a uid-less `{name: team}` are
// two different plan IDs that address one remote contact point, and pushing both would PUT the same
// UID twice with no ordering between them. Both local entries are named, since the ID the plan
// would show for either is exactly what hides the collision.
func planContactPoints(local, remote []*models.EmbeddedContactPoint) ([]Change, error) {
	changes := make([]Change, 0, len(local))
	matched := make(map[*models.EmbeddedContactPoint]string, len(local))

	for _, point := range local {
		if point == nil {
			continue
		}

		match, found, err := matchContactPoint(point, remote)
		if err != nil {
			return nil, err
		}

		id := contactPointID(point)
		change := Change{Action: ActionCreate, Kind: KindContactPoint, ID: id}

		if found {
			if previous, duplicate := matched[match]; duplicate {
				return nil, fmt.Errorf(
					"alerts: plan: contact points %q and %q both address the contact point %q; keep one",
					previous, id, contactPointID(match),
				)
			}

			matched[match] = id

			equal, err := equalContactPoints(point, match)
			if err != nil {
				return nil, err
			}

			change.Action = actionFor(equal)
		}

		changes = append(changes, change)
	}

	return changes, nil
}

// planPolicyTree plans the notification policy tree, which is a single object and therefore at
// most one entry.
//
// A bundle without a policy file plans nothing at all: the tree always exists in Grafana, so
// "absent locally" can only mean "not managed here", never "delete it".
func planPolicyTree(local, remote *models.Route) ([]Change, error) {
	changes := make([]Change, 0, 1)

	if local == nil {
		return changes, nil
	}

	change := Change{Action: ActionCreate, Kind: KindPolicyTree, ID: policyTreeID}

	if remote != nil {
		equal, err := equalRoutes(local, remote)
		if err != nil {
			return nil, err
		}

		change.Action = actionFor(equal)
	}

	return append(changes, change), nil
}

// planRuleGroups plans the rule-group half of a bundle, one entry per local group.
func planRuleGroups(local, remote []*models.AlertRuleGroup) ([]Change, error) {
	changes := make([]Change, 0, len(local))

	for _, group := range local {
		if group == nil {
			continue
		}

		change := Change{Action: ActionCreate, Kind: KindRuleGroup, ID: groupID(group)}

		match := findGroup(remote, group.FolderUID, group.Title)
		if match == nil {
			change.Detail = createdRuleDetails(group.Rules)
			changes = append(changes, change)

			continue
		}

		detail, err := groupDetail(group, match)
		if err != nil {
			return nil, err
		}

		change.Action, change.Detail = actionFor(len(detail) == 0), detail
		changes = append(changes, change)
	}

	return changes, nil
}

// replacedRule returns the remote rule a uid-less local rule would destroy and re-create, or nil
// when there is none.
//
// This is the shape a *second* push of a file that introduced a rule takes: the first push made
// Grafana assign the rule a UID, which the file still does not carry, so the next plan matches
// nothing and reports the same rule as both an addition and a deletion. The write really does
// replace it - the rule loses its UID, its alert state and its silence history - which is why the
// plan says so on a line of its own rather than leaving the `+`/`-` pair to be read as two rules.
//
// It is a guard, not a match: matching by title instead would be worse, since a title is not an
// identity and renaming a rule in the file would then silently update the wrong one.
func replacedRule(
	local *models.ProvisionedAlertRule,
	locals, remote []*models.ProvisionedAlertRule,
) *models.ProvisionedAlertRule {
	if local.UID != "" || local.Title == nil {
		return nil
	}

	for _, rule := range remote {
		if rule == nil || rule.Title == nil || *rule.Title != *local.Title {
			continue
		}

		// Only a remote rule no local rule claims is at risk: one matched by uid elsewhere in the
		// file is simply being updated.
		if findRule(locals, rule.UID) == nil {
			return rule
		}
	}

	return nil
}

// replacedRuleDetail is the line replacedRule's finding renders as. Its marker sorts before every
// other one, so the warning heads the group's detail block.
func replacedRuleDetail(remote *models.ProvisionedAlertRule) string {
	return ruleDetail(replacedMarker, remote) + " would be replaced, not updated; run alerts pull first"
}

// ruleDetail renders one rule as a plan line. The UID is dropped when there is none rather than
// rendered as empty parentheses: a rule with no UID is one the file introduces, and saying so with
// its title alone is the whole of what is known about it.
func ruleDetail(marker string, rule *models.ProvisionedAlertRule) string {
	title := ""
	if rule.Title != nil {
		title = *rule.Title
	}

	if rule.UID == "" {
		return marker + " " + title
	}

	return fmt.Sprintf("%s %s (%s)", marker, title, rule.UID)
}

// ruleDetails describes what pushing one group's local rules over its remote ones would change.
//
// Rules are matched by UID, so moving one within the file is not reported here - the group's own
// `~ rule order` line covers that (see sameRuleOrder) - and a rule the file drops is reported as a
// deletion because the group PUT replaces the group rather than merging into it. The lines are
// sorted, which groups them by marker (the replacement warning first, then additions, deletions
// and changes) and keeps
// the plan identical across runs whatever order the file happened to list its rules in.
//
// Whole groups are taken rather than their rule slices because what is compared is the rule the
// push would send, not the rule the file wrote: each local rule is projected through pinRule under
// the group's own folderUID and title and the organization the matched remote rule is in, which is
// exactly what ruleGroupBody and resolveOrgID do on the write. A file is free to leave those three
// out (or to disagree about them), and without the projection every such rule would report a change
// the push could not close.
func ruleDetails(local, remote *models.AlertRuleGroup) ([]string, error) {
	var details []string

	for _, rule := range local.Rules {
		if rule == nil {
			continue
		}

		match := findRule(remote.Rules, rule.UID)
		if match == nil {
			details = append(details, ruleDetail(addedMarker, rule))

			if replaced := replacedRule(rule, local.Rules, remote.Rules); replaced != nil {
				details = append(details, replacedRuleDetail(replaced))
			}

			continue
		}

		equal, err := equalRules(pinRule(rule, local.FolderUID, local.Title, match.OrgID), match)
		if err != nil {
			return nil, err
		}

		if !equal {
			details = append(details, ruleDetail(changedMarker, rule))
		}
	}

	for _, rule := range remote.Rules {
		if rule != nil && findRule(local.Rules, rule.UID) == nil {
			details = append(details, ruleDetail(removedMarker, rule))
		}
	}

	slices.Sort(details)

	return details, nil
}

// sameRuleOrder reports whether the rules the two groups share stand in the same sequence.
//
// Grafana evaluates a group's rules in the order the group body lists them, and a group PUT is the
// only way to change that order - so a file whose rules were merely re-ordered is a real write,
// even though every rule compares equal to the one it is matched to by uid. Without this the plan
// would say Unchanged, no PUT would be made, and the next pull would silently revert the file.
//
// Only uids present on both sides are compared: a rule the file adds or drops changes the sequence
// by definition, and is already reported as a "+" or "-" line of its own.
func sameRuleOrder(local, remote []*models.ProvisionedAlertRule) bool {
	return slices.Equal(sharedRuleUIDs(local, remote), sharedRuleUIDs(remote, local))
}

// sharedRuleUIDs lists, in order, the uids of rules that also appear in other.
func sharedRuleUIDs(rules, other []*models.ProvisionedAlertRule) []string {
	uids := make([]string, 0, len(rules))

	for _, rule := range rules {
		if rule != nil && findRule(other, rule.UID) != nil {
			uids = append(uids, rule.UID)
		}
	}

	return uids
}

// withoutRules returns a copy of group whose rules exclude every one whose uid is in uids, and
// every nil entry. Only the rule slice is rebuilt; the rules themselves are shared, which is safe
// because every caller only reads them.
func withoutRules(group *models.AlertRuleGroup, uids map[string]struct{}) *models.AlertRuleGroup {
	trimmed := *group
	trimmed.Rules = make([]*models.ProvisionedAlertRule, 0, len(group.Rules))

	for _, rule := range group.Rules {
		if rule == nil {
			continue
		}

		if _, named := uids[rule.UID]; named && rule.UID != "" {
			continue
		}

		trimmed.Rules = append(trimmed.Rules, rule)
	}

	return &trimmed
}
