package alerts

import (
	"errors"

	"github.com/go-openapi/strfmt"
	"github.com/grafana/grafana-openapi-client-go/models"
)

// contactPointBody is the body of one contact-point write: the local point under the UID it is
// addressed by, with the read-only provenance dropped.
//
// Settings are sent exactly as the file holds them, "[REDACTED]" markers included: Grafana reads
// the marker as "keep the stored secret", which is what makes pull -> edit -> push safe.
func contactPointBody(local *models.EmbeddedContactPoint, uid string) *models.EmbeddedContactPoint {
	body := *local
	body.UID = uid
	body.Provenance = ""

	return &body
}

// pinRule is one rule as a rule-group write body carries it: the identity fields the group itself
// decides pinned to the group's own, the read-only ones Grafana owns cleared.
//
// It is the single source of that pinning, shared by the write (ruleGroupBody) and the plan
// (ruleDetails). The two have to agree: a file is allowed to leave folderUID, ruleGroup and orgID
// out, so a plan comparing the file as written would find three keys fewer than the remote rule
// carries, report a change the push cannot close, and re-PUT the group on every single push -
// clobbering whatever was edited meanwhile and re-stamping provenance.
//
// orgID is pinned only when the caller resolved one; nil leaves whatever the rule claims, which is
// what a group being created in a Grafana Cloud context falls back to (see resolveOrgID).
func pinRule(rule *models.ProvisionedAlertRule, folderUID, group string, orgID *int64) *models.ProvisionedAlertRule {
	pinned := *rule
	pinned.FolderUID = &folderUID
	pinned.RuleGroup = &group
	pinned.ID = 0
	pinned.Provenance = ""
	pinned.Updated = strfmt.DateTime{}

	if orgID != nil {
		org := *orgID
		pinned.OrgID = &org
	}

	return &pinned
}

// requireOrgID reports the one organization push cannot decide: resolveOrgID found nothing, so
// every rule has to carry its own - and orgID is not required on read, because push is meant to
// fill it in.
//
// Reaching this needs both a group that does not exist in Grafana yet and a context whose org-id
// is zero (Grafana Cloud, which addresses a stack instead). Naming the group here is what keeps
// that combination from being sent as a body Grafana answers with an opaque 400.
func requireOrgID(local *models.AlertRuleGroup, resolved *int64) error {
	if resolved != nil {
		return nil
	}

	for _, rule := range local.Rules {
		if rule != nil && rule.OrgID == nil {
			return errors.New(
				"cannot resolve orgID: the rule group does not exist in Grafana and the context has no org-id; " +
					"set org-id on the context or orgID on every rule in the group",
			)
		}
	}

	return nil
}

// resolveOrgID decides which organization a pushed rule claims, or nil to keep what the file says.
//
// The remote group wins when there is one: it is the instance's own answer, and a rule written
// into another org would be invisible. The context's org-id is the fallback for a group being
// created; it is zero for a Grafana Cloud context, which addresses a stack instead, and then the
// file's value - which a pull wrote from this same instance - is the best remaining guess, and
// requireOrgID refuses the group when the file has no value either.
func resolveOrgID(remote *models.AlertRuleGroup, contextOrgID int64) *int64 {
	if remote != nil {
		for _, rule := range remote.Rules {
			if rule != nil && rule.OrgID != nil {
				orgID := *rule.OrgID

				return &orgID
			}
		}
	}

	if contextOrgID != 0 {
		orgID := contextOrgID

		return &orgID
	}

	return nil
}

// routeBody is the body of a policy-tree write: a copy of the tree with provenance dropped from
// every node, since Grafana stamps it all the way down and rejects a write that claims to set it.
func routeBody(route *models.Route) *models.Route {
	if route == nil {
		return nil
	}

	body := *route
	body.Provenance = ""

	if route.Routes != nil {
		body.Routes = make([]*models.Route, 0, len(route.Routes))

		for _, child := range route.Routes {
			body.Routes = append(body.Routes, routeBody(child))
		}
	}

	return &body
}

// ruleGroupBody is the body of one rule-group write: a copy of the local group whose rules are
// pinned to the group being written and stripped of the three fields Grafana owns.
//
// Pinning folderUID and ruleGroup is what keeps a hand-edited file from moving rules by accident:
// a group file addresses one group, and the rules inside it belong to that group whatever their
// own fields say. The pinning itself is pinRule, which the plan projects through as well so the
// two never disagree about what this write sends. Nothing the caller passed in is modified - the
// local bundle is still the bundle the plan was computed from.
func ruleGroupBody(local *models.AlertRuleGroup, orgID *int64) *models.AlertRuleGroup {
	body := *local
	body.Rules = make([]*models.ProvisionedAlertRule, 0, len(local.Rules))

	for _, rule := range local.Rules {
		if rule == nil {
			continue
		}

		body.Rules = append(body.Rules, pinRule(rule, local.FolderUID, local.Title, orgID))
	}

	return &body
}
