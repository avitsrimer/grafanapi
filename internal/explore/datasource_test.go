package explore_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	goapi "github.com/grafana/grafana-openapi-client-go/client"
	"github.com/grafana/grafanapi/internal/config"
	"github.com/grafana/grafanapi/internal/explore"
	"github.com/grafana/grafanapi/internal/grafana"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newTestClient builds a *goapi.GrafanaHTTPAPI (via grafana.ClientFromContext,
// the exact seam ResolveDataSource is meant to be used with) pointed at an
// httptest.Server.
func newTestClient(t *testing.T, server *httptest.Server) *goapi.GrafanaHTTPAPI {
	t.Helper()

	gCtx := &config.Context{
		Grafana: &config.GrafanaConfig{Server: server.URL},
	}

	client, err := grafana.ClientFromContext(gCtx)
	require.NoError(t, err)

	return client
}

func writeDataSourceJSON(t *testing.T, w http.ResponseWriter, name, uid, dsType string) {
	t.Helper()

	w.Header().Set("Content-Type", "application/json")
	err := json.NewEncoder(w).Encode(map[string]any{
		"id":   1,
		"uid":  uid,
		"name": name,
		"type": dsType,
	})
	assert.NoError(t, err)
}

func writeErrorBody(t *testing.T, w http.ResponseWriter, code int) {
	t.Helper()

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	err := json.NewEncoder(w).Encode(map[string]any{"message": http.StatusText(code)})
	assert.NoError(t, err)
}

func TestResolveDataSource_UIDHit(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/datasources/uid/", func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/api/datasources/uid/prom-uid", r.URL.Path)
		writeDataSourceJSON(t, w, "Prometheus", "prom-uid", "prometheus")
	})
	mux.HandleFunc("/api/datasources/name/", func(w http.ResponseWriter, _ *http.Request) {
		t.Fatal("name lookup should not be called on a UID hit")
	})

	server := httptest.NewServer(mux)
	defer server.Close()

	ds, err := explore.ResolveDataSource(newTestClient(t, server), "prom-uid")
	require.NoError(t, err)
	assert.Equal(t, "prom-uid", ds.UID)
	assert.Equal(t, "Prometheus", ds.Name)
	assert.Equal(t, "prometheus", ds.Type)
}

func TestResolveDataSource_UIDMissNameHit(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/datasources/uid/", func(w http.ResponseWriter, _ *http.Request) {
		writeErrorBody(t, w, http.StatusNotFound)
	})
	mux.HandleFunc("/api/datasources/name/", func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/api/datasources/name/Prometheus", r.URL.Path)
		writeDataSourceJSON(t, w, "Prometheus", "prom-uid", "prometheus")
	})

	server := httptest.NewServer(mux)
	defer server.Close()

	ds, err := explore.ResolveDataSource(newTestClient(t, server), "Prometheus")
	require.NoError(t, err)
	assert.Equal(t, "prom-uid", ds.UID)
	assert.Equal(t, "Prometheus", ds.Name)
}

func TestResolveDataSource_BothMissListsAvailable(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/datasources/uid/", func(w http.ResponseWriter, _ *http.Request) {
		writeErrorBody(t, w, http.StatusNotFound)
	})
	mux.HandleFunc("/api/datasources/name/", func(w http.ResponseWriter, _ *http.Request) {
		writeErrorBody(t, w, http.StatusNotFound)
	})
	mux.HandleFunc("/api/datasources", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		err := json.NewEncoder(w).Encode([]map[string]any{
			{"uid": "prom-uid", "name": "Prometheus", "type": "prometheus"},
			{"uid": "mysql-uid", "name": "MySQL", "type": "mysql"},
		})
		assert.NoError(t, err)
	})

	server := httptest.NewServer(mux)
	defer server.Close()

	_, err := explore.ResolveDataSource(newTestClient(t, server), "does-not-exist")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "does-not-exist")
	assert.Contains(t, err.Error(), "Prometheus")
	assert.Contains(t, err.Error(), "prom-uid")
	assert.Contains(t, err.Error(), "prometheus")
	assert.Contains(t, err.Error(), "MySQL")
	assert.Contains(t, err.Error(), "mysql-uid")
	assert.Contains(t, err.Error(), "mysql")
}

func TestResolveDataSource_BothMissListingFailsWrapsError(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/datasources/uid/", func(w http.ResponseWriter, _ *http.Request) {
		writeErrorBody(t, w, http.StatusNotFound)
	})
	mux.HandleFunc("/api/datasources/name/", func(w http.ResponseWriter, _ *http.Request) {
		writeErrorBody(t, w, http.StatusNotFound)
	})
	mux.HandleFunc("/api/datasources", func(w http.ResponseWriter, _ *http.Request) {
		writeErrorBody(t, w, http.StatusInternalServerError)
	})

	server := httptest.NewServer(mux)
	defer server.Close()

	_, err := explore.ResolveDataSource(newTestClient(t, server), "does-not-exist")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "does-not-exist")
	assert.Contains(t, err.Error(), "listing available datasources")
}

func TestResolveDataSource_NonNotFoundErrorPropagatesUnchanged(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/datasources/uid/", func(w http.ResponseWriter, _ *http.Request) {
		writeErrorBody(t, w, http.StatusUnauthorized)
	})
	mux.HandleFunc("/api/datasources/name/", func(w http.ResponseWriter, _ *http.Request) {
		t.Fatal("name lookup should not be attempted after a non-404 UID error")
	})

	server := httptest.NewServer(mux)
	defer server.Close()

	_, err := explore.ResolveDataSource(newTestClient(t, server), "prom-uid")
	require.Error(t, err)

	var uidUnauthorized interface{ Code() int }
	require.ErrorAs(t, err, &uidUnauthorized)
	assert.Equal(t, http.StatusUnauthorized, uidUnauthorized.Code())
}
