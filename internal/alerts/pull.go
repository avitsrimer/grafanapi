package alerts

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/go-openapi/runtime"
	goapi "github.com/grafana/grafana-openapi-client-go/client"
	"github.com/grafana/grafana-openapi-client-go/client/folders"
	"github.com/grafana/grafana-openapi-client-go/client/provisioning"
	"github.com/grafana/grafana-openapi-client-go/models"
	"golang.org/x/sync/errgroup"
)

// DefaultMaxConcurrent bounds the rule-group fetches and writes when the caller asks for no
// particular limit. It matches the `resources` commands' own default, and is exported so the
// --max-concurrent flag's default is the same number in one place rather than three.
const DefaultMaxConcurrent = 10

// getFolderOperation names the folder lookup in the *runtime.APIError synthesised for it; see
// asAPIError for why that synthesis is needed at all.
const getFolderOperation = "[GET /folders/{folder_uid}] getFolderByUID"

// Pull fetches one alerting bundle - rule groups, contact points and the notification policy tree
// - from the Grafana behind gClient.
//
// What it fetches depends on opts:
//
//   - opts.Groups non-nil selects exactly those rule groups, one GET each and no listing (an empty
//     selection therefore fetches no group at all). This is what push uses: the local bundle
//     already names every group it intends to write, and listing a whole instance to find three of
//     them is both slower and a larger blast radius.
//
//   - otherwise every rule group is discovered from the rule list and filtered by opts.FolderUIDs.
//     Each requested folder UID is verified first, because GetAlertRules has no folder filter:
//     without the check a misspelled UID and a folder holding no rules are the same empty result,
//     and the pull would silently write an empty bundle - which the next push would read as
//     "these groups no longer exist".
//
//   - opts.RuleUIDs non-empty selects the groups holding those rules: one GetAlertRule per uid,
//     then one GET per distinct group, no rule listing. The groups come back whole (every rule of
//     each, not only the named ones) and the result carries no contact points and no policy tree:
//     a rule selection is about rules only, and fetching the rest is the whole-instance read it
//     exists to avoid. An unknown uid is an error wrapping ErrRuleNotFound.
//
// Otherwise contact points and the policy tree are fetched whole; neither API offers a
// selection, and both are single objects.
//
// A rule group that has disappeared between the listing and its own fetch (or one named in
// opts.Groups that does not exist remotely, which is the normal case for a push that creates it)
// is absent from the result rather than an error.
//
// Errors are wrapped with the operation; a 401 stays reachable through errors.As as a
// *runtime.APIError - including from the folder check, whose generated client reports it as a
// typed response instead (see asAPIError) - so the stale-session exit-2 rendering still fires.
//
// Bundle.Groups and Bundle.ContactPoints are sorted and never nil, so two pulls of an unchanged
// instance produce byte-identical files (a rule selection leaves ContactPoints and Policies nil).
func Pull(ctx context.Context, gClient *goapi.GrafanaHTTPAPI, opts PullOptions) (Bundle, error) {
	if gClient == nil {
		return Bundle{}, errors.New("alerts: no grafana client configured")
	}

	if len(opts.RuleUIDs) > 0 {
		return pullRules(ctx, gClient, opts)
	}

	refs, err := pullGroupRefs(ctx, gClient, opts)
	if err != nil {
		return Bundle{}, err
	}

	groups, err := fetchGroups(ctx, gClient, refs, maxConcurrent(opts.MaxConcurrent))
	if err != nil {
		return Bundle{}, err
	}

	// Reuses the read path rather than repeating the drop-nils-and-sort: it is the same request
	// with the same normalization, and its "list contact points" error prefix still says which
	// call failed.
	points, err := ListContactPoints(ctx, gClient)
	if err != nil {
		return Bundle{}, err
	}

	tree, err := gClient.Provisioning.GetPolicyTreeWithParams(provisioning.NewGetPolicyTreeParams().WithContext(ctx))
	if err != nil {
		return Bundle{}, fmt.Errorf("alerts: pull policy tree: %w", err)
	}

	return Bundle{Groups: groups, ContactPoints: points, Policies: tree.Payload}, nil
}

// GroupRef names one rule group: a folder UID and the group's title, which is the pair the
// provisioning API addresses a group by.
//
// It is deliberately not a *models.AlertRuleGroup - a caller selecting groups to fetch has only
// their identity, not their contents.
type GroupRef struct {
	FolderUID string
	Title     string
}

// PullOptions selects what Pull fetches.
//
// A zero value pulls everything, which is what `alerts pull` with no flags does.
type PullOptions struct {
	// FolderUIDs restricts a discovery pull to the named folders. Every entry is verified to
	// exist before anything is listed, so a typo is an error rather than an empty bundle. It is
	// ignored when Groups is set: naming the groups already names their folders.
	FolderUIDs []string
	// Groups, when non-nil, selects exactly these rule groups and skips the rule listing entirely.
	// A ref with no remote counterpart is simply absent from the result. Nil and empty differ, as
	// they do for WriteBundle's keepFolders: nil discovers every group, while an empty (non-nil)
	// selection fetches none - which is what a push of a bundle holding no rule groups at all
	// needs, since listing every rule to then use none of them is exactly the whole-instance read
	// the selection exists to avoid.
	Groups []GroupRef
	// RuleUIDs, when non-empty, selects the rule groups holding these rules - one GetAlertRule per
	// uid, then one group GET per distinct group, no rule listing - and leaves ContactPoints and
	// Policies nil: a rule selection is about rules only. It cannot be combined with Groups or
	// FolderUIDs.
	RuleUIDs []string
	// MaxConcurrent bounds the in-flight rule-group fetches. Zero or negative means
	// DefaultMaxConcurrent.
	MaxConcurrent int
}

// asAPIError makes sure a generated client failure carries the *runtime.APIError the exit-code
// contract is built on.
//
// Most operations already do: their generated readers hand every non-2xx to runtime.NewAPIError.
// The folders client is the exception - it declares typed 401/403/404/500 responses, which are
// returned as their own error types, so a 401 from the folder check would otherwise never match
// the errors.As in cmd/grafanapi/fail and a stale session would exit 1 with an opaque message
// instead of exiting 2 with the "run login update" suggestion.
//
// Every one of those typed responses exposes Code(), which is what this matches on; anything else
// is returned untouched.
func asAPIError(operation string, err error) error {
	apiErr := &runtime.APIError{}
	if errors.As(err, &apiErr) {
		return err
	}

	var coded interface{ Code() int }
	if errors.As(err, &coded) {
		return runtime.NewAPIError(operation, err, coded.Code())
	}

	return err
}

// compareGroupRefs orders two refs by folder UID then title, so the fetch order - and any error
// naming the first failing group - does not depend on the order Grafana listed the rules in.
func compareGroupRefs(a, b GroupRef) int {
	return cmp.Or(
		cmp.Compare(a.FolderUID, b.FolderUID),
		cmp.Compare(a.Title, b.Title),
	)
}

// dedupeGroupRefs sorts refs and removes duplicates. The discovery path produces one ref per
// *rule*, so a group of ten rules would otherwise be fetched ten times.
func dedupeGroupRefs(refs []GroupRef) []GroupRef {
	deduped := slices.Clone(refs)
	slices.SortStableFunc(deduped, compareGroupRefs)

	return slices.Compact(deduped)
}

// fetchGroup fetches one rule group. The bool reports whether it exists: a 404 is the expected
// answer for a group a push is about to create, and for one deleted between a listing and this
// call, so it is absence rather than failure.
func fetchGroup(
	ctx context.Context,
	gClient *goapi.GrafanaHTTPAPI,
	ref GroupRef,
) (*models.AlertRuleGroup, bool, error) {
	// The params form is mandatory here: the positional GetAlertRuleGroup takes the group title
	// *before* the folder UID, which reads as a caller bug at every call site.
	params := provisioning.NewGetAlertRuleGroupParams().
		WithContext(ctx).
		WithFolderUID(ref.FolderUID).
		WithGroup(ref.Title)

	resp, err := gClient.Provisioning.GetAlertRuleGroupWithParams(params)
	if err != nil {
		notFound := &provisioning.GetAlertRuleGroupNotFound{}
		if errors.As(err, &notFound) {
			return nil, false, nil
		}

		return nil, false, fmt.Errorf("alerts: pull rule group %s/%s: %w", ref.FolderUID, ref.Title, err)
	}

	return resp.Payload, true, nil
}

// fetchGroups fetches every ref concurrently, bounded by limit, and returns the groups that exist
// sorted the way a bundle is laid out on disk.
//
// Results are collected into a pre-sized slice indexed by ref rather than appended under a mutex:
// the order is then independent of which fetch finished first, which is what makes a re-pull of
// an unchanged instance byte-identical.
func fetchGroups(
	ctx context.Context,
	gClient *goapi.GrafanaHTTPAPI,
	refs []GroupRef,
	limit int,
) ([]*models.AlertRuleGroup, error) {
	fetched := make([]*models.AlertRuleGroup, len(refs))

	eg, ctx := errgroup.WithContext(ctx)
	eg.SetLimit(limit)

	for i, ref := range refs {
		eg.Go(func() error {
			group, found, err := fetchGroup(ctx, gClient, ref)
			if err != nil {
				return err
			}

			if found {
				fetched[i] = group
			}

			return nil
		})
	}

	if err := eg.Wait(); err != nil {
		return nil, err
	}

	groups := make([]*models.AlertRuleGroup, 0, len(fetched))

	for _, group := range fetched {
		if group != nil {
			groups = append(groups, group)
		}
	}

	slices.SortStableFunc(groups, compareGroups)

	return groups, nil
}

// folderExists reports whether a folder UID addresses a folder, which both a filtered pull and a
// push's pre-check need to know before they act on it.
//
// The error is unprefixed - the caller says which operation it belongs to - but it is converted
// first: the folders client reports a 401 as its own typed response rather than as the
// *runtime.APIError the exit-code contract is built on (see asAPIError).
func folderExists(ctx context.Context, gClient *goapi.GrafanaHTTPAPI, uid string) (bool, error) {
	params := folders.NewGetFolderByUIDParams().WithContext(ctx).WithFolderUID(uid)

	if _, err := gClient.Folders.GetFolderByUIDWithParams(params); err != nil {
		notFound := &folders.GetFolderByUIDNotFound{}
		if errors.As(err, &notFound) {
			return false, nil
		}

		return false, asAPIError(getFolderOperation, err)
	}

	return true, nil
}

// groupRefID is how a rule group is named in a message and in a plan: the folder UID and title
// that address it.
func groupRefID(ref GroupRef) string {
	return ref.FolderUID + "/" + ref.Title
}

// maxConcurrent resolves the caller's requested bound, treating zero and negative values as
// "unspecified" rather than as "no concurrency at all" (errgroup.SetLimit(0) would deadlock).
func maxConcurrent(requested int) int {
	if requested <= 0 {
		return DefaultMaxConcurrent
	}

	return requested
}

// pullGroupRefs resolves which rule groups Pull should fetch; see Pull for the two modes.
func pullGroupRefs(ctx context.Context, gClient *goapi.GrafanaHTTPAPI, opts PullOptions) ([]GroupRef, error) {
	if opts.Groups != nil {
		return dedupeGroupRefs(opts.Groups), nil
	}

	if err := validateFolders(ctx, gClient, opts.FolderUIDs); err != nil {
		return nil, err
	}

	resp, err := gClient.Provisioning.GetAlertRulesWithParams(provisioning.NewGetAlertRulesParams().WithContext(ctx))
	if err != nil {
		return nil, fmt.Errorf("alerts: pull alert rules: %w", err)
	}

	refs := make([]GroupRef, 0, len(resp.Payload))

	for _, rule := range resp.Payload {
		if rule == nil || rule.FolderUID == nil || rule.RuleGroup == nil {
			continue
		}

		ref := GroupRef{FolderUID: *rule.FolderUID, Title: *rule.RuleGroup}
		if ref.FolderUID == "" || ref.Title == "" {
			continue
		}

		if len(opts.FolderUIDs) > 0 && !slices.Contains(opts.FolderUIDs, ref.FolderUID) {
			continue
		}

		refs = append(refs, ref)
	}

	return dedupeGroupRefs(refs), nil
}

// pullRules is Pull for a rule selection: the groups holding opts.RuleUIDs, whole, and nothing
// else.
func pullRules(ctx context.Context, gClient *goapi.GrafanaHTTPAPI, opts PullOptions) (Bundle, error) {
	if opts.Groups != nil || len(opts.FolderUIDs) > 0 {
		return Bundle{}, errors.New("alerts: pull: RuleUIDs cannot be combined with Groups or FolderUIDs")
	}

	uids := dedupeUIDs(opts.RuleUIDs)

	refs, holders, err := ruleGroupRefs(ctx, gClient, uids)
	if err != nil {
		return Bundle{}, err
	}

	groups, err := fetchGroups(ctx, gClient, refs, maxConcurrent(opts.MaxConcurrent))
	if err != nil {
		return Bundle{}, err
	}

	if err := requireRules(groups, uids, holders); err != nil {
		return Bundle{}, err
	}

	return Bundle{Groups: groups}, nil
}

// requireRules checks that every uid is held by one of the fetched groups. The rule lookup and the
// group fetch are two requests, so a group deleted between them (absent from groups) or a rule
// moved out of it would otherwise yield a bundle silently missing a rule the caller named.
//
// holders maps each uid to the group its rule lookup reported, so the error can name the group even
// when it is the one that is gone.
func requireRules(groups []*models.AlertRuleGroup, uids []string, holders map[string]GroupRef) error {
	for _, uid := range uids {
		found := false

		for _, group := range groups {
			if group != nil && findRule(group.Rules, uid) != nil {
				found = true

				break
			}
		}

		if !found {
			return fmt.Errorf("alerts: pull rule %s: rule group %s no longer holds it", uid, groupRefID(holders[uid]))
		}
	}

	return nil
}

// ruleGroupRefs resolves the rule groups holding the named rules, one GetAlertRule per uid.
//
// The lookups run in order and stop at the first failure rather than concurrently, as
// validateFolders does: there are a handful at most, and the first uid typed is the one reported.
// An unknown uid is fatal here (unlike the rule operations, which record it per uid), because a
// pull or push that silently skipped a named rule would do something other than what was asked.
func ruleGroupRefs(
	ctx context.Context, gClient *goapi.GrafanaHTTPAPI, uids []string,
) ([]GroupRef, map[string]GroupRef, error) {
	refs := make([]GroupRef, 0, len(uids))
	holders := make(map[string]GroupRef, len(uids))

	for _, uid := range uids {
		rule, found, err := fetchRule(ctx, gClient, uid)
		if err != nil {
			return nil, nil, err
		}

		if !found {
			return nil, nil, ruleNotFoundError(uid)
		}

		ref := ruleRef(rule)
		holders[uid] = ref
		refs = append(refs, ref)
	}

	return dedupeGroupRefs(refs), holders, nil
}

// validateFolders checks that every requested folder UID exists, which is what tells a typo apart
// from a folder holding no rules (see Pull).
//
// The checks run in order and stop at the first failure rather than concurrently: there are a
// handful of them at most, and reporting the first UID the user typed is more useful than
// whichever request happened to fail first.
func validateFolders(ctx context.Context, gClient *goapi.GrafanaHTTPAPI, uids []string) error {
	for _, uid := range uids {
		exists, err := folderExists(ctx, gClient, uid)
		if err != nil {
			return fmt.Errorf("alerts: pull folder %s: %w", uid, err)
		}

		if !exists {
			return fmt.Errorf("alerts: pull: folder %s not found", uid)
		}
	}

	return nil
}
