package fail_test

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
	"testing"

	"github.com/go-openapi/runtime"
	"github.com/grafana/grafanapi/cmd/grafanapi/fail"
	"github.com/grafana/grafanapi/internal/alerts"
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

// TestErrorToDetailedError_PermissionDenied covers the authorization half of convertAuthErrors: an
// openapi 403 (e.g. a Viewer running `alerts push`) means the session cookie was accepted and the
// user simply lacks the permission, so it must render its own message, must not suggest
// re-authenticating, and must keep the default exit code (1) rather than the 401 cases' exit 2.
//
// The suggestion is asserted to be operation-neutral: this converter matches every 403 the openapi
// transport produces - `explore`, `datasources`, `config check`, `login` - so naming the role one
// particular operation needs would be wrong advice for all the others.
func TestErrorToDetailedError_PermissionDenied(t *testing.T) {
	err := &runtime.APIError{OperationName: "postAlertRuleGroup", Code: 403, Response: "{}"}

	detailedErr := fail.ErrorToDetailedError(err)

	require.NotNil(t, detailedErr)
	assert.Equal(t, "Grafana denied the request: your user lacks the required permission", detailedErr.Summary)
	assert.NotEqual(t, "Grafana session is stale or unauthorized", detailedErr.Summary)
	assert.Nil(t, detailedErr.ExitCode, "403 is not an authentication failure: exit code stays the default 1")
	assert.Equal(t, err, detailedErr.Parent)
	require.Len(t, detailedErr.Suggestions, 1)
	assert.Contains(t, detailedErr.Suggestions[0], "config current-context")
	assert.NotContains(t, detailedErr.Suggestions[0], "login update")
	assert.NotContains(t, detailedErr.Suggestions[0], "Editor",
		"every openapi 403 lands here, so the suggestion must not name one operation's role")
	assert.NotContains(t, detailedErr.Suggestions[0], "Alerting")
}

// TestErrorToDetailedError_RuleNotFound pins the tidy rendering of a mistyped rule uid: without its
// own converter arm it falls through to the generic "Unexpected error" heading, which reads as a
// bug in grafanapi rather than as a typo. It is not an authentication failure, so the exit code
// stays the default 1.
func TestErrorToDetailedError_RuleNotFound(t *testing.T) {
	err := fmt.Errorf("2 rule(s) failed to delete: %w",
		fmt.Errorf("alerts: rule rule-uid-9 %w", alerts.ErrRuleNotFound))

	detailedErr := fail.ErrorToDetailedError(err)

	require.NotNil(t, detailedErr)
	assert.Equal(t, "Alert rule not found", detailedErr.Summary)
	assert.Nil(t, detailedErr.ExitCode, "a uid no rule carries is not an authentication failure")
	assert.Equal(t, err, detailedErr.Parent)
	require.Len(t, detailedErr.Suggestions, 1)
	assert.Contains(t, detailedErr.Suggestions[0], "alerts rules list")
}

// TestErrorToDetailedError_RuleNotFoundDoesNotOutrankAuth pins the converter ordering that matters:
// convertAlertsErrors runs before convertAuthErrors, so a 401 or a 403 must still win when it is
// the error the command actually returned. A rule operation returns the aggregate wrapping its
// *first* failure, which is exactly the shape asserted here.
func TestErrorToDetailedError_RuleNotFoundDoesNotOutrankAuth(t *testing.T) {
	tests := map[string]struct {
		code    int
		summary string
		exit    *int
	}{
		"401 stays the stale-session exit 2": {
			code:    401,
			summary: "Grafana session is stale or unauthorized",
			exit:    new(2),
		},
		"403 stays the permission-denied exit 1": {
			code:    403,
			summary: "Grafana denied the request: your user lacks the required permission",
		},
	}

	for name, testCase := range tests {
		t.Run(name, func(t *testing.T) {
			err := fmt.Errorf("1 rule(s) failed to delete: %w",
				&runtime.APIError{OperationName: "deleteAlertRule", Code: testCase.code, Response: "{}"})

			detailedErr := fail.ErrorToDetailedError(err)

			require.NotNil(t, detailedErr)
			assert.Equal(t, testCase.summary, detailedErr.Summary)
			assert.Equal(t, testCase.exit, detailedErr.ExitCode)
		})
	}
}

// TestErrorToDetailedError_PermissionDeniedWrappedInURLErrorIsANetworkError pins a known
// consequence of the converter ordering in ErrorToDetailedError: convertNetworkErrors runs before
// convertAuthErrors and claims anything wrapping a *url.Error, so a 403 (like a 401) that reaches
// the renderer inside a *url.Error is reported as a network error. Nothing in grafanapi's
// authenticated paths currently wraps an openapi APIError that way; this test exists so the caveat
// stays visible and any future reordering is a deliberate, test-updating decision.
func TestErrorToDetailedError_PermissionDeniedWrappedInURLErrorIsANetworkError(t *testing.T) {
	err := &url.Error{
		Op:  "Post",
		URL: "https://grafana.example.com/api/v1/provisioning/alert-rules",
		Err: &runtime.APIError{OperationName: "postAlertRule", Code: 403},
	}

	detailedErr := fail.ErrorToDetailedError(err)

	require.NotNil(t, detailedErr)
	assert.Equal(t, "Network error", detailedErr.Summary)
	assert.Nil(t, detailedErr.ExitCode)
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
