package testutils_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/grafana/grafanapi/internal/testutils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCookieTrap_RecordsRequests(t *testing.T) {
	trap := testutils.NewCookieTrap(t)

	assert.Equal(t, 0, trap.Requests())
	assert.Empty(t, trap.LastCookie())
	assert.Empty(t, trap.LastBody())

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, trap.URL, strings.NewReader("payload"))
	require.NoError(t, err)
	req.Header.Set("Cookie", "grafana_session=abc")

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, 1, trap.Requests())
	assert.Equal(t, "grafana_session=abc", trap.LastCookie())
	assert.Equal(t, "payload", trap.LastBody())
}

func TestNewRedirectServer_RedirectsEveryRequest(t *testing.T) {
	trap := testutils.NewCookieTrap(t)
	server := testutils.NewRedirectServer(t, trap.URL+"/landing", http.StatusTemporaryRedirect)

	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}

	for _, path := range []string{"/", "/apis", "/deep/path"} {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL+path, nil)
		require.NoError(t, err)

		resp, err := client.Do(req)
		require.NoError(t, err)
		require.NoError(t, resp.Body.Close())

		assert.Equal(t, http.StatusTemporaryRedirect, resp.StatusCode)
		assert.Equal(t, trap.URL+"/landing", resp.Header.Get("Location"))
	}

	assert.Equal(t, 0, trap.Requests(), "the redirect server must not contact its target")
}
