package explore_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/go-openapi/runtime"
	"github.com/grafana/grafanapi/cmd/grafanapi/fail"
	"github.com/grafana/grafanapi/internal/config"
	"github.com/grafana/grafanapi/internal/explore"
	"github.com/grafana/grafanapi/internal/keychain"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// nopStore is a keychain.Store that discards writes - used by tests whose SessionSource must
// persist a rotated cookie somewhere, but do not care where.
type nopStore struct{}

func (nopStore) Set(string, string) error   { return nil }
func (nopStore) Get(string) (string, error) { return "", keychain.ErrNotFound }
func (nopStore) Delete(string) error        { return nil }

func testQueryBody() map[string]any {
	return map[string]any{
		"from": "now-1h",
		"to":   "now",
		"queries": []map[string]any{
			{
				"refId": "A",
				"datasource": map[string]any{
					"uid":  "prom-uid",
					"type": "prometheus",
				},
				"expr": `up{job="prometheus"}`,
			},
		},
	}
}

func TestRun_Success(t *testing.T) {
	var gotBody map[string]any
	var gotCookie, gotOrgHeader, gotContentType string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Equal(t, "/api/ds/query", r.URL.Path)

		gotCookie = r.Header.Get("Cookie")
		gotOrgHeader = r.Header.Get("X-Grafana-Org-Id")
		gotContentType = r.Header.Get("Content-Type")

		assert.NoError(t, json.NewDecoder(r.Body).Decode(&gotBody))

		w.Header().Set("Content-Type", "application/json")
		_, err := w.Write([]byte(`{"results":{"A":{"status":200,"frames":[]}}}`))
		assert.NoError(t, err)
	}))
	defer server.Close()

	gCtx := &config.Context{
		Grafana: &config.GrafanaConfig{
			Server:        server.URL,
			SessionCookie: "the-cookie",
			OrgID:         7,
		},
	}

	resp, err := explore.Run(t.Context(), gCtx, testQueryBody())
	require.NoError(t, err)
	require.NotNil(t, resp)

	assert.Equal(t, config.CookieHeaderValue("the-cookie"), gotCookie)
	assert.Equal(t, "7", gotOrgHeader)
	assert.Equal(t, "application/json", gotContentType)

	assert.Equal(t, "now-1h", gotBody["from"])
	assert.Equal(t, "now", gotBody["to"])

	queries, ok := gotBody["queries"].([]any)
	require.True(t, ok)
	require.Len(t, queries, 1)

	query, ok := queries[0].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "A", query["refId"])
	assert.Equal(t, `up{job="prometheus"}`, query["expr"])
}

func TestRun_NoOrgHeaderWhenUnset(t *testing.T) {
	var sawHeader bool

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawHeader = r.Header.Get("X-Grafana-Org-Id") != ""
		w.Header().Set("Content-Type", "application/json")
		_, err := w.Write([]byte(`{"results":{}}`))
		assert.NoError(t, err)
	}))
	defer server.Close()

	gCtx := &config.Context{Grafana: &config.GrafanaConfig{Server: server.URL}}

	_, err := explore.Run(t.Context(), gCtx, testQueryBody())
	require.NoError(t, err)
	assert.False(t, sawHeader, "expected no X-Grafana-Org-Id header to be sent")
}

// TestRun_BodyReplayOnRotate scripts a 401 on the first hit and a 200 on the second, with a real
// config.SessionSource wired in via a dedicated rotate server. It asserts the retried request
// carries the rotated cookie and the byte-identical JSON body (proving req.GetBody replay works
// because Run passes bytes.NewReader(jsonBody), not an arbitrary io.Reader).
func TestRun_BodyReplayOnRotate(t *testing.T) {
	const oldCookie = "old-cookie"
	const newCookie = "new-cookie"

	rotateServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/api/user/auth-tokens/rotate", r.URL.Path)
		http.SetCookie(w, &http.Cookie{
			Name: config.SessionCookieName, Value: newCookie,
			Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode,
		})
		w.WriteHeader(http.StatusOK)
	}))
	defer rotateServer.Close()

	var hits atomic.Int32
	var firstBody, secondBody []byte
	var secondCookie string

	queryServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if !assert.NoError(t, err) {
			return
		}

		switch hits.Add(1) {
		case 1:
			firstBody = body
			w.WriteHeader(http.StatusUnauthorized)
		default:
			secondBody = body
			secondCookie = r.Header.Get("Cookie")
			w.Header().Set("Content-Type", "application/json")
			_, werr := w.Write([]byte(`{"results":{"A":{"status":200,"frames":[]}}}`))
			assert.NoError(t, werr)
		}
	}))
	defer queryServer.Close()

	src := config.NewSessionSource(oldCookie, rotateServer.URL, nil, nopStore{}, "acct")

	gCtx := &config.Context{
		Grafana: &config.GrafanaConfig{
			Server:  queryServer.URL,
			Session: src,
		},
	}

	resp, err := explore.Run(t.Context(), gCtx, testQueryBody())
	require.NoError(t, err)
	require.NotNil(t, resp)

	assert.Equal(t, int32(2), hits.Load())
	assert.Equal(t, config.CookieHeaderValue(newCookie), secondCookie)
	assert.Equal(t, firstBody, secondBody, "retried request must replay the identical JSON body")
	assert.NotEmpty(t, secondBody)
}

func TestRun_MultiStatusPerRefIDErrorSurfacesAsGoError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusMultiStatus)
		_, err := w.Write([]byte(`{"results":{"A":{"status":400,"error":"parse error at char 5"}}}`))
		assert.NoError(t, err)
	}))
	defer server.Close()

	gCtx := &config.Context{Grafana: &config.GrafanaConfig{Server: server.URL}}

	_, err := explore.Run(t.Context(), gCtx, testQueryBody())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "A")
	assert.Contains(t, err.Error(), "parse error at char 5")
}

// TestRun_DeadSessionRendersStaleSession is the regression test for the 401->runtime.APIError
// mapping: once rotation itself is exhausted (the rotate endpoint rejects too), Run must surface a
// *runtime.APIError{Code: 401} - never session.ErrUnauthorized - so that
// fail.ErrorToDetailedError renders the stale-session message (exit code 2, "login update"
// suggestion), not the login-rejected one ("verify the session cookie value").
func TestRun_DeadSessionRendersStaleSession(t *testing.T) {
	rotateServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer rotateServer.Close()

	queryServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, err := w.Write([]byte("unauthorized"))
		assert.NoError(t, err)
	}))
	defer queryServer.Close()

	src := config.NewSessionSource("dead-cookie", rotateServer.URL, nil, nopStore{}, "acct")

	gCtx := &config.Context{
		Grafana: &config.GrafanaConfig{
			Server:  queryServer.URL,
			Session: src,
		},
	}

	_, err := explore.Run(t.Context(), gCtx, testQueryBody())
	require.Error(t, err)

	var apiErr *runtime.APIError
	require.ErrorAs(t, err, &apiErr)
	assert.Equal(t, http.StatusUnauthorized, apiErr.Code)

	detailed := fail.ErrorToDetailedError(err)
	assert.Equal(t, "Grafana session is stale or unauthorized", detailed.Summary)
	assert.Contains(t, detailed.Suggestions, "Run: grafanapi login update")
	require.NotNil(t, detailed.ExitCode)
	assert.Equal(t, 2, *detailed.ExitCode)
}

func TestRun_UnexpectedStatusIncludesBodySnippet(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, err := w.Write([]byte(`{"message":"boom","traceID":"abc123"}`))
		assert.NoError(t, err)
	}))
	defer server.Close()

	gCtx := &config.Context{Grafana: &config.GrafanaConfig{Server: server.URL}}

	_, err := explore.Run(t.Context(), gCtx, testQueryBody())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "500")
	assert.Contains(t, err.Error(), "boom")
}

func TestRun_NilContext(t *testing.T) {
	_, err := explore.Run(t.Context(), nil, testQueryBody())
	require.Error(t, err)
}
