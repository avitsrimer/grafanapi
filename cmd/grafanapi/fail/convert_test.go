package fail_test

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
	"testing"

	"github.com/go-openapi/runtime"
	"github.com/grafana/grafanapi/cmd/grafanapi/fail"
	"github.com/grafana/grafanapi/internal/config"
	"github.com/grafana/grafanapi/internal/format"
	"github.com/grafana/grafanapi/internal/session"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	k8sapi "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// TestErrorToDetailedError_StaleSession also serves as the fallback-rendering half of
// docs/plans/20260722-auto-rotate-session-on-401.md's Task 3: when the k8s transport's
// rotatingRoundTripper gives up on a rejected rotation (see
// TestNewNamespacedRESTConfig_RotateRejectedSurfacesOriginal401 in internal/config/rest_test.go),
// client-go turns the surfaced 401 into exactly the k8sapi.StatusError asserted below, which must
// still render as the stale-session error with exit code 2.
func TestErrorToDetailedError_StaleSession(t *testing.T) {
	tests := map[string]struct {
		err error
	}{
		"k8s unauthorized StatusError": {
			err: k8sapi.NewUnauthorized("session expired"),
		},
		"openapi 401 APIError": {
			err: &runtime.APIError{OperationName: "getUser", Code: 401},
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			detailedErr := fail.ErrorToDetailedError(tc.err)

			require.NotNil(t, detailedErr)
			assert.Equal(t, "Grafana session is stale or unauthorized", detailedErr.Summary)
			require.Len(t, detailedErr.Suggestions, 1)
			assert.Equal(t, "Run: grafanapi login update", detailedErr.Suggestions[0])
			require.NotNil(t, detailedErr.ExitCode)
			assert.Equal(t, 2, *detailedErr.ExitCode)
			assert.Equal(t, tc.err, detailedErr.Parent)
		})
	}
}

// TestErrorToDetailedError_LoginRejected covers the login/login-update validation path: when
// session.VerifyCookie itself returns (a wrapped) session.ErrUnauthorized -- as opposed to an
// openapi 401, which login/login-update never produces -- the error must still resolve to a
// detailed, exit-code-2 error, but with a message and suggestion distinct from the "stale session"
// case: suggesting `grafanapi login update` here would either be circular (login update just
// failed) or premature (login itself never succeeded).
func TestErrorToDetailedError_LoginRejected(t *testing.T) {
	err := fmt.Errorf("login: could not validate session cookie against https://grafana.example.com: %w", session.ErrUnauthorized)

	detailedErr := fail.ErrorToDetailedError(err)

	require.NotNil(t, detailedErr)
	assert.Equal(t, "Grafana rejected the provided session cookie", detailedErr.Summary)
	assert.NotEqual(t, "Grafana session is stale or unauthorized", detailedErr.Summary)
	require.Len(t, detailedErr.Suggestions, 1)
	assert.NotContains(t, detailedErr.Suggestions[0], "login update")
	require.NotNil(t, detailedErr.ExitCode)
	assert.Equal(t, 2, *detailedErr.ExitCode)
	assert.Equal(t, err, detailedErr.Parent)
}

func TestErrorToDetailedError_NonStaleErrorsPassThroughUnchanged(t *testing.T) {
	t.Run("k8s forbidden keeps its own message and no exit code", func(t *testing.T) {
		err := k8sapi.NewForbidden(schema.GroupResource{Resource: "dashboards"}, "foo", errors.New("denied"))

		detailedErr := fail.ErrorToDetailedError(err)

		require.NotNil(t, detailedErr)
		assert.Contains(t, detailedErr.Summary, "Forbidden")
		assert.Nil(t, detailedErr.ExitCode)
		assert.NotContains(t, detailedErr.Suggestions, "Run: grafanapi login update")
	})

	t.Run("k8s not found keeps its own message", func(t *testing.T) {
		err := k8sapi.NewNotFound(schema.GroupResource{Resource: "dashboards"}, "foo")

		detailedErr := fail.ErrorToDetailedError(err)

		require.NotNil(t, detailedErr)
		assert.Contains(t, detailedErr.Summary, "Resource not found")
		assert.Nil(t, detailedErr.ExitCode)
	})

	t.Run("network error keeps its own message", func(t *testing.T) {
		err := &url.Error{Op: "Get", URL: "https://example.com", Err: errors.New("connection refused")}

		detailedErr := fail.ErrorToDetailedError(err)

		require.NotNil(t, detailedErr)
		assert.Equal(t, "Network error", detailedErr.Summary)
		assert.Nil(t, detailedErr.ExitCode)
	})

	t.Run("openapi non-401 APIError is not treated as a stale session", func(t *testing.T) {
		err := &runtime.APIError{OperationName: "getUser", Code: 500}

		detailedErr := fail.ErrorToDetailedError(err)

		require.NotNil(t, detailedErr)
		assert.NotEqual(t, "Grafana session is stale or unauthorized", detailedErr.Summary)
		assert.Nil(t, detailedErr.ExitCode)
	})

	t.Run("unexpected error falls through to the generic summary", func(t *testing.T) {
		err := errors.New("boom")

		detailedErr := fail.ErrorToDetailedError(err)

		require.NotNil(t, detailedErr)
		assert.Equal(t, "Unexpected error", detailedErr.Summary)
		assert.Nil(t, detailedErr.ExitCode)
	})
}

// TestErrorToDetailedError_LegacyAuthFieldMigrationMessage covers Task 9's legacy-config
// decision: a config.UnmarshalError caused by a strict-decode "unknown field" error on one of
// the auth fields removed from GrafanaConfig (token/user/password) renders a friendly migration
// message instead of the raw parse error, and critically does NOT set Parent — since the field
// no longer exists on GrafanaConfig it is no longer part of the datapolicy:"secret" denylist, so
// rendering the raw error would leak whatever secret value the legacy key held.
func TestErrorToDetailedError_LegacyAuthFieldMigrationMessage(t *testing.T) {
	tests := map[string]struct {
		field string
	}{
		"token":    {field: "token"},
		"user":     {field: "user"},
		"password": {field: "password"},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			parseErr := errors.New(`[5:7] unknown field "` + tc.field + `"` + "\n      " + tc.field + `: some-secret-value`)
			err := config.UnmarshalError{File: "config.yaml", Err: parseErr}

			detailedErr := fail.ErrorToDetailedError(err)

			require.NotNil(t, detailedErr)
			assert.Equal(t, "Configuration uses a removed authentication field", detailedErr.Summary)
			assert.Contains(t, detailedErr.Details, "'"+tc.field+"'")
			assert.Contains(t, detailedErr.Suggestions, "Run: grafanapi login")
			require.NoError(t, detailedErr.Parent, "Parent must be nil to avoid leaking the legacy secret value")

			rendered := detailedErr.Error()
			assert.NotContains(t, rendered, "some-secret-value")
			assert.Contains(t, rendered, "grafanapi login")
		})
	}

	t.Run("unrelated unknown field keeps the generic parse-error message", func(t *testing.T) {
		parseErr := errors.New(`[5:7] unknown field "bogus-field"`)
		err := config.UnmarshalError{File: "config.yaml", Err: parseErr}

		detailedErr := fail.ErrorToDetailedError(err)

		require.NotNil(t, detailedErr)
		assert.Equal(t, "Could not parse configuration", detailedErr.Summary)
		assert.Equal(t, parseErr, detailedErr.Parent)
		assert.Empty(t, detailedErr.Suggestions)
	})

	// The cases above hand-construct the "unknown field" error string; this one instead runs the
	// real strict-decode goccy/go-yaml path (the same codec internal/config/loader.go uses) so
	// the match in legacyAuthField is verified against genuine error text, not a guess at its
	// format that could silently drift out of sync with a future goccy/go-yaml version.
	t.Run("real strict-decode error on a legacy field matches the same way", func(t *testing.T) {
		var grafana config.GrafanaConfig
		decodeErr := format.NewYAMLCodec().Decode(
			strings.NewReader("token: some-secret-value\nserver: https://grafana.example.com\n"),
			&grafana,
		)
		require.Error(t, decodeErr, "decoding a legacy 'token' key into GrafanaConfig must fail strict decoding")

		err := config.UnmarshalError{File: "config.yaml", Err: decodeErr}

		detailedErr := fail.ErrorToDetailedError(err)

		require.NotNil(t, detailedErr)
		assert.Equal(t, "Configuration uses a removed authentication field", detailedErr.Summary)
		assert.Contains(t, detailedErr.Details, "'token'")
		require.NoError(t, detailedErr.Parent)

		rendered := detailedErr.Error()
		assert.NotContains(t, rendered, "some-secret-value")
	})
}
