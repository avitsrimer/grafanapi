// Package explore implements the `grafanapi explore` command: it resolves a Grafana datasource
// (by UID, then by name), builds a single ad-hoc query for its type, executes it against
// /api/ds/query, and renders the result as a table (default) or as json/yaml for piping.
//
// This package is a thin Cobra wiring layer: all domain logic (datasource resolution, query
// building, request execution, and rendering) lives in internal/explore and is fully
// unit-testable without Cobra.
package explore

import (
	"errors"
	"fmt"
	"strings"
	"time"

	cmdconfig "github.com/grafana/grafanapi/cmd/grafanapi/config"
	cmdio "github.com/grafana/grafanapi/cmd/grafanapi/io"
	"github.com/grafana/grafanapi/internal/explore"
	"github.com/grafana/grafanapi/internal/grafana"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// Command returns the `explore` command.
func Command() *cobra.Command {
	opts := &Options{}
	configOpts := &cmdconfig.Options{}

	cmd := &cobra.Command{
		Use:   "explore DATASOURCE QUERY",
		Args:  cobra.ExactArgs(2),
		Short: "Run an ad-hoc query against a Grafana datasource",
		Long: `Run a single ad-hoc query against a Grafana datasource and print the result, mirroring
Grafana's Explore UI.

DATASOURCE is resolved first as a UID, then as a name. QUERY is mapped onto the
datasource-type-appropriate request field: "expr" for Prometheus/Loki, "rawSql" for SQL
datasources, "target" for Graphite, "query" for Elasticsearch/InfluxDB (override with --field).

This runs a single query (fixed refId "A"); there is no multi-query or query-history support.`,
		Example: `
	# Prometheus/Loki:
	grafanapi explore my-prometheus "up"

	# SQL (rawSql + format:"table" are set automatically):
	grafanapi explore my-postgres "select 1 as n"

	# Pipe JSON output to jq:
	grafanapi explore my-prometheus "up" -o json | jq '.results.A.frames[0].schema'`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runExplore(cmd, configOpts, opts, args[0], args[1])
		},
	}

	configOpts.BindFlags(cmd.Flags())
	opts.BindFlags(cmd.Flags())

	return cmd
}

// Options holds the flags accepted by `explore`.
type Options struct {
	IO cmdio.Options

	Field         string
	Params        []string
	From          string
	To            string
	MaxDataPoints int
	Interval      string
	Instant       bool
}

func (opts *Options) BindFlags(flags *pflag.FlagSet) {
	opts.IO.RegisterCustomCodec("table", &tableCodec{})
	opts.IO.DefaultFormat("table")
	opts.IO.BindFlags(flags)

	flags.StringVar(&opts.Field, "field", "", "Query field to use instead of the type-inferred one (e.g. \"expr\", \"rawSql\"); required for unrecognized datasource types")
	flags.StringArrayVar(&opts.Params, "param", nil, "Additional query parameter as key=value (repeatable); values are parsed as JSON when possible, otherwise kept as a string")
	flags.StringVar(&opts.From, "from", "now-1h", "Start of the query time range, passed through to Grafana verbatim")
	flags.StringVar(&opts.To, "to", "now", "End of the query time range, passed through to Grafana verbatim")
	flags.IntVar(&opts.MaxDataPoints, "max-data-points", 1000, "Maximum number of data points to request")
	flags.StringVar(&opts.Interval, "interval", "", "Minimum query interval, e.g. \"15s\" (default: let Grafana choose)")
	flags.BoolVar(&opts.Instant, "instant", false, "Run an instant query instead of a range query")
}

// Validate checks the output format and every flag that BindFlags cannot validate on its own
// (--param's key=value shape, --interval's duration syntax, non-empty --from/--to).
//
// --param and --interval are re-validated in internal/explore (parseParam, BuildQuery) even
// though they are checked here first. That duplication is deliberate, not an oversight: this
// check gives a fast, pre-network command-line error (before LoadConfig/ResolveDataSource run),
// while internal/explore's own check keeps BuildQuery safe for any other caller that does not go
// through this command's Validate.
func (opts *Options) Validate() error {
	if err := opts.IO.Validate(); err != nil {
		return err
	}

	for _, param := range opts.Params {
		if _, _, found := strings.Cut(param, "="); !found {
			return fmt.Errorf("invalid --param %q: expected key=value", param)
		}
	}

	if opts.Interval != "" {
		if _, err := time.ParseDuration(opts.Interval); err != nil {
			return fmt.Errorf("invalid --interval %q: %w", opts.Interval, err)
		}
	}

	if opts.From == "" {
		return errors.New("--from must not be empty")
	}

	if opts.To == "" {
		return errors.New("--to must not be empty")
	}

	return nil
}

// queryOptions adapts Options to the internal/explore.QueryOptions BuildQuery expects.
func (opts *Options) queryOptions() explore.QueryOptions {
	return explore.QueryOptions{
		Field:         opts.Field,
		MaxDataPoints: opts.MaxDataPoints,
		Interval:      opts.Interval,
		Instant:       opts.Instant,
		Params:        opts.Params,
	}
}

// runExplore is the `explore` command's RunE body: validate flags, load the current context,
// resolve the datasource, build and execute the query, then encode the response.
func runExplore(cmd *cobra.Command, configOpts *cmdconfig.Options, opts *Options, datasourceRef, queryStr string) error {
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

	ds, err := explore.ResolveDataSource(gClient, datasourceRef)
	if err != nil {
		return err
	}

	query, err := explore.BuildQuery(ds, queryStr, opts.queryOptions())
	if err != nil {
		return err
	}

	body := explore.BuildRequest(query, opts.From, opts.To)

	resp, err := explore.Run(ctx, gCtx, body)
	if err != nil {
		return err
	}

	codec, err := opts.IO.Codec()
	if err != nil {
		return err
	}

	return codec.Encode(cmd.OutOrStdout(), resp)
}
