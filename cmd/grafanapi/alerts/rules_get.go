package alerts

import (
	"errors"
	"strings"

	cmdconfig "github.com/grafana/grafanapi/cmd/grafanapi/config"
	cmdio "github.com/grafana/grafanapi/cmd/grafanapi/io"
	"github.com/grafana/grafanapi/internal/alerts"
	"github.com/grafana/grafanapi/internal/format"
	"github.com/grafana/grafanapi/internal/grafana"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// rulesGetOptions holds the flags accepted by `alerts rules get`.
type rulesGetOptions struct {
	IO cmdio.Options
}

func (opts *rulesGetOptions) BindFlags(flags *pflag.FlagSet) {
	opts.IO.DefaultFormat(string(format.YAML))
	opts.IO.BindFlags(flags)
}

// Validate checks the output format and the UID. Only the machine formats exist here - a rule's
// definition is nested, so there is no table to render - and a blank UID is refused up front:
// the lookup makes no request for an empty uid and would otherwise print a misleading "not found".
func (opts *rulesGetOptions) Validate(uid string) error {
	if err := opts.IO.Validate(); err != nil {
		return err
	}

	if strings.TrimSpace(uid) == "" {
		return errors.New("UID must not be empty")
	}

	return nil
}

// rulesGetCmd returns the `alerts rules get` command.
func rulesGetCmd(configOpts *cmdconfig.Options) *cobra.Command {
	opts := &rulesGetOptions{}

	cmd := &cobra.Command{
		Use:   "get UID",
		Args:  cobra.ExactArgs(1),
		Short: "Show one alert rule's full definition and evaluation state",
		Long: `Show one alert rule: its full definition (queries, condition, thresholds) and its live
evaluation state.

The output has two keys. "rule" is the same summary ` + "`alerts rules list`" + ` prints, state included.
"definition" is the rule as the provisioning API holds it, through the projection ` + "`alerts pull`" + `
writes into a group file, so it reads exactly as the rule does in a pulled bundle (without the
read-only id, updated and provenance keys). ` + "`alerts rules list -o json`" + ` carries no queries, which
is the gap this command closes.

Like an unfiltered list, a failure of the state endpoint is not fatal: the command warns on stderr
and reports state "unavailable". An unknown UID fails with "Alert rule not found".

Needs the Viewer role. Output is yaml by default; json is the other format.`,
		Example: `
	grafanapi alerts rules get rule-uid-1

	grafanapi alerts rules get rule-uid-1 -o json | jq '.definition.data'`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runRulesGet(cmd, configOpts, opts, args[0])
		},
	}

	opts.BindFlags(cmd.Flags())

	return cmd
}

// runRulesGet is the `alerts rules get` command's RunE body: validate, load the current context,
// fetch the rule and encode it.
func runRulesGet(cmd *cobra.Command, configOpts *cmdconfig.Options, opts *rulesGetOptions, uid string) error {
	if err := opts.Validate(uid); err != nil {
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

	detail, err := alerts.GetRule(ctx, gCtx, gClient, uid)
	if err != nil {
		return err
	}

	codec, err := opts.IO.Codec()
	if err != nil {
		return err
	}

	return codec.Encode(cmd.OutOrStdout(), detail)
}
