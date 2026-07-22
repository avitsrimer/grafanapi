// Package datasources implements the `grafanapi datasources` command: it lists every datasource
// configured on the current context's Grafana instance.
//
// This package is a thin Cobra wiring layer, mirroring cmd/grafanapi/explore: flag binding and a
// "table" format.Codec adapter live here, while the actual API call goes straight through the
// generated Grafana client (there is no domain logic worth extracting to internal/ for a single
// list call).
package datasources

import (
	"sort"

	cmdconfig "github.com/grafana/grafanapi/cmd/grafanapi/config"
	cmdio "github.com/grafana/grafanapi/cmd/grafanapi/io"
	"github.com/grafana/grafanapi/internal/grafana"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// Command returns the `datasources` command.
func Command() *cobra.Command {
	opts := &Options{}
	configOpts := &cmdconfig.Options{}

	cmd := &cobra.Command{
		Use:   "datasources",
		Args:  cobra.NoArgs,
		Short: "List the datasources configured on the current context",
		Long:  "List every datasource configured on the current context's Grafana instance.",
		Example: `
	grafanapi datasources

	grafanapi datasources -o json | jq '.[].uid'`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runDatasources(cmd, configOpts, opts)
		},
	}

	configOpts.BindFlags(cmd.Flags())
	opts.BindFlags(cmd.Flags())

	return cmd
}

// Options holds the flags accepted by `datasources`.
type Options struct {
	IO cmdio.Options
}

func (opts *Options) BindFlags(flags *pflag.FlagSet) {
	opts.IO.RegisterCustomCodec("table", &tableCodec{})
	opts.IO.DefaultFormat("table")
	opts.IO.BindFlags(flags)
}

func (opts *Options) Validate() error {
	return opts.IO.Validate()
}

// runDatasources is the `datasources` command's RunE body: load the current context, list its
// datasources, sort them by name, then encode the result.
func runDatasources(cmd *cobra.Command, configOpts *cmdconfig.Options, opts *Options) error {
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

	list, err := gClient.Datasources.GetDataSources()
	if err != nil {
		return err
	}

	items := list.Payload
	sort.Slice(items, func(i, j int) bool { return items[i].Name < items[j].Name })

	codec, err := opts.IO.Codec()
	if err != nil {
		return err
	}

	return codec.Encode(cmd.OutOrStdout(), items)
}
