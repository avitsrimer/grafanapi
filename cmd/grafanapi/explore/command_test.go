package explore_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"unicode"

	"github.com/fatih/color"
	"github.com/goccy/go-yaml"
	"github.com/grafana/grafanapi/cmd/grafanapi/config"
	"github.com/grafana/grafanapi/cmd/grafanapi/explore"
	"github.com/grafana/grafanapi/cmd/grafanapi/fail"
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

// setNoColor turns fatih/color off for one test so the renderer's own ESCs do not mask the
// assertions.
func setNoColor(t *testing.T) {
	t.Helper()

	previous := color.NoColor
	color.NoColor = true

	t.Cleanup(func() { color.NoColor = previous })
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

func Test_ExploreCommand_perRefIDErrorIsScrubbedWhenRendered(t *testing.T) {
	setNoColor(t)

	errorResponse := map[string]any{
		"results": map[string]any{
			"A": map[string]any{
				"status": 400,
				"error":  "parse error \u001b]52;c;aGk=\u0007",
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
			func(t *testing.T, result testutils.CommandResult) {
				t.Helper()

				require.Error(t, result.Err)
				// The domain error stays faithful; only the rendering scrubs.
				assert.Contains(t, result.Err.Error(), "\x1b]52;c;aGk=\x07")

				rendered := fail.ErrorToDetailedError(result.Err).Error()
				assert.Contains(t, rendered, "parse error")
				assert.NotContains(t, rendered, "\x1b")
				assert.NotContains(t, rendered, "\x07")
				assert.NotContains(t, rendered, "aGk=")
			},
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

// loadHostileResponse reads the shared hostile /api/ds/query fixture and returns it both as the
// body to script and as the decoded string column the output is compared against.
func loadHostileResponse(t *testing.T) (map[string]any, []string) {
	t.Helper()

	raw, err := os.ReadFile("../../../internal/explore/testdata/hostile_frame.json")
	require.NoError(t, err)

	var body map[string]any
	require.NoError(t, json.Unmarshal(raw, &body))

	return body, hostileColumn(t, body)
}

// hostileColumn extracts the "string" column of the fixture's single frame from a decoded
// response, whichever codec produced it.
func hostileColumn(t *testing.T, body map[string]any) []string {
	t.Helper()

	columns, ok := hostileData(t, body)["values"].([]any)
	require.True(t, ok)
	require.Len(t, columns, 2)

	cells, ok := columns[1].([]any)
	require.True(t, ok)

	out := make([]string, len(cells))
	for i, cell := range cells {
		out[i], ok = cell.(string)
		require.True(t, ok)
	}

	return out
}

// hostileData returns the "data" object of the fixture's single frame.
func hostileData(t *testing.T, body map[string]any) map[string]any {
	t.Helper()

	results, ok := body["results"].(map[string]any)
	require.True(t, ok)

	result, ok := results["A\x1b[2J"].(map[string]any)
	require.True(t, ok, "refID key must round-trip raw")

	frames, ok := result["frames"].([]any)
	require.True(t, ok)
	require.Len(t, frames, 1)

	data, ok := frames[0].(map[string]any)["data"].(map[string]any)
	require.True(t, ok)

	return data
}

// withoutYAMLLossyRows drops the rows the vendored YAML encoder cannot round-trip, so the yaml
// golden check covers everything else byte for byte. goccy/go-yaml drops a raw tab, emits a
// carriage return as a block scalar it cannot read back, and returns U+009D as U+FFFD, in every
// case on the encode path this change does not touch. The json check keeps all of them.
func withoutYAMLLossyRows(t *testing.T, body map[string]any) map[string]any {
	t.Helper()

	data := hostileData(t, body)
	columns, ok := data["values"].([]any)
	require.True(t, ok)

	keep := func(cell any) bool {
		text, isString := cell.(string)

		return !isString || !strings.ContainsAny(text, "\t\r\u009d")
	}

	kept := make([][]any, len(columns))
	text, ok := columns[1].([]any)
	require.True(t, ok)

	for row := range text {
		if !keep(text[row]) {
			continue
		}

		for col := range columns {
			column, ok := columns[col].([]any)
			require.True(t, ok)

			kept[col] = append(kept[col], column[row])
		}
	}

	out := make([]any, len(kept))
	for i := range kept {
		out[i] = kept[i]
	}

	data["values"] = out

	return body
}

func Test_ExploreCommand_tableOutputStripsTerminalControl(t *testing.T) {
	body, _ := loadHostileResponse(t)
	server := newTestServer(t, "prom-uid", "Prometheus", "prometheus", http.StatusOK, body)
	configFile, restoreStore := newTestConfig(t, server)
	defer restoreStore()

	testCase := testutils.CommandTestCase{
		Cmd:     explore.Command(),
		Command: []string{"prom-uid", "up", "--config", configFile},
		Assertions: []testutils.CommandAssertion{
			testutils.CommandSuccess(),
			func(t *testing.T, result testutils.CommandResult) {
				t.Helper()

				for _, r := range result.Stdout {
					if r == '\n' {
						continue
					}

					assert.False(t, unicode.IsControl(r) || unicode.Is(unicode.Cf, r), "unsafe rune %U", r)
				}

				assert.NotContains(t, result.Stdout, "aGk=")
				assert.Len(t, strings.Split(strings.TrimSuffix(result.Stdout, "\n"), "\n"), 12)
			},
		},
	}
	testCase.Run(t)
}

func cloneJSON(t *testing.T, in map[string]any) map[string]any {
	t.Helper()

	raw, err := json.Marshal(in)
	require.NoError(t, err)

	var out map[string]any
	require.NoError(t, json.Unmarshal(raw, &out))

	return out
}

func Test_ExploreCommand_machineFormatsKeepRawValues(t *testing.T) {
	body, want := loadHostileResponse(t)

	for _, format := range []string{"json", "yaml"} {
		t.Run(format, func(t *testing.T) {
			scripted, wantCells := body, want
			if format == "yaml" {
				scripted = withoutYAMLLossyRows(t, cloneJSON(t, body))
				wantCells = hostileColumn(t, scripted)
			}

			server := newTestServer(t, "prom-uid", "Prometheus", "prometheus", http.StatusOK, scripted)
			configFile, restoreStore := newTestConfig(t, server)
			defer restoreStore()

			var gotOutput string
			testCase := testutils.CommandTestCase{
				Cmd:     explore.Command(),
				Command: []string{"prom-uid", "up", "--config", configFile, "-o", format},
				Assertions: []testutils.CommandAssertion{
					testutils.CommandSuccess(),
					func(_ *testing.T, result testutils.CommandResult) { gotOutput = result.Stdout },
				},
			}
			testCase.Run(t)

			var decoded map[string]any
			if format == "json" {
				require.NoError(t, json.Unmarshal([]byte(gotOutput), &decoded))
				assert.Contains(t, gotOutput, `\u001b]52;c;`)
			} else {
				require.NoError(t, yaml.Unmarshal([]byte(gotOutput), &decoded))
			}

			assert.Equal(t, wantCells, hostileColumn(t, decoded))
		})
	}
}
