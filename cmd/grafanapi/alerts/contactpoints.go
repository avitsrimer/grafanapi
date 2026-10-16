package alerts

import (
	cmdconfig "github.com/grafana/grafanapi/cmd/grafanapi/config"
	cmdio "github.com/grafana/grafanapi/cmd/grafanapi/io"
	"github.com/grafana/grafanapi/internal/alerts"
	"github.com/grafana/grafanapi/internal/grafana"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// contactPointsListOptions holds the flags accepted by `alerts contact-points list`. There is no
// filter flag: the list is short by nature, and the generated client's only server-side filter
// (by name) buys nothing over a `jq`/`grep` pipe.
type contactPointsListOptions struct {
	IO cmdio.Options
}

func (opts *contactPointsListOptions) BindFlags(flags *pflag.FlagSet) {
	opts.IO.RegisterCustomCodec(tableFormat, &contactPointsTableCodec{})
	opts.IO.DefaultFormat(tableFormat)
	opts.IO.BindFlags(flags)
}

func (opts *contactPointsListOptions) Validate() error {
	return opts.IO.Validate()
}

// contactPointsCmd returns the `alerts contact-points` parent command. Like every other parent in
// this group it has no RunE, so running it bare prints help and exits 0.
func contactPointsCmd(configOpts *cmdconfig.Options) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "contact-points",
		Short: "Inspect notification contact points",
		Long:  "Inspect the notification contact points alert rules deliver to.",
	}

	cmd.AddCommand(contactPointsListCmd(configOpts))

	return cmd
}

// contactPointsListCmd returns the `alerts contact-points list` command.
func contactPointsListCmd(configOpts *cmdconfig.Options) *cobra.Command {
	opts := &contactPointsListOptions{}

	cmd := &cobra.Command{
		Use:   "list",
		Args:  cobra.NoArgs,
		Short: "List notification contact points",
		Long: `List every notification contact point visible to the current context's session.

Contact points are org-wide, not folder-scoped.

Grafana replaces the secure fields of a contact point's settings - the fields its type declares as
secret, such as a Slack token - with "[REDACTED]" server-side. A credential embedded somewhere
else, for example inside a webhook URL or a custom header value, is NOT a secure field and is
returned verbatim, so check the settings before pasting them anywhere. Settings are shown by the
json and yaml output formats only; the table shows NAME, TYPE, UID and PROVENANCE.

An empty PROVENANCE means the contact point is editable in the UI; "api" means it was created
through the provisioning API.`,
		Example: `
	grafanapi alerts contact-points list

	grafanapi alerts contact-points list -o json | jq '.[] | {name, type}'`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runContactPointsList(cmd, configOpts, opts)
		},
	}

	opts.BindFlags(cmd.Flags())

	return cmd
}

// runContactPointsList is the `alerts contact-points list` command's RunE body: validate the
// flags, load the current context, list the contact points, then encode the result. Same order as
// runRulesList, so a validation error never costs a config load or a network call.
func runContactPointsList(cmd *cobra.Command, configOpts *cmdconfig.Options, opts *contactPointsListOptions) error {
	if err := opts.Validate(); err != nil {
		return err
	}

	ctx := cmd.Context()

	cfg, err := configOpts.LoadConfig(ctx)
	if err != nil {
		return err
	}

	gClient, err := grafana.ClientFromContext(cfg.GetCurrentContext())
	if err != nil {
		return err
	}

	points, err := alerts.ListContactPoints(ctx, gClient)
	if err != nil {
		return err
	}

	codec, err := opts.IO.Codec()
	if err != nil {
		return err
	}

	return codec.Encode(cmd.OutOrStdout(), points)
}
