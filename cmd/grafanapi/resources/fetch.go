package resources

import (
	"context"

	"github.com/grafana/grafanapi/cmd/grafanapi/fail"
	"github.com/grafana/grafanapi/internal/config"
	"github.com/grafana/grafanapi/internal/resources"
	"github.com/grafana/grafanapi/internal/resources/discovery"
	"github.com/grafana/grafanapi/internal/resources/remote"
)

type fetchRequest struct {
	Config             config.NamespacedRESTConfig
	StopOnError        bool
	ExcludeManaged     bool
	ExpectSingleTarget bool
	Processors         []remote.Processor
}

type fetchResponse struct {
	Resources      resources.Resources
	IsSingleTarget bool
	PullSummary    *remote.OperationSummary
}

func fetchResources(ctx context.Context, opts fetchRequest, args []string) (*fetchResponse, error) {
	sels, err := resources.ParseSelectors(args)
	if err != nil {
		return nil, err
	}

	if opts.ExpectSingleTarget && !sels.IsSingleTarget() {
		return nil, fail.DetailedError{
			Summary: "Invalid resource selector",
			Details: "Expected a resource selector targeting a single resource. Example: dashboard/some-dashboard",
		}
	}

	reg, err := discovery.NewDefaultRegistry(ctx, opts.Config)
	if err != nil {
		return nil, err
	}

	filters, err := reg.MakeFilters(discovery.MakeFiltersOptions{
		Selectors:            sels,
		PreferredVersionOnly: true,
	})
	if err != nil {
		return nil, err
	}

	pull, err := remote.NewDefaultPuller(ctx, opts.Config)
	if err != nil {
		return nil, err
	}

	res := fetchResponse{
		IsSingleTarget: sels.IsSingleTarget(),
	}

	req := remote.PullRequest{
		Filters:        filters,
		Resources:      &res.Resources,
		Processors:     opts.Processors,
		ExcludeManaged: opts.ExcludeManaged,
		StopOnError:    opts.StopOnError || sels.IsSingleTarget(),
	}

	summary, err := pull.Pull(ctx, req)
	if err != nil {
		return nil, err
	}

	res.PullSummary = summary

	return &res, nil
}
