package alerts

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	goapi "github.com/grafana/grafana-openapi-client-go/client"
	"github.com/grafana/grafana-openapi-client-go/client/provisioning"
	"github.com/grafana/grafana-openapi-client-go/models"
	"golang.org/x/sync/errgroup"
)

// Push applies a local bundle to the Grafana behind gClient and reports what it did.
//
// The work is exactly the plan Plan produces, applied in the order it sorts into: contact points
// first (a policy may route to one), then the notification policy tree, then the rule groups. An
// Unchanged entry is reported without a request being made.
//
// Only what the bundle names is fetched to build that plan - one rule-group GET per local group,
// one contact-point listing, one policy-tree GET - so pushing three groups does not read a whole
// instance. A bundle with no rule groups at all issues no rule requests, rather than falling back
// to discovering every group.
//
// Writes mirror each object's stored provenance (see ProvenanceHeader): an object Grafana has
// claimed is written without the header, an unclaimed one with it, and an object owned by another
// provisioner is refused. opts.UIEditable chooses that mode for objects being *created*, where
// there is no stored value to mirror.
//
// Failure handling has three levels:
//
//   - A failure that makes the rest of the push meaningless - a dead session, or a folder
//     pre-check that failed for a reason other than the folder itself - is returned as an error and
//     stops everything, whatever opts.StopOnError says. A 401 stays reachable through errors.As as
//     a *runtime.APIError so the exit-2 rendering fires.
//   - A per-object failure (a validation error, a 403, a foreign provenance, a folder that is
//     missing or unreadable) is recorded in Result.Failed. With opts.StopOnError it is also
//     returned, stopping the push at that object; without it every remaining object is still
//     attempted and the cmd layer decides the exit code from Result.Failed.
//   - opts.DryRun makes no write at all. It still runs every check that decides *whether* a write
//     is allowed - the read-only folder pre-check, the provenance classification, the redacted-
//     secret check - and reports the entries an apply would refuse in Result.Failed, so the plan
//     is the refusals too and not only the intentions. opts.StopOnError has no say over a dry run:
//     stopping at the first refusal would hide the rest of the plan, which is what it is for.
//
// Result.Applied and Result.Failed are sorted the way the plan is, so the output of a push does
// not depend on which write finished first.
//
// # Rule-scoped push
//
// With opts.RuleUIDs the push is about those rules only. Each named rule is looked up in the bundle
// (a uid the bundle does not hold is an error before any request) and in Grafana (an unknown uid is
// an error wrapping ErrRuleNotFound, whatever opts.StopOnError says), and the group PUT carries the
// rule group as Grafana holds it at the time of the push - every other rule, the interval and the
// order, never the file's - with the named rules replaced by the file's versions, sent whole. So an
// edit to a sibling in the file is not pushed, and a sibling Grafana gained or lost since the pull
// is neither deleted nor resurrected. The bundle's contact points and policy tree are neither
// fetched nor written, and no group is created.
func Push(ctx context.Context, gClient *goapi.GrafanaHTTPAPI, local Bundle, opts PushOptions) (Result, error) {
	return push(ctx, gClient, local, opts, scopeBundles)
}

// FailedChange is one plan entry that could not be applied, with the error explaining why. The
// error is already prefixed with the operation, kind and ID, so a caller can print it as-is.
type FailedChange struct {
	Change Change
	Err    error
}

// PushOptions controls one push.
//
// It carries no cmd/ types on purpose - like remote.PushRequest it takes a plain StopOnError bool,
// leaving the --on-error vocabulary (and the exit code it implies) to the command layer.
type PushOptions struct {
	// DryRun plans the push and makes no write at all.
	DryRun bool
	// StopOnError stops at the first per-object failure instead of attempting every object.
	StopOnError bool
	// UIEditable leaves objects this push *creates* editable in the UI (provenance ""), instead of
	// claiming them for the API. It has no effect on an update, which always mirrors the stored
	// provenance.
	UIEditable bool
	// MaxConcurrent bounds the in-flight rule-group writes, and the fetches that precede them.
	// Zero or negative means DefaultMaxConcurrent.
	MaxConcurrent int
	// RuleUIDs, when non-empty, restricts the push to the rules with these uids: see Push, "Rule-scoped
	// push". The rest of the bundle is ignored.
	RuleUIDs []string
	// OrgID is the current context's organization, used for a rule the remote side cannot supply
	// one for. Zero means "unknown" - a Grafana Cloud context addresses a stack rather than an org
	// - in which case the file's own orgID is kept.
	OrgID int64
}

// Result is what a push did: the entries it applied (including the Unchanged ones, which needed no
// request) and the ones that failed.
type Result struct {
	Applied []Change
	Failed  []FailedChange
}

// pushRun carries what every phase of one push shares - the client, the two bundles being
// reconciled, the options and the result being accumulated - so the phases take a change or a
// target rather than the same six parameters each.
type pushRun struct {
	gClient *goapi.GrafanaHTTPAPI
	local   Bundle
	remote  Bundle
	opts    PushOptions
	result  Result
}

// ruleGroupTarget is one rule group about to be written: the plan entry naming it, the local group
// that is the body, and its remote counterpart (nil when the group is new), which decides both the
// provenance header and the org the rules claim.
type ruleGroupTarget struct {
	change Change
	local  *models.AlertRuleGroup
	remote *models.AlertRuleGroup
}

// apply performs a plan, phase by phase, and returns the first error that stopped it.
func (p *pushRun) apply(ctx context.Context, changes []Change) error {
	pending := p.recordUnchanged(changes)

	if err := p.applyContactPoints(ctx, pending); err != nil {
		return err
	}

	if err := p.applyPolicyTree(ctx, pending); err != nil {
		return err
	}

	return p.applyRuleGroups(ctx, pending)
}

// applyContactPoints writes every contact point the plan asks for, in the order the local bundle
// lists them.
//
// It walks the local bundle rather than the plan so that each entry is applied with the object it
// was planned from; the plan's IDs are how the two are paired.
func (p *pushRun) applyContactPoints(ctx context.Context, changes []Change) error {
	pending := changesByID(changes, KindContactPoint)
	if len(pending) == 0 {
		return nil
	}

	for _, point := range p.local.ContactPoints {
		if point == nil {
			continue
		}

		change, planned := pending[contactPointID(point)]
		if !planned {
			continue
		}

		if err := p.record(change, p.pushContactPoint(ctx, change, point)); err != nil {
			return err
		}
	}

	return nil
}

// applyPolicyTree writes the notification policy tree when the plan asks for it. The tree is a
// single object, so this is at most one write.
func (p *pushRun) applyPolicyTree(ctx context.Context, changes []Change) error {
	index := slices.IndexFunc(changes, func(change Change) bool { return change.Kind == KindPolicyTree })
	if index < 0 || p.local.Policies == nil {
		return nil
	}

	return p.record(changes[index], p.pushPolicyTree(ctx, changes[index]))
}

// applyRuleGroups writes every rule group the plan asks for, after checking that each group's
// folder exists and can be read.
//
// The folder check is separate from the write because Grafana's answer to a group PUT into a
// missing folder is a 400 for an Editor and a 403 for anyone else - neither of which says that the
// folder is what is missing, nor that folders are not created by this command.
//
// A folder the check ruled out fails every group in it, and only those: the groups in the other
// folders are still written (or still planned, under --dry-run), since neither a missing folder nor
// a folder this session may not read says anything about the rest of the bundle.
func (p *pushRun) applyRuleGroups(ctx context.Context, changes []Change) error {
	targets := p.ruleGroupTargets(changes)
	if len(targets) == 0 {
		return nil
	}

	blocked, err := p.checkFolders(ctx, targets)
	if err != nil {
		return err
	}

	writable := make([]ruleGroupTarget, 0, len(targets))

	for _, target := range targets {
		folderErr, refused := blocked[target.local.FolderUID]
		if !refused {
			writable = append(writable, target)

			continue
		}

		if err := p.record(target.change, changeError(target.change, folderErr)); err != nil {
			return err
		}
	}

	if p.opts.StopOnError {
		return p.applyRuleGroupsInOrder(ctx, writable)
	}

	return p.applyRuleGroupsConcurrently(ctx, writable)
}

// applyRuleGroupsConcurrently writes every target, bounded by MaxConcurrent.
//
// The errgroup carries no context of its own: cancelling the siblings of a failed write would
// report them as failures too, when what actually happened is that they never ran. Every write is
// therefore allowed to finish, and only a dead session is propagated as the error that stops the
// push - there is nothing left for the remaining writes to succeed at once the session is gone.
func (p *pushRun) applyRuleGroupsConcurrently(ctx context.Context, targets []ruleGroupTarget) error {
	outcomes := make([]error, len(targets))

	var eg errgroup.Group

	eg.SetLimit(maxConcurrent(p.opts.MaxConcurrent))

	for i, target := range targets {
		eg.Go(func() error {
			outcomes[i] = p.pushRuleGroup(ctx, target)

			if isUnauthorized(outcomes[i]) {
				return outcomes[i]
			}

			return nil
		})
	}

	fatal := eg.Wait()

	for i, target := range targets {
		// record's stop signal is redundant here: this path runs only when StopOnError is unset,
		// so the only error it can report is the 401 eg.Wait has already returned.
		_ = p.record(target.change, outcomes[i])
	}

	return fatal
}

// applyRuleGroupsInOrder writes the targets one at a time and stops at the first failure, which is
// what --on-error abort asks for: "the first failure" is only meaningful when the writes are
// ordered, and a bounded-but-concurrent phase would have several of them in flight past the point
// the user wanted to stop.
func (p *pushRun) applyRuleGroupsInOrder(ctx context.Context, targets []ruleGroupTarget) error {
	for _, target := range targets {
		if err := p.record(target.change, p.pushRuleGroup(ctx, target)); err != nil {
			return err
		}
	}

	return nil
}

// checkFolders verifies each target's folder once and returns the per-folder error for every
// folder a rule-group write cannot go into: one that does not exist, and one this session is not
// allowed to read.
//
// A 403 is per folder rather than fatal because Grafana's alerting permissions are: a role granted
// folders:read on two folders out of ten answers 403 for the other eight, and letting that end the
// push would refuse the eight *and* hide the plan for the two - including under --dry-run, which is
// exactly when a user is trying to find out which of their folders they may write. Anything else -
// a dead session, a transport failure - is the same answer every folder that follows would get, so
// it is returned as the error that stops the push.
func (p *pushRun) checkFolders(ctx context.Context, targets []ruleGroupTarget) (map[string]error, error) {
	blocked := map[string]error{}
	checked := make(map[string]struct{}, len(targets))

	for _, target := range targets {
		uid := target.local.FolderUID
		if _, seen := checked[uid]; seen {
			continue
		}

		checked[uid] = struct{}{}

		exists, err := folderExists(ctx, p.gClient, uid)

		switch {
		case isForbidden(err):
			blocked[uid] = &messageError{message: fmt.Sprintf("folder %s: permission denied", uid), err: err}
		case err != nil:
			return nil, fmt.Errorf("alerts: push folder %s: %w", uid, err)
		case !exists:
			blocked[uid] = fmt.Errorf("folder %s does not exist; push folders first (grafanapi resources push)", uid)
		}
	}

	return blocked, nil
}

// createContactPoint POSTs a contact point that has no remote counterpart.
//
// A create carrying the "[REDACTED]" marker is refused rather than sent. The marker means "keep
// the stored secret", and there is nothing stored for an object that does not exist yet: Grafana
// would store the literal string as the secret, leaving a contact point that looks provisioned and
// cannot authenticate. This is the one case pull -> push into a *different* instance runs into,
// where every contact point is a create.
func (p *pushRun) createContactPoint(ctx context.Context, change Change, local *models.EmbeddedContactPoint) error {
	redacted, err := redactedFields(local)
	if err != nil {
		return changeError(change, err)
	}

	if len(redacted) != 0 {
		return changeError(change, fmt.Errorf(
			"secret field(s) %s are redacted; set the real value(s) in the file before creating",
			strings.Join(redacted, ", "),
		))
	}

	if p.opts.DryRun {
		return nil
	}

	params := provisioning.NewPostContactpointsParams().
		WithContext(ctx).
		WithBody(contactPointBody(local, local.UID)).
		WithXDisableProvenance(CreateProvenanceHeader(p.opts.UIEditable))

	if _, err := p.gClient.Provisioning.PostContactpoints(params); err != nil {
		return changeError(change, err)
	}

	return nil
}

// pushContactPoint creates or updates one contact point, depending on whether the remote bundle
// holds the point it addresses.
func (p *pushRun) pushContactPoint(ctx context.Context, change Change, local *models.EmbeddedContactPoint) error {
	match, found, err := matchContactPoint(local, p.remote.ContactPoints)
	if err != nil {
		return err
	}

	if !found {
		return p.createContactPoint(ctx, change, local)
	}

	return p.updateContactPoint(ctx, change, local, match)
}

// pushPolicyTree PUTs the whole notification policy tree; there is no partial update of it and no
// create either, since Grafana always has a tree.
func (p *pushRun) pushPolicyTree(ctx context.Context, change Change) error {
	header := CreateProvenanceHeader(p.opts.UIEditable)

	if p.remote.Policies != nil {
		stored, err := ProvenanceHeader(string(p.remote.Policies.Provenance))
		if err != nil {
			return changeError(change, err)
		}

		header = stored
	}

	if p.opts.DryRun {
		return nil
	}

	params := provisioning.NewPutPolicyTreeParams().
		WithContext(ctx).
		WithBody(routeBody(p.local.Policies)).
		WithXDisableProvenance(header)

	if _, err := p.gClient.Provisioning.PutPolicyTree(params); err != nil {
		return changeError(change, err)
	}

	return nil
}

// pushRuleGroup PUTs one whole rule group. Grafana replaces a group on PUT rather than merging
// into it, so the body has to carry every rule the group should end up with - which is exactly
// what the local file lists, and why the plan spells out the rules such a write would delete.
func (p *pushRun) pushRuleGroup(ctx context.Context, target ruleGroupTarget) error {
	header, err := ruleGroupHeader(target, p.opts.UIEditable)
	if err != nil {
		return changeError(target.change, err)
	}

	orgID := resolveOrgID(target.remote, p.opts.OrgID)
	if err := requireOrgID(target.local, orgID); err != nil {
		return changeError(target.change, err)
	}

	if p.opts.DryRun {
		return nil
	}

	params := provisioning.NewPutAlertRuleGroupParams().
		WithContext(ctx).
		WithFolderUID(target.local.FolderUID).
		WithGroup(target.local.Title).
		WithBody(ruleGroupBody(target.local, orgID)).
		WithXDisableProvenance(header)

	if _, err := p.gClient.Provisioning.PutAlertRuleGroup(params); err != nil {
		return changeError(target.change, err)
	}

	return nil
}

// record files one applied or failed change and reports whether the push must stop: on the first
// failure when StopOnError is set, and on a dead session always - nothing that follows could
// succeed, and the error has to reach the caller unchanged for the exit-2 rendering.
func (p *pushRun) record(change Change, err error) error {
	if err == nil {
		p.result.Applied = append(p.result.Applied, change)

		return nil
	}

	p.result.Failed = append(p.result.Failed, FailedChange{Change: change, Err: err})

	if p.opts.StopOnError || isUnauthorized(err) {
		return err
	}

	return nil
}

// recordUnchanged files every entry that needs no write and returns the ones that do.
func (p *pushRun) recordUnchanged(changes []Change) []Change {
	pending := make([]Change, 0, len(changes))

	for _, change := range changes {
		if change.Action == ActionUnchanged {
			p.result.Applied = append(p.result.Applied, change)

			continue
		}

		pending = append(pending, change)
	}

	return pending
}

// ruleGroupTargets pairs each planned rule-group entry with the local group it was planned from
// and that group's remote counterpart.
func (p *pushRun) ruleGroupTargets(changes []Change) []ruleGroupTarget {
	pending := changesByID(changes, KindRuleGroup)

	targets := make([]ruleGroupTarget, 0, len(pending))

	for _, group := range p.local.Groups {
		if group == nil {
			continue
		}

		change, planned := pending[groupID(group)]
		if !planned {
			continue
		}

		targets = append(targets, ruleGroupTarget{
			change: change,
			local:  group,
			remote: findGroup(p.remote.Groups, group.FolderUID, group.Title),
		})
	}

	return targets
}

// updateContactPoint PUTs a contact point over its remote counterpart, addressing it by the
// remote UID: a local entry may have been written without one and matched by name.
func (p *pushRun) updateContactPoint(
	ctx context.Context,
	change Change,
	local, remote *models.EmbeddedContactPoint,
) error {
	header, err := ProvenanceHeader(remote.Provenance)
	if err != nil {
		return changeError(change, err)
	}

	if p.opts.DryRun {
		return nil
	}

	params := provisioning.NewPutContactpointParams().
		WithContext(ctx).
		WithUID(remote.UID).
		WithBody(contactPointBody(local, remote.UID)).
		WithXDisableProvenance(header)

	if _, err := p.gClient.Provisioning.PutContactpoint(params); err != nil {
		return changeError(change, err)
	}

	return nil
}

// changeError renders one failed write as the guide documents it - "alerts: push <kind> <id>:
// <what went wrong>".
func changeError(change Change, err error) error {
	return operationError("push "+kindLabel(change.Kind), change.ID, err)
}

// changesByID indexes the entries of one kind by their ID, which is how a plan entry is paired
// back up with the object it was planned from.
func changesByID(changes []Change, kind Kind) map[string]Change {
	indexed := make(map[string]Change, len(changes))

	for _, change := range changes {
		if change.Kind == kind {
			indexed[change.ID] = change
		}
	}

	return indexed
}

// collectRedacted walks a decoded value and appends the dotted path of every "[REDACTED]" leaf it
// holds to found. The walk has to be recursive because settings is free-form JSON (models.JSON is
// an interface) and a type's secure fields can sit at any depth.
func collectRedacted(value any, path string, found *[]string) {
	switch typed := value.(type) {
	case map[string]any:
		for key, item := range typed {
			child := key
			if path != "" {
				child = path + "." + key
			}

			collectRedacted(item, child, found)
		}
	case []any:
		for i, item := range typed {
			collectRedacted(item, fmt.Sprintf("%s[%d]", path, i), found)
		}
	case string:
		if typed == redactedValue && path != "" {
			*found = append(*found, path)
		}
	}
}

// localGroupRefs names the remote counterparts a push needs to fetch.
//
// The result is never nil, which is what tells Pull "exactly these groups, and none when the
// bundle has none" apart from "discover every group in the instance".
func localGroupRefs(groups []*models.AlertRuleGroup) []GroupRef {
	refs := make([]GroupRef, 0, len(groups))

	for _, group := range groups {
		if group != nil {
			refs = append(refs, GroupRef{FolderUID: group.FolderUID, Title: group.Title})
		}
	}

	return refs
}

// redactedFields lists, sorted and in dotted-path form, every "[REDACTED]" leaf a contact point
// carries.
//
// The walk goes over the *encoded* contact point - the same projection Plan compares through - so
// it sees the same shape whether the point was decoded from YAML or from JSON.
func redactedFields(point *models.EmbeddedContactPoint) ([]string, error) {
	value, err := encodableValue(point, stripContactPointFields)
	if err != nil {
		return nil, err
	}

	var found []string

	collectRedacted(value, "", &found)
	slices.Sort(found)

	return found, nil
}

// ruleGroupHeader returns the X-Disable-Provenance header one rule-group write must carry: the
// remote group's own provenance when it exists (a group whose rules disagree, or that another
// provisioner owns, is refused), and the caller's choice for a group being created.
func ruleGroupHeader(target ruleGroupTarget, uiEditable bool) (*string, error) {
	if target.remote == nil {
		return CreateProvenanceHeader(uiEditable), nil
	}

	return GroupProvenanceHeader(target.remote.Rules)
}

// sortResult puts both halves of a result in plan order, so what a push prints does not depend on
// which of its concurrent writes finished first.
func sortResult(result *Result) {
	slices.SortStableFunc(result.Applied, compareChanges)
	slices.SortStableFunc(result.Failed, func(a, b FailedChange) int {
		return compareChanges(a.Change, b.Change)
	})
}

// push is Push with the scoping step as a parameter, so a test can prove the plan of a rule-scoped
// push is what guards the write rather than the builder that feeds it.
func push(
	ctx context.Context,
	gClient *goapi.GrafanaHTTPAPI,
	local Bundle,
	opts PushOptions,
	scope scopeFunc,
) (Result, error) {
	if gClient == nil {
		return Result{}, errors.New("alerts: no grafana client configured")
	}

	var (
		remote  Bundle
		changes []Change
		err     error
	)

	if len(opts.RuleUIDs) > 0 {
		local, remote, err = scopedBundles(ctx, gClient, local, opts, scope)
		if err != nil {
			return Result{}, err
		}

		changes, err = planScoped(local, remote, dedupeUIDs(opts.RuleUIDs))
	} else {
		remote, err = Pull(ctx, gClient, PullOptions{
			Groups:        localGroupRefs(local.Groups),
			MaxConcurrent: opts.MaxConcurrent,
		})
		if err != nil {
			return Result{}, err
		}

		changes, err = Plan(local, remote)
	}

	if err != nil {
		return Result{}, err
	}

	if opts.DryRun {
		opts.StopOnError = false
	}

	run := &pushRun{gClient: gClient, local: local, remote: remote, opts: opts}
	applyErr := run.apply(ctx, changes)

	sortResult(&run.result)

	return run.result, applyErr
}
