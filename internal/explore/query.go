package explore

import (
	"encoding/json"
	"fmt"
	"maps"
	"strings"
	"time"

	"github.com/grafana/grafana-openapi-client-go/models"
)

// typeMapping describes how a datasource type maps onto a /api/ds/query
// query: the field the query string is assigned to, and any extra fields
// that type of query always needs (e.g. SQL's "format":"table").
type typeMapping struct {
	Field  string
	Extras map[string]any
}

// fieldByType maps a datasource type (lowercased) to its query field and
// extra defaults. Compared case-insensitively against models.DataSource.Type.
//
//nolint:gochecknoglobals
var fieldByType = map[string]typeMapping{
	"prometheus":                    {Field: "expr"},
	"loki":                          {Field: "expr"},
	"mysql":                         {Field: "rawSql", Extras: map[string]any{"format": "table"}},
	"postgres":                      {Field: "rawSql", Extras: map[string]any{"format": "table"}},
	"grafana-postgresql-datasource": {Field: "rawSql", Extras: map[string]any{"format": "table"}},
	"mssql":                         {Field: "rawSql", Extras: map[string]any{"format": "table"}},
	"elasticsearch":                 {Field: "query"},
	"influxdb":                      {Field: "query"},
	"graphite":                      {Field: "target"},
}

// QueryFieldForType returns the query field key and extra request fields for
// dsType. When override is non-empty it is used as the key instead of the
// type-inferred one, but the type's extra defaults (e.g. SQL's
// "format":"table") are still returned since they describe the type, not the
// field name. An unrecognized type with no override is an error naming the
// type and suggesting --field.
func QueryFieldForType(dsType, override string) (string, map[string]any, error) {
	mapping, found := fieldByType[strings.ToLower(dsType)]

	if override != "" {
		return override, mapping.Extras, nil
	}

	if !found {
		return "", nil, fmt.Errorf("unsupported datasource type %q: use --field to specify the query field", dsType)
	}

	return mapping.Field, mapping.Extras, nil
}

// BuildQuery assembles a single /api/ds/query query entry for queryStr
// against ds, applying opts in this precedence (lowest to highest): the base
// fields (refId, datasource, maxDataPoints), the datasource type's extra
// defaults, the type-appropriate query field, --interval/--instant, and
// finally --param entries, which can override any of the above.
func BuildQuery(ds *models.DataSource, queryStr string, opts QueryOptions) (map[string]any, error) {
	field, extras, err := QueryFieldForType(ds.Type, opts.Field)
	if err != nil {
		return nil, err
	}

	query := map[string]any{
		"refId": "A",
		"datasource": map[string]any{
			"uid":  ds.UID,
			"type": ds.Type,
		},
		"maxDataPoints": opts.MaxDataPoints,
	}

	maps.Copy(query, extras)

	query[field] = queryStr

	if opts.Interval != "" {
		d, err := time.ParseDuration(opts.Interval)
		if err != nil {
			return nil, fmt.Errorf("invalid --interval %q: %w", opts.Interval, err)
		}

		query["intervalMs"] = d.Milliseconds()
	}

	if opts.Instant {
		query["instant"] = true
		query["range"] = false
	}

	for _, param := range opts.Params {
		key, value, err := parseParam(param)
		if err != nil {
			return nil, err
		}

		query[key] = value
	}

	return query, nil
}

// BuildRequest wraps query into a models.MetricRequest with from/to passed
// through verbatim (relative Grafana time units like "now-1h" are not parsed
// client-side).
func BuildRequest(query map[string]any, from, to string) models.MetricRequest {
	return models.MetricRequest{
		From:    &from,
		To:      &to,
		Queries: []models.JSON{query},
	}
}

// QueryOptions configures BuildQuery.
type QueryOptions struct {
	// Field overrides the type-inferred query field key (e.g. "expr",
	// "rawSql"). Empty means infer it from the datasource type.
	Field string
	// MaxDataPoints is the requested query resolution.
	MaxDataPoints int
	// Interval, when non-empty, is parsed via time.ParseDuration and set as
	// intervalMs.
	Interval string
	// Instant requests a single instant query instead of a range query.
	Instant bool
	// Params are raw "key=value" entries applied last, so they can override
	// any field BuildQuery would otherwise generate. Values are parsed as
	// JSON when possible (so "5", "true", `{"a":1}` become typed), otherwise
	// kept as the raw string.
	Params []string
}

// parseParam splits a raw "key=value" --param entry and decodes its value as
// JSON when possible, falling back to the raw string.
func parseParam(param string) (string, any, error) {
	key, value, found := strings.Cut(param, "=")
	if !found {
		return "", nil, fmt.Errorf("invalid --param %q: expected key=value", param)
	}

	var typed any
	if err := json.Unmarshal([]byte(value), &typed); err == nil {
		return key, typed, nil
	}

	return key, value, nil
}
