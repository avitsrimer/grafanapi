package alerts

import (
	"context"
	"errors"
	"fmt"

	"github.com/grafana/grafana-app-sdk/logging"
	goapi "github.com/grafana/grafana-openapi-client-go/client"
	"github.com/grafana/grafanapi/internal/config"
	"github.com/grafana/grafanapi/internal/logs"
)

// GetRule returns one alert rule's full definition together with its live evaluation state.
//
// The rule comes from the provisioning API (fetchRule), the state from the same endpoint ListRules
// reads, and the failure policy matches an unfiltered ListRules:
//
//   - An unknown uid is ErrRuleNotFound, rendered "Alert rule not found" by cmd/grafanapi/fail.
//   - A 401 from either call is fatal and stays reachable via errors.As, so the stale-session
//     exit-2 rendering still fires.
//   - Any other state-call failure is logged as a warning and the rule is reported with State
//     "unavailable", which is the machine-readable trace of the degraded run.
func GetRule(
	ctx context.Context,
	gCtx *config.Context,
	gClient *goapi.GrafanaHTTPAPI,
	uid string,
) (RuleDetail, error) {
	if gClient == nil {
		return RuleDetail{}, errors.New("alerts: no grafana client configured")
	}

	if gCtx == nil || gCtx.Grafana == nil {
		return RuleDetail{}, errors.New("alerts: no grafana context configured")
	}

	def, found, err := fetchRule(ctx, gClient, uid)
	if err != nil {
		return RuleDetail{}, err
	}

	if !found {
		return RuleDetail{}, ruleNotFoundError(uid)
	}

	unjoinedState := unknownState

	states, err := FetchRuleStates(ctx, gCtx)
	if err != nil {
		if isUnauthorized(err) {
			return RuleDetail{}, err
		}

		unjoinedState = unavailableState

		logging.FromContext(ctx).Warn(stateUnavailableWarning, logs.Err(err))
	}

	rule := ruleFromProvisioned(def)
	rule.applyState(states, unjoinedState)

	definition, err := encodableValue(def, stripRuleFields)
	if err != nil {
		return RuleDetail{}, fmt.Errorf("alerts: get rule %s: %w", uid, err)
	}

	return RuleDetail{Rule: rule, Definition: definition}, nil
}

// RuleDetail is one alert rule as `alerts rules get` reports it.
//
// Rule is the summary `alerts rules list` prints for the rule, state included. Definition is the
// provisioning model - queries, condition, thresholds and the rest - through the same projection
// `alerts pull` writes into a group file: the read-only id, updated and provenance keys are removed
// and integral numbers stay integral. It is therefore what the rule looks like in a pulled bundle,
// and it is a generic value rather than a typed one so the key set is identical in json and yaml.
type RuleDetail struct {
	Rule       Rule `json:"rule" yaml:"rule"`
	Definition any  `json:"definition" yaml:"definition"`
}
