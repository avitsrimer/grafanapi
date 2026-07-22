package explore_test

import (
	"testing"

	"github.com/grafana/grafana-openapi-client-go/models"
	"github.com/grafana/grafanapi/internal/explore"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestQueryFieldForType(t *testing.T) {
	tests := []struct {
		name       string
		dsType     string
		override   string
		wantField  string
		wantExtras map[string]any
		wantErr    string
	}{
		{name: "prometheus", dsType: "prometheus", wantField: "expr"},
		{name: "prometheus mixed case", dsType: "Prometheus", wantField: "expr"},
		{name: "loki", dsType: "loki", wantField: "expr"},
		{name: "mysql defaults to table format", dsType: "mysql", wantField: "rawSql", wantExtras: map[string]any{"format": "table"}},
		{name: "postgres defaults to table format", dsType: "postgres", wantField: "rawSql", wantExtras: map[string]any{"format": "table"}},
		{name: "grafana-postgresql-datasource defaults to table format", dsType: "grafana-postgresql-datasource", wantField: "rawSql", wantExtras: map[string]any{"format": "table"}},
		{name: "mssql defaults to table format", dsType: "mssql", wantField: "rawSql", wantExtras: map[string]any{"format": "table"}},
		{name: "elasticsearch", dsType: "elasticsearch", wantField: "query"},
		{name: "influxdb", dsType: "influxdb", wantField: "query"},
		{name: "graphite", dsType: "graphite", wantField: "target"},
		{
			name:      "override replaces field but keeps type extras",
			dsType:    "mysql",
			override:  "customSql",
			wantField: "customSql", wantExtras: map[string]any{"format": "table"},
		},
		{
			name:      "override on unknown type has no extras",
			dsType:    "some-custom-type",
			override:  "customField",
			wantField: "customField",
		},
		{
			name:    "unknown type without override errors",
			dsType:  "some-custom-type",
			wantErr: `unsupported datasource type "some-custom-type"`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			field, extras, err := explore.QueryFieldForType(tt.dsType, tt.override)

			if tt.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
				assert.Contains(t, err.Error(), "--field")

				return
			}

			require.NoError(t, err)
			assert.Equal(t, tt.wantField, field)
			assert.Equal(t, tt.wantExtras, extras)
		})
	}
}

func TestBuildQuery(t *testing.T) {
	ds := &models.DataSource{UID: "ds-uid", Type: "prometheus"}

	t.Run("base fields", func(t *testing.T) {
		query, err := explore.BuildQuery(ds, "up", explore.QueryOptions{MaxDataPoints: 1000})
		require.NoError(t, err)

		assert.Equal(t, "A", query["refId"])
		assert.Equal(t, map[string]any{"uid": "ds-uid", "type": "prometheus"}, query["datasource"])
		assert.Equal(t, 1000, query["maxDataPoints"])
		assert.Equal(t, "up", query["expr"])
	})

	t.Run("sql type sets format table", func(t *testing.T) {
		sqlDS := &models.DataSource{UID: "sql-uid", Type: "postgres"}
		query, err := explore.BuildQuery(sqlDS, "select 1", explore.QueryOptions{})
		require.NoError(t, err)

		assert.Equal(t, "table", query["format"])
		assert.Equal(t, "select 1", query["rawSql"])
	})

	t.Run("field override", func(t *testing.T) {
		query, err := explore.BuildQuery(ds, "up", explore.QueryOptions{Field: "customExpr"})
		require.NoError(t, err)

		assert.Equal(t, "up", query["customExpr"])
		assert.NotContains(t, query, "expr")
	})

	t.Run("unknown type errors", func(t *testing.T) {
		unknownDS := &models.DataSource{UID: "u", Type: "some-custom-type"}
		_, err := explore.BuildQuery(unknownDS, "q", explore.QueryOptions{})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "some-custom-type")
	})

	t.Run("interval sets intervalMs", func(t *testing.T) {
		query, err := explore.BuildQuery(ds, "up", explore.QueryOptions{Interval: "15s"})
		require.NoError(t, err)

		assert.Equal(t, int64(15000), query["intervalMs"])
	})

	t.Run("invalid interval errors", func(t *testing.T) {
		_, err := explore.BuildQuery(ds, "up", explore.QueryOptions{Interval: "not-a-duration"})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "--interval")
	})

	t.Run("instant sets instant true and range false", func(t *testing.T) {
		query, err := explore.BuildQuery(ds, "up", explore.QueryOptions{Instant: true})
		require.NoError(t, err)

		assert.Equal(t, true, query["instant"])
		assert.Equal(t, false, query["range"])
	})

	t.Run("params parse json when valid", func(t *testing.T) {
		query, err := explore.BuildQuery(ds, "up", explore.QueryOptions{
			Params: []string{"count=5", "enabled=true", "obj={\"a\":1}", "name=bare-word"},
		})
		require.NoError(t, err)

		assert.InEpsilon(t, float64(5), query["count"], 0)
		assert.Equal(t, true, query["enabled"])
		assert.Equal(t, map[string]any{"a": float64(1)}, query["obj"])
		assert.Equal(t, "bare-word", query["name"])
	})

	t.Run("params override generated keys with highest precedence", func(t *testing.T) {
		query, err := explore.BuildQuery(ds, "up", explore.QueryOptions{
			Instant: true,
			Params:  []string{"expr=overridden", "instant=false", "maxDataPoints=42"},
		})
		require.NoError(t, err)

		assert.Equal(t, "overridden", query["expr"])
		assert.Equal(t, false, query["instant"])
		assert.InEpsilon(t, float64(42), query["maxDataPoints"], 0)
	})

	t.Run("invalid param errors", func(t *testing.T) {
		_, err := explore.BuildQuery(ds, "up", explore.QueryOptions{Params: []string{"no-equals-sign"}})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "--param")
	})
}

func TestBuildRequest(t *testing.T) {
	query := map[string]any{"refId": "A", "expr": "up"}

	req := explore.BuildRequest(query, "now-1h", "now")

	require.NotNil(t, req.From)
	require.NotNil(t, req.To)
	assert.Equal(t, "now-1h", *req.From)
	assert.Equal(t, "now", *req.To)
	require.Len(t, req.Queries, 1)
	assert.Equal(t, query, req.Queries[0])
}
