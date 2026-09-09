package alerts_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/go-openapi/runtime"
	goapi "github.com/grafana/grafana-openapi-client-go/client"
	"github.com/grafana/grafana-openapi-client-go/models"
	"github.com/grafana/grafanapi/internal/alerts"
	"github.com/grafana/grafanapi/internal/config"
	"github.com/grafana/grafanapi/internal/grafana"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newContactPointsFixture wires a generated client against a server that serves only the
// contact-points endpoint with the given response.
func newContactPointsFixture(t *testing.T, resp stubResponse) *goapi.GrafanaHTTPAPI {
	t.Helper()

	server := alertsServer(t, map[string]stubResponse{contactPointsPath: resp})

	gClient, err := grafana.ClientFromContext(&config.Context{
		Grafana: &config.GrafanaConfig{Server: server.URL, SessionCookie: "abc123"},
	})
	require.NoError(t, err)

	return gClient
}

// TestListContactPoints_SortedByName pins the ordering the table renderer relies on, and that
// the server-redacted settings survive into the payload untouched (they are what json/yaml
// output shows).
func TestListContactPoints_SortedByName(t *testing.T) {
	const body = `[
	  {"uid":"bfx","name":"probe-slack","type":"slack","provenance":"api",
	   "settings":{"recipient":"#probe","url":"[REDACTED]"}},
	  {"uid":"","name":"email receiver","type":"email","disableResolveMessage":true,
	   "settings":{"addresses":"admin@localhost"}}
	]`

	points, err := alerts.ListContactPoints(t.Context(), newContactPointsFixture(t, okResponse(body)))
	require.NoError(t, err)
	require.Len(t, points, 2)

	assert.Equal(t, "email receiver", points[0].Name)
	assert.Equal(t, "probe-slack", points[1].Name)

	require.NotNil(t, points[1].Type)
	assert.Equal(t, "slack", *points[1].Type)
	assert.Equal(t, "bfx", points[1].UID)
	assert.Equal(t, "api", points[1].Provenance)
	assert.True(t, points[0].DisableResolveMessage)

	encoded, err := json.Marshal(points[1].Settings)
	require.NoError(t, err)
	assert.Contains(t, string(encoded), "[REDACTED]")
}

// TestListContactPoints_SameNameTieBreakByUID covers the sort tie-break: Grafana treats a
// contact point name as a UI grouping key, so duplicates are legal and must still order stably.
func TestListContactPoints_SameNameTieBreakByUID(t *testing.T) {
	const body = `[
	  {"uid":"z","name":"dup","type":"webhook"},
	  {"uid":"a","name":"dup","type":"webhook"}
	]`

	points, err := alerts.ListContactPoints(t.Context(), newContactPointsFixture(t, okResponse(body)))
	require.NoError(t, err)
	require.Len(t, points, 2)

	assert.Equal(t, "a", points[0].UID)
	assert.Equal(t, "z", points[1].UID)
}

// TestListContactPoints_NilElementDropped covers the payload being a slice of pointers: a `null`
// element must be dropped rather than kept. Keeping it would panic the sort comparator, and even
// with a nil-safe comparator it would reach `-o json`/`-o yaml` as a bare `null` entry, which no
// consumer of a contact-point list can do anything with.
func TestListContactPoints_NilElementDropped(t *testing.T) {
	const body = `[{"uid":"b","name":"beta","type":"webhook"}, null, {"uid":"a","name":"alpha","type":"email"}]`

	points, err := alerts.ListContactPoints(t.Context(), newContactPointsFixture(t, okResponse(body)))
	require.NoError(t, err)
	require.Len(t, points, 2, "the null element must not survive into the output")
	assert.NotContains(t, points, (*models.EmbeddedContactPoint)(nil))

	require.NotNil(t, points[0])
	assert.Equal(t, "alpha", points[0].Name)
	require.NotNil(t, points[1])
	assert.Equal(t, "beta", points[1].Name)
}

// TestListContactPoints_ContextCancellationPropagates pins that the caller's context reaches the
// generated client call.
func TestListContactPoints_ContextCancellationPropagates(t *testing.T) {
	fixture := newContactPointsFixture(t, okResponse(`[]`))

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, err := alerts.ListContactPoints(ctx, fixture)
	require.Error(t, err)
	require.ErrorIs(t, err, context.Canceled)
	assert.Contains(t, err.Error(), "list contact points")
}

func TestListContactPoints_EmptyListIsNonNil(t *testing.T) {
	points, err := alerts.ListContactPoints(t.Context(), newContactPointsFixture(t, okResponse(`[]`)))
	require.NoError(t, err)
	assert.NotNil(t, points, "-o json must emit [] rather than null")
	assert.Empty(t, points)
}

// TestListContactPoints_NullPayloadIsNonNil guards the other empty shape: a server answering
// `null` must not make the command print null either.
func TestListContactPoints_NullPayloadIsNonNil(t *testing.T) {
	points, err := alerts.ListContactPoints(t.Context(), newContactPointsFixture(t, okResponse(`null`)))
	require.NoError(t, err)
	assert.NotNil(t, points)
	assert.Empty(t, points)
}

func TestListContactPoints_401(t *testing.T) {
	fixture := newContactPointsFixture(t, stubResponse{
		status: http.StatusUnauthorized,
		body:   `{"message":"Unauthorized"}`,
	})

	_, err := alerts.ListContactPoints(t.Context(), fixture)
	require.Error(t, err)

	var apiErr *runtime.APIError
	require.ErrorAs(t, err, &apiErr)
	assert.Equal(t, http.StatusUnauthorized, apiErr.Code)
}

func TestListContactPoints_500IsWrapped(t *testing.T) {
	fixture := newContactPointsFixture(t, stubResponse{
		status: http.StatusInternalServerError,
		body:   `{"message":"boom"}`,
	})

	_, err := alerts.ListContactPoints(t.Context(), fixture)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "list contact points")
	assert.Contains(t, err.Error(), "status 500")
}

func TestListContactPoints_NilClient(t *testing.T) {
	_, err := alerts.ListContactPoints(t.Context(), nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no grafana client configured")
}
