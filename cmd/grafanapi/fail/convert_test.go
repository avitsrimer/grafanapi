package fail_test

import (
	"errors"
	"net/url"
	"testing"

	"github.com/go-openapi/runtime"
	"github.com/grafana/grafanapi/cmd/grafanapi/fail"
	"github.com/grafana/grafanapi/internal/session"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	k8sapi "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

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
		"StaleSessionError from login validation": {
			err: &session.StaleSessionError{Context: "default", Parent: session.ErrUnauthorized},
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
