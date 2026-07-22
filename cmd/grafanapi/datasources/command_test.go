package datasources_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/grafana/grafanapi/cmd/grafanapi/config"
	"github.com/grafana/grafanapi/cmd/grafanapi/datasources"
	"github.com/grafana/grafanapi/internal/keychain"
	"github.com/grafana/grafanapi/internal/testutils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newTestServer scripts the single endpoint `datasources` drives: GET /api/datasources. Handlers
// use assert (not require) since they run on the server's own goroutine, where a FailNow-based
// require would only abort the handler, not the test.
func newTestServer(t *testing.T, status int, body any) *httptest.Server {
	t.Helper()

	mux := http.NewServeMux()
	mux.HandleFunc("/api/datasources", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		assert.NoError(t, json.NewEncoder(w).Encode(body))
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

// sampleDataSources returns an unsorted /api/datasources payload so table/JSON tests can also
// assert the command sorts by name.
func sampleDataSources() []map[string]any {
	return []map[string]any{
		{"uid": "prometheus-uid", "name": "example-prometheus", "type": "prometheus", "isDefault": true},
		{"uid": "postgres-uid", "name": "example-postgres", "type": "postgres", "isDefault": false},
	}
}

// assertSortedByName confirms "example-postgres" is rendered before "example-prometheus", i.e.
// the command sorted sampleDataSources' unsorted payload by name.
func assertSortedByName(t *testing.T, result testutils.CommandResult) {
	t.Helper()

	postgresIdx := strings.Index(result.Stdout, "example-postgres")
	prometheusIdx := strings.Index(result.Stdout, "example-prometheus")
	require.NotEqual(t, -1, postgresIdx)
	require.NotEqual(t, -1, prometheusIdx)
	assert.Less(t, postgresIdx, prometheusIdx)
}

func Test_DatasourcesCommand_tableOutput(t *testing.T) {
	server := newTestServer(t, http.StatusOK, sampleDataSources())
	configFile, restoreStore := newTestConfig(t, server)
	defer restoreStore()

	testCase := testutils.CommandTestCase{
		Cmd:     datasources.Command(),
		Command: []string{"--config", configFile},
		Assertions: []testutils.CommandAssertion{
			testutils.CommandSuccess(),
			testutils.CommandOutputContains("NAME"),
			testutils.CommandOutputContains("UID"),
			testutils.CommandOutputContains("TYPE"),
			testutils.CommandOutputContains("DEFAULT"),
			testutils.CommandOutputContains("example-postgres"),
			testutils.CommandOutputContains("example-prometheus"),
			testutils.CommandOutputContains("true"),
			testutils.CommandOutputContains("false"),
			// Sorted by name: "example-postgres" must appear before "example-prometheus".
			assertSortedByName,
		},
	}
	testCase.Run(t)
}

func Test_DatasourcesCommand_jsonOutputRoundTrips(t *testing.T) {
	server := newTestServer(t, http.StatusOK, sampleDataSources())
	configFile, restoreStore := newTestConfig(t, server)
	defer restoreStore()

	var gotOutput string
	testCase := testutils.CommandTestCase{
		Cmd:     datasources.Command(),
		Command: []string{"--config", configFile, "-o", "json"},
		Assertions: []testutils.CommandAssertion{
			testutils.CommandSuccess(),
			func(_ *testing.T, result testutils.CommandResult) { gotOutput = result.Stdout },
		},
	}
	testCase.Run(t)

	var decoded []map[string]any
	require.NoError(t, json.Unmarshal([]byte(gotOutput), &decoded))
	require.Len(t, decoded, 2)
	assert.Equal(t, "example-postgres", decoded[0]["name"])
	assert.Equal(t, "example-prometheus", decoded[1]["name"])
}

func Test_DatasourcesCommand_apiErrorPropagates(t *testing.T) {
	server := newTestServer(t, http.StatusInternalServerError, map[string]any{"message": "boom"})
	configFile, restoreStore := newTestConfig(t, server)
	defer restoreStore()

	testCase := testutils.CommandTestCase{
		Cmd:     datasources.Command(),
		Command: []string{"--config", configFile},
		Assertions: []testutils.CommandAssertion{
			testutils.CommandErrorContains(""), // any error: the 500 must surface, not panic
		},
	}
	testCase.Run(t)
}

func Test_DatasourcesCommand_loadConfigFailure(t *testing.T) {
	testCase := testutils.CommandTestCase{
		Cmd:     datasources.Command(),
		Command: []string{"--config", "/nonexistent/path/config.yaml"},
		Assertions: []testutils.CommandAssertion{
			testutils.CommandErrorContains(""), // any error: LoadConfig must fail, not panic
		},
	}
	testCase.Run(t)
}
