package explore_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/grafana/grafanapi/cmd/grafanapi/config"
	"github.com/grafana/grafanapi/cmd/grafanapi/explore"
	"github.com/grafana/grafanapi/internal/keychain"
	"github.com/grafana/grafanapi/internal/testutils"
	"github.com/spf13/pflag"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newTestServer scripts the two endpoints `explore` drives end to end: datasource resolution by
// UID and the query POST. Handlers use assert (not require) since they run on the server's own
// goroutine, where a FailNow-based require would only abort the handler, not the test.
func newTestServer(t *testing.T, dsUID, dsName, dsType string, queryStatus int, queryBody map[string]any) *httptest.Server {
	t.Helper()

	mux := http.NewServeMux()
	mux.HandleFunc("/api/datasources/uid/"+dsUID, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		assert.NoError(t, json.NewEncoder(w).Encode(map[string]any{
			"id":   1,
			"uid":  dsUID,
			"name": dsName,
			"type": dsType,
		}))
	})
	mux.HandleFunc("/api/ds/query", func(w http.ResponseWriter, r *http.Request) {
		var decoded map[string]any
		assert.NoError(t, json.NewDecoder(r.Body).Decode(&decoded))

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(queryStatus)
		assert.NoError(t, json.NewEncoder(w).Encode(queryBody))
	})

	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	return server
}

// newTestConfig writes a temp config file whose current context points at server and has org-id
// set (so LoadConfig's validation does not attempt Grafana Cloud stack-id discovery), and seeds
// the fake keychain with a session cookie for that context.
func newTestConfig(t *testing.T, server *httptest.Server) (string, func()) {
	t.Helper()

	configFile := testutils.CreateTempFile(t, "current-context: test\ncontexts:\n  test:\n    grafana:\n      server: "+server.URL+"\n      org-id: 1\n")

	store := testutils.NewFakeKeychainStore()
	require.NoError(t, store.Set(keychain.Account("test"), "the-cookie"))
	restoreStore := config.SetKeychainStore(store)

	return configFile, restoreStore
}

// samplePromResponse returns a fresh /api/ds/query success body (a single Prometheus-style
// time+value frame) for tests that don't care about the specific error/status scripted.
func samplePromResponse() map[string]any {
	return map[string]any{
		"results": map[string]any{
			"A": map[string]any{
				"status": 200,
				"frames": []any{
					map[string]any{
						"schema": map[string]any{
							"name":  "value",
							"refId": "A",
							"fields": []any{
								map[string]any{"name": "Time", "type": "time"},
								map[string]any{"name": "Value", "type": "number"},
							},
						},
						"data": map[string]any{
							"values": []any{
								[]any{1721606400000},
								[]any{1.5},
							},
						},
					},
				},
			},
		},
	}
}

func TestOptions_Validate(t *testing.T) {
	// baseOpts returns Options wired exactly as BindFlags would leave them (table codec
	// registered/defaulted, valid --from/--to), so each subtest only has to break the one thing
	// it targets.
	baseOpts := func() *explore.Options {
		opts := &explore.Options{From: "now-1h", To: "now"}
		opts.BindFlags(pflag.NewFlagSet("explore", pflag.ContinueOnError))

		return opts
	}

	t.Run("IO validate failure surfaces (unknown output format)", func(t *testing.T) {
		opts := baseOpts()
		opts.IO.OutputFormat = "not-a-real-format"

		err := opts.Validate()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "unknown output format")
	})

	t.Run("invalid --param errors", func(t *testing.T) {
		opts := baseOpts()
		opts.Params = []string{"no-equals-sign"}

		err := opts.Validate()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "--param")
	})

	t.Run("invalid --interval errors", func(t *testing.T) {
		opts := baseOpts()
		opts.Interval = "not-a-duration"

		err := opts.Validate()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "--interval")
	})

	t.Run("empty --from errors", func(t *testing.T) {
		opts := baseOpts()
		opts.From = ""

		err := opts.Validate()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "--from")
	})

	t.Run("empty --to errors", func(t *testing.T) {
		opts := baseOpts()
		opts.To = ""

		err := opts.Validate()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "--to")
	})

	t.Run("valid options pass", func(t *testing.T) {
		require.NoError(t, baseOpts().Validate())
	})
}

// Test_ExploreCommand_validateFailureShortCircuits confirms a Validate error (an invalid --param
// here) is returned before any network call is attempted - LoadConfig/ResolveDataSource never run.
func Test_ExploreCommand_validateFailureShortCircuits(t *testing.T) {
	testCase := testutils.CommandTestCase{
		Cmd:     explore.Command(),
		Command: []string{"prom-uid", "up", "--param", "no-equals-sign"},
		Assertions: []testutils.CommandAssertion{
			testutils.CommandErrorContains("--param"),
		},
	}
	testCase.Run(t)
}

// Test_ExploreCommand_loadConfigFailure covers runExplore's LoadConfig error branch: a --config
// flag pointing at a file that does not exist.
func Test_ExploreCommand_loadConfigFailure(t *testing.T) {
	testCase := testutils.CommandTestCase{
		Cmd:     explore.Command(),
		Command: []string{"prom-uid", "up", "--config", "/nonexistent/path/config.yaml"},
		Assertions: []testutils.CommandAssertion{
			testutils.CommandErrorContains(""), // any error: LoadConfig must fail, not panic
		},
	}
	testCase.Run(t)
}

// Test_ExploreCommand_buildQueryFailure covers runExplore's BuildQuery error branch: an
// unrecognized datasource type with no --field override.
func Test_ExploreCommand_buildQueryFailure(t *testing.T) {
	// BuildQuery fails before Run ever POSTs /api/ds/query, so the query endpoint is never hit;
	// queryStatus is intentionally a value distinct from the other newTestServer callers here to
	// make that plain.
	server := newTestServer(t, "custom-uid", "Custom", "some-custom-type", http.StatusTeapot, samplePromResponse())
	configFile, restoreStore := newTestConfig(t, server)
	defer restoreStore()

	testCase := testutils.CommandTestCase{
		Cmd:     explore.Command(),
		Command: []string{"custom-uid", "up", "--config", configFile},
		Assertions: []testutils.CommandAssertion{
			testutils.CommandErrorContains(`unsupported datasource type "some-custom-type"`),
		},
	}
	testCase.Run(t)
}

func Test_ExploreCommand_tableOutput(t *testing.T) {
	server := newTestServer(t, "prom-uid", "Prometheus", "prometheus", http.StatusOK, samplePromResponse())
	configFile, restoreStore := newTestConfig(t, server)
	defer restoreStore()

	testCase := testutils.CommandTestCase{
		Cmd:     explore.Command(),
		Command: []string{"prom-uid", "up", "--config", configFile},
		Assertions: []testutils.CommandAssertion{
			testutils.CommandSuccess(),
			testutils.CommandOutputContains("Value"),
			testutils.CommandOutputContains("2024-07-22T00:00:00Z"),
			testutils.CommandOutputContains("1.5"),
		},
	}
	testCase.Run(t)
}

func Test_ExploreCommand_jsonOutputRoundTrips(t *testing.T) {
	server := newTestServer(t, "prom-uid", "Prometheus", "prometheus", http.StatusOK, samplePromResponse())
	configFile, restoreStore := newTestConfig(t, server)
	defer restoreStore()

	var gotOutput string
	testCase := testutils.CommandTestCase{
		Cmd:     explore.Command(),
		Command: []string{"prom-uid", "up", "--config", configFile, "-o", "json"},
		Assertions: []testutils.CommandAssertion{
			testutils.CommandSuccess(),
			func(_ *testing.T, result testutils.CommandResult) { gotOutput = result.Stdout },
		},
	}
	testCase.Run(t)

	var decoded map[string]any
	require.NoError(t, json.Unmarshal([]byte(gotOutput), &decoded))
	require.Contains(t, decoded, "results")
}

func Test_ExploreCommand_perRefIDErrorExitsNonZero(t *testing.T) {
	errorResponse := map[string]any{
		"results": map[string]any{
			"A": map[string]any{
				"status": 400,
				"error":  "parse error: unexpected identifier",
			},
		},
	}

	server := newTestServer(t, "prom-uid", "Prometheus", "prometheus", http.StatusOK, errorResponse)
	configFile, restoreStore := newTestConfig(t, server)
	defer restoreStore()

	testCase := testutils.CommandTestCase{
		Cmd:     explore.Command(),
		Command: []string{"prom-uid", "up(", "--config", configFile},
		Assertions: []testutils.CommandAssertion{
			testutils.CommandErrorContains("parse error"),
		},
	}
	testCase.Run(t)
}

func Test_ExploreCommand_datasourceNotFound(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/datasources/uid/", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		assert.NoError(t, json.NewEncoder(w).Encode(map[string]any{"message": "Not Found"}))
	})
	mux.HandleFunc("/api/datasources/name/", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		assert.NoError(t, json.NewEncoder(w).Encode(map[string]any{"message": "Not Found"}))
	})
	mux.HandleFunc("/api/datasources", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		assert.NoError(t, json.NewEncoder(w).Encode([]any{}))
	})

	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	configFile, restoreStore := newTestConfig(t, server)
	defer restoreStore()

	testCase := testutils.CommandTestCase{
		Cmd:     explore.Command(),
		Command: []string{"does-not-exist", "up", "--config", configFile},
		Assertions: []testutils.CommandAssertion{
			testutils.CommandErrorContains(`datasource "does-not-exist" not found`),
		},
	}
	testCase.Run(t)
}
