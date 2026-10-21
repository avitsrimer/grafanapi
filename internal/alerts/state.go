package alerts

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/go-openapi/runtime"
	"github.com/grafana/grafanapi/internal/config"
	"github.com/grafana/grafanapi/internal/httputils"
)

// stateTimeout bounds the GET /api/prometheus/grafana/api/v1/rules request FetchRuleStates
// issues. Without it, an unreachable or firewalled Grafana host would hang the calling command
// indefinitely - matches internal/explore.queryTimeout and internal/session.verifyTimeout.
const stateTimeout = 30 * time.Second

// maxErrorBodySnippet bounds how much of a non-2xx response body is read into an error message,
// so a misbehaving server returning an oversized body cannot make FetchRuleStates buffer
// unbounded memory.
const maxErrorBodySnippet = 2048

// normalAlertState is the alert-instance state that does not count towards
// RuleState.ActiveAlerts. The wire `alerts` array lists Normal instances alongside firing ones,
// so the active count has to be computed rather than taken from len(alerts).
//
// It is matched case-insensitively purely defensively: Grafana capitalizes instance states
// ("Normal"), but the same word is lowercased elsewhere in the same payload.
const normalAlertState = "normal"

// statusSuccess is the only `status` value a 200 response from the state endpoint may carry for
// its payload to be usable; anything else means the envelope reports a failure (see statusError).
const statusSuccess = "success"

// stateReasonSeparator is what Grafana puts between an instance's evaluation state and the
// *reason* it is in that state when it renders `alerts[].state`: an instance that evaluated to
// Normal because the query errored arrives as "Normal (Error)", not "Normal" (verified on
// Grafana 12.3.2). Only the part before the separator is the state, so the suffix has to be
// trimmed before comparing against normalAlertState - otherwise every reason-carrying Normal
// instance would be counted as active.
const stateReasonSeparator = " ("

// FetchRuleStates returns the live evaluation state of every alert rule visible to gCtx's
// session, keyed by rule UID, together with the folder paths the same response revealed.
//
// The request goes over the raw-HTTP seam - httputils.NewTransport wrapped with
// gCtx.Grafana.WrapWithSession - rather than the generated client, because
// grafana-openapi-client-go has no service for /api/prometheus/grafana/api/v1/rules at all (see
// the package doc). Wrapping with WrapWithSession is deliberate and load-bearing: it is what
// makes a 401 rotate-and-retry exactly like every other authenticated command.
//
// Status handling:
//   - 200: the body is decoded; a `status` other than "success" is an error.
//   - 401: rotation (if any) has already been attempted by the transport and failed or was
//     exhausted, so the session is truly dead. A *runtime.APIError{Code: 401} is returned
//     unwrapped (never session.ErrUnauthorized) so cmd/grafanapi/fail's convertAuthErrors
//     renders it as the stale-session message, not the login-rejected one.
//   - anything else: a bounded snippet of the body is included in a plain error.
func FetchRuleStates(ctx context.Context, gCtx *config.Context) (RuleStates, error) {
	if gCtx == nil || gCtx.Grafana == nil {
		return RuleStates{}, errors.New("alerts: no grafana context configured")
	}

	stateURL, err := buildStateURL(gCtx.Grafana.Server)
	if err != nil {
		return RuleStates{}, fmt.Errorf("alerts: invalid server address: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, stateURL.String(), nil)
	if err != nil {
		return RuleStates{}, fmt.Errorf("alerts: building request: %w", err)
	}

	req.Header.Set("Accept", "application/json")
	if gCtx.Grafana.OrgID != 0 {
		req.Header.Set("X-Grafana-Org-Id", strconv.FormatInt(gCtx.Grafana.OrgID, 10))
	}

	transport, err := httputils.NewTransport(gCtx)
	if err != nil {
		return RuleStates{}, fmt.Errorf("alerts: %w", err)
	}

	client := &http.Client{
		Timeout:       stateTimeout,
		Transport:     gCtx.Grafana.WrapWithSession(transport),
		CheckRedirect: config.SameOriginRedirects(gCtx.Grafana.Server),
	}

	resp, err := client.Do(req)
	if err != nil {
		return RuleStates{}, fmt.Errorf("alerts: alert rule state request: %w", err)
	}
	defer resp.Body.Close()

	return handleStateResponse(resp)
}

// RuleStates is everything ListRules can learn from one call to the state endpoint: the per-rule
// evaluation state, plus the folder paths the same response revealed.
//
// It is a struct rather than the bare uid-keyed map because those are two independent joins, and a
// rule can need the second without appearing in the first. FolderPaths is built from the *group*
// objects, so it resolves a folder path for a rule that has no state entry at all - one that has
// never been evaluated - which would otherwise render an empty FOLDER and be invisible to
// `--folder <path>` even though the folder itself is right there in the response (see
// Rule.applyState and Rule.matchesFolder).
type RuleStates struct {
	// ByUID is the per-rule state, keyed by rule UID. A rule the endpoint did not list is simply
	// absent, and the zero RuleState a lookup then yields is exactly what the join applies.
	ByUID map[string]RuleState
	// FolderPaths maps a folder UID onto that folder's full path ("Parent/Child"), for every
	// folder the response mentioned. A folder whose rules are *all* unevaluated has no group in
	// the response and therefore no entry here, so its rules keep an empty Folder and stay
	// reachable by `--folder <uid>` only.
	FolderPaths map[string]string
}

// RuleState is the live evaluation state of a single alert rule, as seen by Grafana's
// prometheus-compatible rules endpoint. It is the enrichment half of a Rule: definitions come
// from the provisioning API, this comes from the evaluation engine.
type RuleState struct {
	// Folder is the rule's folder *full path* (the endpoint's `file` field): a nested folder is
	// reported as "Parent/Child", not as its leaf title. The state endpoint is the only source of
	// folder names in this package - the provisioning API exposes UIDs only - and a rule it does
	// not list falls back to RuleStates.FolderPaths for the same value.
	Folder string
	// State is the evaluation state of an alerting rule: inactive, pending, firing or
	// recovering. Recording rules carry no state.
	State string
	// Health is the rule's health: ok, error, nodata or unknown.
	Health string
	// LastEvaluation is the time of the last evaluation, or nil when the rule has never been
	// evaluated (the endpoint reports a zero time in that case).
	LastEvaluation *time.Time
	// ActiveAlerts counts the rule's alert instances whose state is not Normal - i.e. Alerting,
	// Pending, Recovering, NoData or Error. It is an exact instance count: a Normal instance
	// carrying a state reason ("Normal (Error)") is still Normal and does not count, and so does
	// an instance with no state at all (see isNormalInstance). See activeAlerts for why the
	// endpoint's own `totals` map cannot answer this.
	ActiveAlerts int
}

// ruleDiscoveryResponse is the {status, data} envelope of GET
// /api/prometheus/grafana/api/v1/rules.
//
// These are local wire structs rather than the vendored models.RuleDiscovery / models.RuleGroup /
// models.AlertingRule. Unlike internal/explore's dataframes, those generated models are *not*
// lossy - they would round-trip the fields used here. The decision is about ergonomics: they do
// not model the {status, data} envelope at all, and every field arrives as a pointer or a
// strfmt type needing a nil-safe deref for the handful of values this package actually reads.
type ruleDiscoveryResponse struct {
	Status string `json:"status"`
	// Error and ErrorType carry the server's own diagnostic when Status is not "success". They
	// are decoded so that the degraded-mode warning can say *why* the state view was unusable.
	Error     string `json:"error"`
	ErrorType string `json:"errorType"`
	Data      struct {
		Groups []ruleGroup `json:"groups"`
	} `json:"data"`
}

// ruleGroup is one evaluation group within a folder. `file` carries the folder full path, and
// `folderUid` the UID of that same folder - the pair is what lets a folder path be resolved for a
// rule this response does not list at all (see RuleStates.FolderPaths).
type ruleGroup struct {
	File      string         `json:"file"`
	FolderUID string         `json:"folderUid"`
	Rules     []alertingRule `json:"rules"`
}

// alertingRule is a single rule inside a group. Recording rules appear here too, with no State.
//
// The per-rule `totals` map the endpoint also sends is deliberately *not* decoded: see
// activeAlerts for why it cannot answer the only question this package asks of it.
type alertingRule struct {
	UID            string          `json:"uid"`
	State          string          `json:"state"`
	Health         string          `json:"health"`
	LastEvaluation time.Time       `json:"lastEvaluation"`
	Alerts         []alertInstance `json:"alerts"`
}

// alertInstance is one alert instance of a rule; only its state is of interest here.
type alertInstance struct {
	State string `json:"state"`
}

// activeAlerts counts the rule's alert instances whose state is not Normal, by scanning the
// instances themselves.
//
// The response also carries a per-rule `totals` map, which would let this be an O(1) read of an
// already-aggregated count (and would let the request drop the instance arrays via
// `limit_alerts=0`, keeping the response proportional to the number of *rules*). It is not used,
// because `totals` is not a partition of the instance set: one instance can contribute to two
// keys, and which keys depends on the rule's execErrState, which the endpoint does not report.
//
// Verified on Grafana 12.3.2, one instance per rule:
//
//	execErrState  alerts[].state      totals
//	Error         "Alerting"          {"alerting":1}
//	Error         "Error"             {"error":1}
//	OK            "Normal (Error)"    {"normal":1}
//	Alerting      "Alerting (Error)"  {"alerting":1,"error":1}
//
// So `sum(totals) - totals["normal"]` reads 2 for the last row's single active instance, while
// ignoring the "error" key instead reads 0 for the second row's single active instance. Neither
// arithmetic is exact for both, and no other combination can be: the last two rows differ in
// their instance count contribution but not in any field that distinguishes them. Scanning
// `alerts[]` is exact, at the cost of a response that grows with the org's alert-instance count.
func (r alertingRule) activeAlerts() int {
	count := 0

	for _, instance := range r.Alerts {
		if !isNormalInstance(instance.State) {
			count++
		}
	}

	return count
}

// state builds the RuleState for r, normalizing a zero lastEvaluation to nil so callers (and
// JSON output) can distinguish "never evaluated" from a real timestamp.
func (r alertingRule) state(folder string) RuleState {
	rs := RuleState{
		Folder:       folder,
		State:        r.State,
		Health:       r.Health,
		ActiveAlerts: r.activeAlerts(),
	}

	if !r.LastEvaluation.IsZero() {
		lastEval := r.LastEvaluation
		rs.LastEvaluation = &lastEval
	}

	return rs
}

// handleStateResponse implements FetchRuleStates's status-code decision tree; split out so
// FetchRuleStates itself stays focused on request construction.
func handleStateResponse(resp *http.Response) (RuleStates, error) {
	switch resp.StatusCode {
	case http.StatusOK:
		var decoded ruleDiscoveryResponse
		if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
			return RuleStates{}, fmt.Errorf("alerts: decoding alert rule state: %w", err)
		}

		if decoded.Status != statusSuccess {
			return RuleStates{}, statusError(decoded)
		}

		return ruleStatesFrom(decoded), nil
	case http.StatusUnauthorized:
		var payload any
		if snippet := readBodySnippet(resp.Body); snippet != "" {
			payload = snippet
		}

		return RuleStates{}, runtime.NewAPIError("alert rule state", payload, http.StatusUnauthorized)
	default:
		snippet := readBodySnippet(resp.Body)
		if snippet == "" {
			return RuleStates{}, fmt.Errorf("alerts: alert rule state: unexpected status %d", resp.StatusCode)
		}

		return RuleStates{}, fmt.Errorf(
			"alerts: alert rule state: unexpected status %d: %s", resp.StatusCode, snippet,
		)
	}
}

// statusError renders a 200 response whose envelope reports a failure. The server's own
// `errorType`/`error` fields are included whenever *either* is present: they are the only
// diagnostic the caller (and, through ListRules' degraded mode, the user) ever gets for this
// branch, so an envelope carrying only one of the two must not silently drop it.
func statusError(decoded ruleDiscoveryResponse) error {
	msg := fmt.Sprintf("alerts: alert rule state returned status %q", decoded.Status)

	if decoded.ErrorType != "" {
		msg += " (" + decoded.ErrorType + ")"
	}

	if decoded.Error != "" {
		msg += ": " + decoded.Error
	}

	return errors.New(msg)
}

// ruleStatesFrom flattens the group tree into the two maps ListRules joins on.
//
// Rules without a UID are skipped: they cannot be joined onto a provisioning definition, which is
// ByUID's only purpose. A group is likewise only recorded in FolderPaths when it names both halves
// of the mapping - a group carrying no `folderUid` (or no `file`) cannot contribute one, and an
// entry pointing at an empty path would be worse than none, since it would look like a resolved
// folder while matching only an empty --folder.
func ruleStatesFrom(decoded ruleDiscoveryResponse) RuleStates {
	states := RuleStates{
		ByUID:       make(map[string]RuleState),
		FolderPaths: make(map[string]string),
	}

	for _, group := range decoded.Data.Groups {
		if group.FolderUID != "" && group.File != "" {
			states.FolderPaths[group.FolderUID] = group.File
		}

		for _, rule := range group.Rules {
			if rule.UID == "" {
				continue
			}

			states.ByUID[rule.UID] = rule.state(group.File)
		}
	}

	return states
}

// buildStateURL appends /api/prometheus/grafana/api/v1/rules to server, preserving any existing
// path - mirrors buildQueryURL (internal/explore/run.go), buildUserURL
// (internal/session/session.go) and buildRotateURL (internal/config/session_source.go).
//
// No `limit_alerts` is sent, so the response carries every rule's alert instances. That is a
// deliberate size-for-exactness trade: `limit_alerts=0` would keep the response proportional to
// the number of rules rather than to the org's alert-instance count, but it also drops the only
// exact source for ActiveAlerts (see activeAlerts). Any query or fragment already present on the
// configured server address is dropped rather than merged.
func buildStateURL(server string) (*url.URL, error) {
	parsed, err := url.Parse(server)
	if err != nil {
		return nil, err
	}

	parsed.Path = strings.TrimSuffix(parsed.Path, "/") + "/api/prometheus/grafana/api/v1/rules"
	parsed.RawQuery = ""
	parsed.Fragment = ""

	return parsed, nil
}

// isNormalInstance reports whether an `alerts[].state` value denotes a Normal instance, ignoring
// any " (reason)" suffix Grafana appends: "Normal", "Normal (Error)" and "Normal (KeepLast)" are
// all Normal instances and none of them count towards ActiveAlerts.
//
// An instance with no state at all counts as Normal, i.e. as not active. Grafana always sets the
// field, so this only decides what an unexpected payload does: reporting ALERTS as non-zero for a
// rule with nothing firing is the more misleading of the two answers, and it is not something the
// user could investigate - the count is all the output carries.
func isNormalInstance(state string) bool {
	base, _, _ := strings.Cut(state, stateReasonSeparator)
	base = strings.TrimSpace(base)

	return base == "" || strings.EqualFold(base, normalAlertState)
}

// readBodySnippet reads up to maxErrorBodySnippet bytes of r for inclusion in an error message,
// trimming surrounding whitespace. Read errors are ignored: a best-effort, possibly-empty
// snippet is preferable to failing the already-failing request handling.
func readBodySnippet(r io.Reader) string {
	data, _ := io.ReadAll(io.LimitReader(r, maxErrorBodySnippet))

	return strings.TrimSpace(string(data))
}
