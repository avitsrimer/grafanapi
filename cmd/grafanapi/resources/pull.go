package resources

import (
	"errors"
	"fmt"

	cmdconfig "github.com/grafana/grafanapi/cmd/grafanapi/config"
	cmdio "github.com/grafana/grafanapi/cmd/grafanapi/io"
	"github.com/grafana/grafanapi/internal/resources/local"
	"github.com/grafana/grafanapi/internal/resources/process"
	"github.com/grafana/grafanapi/internal/resources/remote"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

const (
	defaultResourcesPath = "./resources"
)

type pullOpts struct {
	IO             cmdio.Options
	OnError        OnErrorMode
	IncludeManaged bool
	Path           string
}

func (opts *pullOpts) setup(flags *pflag.FlagSet) {
	// Bind all the flags
	opts.IO.BindFlags(flags)

	bindOnErrorFlag(flags, &opts.OnError)
	flags.StringVarP(&opts.Path, "path", "p", defaultResourcesPath, "Path on disk in which the resources will be written")
	flags.BoolVar(
		&opts.IncludeManaged,
		"include-managed",
		opts.IncludeManaged,
		"Include resources managed by tools other than grafanapi",
	)
}

func (opts *pullOpts) Validate() error {
	if err := opts.IO.Validate(); err != nil {
		return err
	}

	if opts.Path == "" {
		return errors.New("--path is required")
	}

	return opts.OnError.Validate()
}

func pullCmd(configOpts *cmdconfig.Options) *cobra.Command {
	opts := &pullOpts{}

	cmd := &cobra.Command{
		Use:   "pull [RESOURCE_SELECTOR]...",
		Args:  cobra.ArbitraryArgs,
		Short: "Pull resources from Grafana",
		Long:  "Pull resources from Grafana using a specific format. See examples below for more details.",
		Example: `
	# Everything:

	grafanapi resources pull

	# All instances for a given kind(s):

	grafanapi resources pull dashboards
	grafanapi resources pull dashboards folders

	# Single resource kind, one or more resource instances:

	grafanapi resources pull dashboards/foo
	grafanapi resources pull dashboards/foo,bar

	# Single resource kind, long kind format:

	grafanapi resources pull dashboard.dashboards/foo
	grafanapi resources pull dashboard.dashboards/foo,bar

	# Single resource kind, long kind format with version:

	grafanapi resources pull dashboards.v1alpha1.dashboard.grafana.app/foo
	grafanapi resources pull dashboards.v1alpha1.dashboard.grafana.app/foo,bar

	# Multiple resource kinds, one or more resource instances:

	grafanapi resources pull dashboards/foo folders/qux
	grafanapi resources pull dashboards/foo,bar folders/qux,quux

	# Multiple resource kinds, long kind format:

	grafanapi resources pull dashboard.dashboards/foo folder.folders/qux
	grafanapi resources pull dashboard.dashboards/foo,bar folder.folders/qux,quux

	# Multiple resource kinds, long kind format with version:

	grafanapi resources pull dashboards.v1alpha1.dashboard.grafana.app/foo folders.v1alpha1.folder.grafana.app/qux`,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()

			if err := opts.Validate(); err != nil {
				return err
			}

			codec, err := opts.IO.Codec()
			if err != nil {
				return err
			}

			cfg, err := configOpts.LoadRESTConfig(ctx)
			if err != nil {
				return err
			}

			res, err := fetchResources(cmd.Context(), fetchRequest{
				Config: cfg,
				// Strip server fields from the resources.
				// This includes fields like `resourceVersion`, `uid`, etc.
				Processors: []remote.Processor{
					&process.ServerFieldsStripper{},
				},
				ExcludeManaged: !opts.IncludeManaged,
				StopOnError:    opts.OnError.StopOnError(),
			}, args)
			if err != nil {
				return err
			}

			writer := local.FSWriter{
				Path:        opts.Path,
				Namer:       local.GroupResourcesByKind(opts.IO.OutputFormat),
				Encoder:     codec,
				StopOnError: opts.OnError.StopOnError(),
			}

			if err := writer.Write(ctx, &res.Resources); err != nil {
				return err
			}

			pullSummary := res.PullSummary

			printer := cmdio.Success
			if pullSummary.FailedCount() != 0 {
				printer = cmdio.Warning
				if pullSummary.SuccessCount() == 0 {
					printer = cmdio.Error
				}
			}

			printer(cmd.OutOrStdout(), "%d resources pulled, %d errors", pullSummary.SuccessCount(), pullSummary.FailedCount())

			if opts.OnError.FailOnErrors() && pullSummary.FailedCount() > 0 {
				return fmt.Errorf("%d resource(s) failed to pull", pullSummary.FailedCount())
			}

			return nil
		},
	}

	opts.setup(cmd.Flags())

	return cmd
}
