// Package alerts implements read-only access to Grafana Unified Alerting - alert rule
// definitions joined with their live evaluation state, plus notification contact points.
//
// It holds the domain logic for the `grafanapi alerts` command group; no Cobra types appear
// here (cmd/grafanapi/alerts is the thin wiring layer).
//
// Two data sources are combined:
//
//   - Definitions come from the generated Grafana client's provisioning service
//     (GET /api/v1/provisioning/alert-rules, GET /api/v1/provisioning/contact-points). They
//     answer "what is defined", carry no evaluation state, and expose folder UIDs only.
//   - Evaluation state comes from GET /api/prometheus/grafana/api/v1/rules over the raw-HTTP
//     seam (see state.go), because the generated client has no service for that endpoint at
//     all. It answers "what is firing" and is the only source of folder *names* (as full
//     paths, e.g. "Parent/Child").
//
// ListRules joins the two on rule UID, and folder paths onto definitions on folder UID. Definitions
// are the primary data and state is enrichment: a rule with no state entry is reported with State
// "unknown" rather than dropped - keeping its folder path, which the second join resolves from the
// state response's group objects rather than from the missing per-rule entry -
// and a state-endpoint failure that is not a 401 degrades to a warning instead of an error -
// but only for a request that does not depend on state, since applying a state-dependent filter
// to a fabricated state would report an empty list as a success (see ListRules and
// stateDependentFilter). A degraded run reports State "unavailable", never "unknown", so the
// missing state half is visible in the machine-readable output and not only in the stderr warning.
// A 401 from either call is fatal, so the stale-session exit-2 contract holds.
//
// ListRules, ListContactPoints and the state vocabulary helpers are what the command layer calls;
// FetchRuleStates is exported deliberately as a second entry point, so the raw state view can be
// consumed on its own - by a future non-rules caller and by its own unit tests - without going
// through the provisioning join.
//
// Design notes and the verified wire shapes are recorded in
// docs/plans/completed/20260909-alerts-command.md.
package alerts

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/go-openapi/runtime"
	"github.com/grafana/grafana-app-sdk/logging"
	goapi "github.com/grafana/grafana-openapi-client-go/client"
	"github.com/grafana/grafana-openapi-client-go/client/provisioning"
	"github.com/grafana/grafana-openapi-client-go/models"
	"github.com/grafana/grafanapi/internal/config"
	"github.com/grafana/grafanapi/internal/logs"
)

// NormalStateAlias is accepted as a user-facing synonym of inactiveState: Grafana's UI labels a
// non-firing rule "Normal" while the API calls it "inactive". It is exported so the `--state`
// flag help has one source for the alias name, and is deliberately not part of KnownStates -
// no rule ever reports it as its state.
const NormalStateAlias = "normal"

// alertingRuleType and recordingRuleType are the two values Grafana's prometheus-compatible
// rules endpoint reports in a rule's `type` field, and the two values Rule.Type can take. A
// provisioned rule is a recording rule exactly when its Record field is set.
const (
	alertingRuleType  = "alerting"
	recordingRuleType = "recording"
)

// The values Rule.State can take. firing, pending, recovering and inactive come from the state
// endpoint verbatim; the other three are assigned by the join:
//
//   - recordingState: recording rules have no evaluation state by nature, so they get their own
//     value rather than looking like a broken join.
//   - unknownState: the state view was read successfully but had no entry for this alerting rule,
//     i.e. Grafana has not evaluated it yet.
//   - unavailableState: the state view could not be read at all (a non-401 failure), so *no*
//     alerting rule's state is known. It is deliberately distinct from unknownState: it is the
//     only thing in the machine-readable output that says the state half of every row is missing,
//     and without it degraded mode would be visible on stderr only - `-o json` would carry a
//     plausible "unknown" for every rule and a jq pipe could not tell an outage from a fleet of
//     never-evaluated rules (which is also why `--state unknown` must not be answerable in
//     degraded mode; see stateDependentFilter).
const (
	firingState      = "firing"
	pendingState     = "pending"
	recoveringState  = "recovering"
	inactiveState    = "inactive"
	unknownState     = "unknown"
	recordingState   = "recording"
	unavailableState = "unavailable"
)

// zeroDuration is the For value of a rule with no pending period, matching formatDuration(0).
const zeroDuration = "0s"

// stateUnavailableWarning is logged when the state endpoint fails with anything other than a 401
// on a request that does not depend on state. It names both consequences of the degraded mode,
// since neither is obvious from the table alone: every STATE reads "unavailable", and folder
// names are unavailable too so FOLDER is empty for every rule.
//
// The warning is the human-facing half of the signal only; the machine-readable half is the
// "unavailable" state stamped on every row (see unavailableState), because stderr is invisible to
// a `-o json | jq` pipe.
//
// A request that *does* depend on state fails instead of warning; see stateDependentFilter.
const stateUnavailableWarning = "alert rule state unavailable; STATE shows unavailable and FOLDER is empty for every rule"

// KnownStates returns every value Rule.State can take, in the order they are documented to the
// user. It is a function rather than a package-level slice so no caller can mutate the shared
// list (and so gochecknoglobals stays quiet).
//
// The user-facing --state flag additionally accepts "normal" as an alias of "inactive"; see
// NormalizeState.
func KnownStates() []string {
	return []string{
		firingState, pendingState, recoveringState, inactiveState,
		unknownState, recordingState, unavailableState,
	}
}

// NormalizeState canonicalizes a user-supplied rule state: surrounding whitespace is trimmed,
// the value is lowercased, and "normal" is mapped onto "inactive" (the API's name for the same
// thing). The empty string (and a blank one) normalizes to the empty string, meaning "no state
// filter".
//
// The result is not validated - use ValidateState for that.
func NormalizeState(state string) string {
	normalized := strings.ToLower(strings.TrimSpace(state))
	if normalized == NormalStateAlias {
		return inactiveState
	}

	return normalized
}

// ValidateState normalizes a user-supplied rule state (see NormalizeState) and rejects anything
// that is neither empty nor one of KnownStates, reporting the value as the user typed it.
//
// It is the single validation path for the `--state` filter: both the command layer's
// fast pre-network check and ListRules' own guard call it, so there is exactly one accepted set
// of values and exactly one error message.
func ValidateState(state string) (string, error) {
	normalized := NormalizeState(state)
	if normalized == "" || slices.Contains(KnownStates(), normalized) {
		return normalized, nil
	}

	return "", fmt.Errorf(
		"invalid rule state %q: expected one of %s",
		state, strings.Join(append(KnownStates(), NormalStateAlias), ", "),
	)
}

// ListRules returns every alert rule visible to gCtx's session, with its definition joined onto
// its live evaluation state, filtered by opts and sorted by folder, group and title.
//
// The definitions come from the provisioning API via gClient; the state comes from
// FetchRuleStates. State entries with no matching definition are ignored - the provisioning
// list is authoritative for what exists. A definition with no state entry is still listed, and
// still gets its folder path resolved by folder UID where the state response mentioned that
// folder at all (see Rule.applyState).
//
// Failure policy differs between the two calls on purpose:
//
//   - Any provisioning error is fatal, wrapped with the operation name. A 401 stays reachable
//     via errors.As, so the stale-session exit-2 rendering still fires.
//   - A 401 from the state call is likewise fatal and returned unchanged.
//   - Any other state-call failure is fatal too when opts names a filter that cannot be applied
//     without state (see stateDependentFilter): the filter would otherwise be applied to a
//     fabricated state and report an empty list as a success.
//   - Any other state-call failure on a request that does not depend on state is logged as a
//     warning and the command continues with State "unavailable" for every alerting rule:
//     definitions are useful without state, and folder names simply become unavailable (see
//     stateUnavailableWarning). "unavailable" rather than "unknown" is what makes the degraded
//     run detectable in the machine-readable output and not on stderr alone.
//
// The returned slice is never nil, so `-o json` emits [] rather than null.
func ListRules(
	ctx context.Context,
	gCtx *config.Context,
	gClient *goapi.GrafanaHTTPAPI,
	opts ListRulesOptions,
) ([]Rule, error) {
	if gClient == nil {
		return nil, errors.New("alerts: no grafana client configured")
	}

	// Guarded as explicitly as the client is: without it a nil context would only surface as a
	// FetchRuleStates failure, which degraded mode turns into a warning - so a caller bug would
	// print a plausible, state-less rule list and exit 0.
	if gCtx == nil || gCtx.Grafana == nil {
		return nil, errors.New("alerts: no grafana context configured")
	}

	stateFilter, err := ValidateState(opts.State)
	if err != nil {
		return nil, fmt.Errorf("alerts: %w", err)
	}

	defs, err := gClient.Provisioning.GetAlertRulesWithParams(provisioning.NewGetAlertRulesParams().WithContext(ctx))
	if err != nil {
		return nil, fmt.Errorf("alerts: list alert rules: %w", err)
	}

	// The state an alerting rule with no state entry is reported as. It is "unknown" - not yet
	// evaluated - while the state view is healthy, and "unavailable" once reading it has failed,
	// which is the only trace of the degraded run the output itself carries.
	unjoinedState := unknownState

	states, err := FetchRuleStates(ctx, gCtx)
	if err != nil {
		if isUnauthorized(err) {
			return nil, err
		}

		if filter := stateDependentFilter(opts.Folder, stateFilter); filter != "" {
			return nil, fmt.Errorf("alerts: cannot apply %s without the alert rule state: %w", filter, err)
		}

		unjoinedState = unavailableState

		logging.FromContext(ctx).Warn(stateUnavailableWarning, logs.Err(err))
	}

	rules := make([]Rule, 0, len(defs.Payload))

	for _, def := range defs.Payload {
		if def == nil {
			continue
		}

		rule := ruleFromProvisioned(def)
		rule.applyState(states, unjoinedState)

		if !rule.matches(opts.Folder, stateFilter) {
			continue
		}

		rules = append(rules, rule)
	}

	sortRules(rules)

	return rules, nil
}

// ListRulesOptions holds the client-side filters ListRules applies to the joined rule list. A
// zero value means "no filtering".
type ListRulesOptions struct {
	// Folder matches a rule's folder UID, its full folder path ("Parent/Child") or that path's
	// last segment ("Child") - exactly and case-sensitively in all three cases. The last-segment
	// form matches *every* folder with that title regardless of its parent, so it can span two
	// folders ("A/Reports" and "B/Reports" both match "Reports"); see matchesFolder.
	//
	// Folder paths come only from the state endpoint, so a non-empty Folder makes a failed state
	// call fatal rather than degrading; see ListRules and stateDependentFilter. For the same
	// reason the path and leaf forms cannot reach a folder the state response never mentions -
	// one whose every rule is unevaluated; such rules match by UID only.
	Folder string
	// State matches a rule's joined state, case-insensitively. It is normalized via NormalizeState
	// and validated against KnownStates, so an unrecognized value is an error rather than an empty
	// result.
	//
	// Every value except "recording" and "unavailable" needs the state call to have succeeded, so
	// it makes a failed state call fatal rather than degrading; see stateDependentFilter.
	State string
}

// Rule is one alert rule as `grafanapi alerts rules list` reports it: the provisioning
// definition (UID through Updated) joined with the live evaluation state (State through
// ActiveAlerts).
//
// It is the single output shape for all three output formats - the json and yaml codecs encode
// it directly, the table codec renders a fixed subset of its fields.
type Rule struct {
	UID   string `json:"uid" yaml:"uid"`
	Title string `json:"title" yaml:"title"`
	// Type is "alerting" or "recording".
	Type      string `json:"type" yaml:"type"`
	FolderUID string `json:"folderUID" yaml:"folderUID"`
	// Folder is the folder's *full path*: a nested folder reads "Parent/Child", not "Child". It
	// comes only from the state endpoint - either from the rule's own state entry or from the
	// folder path a group object carries (see applyState) - so it is empty when that endpoint was
	// unavailable, or when no group in its response mentioned this rule's folder at all.
	Folder    string `json:"folder,omitempty" yaml:"folder,omitempty"`
	RuleGroup string `json:"ruleGroup" yaml:"ruleGroup"`
	// For is the rule's pending period, formatted for humans ("1m", not "1m0s").
	For         string            `json:"for" yaml:"for"`
	IsPaused    bool              `json:"isPaused" yaml:"isPaused"`
	Provenance  string            `json:"provenance,omitempty" yaml:"provenance,omitempty"`
	Labels      map[string]string `json:"labels,omitempty" yaml:"labels,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty" yaml:"annotations,omitempty"`
	// Updated is nil when the server reported no update timestamp, so output omits the key
	// rather than showing a zero time - the provisioning model marks `updated` optional.
	Updated *time.Time `json:"updated,omitempty" yaml:"updated,omitempty"`
	// State is one of KnownStates. "unavailable" is the one value that says nothing about the rule
	// itself: it means the state view could not be read, so every alerting rule in the same
	// response carries it (see unavailableState) - automation reading this field should treat it
	// as "no state was fetched", not as a rule state. "unknown" is the narrower claim that the
	// state view was read and simply had no entry for this rule yet.
	State  string `json:"state" yaml:"state"`
	Health string `json:"health,omitempty" yaml:"health,omitempty"`
	// LastEvaluation is nil when the rule has never been evaluated or could not be joined onto
	// a state entry, so json output omits the key entirely rather than showing a zero time.
	LastEvaluation *time.Time `json:"lastEvaluation,omitempty" yaml:"lastEvaluation,omitempty"`
	// ActiveAlerts counts the rule's alert instances whose state is not Normal.
	ActiveAlerts int `json:"activeAlerts" yaml:"activeAlerts"`
}

// applyState merges r's entry in the state view into r. A missing entry yields a zero RuleState,
// which assigns exactly the zero values ruleFromProvisioned already left in place, so the state
// half needs no "was it found" flag.
//
// The folder path is the one field that does not come from the per-rule entry alone: a rule with
// no entry still belongs to a folder, and the *group* objects of the same response name that
// folder's path next to its UID. So a join miss falls back to states.FolderPaths, which is what
// keeps FOLDER populated - and --folder by path or leaf title working - for a rule that has never
// been evaluated. The fallback cannot resolve a folder no group in the response mentions at all
// (every one of its rules unevaluated); those keep an empty Folder, as they must.
//
// The State field is decided last, because a recording rule must report recordingState whether
// or not the state endpoint listed it, and an alerting rule with no usable state must report
// unjoined rather than an empty column. unjoined is what ListRules assigns to an alerting rule the
// state view did not list: unknownState when that view was read successfully (the rule is simply
// not evaluated yet) and unavailableState when it could not be read at all. Keeping it a
// parameter rather than a constant is what makes the difference visible in the output, so a
// degraded run is detectable from `-o json` and not from stderr alone.
func (r *Rule) applyState(states RuleStates, unjoined string) {
	state := states.ByUID[r.UID]

	r.Folder = state.Folder
	if r.Folder == "" {
		r.Folder = states.FolderPaths[r.FolderUID]
	}

	r.Health = state.Health
	r.LastEvaluation = state.LastEvaluation
	r.ActiveAlerts = state.ActiveAlerts

	switch {
	case r.Type == recordingRuleType:
		r.State = recordingState
	case state.State != "":
		r.State = state.State
	default:
		r.State = unjoined
	}
}

// matches reports whether r passes the (already normalized) folder and state filters. An empty
// filter matches everything; see ListRulesOptions.Folder for the folder matching rule.
//
// The state comparison is case-insensitive on purpose. The filter has been lowercased by
// NormalizeState, while r.State is whatever the state endpoint sent verbatim for a joined rule -
// today lowercase, but a capitalized upstream value would otherwise make every --state filter
// match nothing and report it as a successful empty list.
func (r *Rule) matches(folder, state string) bool {
	if folder != "" && !r.matchesFolder(folder) {
		return false
	}

	return state == "" || strings.EqualFold(r.State, state)
}

// matchesFolder implements the three ways --folder can name a folder: its UID, its full path
// ("Parent/Child") or its last path segment ("Child"). All three are exact and case-sensitive -
// there is no prefix, substring or glob matching, and a parent never matches its children.
//
// The leaf segment is accepted because the full path is a state-endpoint concept the user never
// sees anywhere else - the Grafana UI, the provisioning API and the folder picker all show the
// leaf title - so requiring the whole path would make --folder "Child" silently return nothing.
//
// Consequence, deliberate and documented for the user: the leaf form is a match on the folder's
// *title*, not on one folder. Grafana allows the same title under two different parents, so
// --folder "Reports" matches the rules of both "A/Reports" and "B/Reports". The rows say which is
// which - FOLDER carries the full path - and naming one of those paths in full selects just that
// folder.
//
// The path and leaf forms depend on r.Folder having been resolved, which applyState does from the
// rule's own state entry or, failing that, from the folder paths the state response's group
// objects carry. One residual case cannot be resolved: a folder that has no group in the state
// response at all - i.e. every rule in it is unevaluated - has no path anywhere in the payload, so
// its rules match by UID only.
func (r *Rule) matchesFolder(folder string) bool {
	return r.FolderUID == folder || r.Folder == folder || folderLeaf(r.Folder) == folder
}

// ruleFromProvisioned converts a provisioning definition into a Rule, leaving the state half
// zeroed for applyState to fill in.
//
// Almost every field of models.ProvisionedAlertRule is a pointer or a strfmt wrapper, and a
// server that omits a "required" field would otherwise panic the command, so every deref is
// guarded.
func ruleFromProvisioned(def *models.ProvisionedAlertRule) Rule {
	rule := Rule{
		UID:         def.UID,
		Type:        alertingRuleType,
		For:         zeroDuration,
		IsPaused:    def.IsPaused,
		Provenance:  string(def.Provenance),
		Labels:      def.Labels,
		Annotations: def.Annotations,
	}

	if updated := time.Time(def.Updated); !updated.IsZero() {
		rule.Updated = &updated
	}

	if def.Title != nil {
		rule.Title = *def.Title
	}

	if def.FolderUID != nil {
		rule.FolderUID = *def.FolderUID
	}

	if def.RuleGroup != nil {
		rule.RuleGroup = *def.RuleGroup
	}

	if def.For != nil {
		rule.For = formatDuration(time.Duration(*def.For))
	}

	if def.Record != nil {
		rule.Type = recordingRuleType
	}

	return rule
}

// formatDuration renders d the way Grafana's alerting UI does: time.Duration.String() with a
// trailing zero component trimmed, so 1m0s reads "1m" and 1h0m0s reads "1h". Only whole
// trailing components are dropped, so 10s stays "10s" and 1h0m30s stays "1h0m30s".
func formatDuration(d time.Duration) string {
	if d <= 0 {
		return zeroDuration
	}

	formatted := d.String()

	if strings.HasSuffix(formatted, "m0s") {
		formatted = strings.TrimSuffix(formatted, "0s")
	}

	if strings.HasSuffix(formatted, "h0m") {
		formatted = strings.TrimSuffix(formatted, "0m")
	}

	return formatted
}

// folderLeaf returns the last segment of a folder full path, i.e. the folder's own title:
// "Parent/Child" -> "Child", "Root" -> "Root". A path with no separator is returned unchanged.
//
// The escaping rule, verified against Grafana 12.3.2:
//
//	folder titles              reported path
//	"Probe Folder" / "a/b"     `Probe Folder/a\/b`
//	`bs\` / "Child"            `bs\/Child`
//
// Grafana escapes a `/` inside a title as `\/`, so a `/` preceded by a backslash is part of a
// title rather than a separator, and that escape is undone in the returned segment - a folder
// literally titled "a/b" therefore still matches --folder "a/b".
//
// Grafana does *not* escape a literal backslash, which makes the path genuinely ambiguous for a
// title ending in one: `bs\/Child` is the exact same string whether it is the folder `bs\` with
// the child `Child`, or a root folder titled `bs/Child`. Nothing in the payload can tell the two
// apart, so this reads it the way that serves the common case - as one segment, the title
// "bs/Child" - and a rule in a child of a backslash-terminated folder is therefore reachable
// only via --folder with its UID or its full path, not its leaf title. The behaviour is pinned
// by the backslash cases in TestListRules_FolderFilterNestedPath so it cannot drift silently.
func folderLeaf(path string) string {
	for i := len(path) - 1; i >= 0; i-- {
		if path[i] != '/' || (i > 0 && path[i-1] == '\\') {
			continue
		}

		return strings.ReplaceAll(path[i+1:], `\/`, "/")
	}

	return strings.ReplaceAll(path, `\/`, "/")
}

// sortRules orders rules by folder path, then group, then title, with the UID as a final
// tie-break so the output is stable even for two identically named rules. Folder paths are
// empty in degraded mode, in which case the group and title still order the output.
func sortRules(rules []Rule) {
	slices.SortStableFunc(rules, func(a, b Rule) int {
		return cmp.Or(
			cmp.Compare(a.Folder, b.Folder),
			cmp.Compare(a.RuleGroup, b.RuleGroup),
			cmp.Compare(a.Title, b.Title),
			cmp.Compare(a.UID, b.UID),
		)
	})
}

// stateDependentFilter names the filter that cannot be answered without the live state view, or
// returns "" when the (already normalized) filters do not depend on state at all. It is what
// decides whether a failed state call degrades to a warning or fails the command.
//
// A --folder value always depends on state: folder paths come only from the state endpoint, and a
// UID cannot be told apart from a path or a title, so without state the filter would silently
// match a subset of what the user asked for (only the UID form still matches) - or, far more
// likely, nothing at all.
//
// A --state value depends on state except for the two values the join assigns without consulting
// the state view at all: "recording" (decided by the definition's Record field) and "unavailable"
// (which is exactly what every alerting rule is stamped with once the state view could not be
// read - so `--state unavailable` is the way to list rules *during* an outage). Those two stay
// answerable, so they keep degrading rather than failing.
//
// "unknown" is deliberately not in that set even though the join assigns it too: it means "the
// state view was read and had no entry for this rule", which is a claim only a healthy state view
// can support. Exempting it would silently redefine the filter as "every alerting rule" the moment
// the state call failed, and answer with exit 0.
//
// When both filters depend on state, --folder is reported: it is the one that cannot be applied in
// any form.
func stateDependentFilter(folder, state string) string {
	switch {
	case folder != "":
		return "--folder"
	case state != "" && state != recordingState && state != unavailableState:
		return "--state " + state
	default:
		return ""
	}
}

// isUnauthorized reports whether err carries a 401 from either the generated client or the
// raw-HTTP state call. Both surface it as a *runtime.APIError, which is exactly what
// cmd/grafanapi/fail turns into the stale-session message and exit code 2.
func isUnauthorized(err error) bool {
	var apiErr *runtime.APIError

	return errors.As(err, &apiErr) && apiErr.Code == http.StatusUnauthorized
}
