package alerts

import (
	"fmt"
	"strings"

	cmdconfig "github.com/grafana/grafanapi/cmd/grafanapi/config"
	cmdio "github.com/grafana/grafanapi/cmd/grafanapi/io"
	"github.com/grafana/grafanapi/internal/alerts"
	"github.com/grafana/grafanapi/internal/grafana"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// rulesListOptions holds the flags accepted by `alerts rules list`.
type rulesListOptions struct {
	IO cmdio.Options

	Folder string
	State  string
}

func (opts *rulesListOptions) BindFlags(flags *pflag.FlagSet) {
	opts.IO.RegisterCustomCodec(tableFormat, &rulesTableCodec{})
	opts.IO.DefaultFormat(tableFormat)
	opts.IO.BindFlags(flags)

	flags.StringVar(&opts.Folder, "folder", "",
		"Only list rules in this folder, matched against its UID, its full path (\"Parent/Child\") "+
			"or that path's last segment, which matches every folder with that title (exact, case-sensitive)")
	flags.StringVar(&opts.State, "state", "",
		fmt.Sprintf("Only list rules in this evaluation state. One of: %s (%q is accepted as an alias of %q)",
			strings.Join(alerts.KnownStates(), ", "),
			alerts.NormalStateAlias, alerts.NormalizeState(alerts.NormalStateAlias)))
}

// Validate checks the output format and the --state value.
//
// --state is validated through the same internal/alerts.ValidateState that ListRules itself
// calls, so there is one accepted set of values and one error message; running it here as well
// is deliberate, the same way explore's Options.Validate re-checks --param, so an invalid value
// fails before LoadConfig and any network call.
func (opts *rulesListOptions) Validate() error {
	if err := opts.IO.Validate(); err != nil {
		return err
	}

	_, err := alerts.ValidateState(opts.State)

	return err
}

// listRulesOptions adapts rulesListOptions to the internal/alerts.ListRulesOptions ListRules
// expects.
func (opts *rulesListOptions) listRulesOptions() alerts.ListRulesOptions {
	return alerts.ListRulesOptions{
		Folder: opts.Folder,
		State:  opts.State,
	}
}

// rulesCmd returns the `alerts rules` parent command. Like its own parent it has no RunE, so
// running it bare prints help and exits 0.
func rulesCmd(configOpts *cmdconfig.Options) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "rules",
		Short: "Inspect and manage alert rules",
		Long: `Inspect and manage Grafana alert rules.

` + "`list`" + ` reports each rule's definition joined with its live evaluation state, and ` + "`get`" + ` shows one
rule in full, queries and thresholds included. ` + "`pause`" + `, ` + "`resume`" + ` and
` + "`delete`" + ` change the named rules in place, addressing them by the UID that listing prints.

Pausing a rule rewrites the whole rule group it lives in, since Grafana has no endpoint for the
flag alone; the group's other rules are sent back untouched. Rules another provisioner owns are
refused rather than overwritten, and all three write commands accept --dry-run.

Listing and getting need the Viewer role; pausing, resuming and deleting need Editor.`,
	}

	cmd.AddCommand(rulesDeleteCmd(configOpts))
	cmd.AddCommand(rulesGetCmd(configOpts))
	cmd.AddCommand(rulesListCmd(configOpts))
	cmd.AddCommand(rulesPauseCmd(configOpts, true))
	cmd.AddCommand(rulesPauseCmd(configOpts, false))

	return cmd
}

// rulesListCmd returns the `alerts rules list` command.
func rulesListCmd(configOpts *cmdconfig.Options) *cobra.Command {
	opts := &rulesListOptions{}

	cmd := &cobra.Command{
		Use:   "list",
		Args:  cobra.NoArgs,
		Short: "List alert rules and their evaluation state",
		Long: `List every alert rule visible to the current context's session.

Each rule's definition (from the provisioning API) is joined with its live evaluation state (from
Grafana's prometheus-compatible rules endpoint) on the rule UID, so a single command answers both
"what is defined" and "what is firing".

STATE reads "recording" for recording rules, which have no evaluation state by nature, "unknown"
for an alerting rule Grafana has not evaluated yet, and "unavailable" when the rule's state could
not be fetched at all. ALERTS counts the rule's alert instances that are not Normal.

FOLDER is the folder's full path, so a nested folder reads "Parent/Child". --folder accepts the
folder UID, that full path, or the path's last segment - which matches every folder with that
title, so two folders sharing a leaf title are selected together.

Folder paths and evaluation states both come from the state endpoint. When it is unavailable an
unfiltered list still succeeds: it warns on stderr and reports every alerting rule with STATE
"unavailable" and an empty FOLDER, so automation reading the output can detect the degraded run
rather than mistaking it for an empty result. A --folder value, or a --state value other than
"unavailable" or "recording", fails instead - filtering on data that could not be fetched would
report an empty list as a success.`,
		Example: `
	grafanapi alerts rules list

	grafanapi alerts rules list --folder "Probe Folder" --state firing

	grafanapi alerts rules list -o json | jq '.[] | select(.state == "firing") | .title'`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runRulesList(cmd, configOpts, opts)
		},
	}

	opts.BindFlags(cmd.Flags())

	return cmd
}

// runRulesList is the `alerts rules list` command's RunE body: validate the flags, load the
// current context, list and filter the rules, then encode the result.
func runRulesList(cmd *cobra.Command, configOpts *cmdconfig.Options, opts *rulesListOptions) error {
	if err := opts.Validate(); err != nil {
		return err
	}

	ctx := cmd.Context()

	cfg, err := configOpts.LoadConfig(ctx)
	if err != nil {
		return err
	}

	gCtx := cfg.GetCurrentContext()

	gClient, err := grafana.ClientFromContext(gCtx)
	if err != nil {
		return err
	}

	rules, err := alerts.ListRules(ctx, gCtx, gClient, opts.listRulesOptions())
	if err != nil {
		return err
	}

	codec, err := opts.IO.Codec()
	if err != nil {
		return err
	}

	return codec.Encode(cmd.OutOrStdout(), rules)
}
