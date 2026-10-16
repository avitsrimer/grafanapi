package alerts

import (
	"context"
	"fmt"
	"io"

	goapi "github.com/grafana/grafana-openapi-client-go/client"
	cmdconfig "github.com/grafana/grafanapi/cmd/grafanapi/config"
	cmdio "github.com/grafana/grafanapi/cmd/grafanapi/io"
	"github.com/grafana/grafanapi/internal/alerts"
	"github.com/grafana/grafanapi/internal/grafana"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// dryRunFlagUsage is the same sentence on all three rule operations: they differ in what they
// write, not in what --dry-run promises.
const dryRunFlagUsage = "If set, the operation is planned and printed, without changing anything in Grafana"

// ruleOpApplyFunc performs one rule operation. All three take the same options - pause and resume
// are the same internal call with RuleOpOptions.Paused flipped, and delete ignores that field - so
// every operation's apply is the internal function itself rather than a closure adapting it.
type ruleOpApplyFunc func(
	ctx context.Context,
	gClient *goapi.GrafanaHTTPAPI,
	uids []string,
	opts alerts.RuleOpOptions,
) ([]alerts.RuleOpResult, error)

// ruleOpOptions holds the flags every rule operation accepts. The rules themselves are positional
// arguments, so --dry-run is the only one.
//
// There is no --force: `resources delete` requires it for un-named selectors only, and these
// commands name every rule they touch on the command line.
type ruleOpOptions struct {
	DryRun bool
}

func (opts *ruleOpOptions) BindFlags(flags *pflag.FlagSet) {
	flags.BoolVar(&opts.DryRun, "dry-run", opts.DryRun, dryRunFlagUsage)
}

// ruleOperation is one rule-level write reduced to what the command layer needs: the call that
// performs it and every word that tells it apart from its siblings. Pause and resume share every
// line of wiring below and differ only in this value, which is why the help wording lives here
// rather than in a second struct beside it.
type ruleOperation struct {
	// verb names the operation in the aggregate error, and in `rules <verb>` itself ("pause").
	verb string
	// done is what a rule the operation wrote is reported as ("paused").
	done string
	// skipped is what a rule that needed no write is reported as ("already paused"). Deleting has
	// no such case - a rule either exists or is not found - so it is empty there.
	skipped string
	// short is the command's one-line help. It is empty for delete, whose help is written out in
	// full at its own command rather than assembled from shared paragraphs.
	short string
	// action opens the long help: what the command does, before the paragraphs pause and resume
	// share. Empty for delete, as short is.
	action string
	// gerund names the operation where a sentence needs it capitalised ("Pausing"). Empty for
	// delete, as short is.
	gerund string
	// paused is the state apply should leave the named rules in. It is meaningless for delete,
	// which ignores it.
	paused bool
	// apply performs the operation against Grafana.
	apply ruleOpApplyFunc
}

// doneLabel is how a written rule is reported. A dry run has changed nothing, so it says so rather
// than claiming the write happened.
func (op ruleOperation) doneLabel(dryRun bool) string {
	if dryRun {
		return "would be " + op.done
	}

	return op.done
}

// newDeleteOperation describes `alerts rules delete`.
func newDeleteOperation() ruleOperation {
	return ruleOperation{verb: "delete", done: "deleted", apply: alerts.DeleteRules}
}

// newPauseOperation describes `alerts rules pause` (paused) and `alerts rules resume` (not
// paused). One write, told apart only by what the user asked for - which is what the paused field
// carries, leaving apply the internal function itself.
func newPauseOperation(paused bool) ruleOperation {
	if paused {
		return ruleOperation{
			verb:    "pause",
			done:    "paused",
			skipped: "already paused",
			short:   "Pause alert rules",
			action:  "Pause the named alert rules, so Grafana stops evaluating them.",
			gerund:  "Pausing",
			paused:  true,
			apply:   alerts.SetRulesPaused,
		}
	}

	return ruleOperation{
		verb:    "resume",
		done:    "resumed",
		skipped: "not paused",
		short:   "Resume alert rules",
		action:  "Resume the named alert rules, so Grafana evaluates them again.",
		gerund:  "Resuming",
		apply:   alerts.SetRulesPaused,
	}
}

// reportRuleOp prints one line per named rule and the summary that closes them, then turns the
// failures into an exit code.
//
// The returned error wraps the first failure instead of only counting them, exactly as reportPush
// does: a rule Grafana refused to write must still render as the permission-denied message, which
// cmd/grafanapi/fail derives from the underlying *runtime.APIError.
//
// Each line is assembled first and scrubbed whole (see cell), exactly as reportPush's are: an
// error text quotes the same untrusted rule title or group name the label does, so scrubbing only
// the label would still let a newline in one of them forge a "✔" line.
func reportRuleOp(out io.Writer, results []alerts.RuleOpResult, op ruleOperation, dryRun bool) error {
	var changed, unchanged int

	var failures []alerts.RuleOpResult

	for _, result := range results {
		switch {
		case result.Err != nil:
			failures = append(failures, result)

			cmdio.Error(out, "%s", cell(fmt.Sprintf("%s %v", ruleOpLabel(result), result.Err)))
		case result.Changed:
			changed++

			cmdio.Success(out, "%s", cell(fmt.Sprintf("%s %s", ruleOpLabel(result), op.doneLabel(dryRun))))
		default:
			unchanged++

			cmdio.Success(out, "%s", cell(fmt.Sprintf("%s %s", ruleOpLabel(result), op.skipped)))
		}
	}

	printer := summaryPrinter(changed+unchanged, len(failures))
	printer(out, "%d rule(s) %s, %d unchanged, %d error(s)",
		changed, op.doneLabel(dryRun), unchanged, len(failures))

	if len(failures) != 0 {
		return fmt.Errorf("%d rule(s) failed to %s: %w", len(failures), op.verb, failures[0].Err)
	}

	return nil
}

// ruleOpLabel names one rule in a result line: the uid the user typed, plus the title and rule
// group Grafana holds for it when the lookup got that far. A uid that could not be looked up at
// all is printed on its own rather than with empty parentheses.
//
// The label is left unscrubbed here on purpose: reportRuleOp scrubs the whole line it ends up in,
// which also covers the error text beside it, and scrubbing twice would only hide that the line -
// not the label - is the unit that has to be safe.
func ruleOpLabel(result alerts.RuleOpResult) string {
	uid, title, group := result.UID, result.Title, result.Group

	if title == "" {
		return uid
	}

	if group == "" {
		return fmt.Sprintf("%s (%s)", uid, title)
	}

	return fmt.Sprintf("%s (%s in %s)", uid, title, group)
}

// rulesDeleteCmd returns the `alerts rules delete` command.
func rulesDeleteCmd(configOpts *cmdconfig.Options) *cobra.Command {
	opts := &ruleOpOptions{}

	cmd := &cobra.Command{
		Use:   "delete UID [UID...]",
		Args:  cobra.MinimumNArgs(1),
		Short: "Delete alert rules",
		Long: `Delete the named alert rules.

Every rule is fetched before it is deleted, so a UID no rule carries is reported as "not found"
rather than as the 500 Grafana answers a DELETE for an unknown UID with. The other named rules are
still deleted.

A rule another provisioner owns - file provisioning, imported Prometheus rules, ... - is refused
rather than deleted: its owner would re-create it on its next run.

Rule UIDs are the ones ` + "`alerts rules list`" + ` prints in its UID column. There is no --force, since every
rule this command deletes is named on the command line; run --dry-run first to see the list.

Deleting needs the Editor role.`,
		Example: `
	grafanapi alerts rules delete rule-uid-1

	grafanapi alerts rules delete rule-uid-1 rule-uid-2 --dry-run`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runRuleOp(cmd, configOpts, opts, args, newDeleteOperation())
		},
	}

	opts.BindFlags(cmd.Flags())

	return cmd
}

// rulesPauseCmd returns the `alerts rules pause` command, or `alerts rules resume` when paused is
// false. The two are one write - a rule group PUT with isPaused flipped - so they share their
// wiring and differ only in their wording.
func rulesPauseCmd(configOpts *cmdconfig.Options, paused bool) *cobra.Command {
	opts := &ruleOpOptions{}
	op := newPauseOperation(paused)

	cmd := &cobra.Command{
		Use:   op.verb + " UID [UID...]",
		Args:  cobra.MinimumNArgs(1),
		Short: op.short,
		Long: op.action + `

Grafana has no endpoint for this: a rule's isPaused flag is flipped by rewriting the whole rule
group it lives in. Naming several rules of one group is therefore a single write, and that group's
other rules are sent back exactly as Grafana reported them.

A rule already in the requested state is reported as such and its group is not rewritten. A group
whose rules are owned by another provisioner - file provisioning, imported Prometheus rules, ... -
or that disagree about their provenance is refused rather than claimed.

Rule UIDs are the ones ` + "`alerts rules list`" + ` prints in its UID column, and the PAUSED column shows
the result. Run --dry-run first to see what a group write would cover.

` + op.gerund + ` needs the Editor role.`,
		Example: `
	grafanapi alerts rules ` + op.verb + ` rule-uid-1

	grafanapi alerts rules ` + op.verb + ` rule-uid-1 rule-uid-2 --dry-run`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runRuleOp(cmd, configOpts, opts, args, op)
		},
	}

	opts.BindFlags(cmd.Flags())

	return cmd
}

// runRuleOp is the RunE body all three rule operations share: load the current context, apply the
// operation to the named rules, then report what happened to each one.
//
// Same order as the rest of the group. There is nothing to validate before the config load - the
// rules are positional arguments Cobra has already counted, and --dry-run is a bool.
//
// The report is printed even when the run was cut short, so the rules that were already handled
// are visible; the operation's own error then wins, so a dead session is still the unwrapped
// *runtime.APIError the exit-2 rendering needs and a 403 the exit-1 permission one.
func runRuleOp(
	cmd *cobra.Command,
	configOpts *cmdconfig.Options,
	opts *ruleOpOptions,
	uids []string,
	op ruleOperation,
) error {
	ctx := cmd.Context()

	cfg, err := configOpts.LoadConfig(ctx)
	if err != nil {
		return err
	}

	gClient, err := grafana.ClientFromContext(cfg.GetCurrentContext())
	if err != nil {
		return err
	}

	results, applyErr := op.apply(ctx, gClient, uids, alerts.RuleOpOptions{
		Paused: op.paused,
		DryRun: opts.DryRun,
	})

	reportErr := reportRuleOp(cmd.OutOrStdout(), results, op, opts.DryRun)

	if applyErr != nil {
		return applyErr
	}

	return reportErr
}
