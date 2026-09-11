package alerts

import (
	"context"
	"errors"
	"fmt"

	goapi "github.com/grafana/grafana-openapi-client-go/client"
	"github.com/grafana/grafana-openapi-client-go/client/provisioning"
	"github.com/grafana/grafana-openapi-client-go/models"
)

// The words one rule operation names itself with in its errors. Pausing and resuming are the same
// write - a group PUT with isPaused flipped - so the message is the only thing that tells them
// apart.
const (
	deleteOperation = "delete"
	pauseOperation  = "pause"
	resumeOperation = "resume"
)

// DeleteRules deletes the named alert rules and reports what happened to each one.
//
// Every rule is fetched before it is deleted, for two reasons. Grafana 12.3 answers a DELETE for a
// uid it does not know with a 500 rather than a 404, so an unknown uid would otherwise read as an
// outage; and the delete has to carry the header the rule's stored provenance implies (see
// ProvenanceHeader), which only the fetch can supply. A rule another provisioner owns is refused
// rather than deleted.
//
// One uid failing - unknown, refused or rejected - is recorded against that uid, and the remaining
// rules are still deleted. A dead session is the exception: it ends the run and is returned as the
// *runtime.APIError the exit-2 rendering matches on, together with the results collected so far.
//
// opts.DryRun makes no request that could delete anything and still reports what the run would
// delete. opts.Paused says nothing about a delete and is ignored: the three rule operations share
// one options type so the command layer can hand all of them the same value.
func DeleteRules(
	ctx context.Context,
	gClient *goapi.GrafanaHTTPAPI,
	uids []string,
	opts RuleOpOptions,
) ([]RuleOpResult, error) {
	if gClient == nil {
		return nil, errors.New("alerts: no grafana client configured")
	}

	run := newRuleOpRun(gClient, uids)

	rules, err := run.resolve(ctx)
	if err != nil {
		return run.results(), err
	}

	for _, rule := range rules {
		if err := run.deleteRule(ctx, rule, opts.DryRun); err != nil {
			return run.results(), err
		}
	}

	return run.results(), nil
}

// SetRulesPaused pauses or resumes the named alert rules and reports what happened to each one.
//
// There is no endpoint for it. A single-rule PUT carrying nothing but a flipped isPaused returns
// 200 and does not persist for an Editor on 12.3.x, while the same flip inside a *group* PUT does
// - so the rules are fetched, grouped by the rule group they live in, and each group is written
// once with every one of its rules, the named ones flipped (see decision 7 of the plan).
//
// Writing a whole group has three consequences this implements deliberately:
//
//   - The group's rules must agree on their provenance, since the PUT carries one header for all
//     of them; a mixed group, or one another provisioner owns, is refused (see
//     GroupProvenanceHeader).
//   - A group in which nothing needs flipping is not written at all. Rules already in the
//     requested state are reported as unchanged, which is what keeps `pause` idempotent without a
//     no-op PUT that would re-stamp the whole group.
//   - Naming several rules of one group is one PUT, not one per rule: two full replacements of the
//     same group would otherwise race each other.
//
// A uid that fails is recorded against that uid (or, for a failure that belongs to the group
// write, against each of the uids named in it) and the other groups are still written. A dead
// session ends the run and is returned unchanged, as in DeleteRules.
//
// opts.DryRun makes no write at all and still reports what the run would flip.
func SetRulesPaused(
	ctx context.Context,
	gClient *goapi.GrafanaHTTPAPI,
	uids []string,
	opts RuleOpOptions,
) ([]RuleOpResult, error) {
	if gClient == nil {
		return nil, errors.New("alerts: no grafana client configured")
	}

	run := newRuleOpRun(gClient, uids)

	rules, err := run.resolve(ctx)
	if err != nil {
		return run.results(), err
	}

	for _, batch := range groupBatches(rules) {
		if err := run.pauseGroup(ctx, batch, opts); err != nil {
			return run.results(), err
		}
	}

	return run.results(), nil
}

// RuleOpOptions controls one pause, resume or delete.
type RuleOpOptions struct {
	// Paused is the state the named rules should end up in: true pauses them, false resumes them.
	// DeleteRules ignores it - there is no state a deleted rule ends up in.
	Paused bool
	// DryRun makes no write at all and still reports what the run would change.
	DryRun bool
}

// RuleOpResult is what one named rule's operation did, as the command layer prints it: one line
// per uid, whatever happened to it.
//
// Title and Group come from the rule as Grafana holds it, so they are empty for a uid that could
// not be looked up at all.
type RuleOpResult struct {
	UID   string
	Title string
	Group string
	// Changed reports that the rule was written, or - under a dry run - that it would be. It is
	// false for a rule that was already in the requested state, and for every failure.
	Changed bool
	// Err is nil unless this rule failed. It is already prefixed with the operation and the rule
	// or group it is about, so a caller can print it as-is.
	Err error
}

// ruleGroupBatch is one rule group together with the rules of it this run was asked to change.
type ruleGroupBatch struct {
	ref   GroupRef
	rules []*models.ProvisionedAlertRule
}

// ruleOpRun carries what one pause, resume or delete shares: the client, the uids in the order the
// user named them, and the outcome recorded for each so far.
//
// Outcomes are kept by uid rather than appended, so the order of the report is the order of the
// command line even though the work is done group by group.
type ruleOpRun struct {
	gClient *goapi.GrafanaHTTPAPI
	order   []string
	outcome map[string]RuleOpResult
}

// deleteRule deletes one rule, with the header its stored provenance implies.
func (r *ruleOpRun) deleteRule(ctx context.Context, rule *models.ProvisionedAlertRule, dryRun bool) error {
	header, err := ProvenanceHeader(string(rule.Provenance))
	if err != nil {
		return r.record(ruleOpFailure(rule, operationError(deleteOperation+" rule", rule.UID, err)))
	}

	if dryRun {
		return r.recordChanged([]*models.ProvisionedAlertRule{rule})
	}

	params := provisioning.NewDeleteAlertRuleParams().
		WithContext(ctx).
		WithUID(rule.UID).
		WithXDisableProvenance(header)

	if _, err := r.gClient.Provisioning.DeleteAlertRule(params); err != nil {
		return r.record(ruleOpFailure(rule, operationError(deleteOperation+" rule", rule.UID, err)))
	}

	return r.recordChanged([]*models.ProvisionedAlertRule{rule})
}

// failRules records the same failure against every one of the named rules, which is what a failure
// belonging to a whole group write means for the uids that asked for it.
func (r *ruleOpRun) failRules(rules []*models.ProvisionedAlertRule, err error) error {
	for _, rule := range rules {
		if stop := r.record(ruleOpFailure(rule, err)); stop != nil {
			return stop
		}
	}

	return nil
}

// pauseGroup flips isPaused on one group's named rules and writes the group, or writes nothing at
// all when there is nothing to flip.
func (r *ruleOpRun) pauseGroup(ctx context.Context, batch ruleGroupBatch, opts RuleOpOptions) error {
	operation := setPausedOperation(opts.Paused)

	group, found, err := fetchGroup(ctx, r.gClient, batch.ref)
	if err != nil {
		return r.failRules(batch.rules, err)
	}

	if !found {
		return r.failRules(batch.rules, fmt.Errorf(
			"alerts: %s rule group %s: not found", operation, groupRefID(batch.ref),
		))
	}

	// The body is a copy, so flipping a rule in it leaves the fetched group - the provenance
	// header's only source - as Grafana reported it.
	body := ruleGroupBody(group, nil)

	flip, err := r.pauseTargets(batch, body, opts)
	if err != nil || len(flip) == 0 {
		return err
	}

	header, err := GroupProvenanceHeader(group.Rules)
	if err != nil {
		return r.failRules(flip, operationError(operation+" rule group", groupRefID(batch.ref), err))
	}

	for _, rule := range flip {
		rule.IsPaused = opts.Paused
	}

	if opts.DryRun {
		return r.recordChanged(flip)
	}

	return r.putPausedGroup(ctx, batch, body, header, flip, opts)
}

// pauseTargets returns the rules of one batch a pause or resume would flip, recording the outcome
// of the ones it would not: a rule already in the requested state is an unchanged result, and one
// the group no longer holds is a failure.
//
// The returned rules point into body, so the caller flips them there rather than in the group the
// provenance header is read from.
func (r *ruleOpRun) pauseTargets(
	batch ruleGroupBatch,
	body *models.AlertRuleGroup,
	opts RuleOpOptions,
) ([]*models.ProvisionedAlertRule, error) {
	operation := setPausedOperation(opts.Paused)
	flip := make([]*models.ProvisionedAlertRule, 0, len(batch.rules))

	for _, rule := range batch.rules {
		current := findRule(body.Rules, rule.UID)

		switch {
		case current == nil:
			err := fmt.Errorf(
				"alerts: %s rule %s: rule group %s no longer holds it",
				operation, rule.UID, groupRefID(batch.ref),
			)
			if stop := r.record(ruleOpFailure(rule, err)); stop != nil {
				return nil, stop
			}
		case current.IsPaused == opts.Paused:
			if stop := r.record(ruleOpResult(rule)); stop != nil {
				return nil, stop
			}
		default:
			flip = append(flip, current)
		}
	}

	return flip, nil
}

// putPausedGroup writes the group whose rules were flipped and records the outcome against every
// uid the write was made for.
func (r *ruleOpRun) putPausedGroup(
	ctx context.Context,
	batch ruleGroupBatch,
	body *models.AlertRuleGroup,
	header *string,
	flip []*models.ProvisionedAlertRule,
	opts RuleOpOptions,
) error {
	params := provisioning.NewPutAlertRuleGroupParams().
		WithContext(ctx).
		WithFolderUID(batch.ref.FolderUID).
		WithGroup(batch.ref.Title).
		WithBody(body).
		WithXDisableProvenance(header)

	if _, err := r.gClient.Provisioning.PutAlertRuleGroup(params); err != nil {
		operation := setPausedOperation(opts.Paused)

		return r.failRules(flip, operationError(operation+" rule group", groupRefID(batch.ref), err))
	}

	return r.recordChanged(flip)
}

// record files one rule's outcome and reports whether the run must stop, which only a dead session
// asks for: nothing after it could succeed, and the error has to reach the caller unchanged for
// the exit-2 rendering.
func (r *ruleOpRun) record(result RuleOpResult) error {
	r.outcome[result.UID] = result

	if isUnauthorized(result.Err) {
		return result.Err
	}

	return nil
}

// recordChanged files every named rule as written - or, under a dry run, as one the run would
// write.
func (r *ruleOpRun) recordChanged(rules []*models.ProvisionedAlertRule) error {
	for _, rule := range rules {
		result := ruleOpResult(rule)
		result.Changed = true

		if stop := r.record(result); stop != nil {
			return stop
		}
	}

	return nil
}

// resolve looks up every named uid and returns the rules that exist, recording a failure for the
// ones that do not.
//
// It runs to completion before any write, which is what "an unknown uid changes nothing" means:
// the uid is reported as not found, and every other rule is still operated on.
func (r *ruleOpRun) resolve(ctx context.Context) ([]*models.ProvisionedAlertRule, error) {
	rules := make([]*models.ProvisionedAlertRule, 0, len(r.order))

	for _, uid := range r.order {
		rule, found, err := fetchRule(ctx, r.gClient, uid)

		switch {
		case err != nil:
			if stop := r.record(RuleOpResult{UID: uid, Err: err}); stop != nil {
				return nil, stop
			}
		case !found:
			err := fmt.Errorf("alerts: rule %s %w", uid, ErrRuleNotFound)
			if stop := r.record(RuleOpResult{UID: uid, Err: err}); stop != nil {
				return nil, stop
			}
		default:
			rules = append(rules, rule)
		}
	}

	return rules, nil
}

// results reports one line per named uid, in the order the user named them. A uid the run never
// reached - because a dead session ended it - has no line rather than an invented one.
func (r *ruleOpRun) results() []RuleOpResult {
	results := make([]RuleOpResult, 0, len(r.order))

	for _, uid := range r.order {
		if result, recorded := r.outcome[uid]; recorded {
			results = append(results, result)
		}
	}

	return results
}

// dedupeUIDs keeps the first occurrence of every uid. Naming a rule twice is one rule: fetching,
// flipping or deleting it twice would either be wasted work or, for a group PUT, two full
// replacements of the same group.
func dedupeUIDs(uids []string) []string {
	deduped := make([]string, 0, len(uids))
	seen := make(map[string]struct{}, len(uids))

	for _, uid := range uids {
		if _, found := seen[uid]; found {
			continue
		}

		seen[uid] = struct{}{}

		deduped = append(deduped, uid)
	}

	return deduped
}

// fetchRule fetches one rule by uid. The bool reports whether it exists, which every rule
// operation has to know before it writes: Grafana answers a DELETE for an unknown uid with a 500,
// so only this lookup can tell a typo apart from an outage.
//
// A 200 carrying no rule is read as absence too - there is nothing to operate on either way, and
// the alternative is a nil dereference further down.
func fetchRule(
	ctx context.Context,
	gClient *goapi.GrafanaHTTPAPI,
	uid string,
) (*models.ProvisionedAlertRule, bool, error) {
	if uid == "" {
		return nil, false, nil
	}

	params := provisioning.NewGetAlertRuleParams().WithContext(ctx).WithUID(uid)

	resp, err := gClient.Provisioning.GetAlertRuleWithParams(params)
	if err != nil {
		notFound := &provisioning.GetAlertRuleNotFound{}
		if errors.As(err, &notFound) {
			return nil, false, nil
		}

		return nil, false, fmt.Errorf("alerts: get rule %s: %w", uid, err)
	}

	if resp.Payload == nil {
		return nil, false, nil
	}

	return resp.Payload, true, nil
}

// groupBatches groups rules by the rule group they live in, keeping the order the user named them
// in for both the groups and the rules inside each one.
func groupBatches(rules []*models.ProvisionedAlertRule) []ruleGroupBatch {
	batches := make([]ruleGroupBatch, 0, len(rules))
	index := make(map[GroupRef]int, len(rules))

	for _, rule := range rules {
		ref := ruleRef(rule)

		position, seen := index[ref]
		if !seen {
			position = len(batches)
			index[ref] = position
			batches = append(batches, ruleGroupBatch{ref: ref})
		}

		batches[position].rules = append(batches[position].rules, rule)
	}

	return batches
}

// newRuleOpRun starts a run over the named uids.
func newRuleOpRun(gClient *goapi.GrafanaHTTPAPI, uids []string) *ruleOpRun {
	order := dedupeUIDs(uids)

	return &ruleOpRun{
		gClient: gClient,
		order:   order,
		outcome: make(map[string]RuleOpResult, len(order)),
	}
}

// ruleOpFailure is one rule's outcome when it failed: named as far as it is known, changed by
// nothing.
func ruleOpFailure(rule *models.ProvisionedAlertRule, err error) RuleOpResult {
	result := ruleOpResult(rule)
	result.Err = err

	return result
}

// ruleOpResult names one rule in a report, leaving the outcome to the caller.
func ruleOpResult(rule *models.ProvisionedAlertRule) RuleOpResult {
	return RuleOpResult{
		UID:   rule.UID,
		Title: stringValue(rule.Title),
		Group: stringValue(rule.RuleGroup),
	}
}

// ruleRef names the rule group a rule lives in, which is the pair a group write addresses.
func ruleRef(rule *models.ProvisionedAlertRule) GroupRef {
	return GroupRef{FolderUID: stringValue(rule.FolderUID), Title: stringValue(rule.RuleGroup)}
}

// setPausedOperation is the word a pause or a resume names itself with. They are one write, told
// apart only by what the user asked for.
func setPausedOperation(paused bool) string {
	if paused {
		return pauseOperation
	}

	return resumeOperation
}

// stringValue reads an optional string field. Almost every field of models.ProvisionedAlertRule is
// a pointer, including the ones the API marks required, so a server that omits one must not panic
// a command that only wanted to name the rule.
func stringValue(value *string) string {
	if value == nil {
		return ""
	}

	return *value
}
