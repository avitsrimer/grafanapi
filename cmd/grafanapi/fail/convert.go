package fail

import (
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/go-openapi/runtime"
	"github.com/grafana/grafanapi/internal/alerts"
	"github.com/grafana/grafanapi/internal/config"
	"github.com/grafana/grafanapi/internal/resources"
	"github.com/grafana/grafanapi/internal/session"
	k8sapi "k8s.io/apimachinery/pkg/api/errors"
)

// staleSessionSuggestion is shown whenever a previously-stored Grafana session cookie is rejected
// or missing during normal use, pointing the user at the command that refreshes it.
const staleSessionSuggestion = "Run: grafanapi login update"

// loginRejectedSuggestion is shown when the cookie the user just entered at a login/login-update
// prompt is itself rejected: unlike staleSessionSuggestion, it must not tell the user to run
// `login update`, since that is either the very command that just failed (login update) or a
// command that isn't valid yet for a context that was never successfully authenticated (login).
const loginRejectedSuggestion = "Verify the session cookie value and try again"

// permissionDeniedSuggestion is shown when Grafana authenticates the user but refuses the request
// (403): the session is valid, so re-authenticating would not help — the user needs a role with
// enough permissions, or a context pointing at an instance where they have one.
//
// It is deliberately operation-neutral: convertAuthErrors matches every 403 the openapi transport
// produces, so this text is also what `explore`, `datasources`, `config check` and `login` show.
// Which role a given operation needs belongs in that command's own help and guide (alerting writes
// need Editor, for instance), not in a converter that cannot know what was being attempted.
const permissionDeniedSuggestion = "Check the permissions of your user, and that the current context targets " +
	"the intended Grafana instance: grafanapi config current-context"

// sessionAuthExitCode is the exit code used for both the "stale session" and "login rejected"
// cases: any error condition where the user must (re-)authenticate before retrying.
const sessionAuthExitCode = 2

// legacyAuthFieldNames lists the GrafanaConfig fields that were removed in favor of
// session-cookie authentication. A config file that still contains one of these keys fails
// strict YAML decoding as an "unknown field" error; convertConfigErrors recognizes that shape
// and renders a migration message instead of the raw parse error.
//
//nolint:gochecknoglobals // read-only lookup table, not mutated after init
var legacyAuthFieldNames = []string{"token", "user", "password"}

func ErrorToDetailedError(err error) *DetailedError {
	var converted bool
	detailedErr := &DetailedError{}
	if errors.As(err, detailedErr) {
		return detailedErr
	}

	// Try to convert the error for common error categories
	errorConverters := []func(err error) (*DetailedError, bool){
		convertConfigErrors,       // Config-related
		convertFSErrors,           // FS-related
		convertResourcesErrors,    // Resources-related
		convertAlertsErrors,       // Alerting-specific failures (a uid no rule carries)
		convertServerPolicyErrors, // Refused redirects and unusable server URLs (before network: a *url.Error wraps them)
		convertNetworkErrors,      // Network-related errors
		convertAuthErrors,         // Grafana auth failures (openapi 401/403, session.ErrUnauthorized)
		convertAPIErrors,          // API-related errors (k8s dynamic-client StatusError)
	}

	for _, converter := range errorConverters {
		detailedErr, converted = converter(err)
		if converted {
			return detailedErr
		}
	}

	return &DetailedError{
		Summary: "Unexpected error",
		Parent:  err,
	}
}

func convertConfigErrors(err error) (*DetailedError, bool) {
	validationErr := config.ValidationError{}
	if errors.As(err, &validationErr) {
		message := fmt.Sprintf("Invalid configuration found in '%s':\n%s", validationErr.File, validationErr.Message)
		if validationErr.AnnotatedSource != "" {
			message += "\n\n" + validationErr.AnnotatedSource
		}

		return &DetailedError{
			Summary: "Invalid configuration",
			Details: message,
			Suggestions: append([]string{
				"Review your configuration: grafanapi config view",
			}, validationErr.Suggestions...),
		}, true
	}

	unmarshalErr := config.UnmarshalError{}
	if errors.As(err, &unmarshalErr) {
		if field, ok := legacyAuthField(unmarshalErr.Err); ok {
			// Deliberately do NOT set Parent here: the underlying parse error's
			// annotated source echoes the raw file bytes at the offending line, and
			// because the field no longer exists on GrafanaConfig it is no longer part
			// of the datapolicy:"secret" denylist, so the redactor never touches it.
			// Rendering Parent would leak the literal legacy secret value.
			return &DetailedError{
				Summary: "Configuration uses a removed authentication field",
				Details: fmt.Sprintf(
					"The '%s' field in '%s' is no longer supported: grafanapi authenticates using a Grafana session cookie instead of API tokens or basic-auth credentials.",
					field, unmarshalErr.File,
				),
				Suggestions: []string{
					"Run: grafanapi login",
				},
			}, true
		}

		return &DetailedError{
			Summary: "Could not parse configuration",
			Details: fmt.Sprintf("Invalid configuration found in '%s'.", unmarshalErr.File),
			Parent:  unmarshalErr.Err,
		}, true
	}

	if errors.Is(err, config.ErrContextNotFound) {
		return &DetailedError{
			Summary: "Invalid configuration",
			Parent:  err,
			Suggestions: []string{
				"Check for typos in the context name",
				"Review your configuration: grafanapi config view",
			},
		}, true
	}

	return nil, false
}

func convertNetworkErrors(err error) (*DetailedError, bool) {
	urlErr := &url.Error{}
	if errors.As(err, &urlErr) {
		return &DetailedError{
			Parent:  err,
			Summary: "Network error",
			Suggestions: []string{
				"Make sure that the API is reachable",
				"Make sure that the configured target server is correct",
			},
		}, true
	}

	return nil, false
}

func convertAPIErrors(err error) (*DetailedError, bool) {
	statusErr := &k8sapi.StatusError{}
	if !errors.As(err, &statusErr) {
		return nil, false
	}

	reason := k8sapi.ReasonForError(statusErr)
	code := statusErr.Status().Code

	switch {
	case k8sapi.IsUnauthorized(statusErr):
		return staleSessionError(err), true
	case k8sapi.IsForbidden(statusErr):
		return &DetailedError{
			Parent:  err,
			Summary: fmt.Sprintf("%s - code %d", reason, code),
			Suggestions: []string{
				"Make sure that the configured credentials are correct",
				"Make sure that the configured credentials have enough permissions",
			},
		}, true
	case k8sapi.IsNotFound(statusErr):
		return &DetailedError{
			Parent:  err,
			Summary: fmt.Sprintf("Resource not found - code %d", code),
			Suggestions: []string{
				"Make sure that your are passing in valid resource selectors",
			},
		}, true
	}

	return &DetailedError{
		Parent:  err,
		Summary: fmt.Sprintf("API error: %s - code %d", reason, code),
	}, true
}

// convertAuthErrors handles Grafana authentication and authorization failures that do not surface
// as a k8s StatusError: a 401 or 403 from the grafana-openapi-client-go transport (e.g.
// `config check` / grafana.GetVersion, or an `alerts push` write) produced during normal use of an
// already-established context, and a bare session.ErrUnauthorized returned directly by
// session.VerifyCookie when login/login-update's own cookie-validation prompt is rejected.
//
// A 401 renders as the "stale session" message (exit code sessionAuthExitCode, suggesting
// `login update`); session.ErrUnauthorized renders as a distinct "login rejected" message (same
// exit code) since suggesting `login update` there is either circular (that's the command that just
// failed) or premature (the context was never successfully authenticated in the first place). A 403
// means the opposite of both: the cookie *was* accepted and the user simply lacks the permission,
// so it renders as a permission-denied error with the default exit code (1) and never suggests
// re-authenticating. A raw k8s 401/403 is handled by convertAPIErrors instead, since it needs the
// *k8sapi.StatusError type assertion this function does not perform.
func convertAuthErrors(err error) (*DetailedError, bool) {
	apiErr := &runtime.APIError{}
	if errors.As(err, &apiErr) {
		switch apiErr.Code {
		case http.StatusUnauthorized:
			return staleSessionError(err), true
		case http.StatusForbidden:
			return permissionDeniedError(err), true
		}
	}

	if errors.Is(err, session.ErrUnauthorized) {
		return loginRejectedError(err), true
	}

	return nil, false
}

// staleSessionError builds the user-facing DetailedError for a previously-established Grafana
// session-cookie that is no longer accepted (stale/expired/missing cookie) during normal use,
// regardless of which transport surfaced the underlying 401.
func staleSessionError(err error) *DetailedError {
	exitCode := sessionAuthExitCode

	return &DetailedError{
		Parent:      err,
		Summary:     "Grafana session is stale or unauthorized",
		Suggestions: []string{staleSessionSuggestion},
		ExitCode:    &exitCode,
	}
}

// loginRejectedError builds the user-facing DetailedError for a session cookie that was rejected
// (401) while being validated at a login/login-update prompt, i.e. before it was ever persisted.
func loginRejectedError(err error) *DetailedError {
	exitCode := sessionAuthExitCode

	return &DetailedError{
		Parent:      err,
		Summary:     "Grafana rejected the provided session cookie",
		Suggestions: []string{loginRejectedSuggestion},
		ExitCode:    &exitCode,
	}
}

// permissionDeniedError builds the user-facing DetailedError for a request Grafana authenticated
// but refused (403). Deliberately no ExitCode: unlike the 401 cases this is not an authentication
// problem, so it falls back to the generic failure exit code (1) and must not suggest re-running
// login. Parent is kept even though Grafana's 403 body is frequently an opaque `{}` — for the
// operations that do carry a message, it is the only place that message is shown.
func permissionDeniedError(err error) *DetailedError {
	return &DetailedError{
		Parent:      err,
		Summary:     "Grafana denied the request: your user lacks the required permission",
		Suggestions: []string{permissionDeniedSuggestion},
	}
}

// convertAlertsErrors handles the alerting failures that are neither an authentication problem nor
// a transport one. Today that is a single case: `alerts rules pause|resume|delete` naming a uid no
// rule carries, which internal/alerts reports as alerts.ErrRuleNotFound wrapped with that uid.
//
// It runs before convertAuthErrors, which is unambiguous rather than delicate: a not-found error
// carries no *runtime.APIError, and the aggregate a rule operation returns wraps its *first*
// failure - so a run whose first failure is a 401 or a 403 never reaches this converter at all and
// keeps its own exit code.
func convertAlertsErrors(err error) (*DetailedError, bool) {
	if errors.Is(err, alerts.ErrRuleNotFound) {
		return &DetailedError{
			Parent:  err,
			Summary: "Alert rule not found",
			Suggestions: []string{
				"Check the UID against the UID column of: grafanapi alerts rules list",
			},
		}, true
	}

	return nil, false
}

// convertServerPolicyErrors renders the two refusals of the server-origin policy: a request or
// redirect that leaves the configured Grafana origin, and a server URL that may not carry the
// session cookie. It must run before convertNetworkErrors, because net/http wraps a refusal in a
// *url.Error that would otherwise be claimed as a generic network error.
//
// Parent is the typed error found by errors.As, never err itself: the outer *url.Error carries the
// raw redirect target, path and query included, while the typed error holds origins only. Neither
// case sets an ExitCode - a policy refusal is not an authentication problem (exit 2).
func convertServerPolicyErrors(err error) (*DetailedError, bool) {
	redirectErr := &config.UnsafeRedirectError{}
	if errors.As(err, &redirectErr) {
		return &DetailedError{
			Parent:  redirectErr,
			Summary: "Refused a redirect away from the configured Grafana server",
			Suggestions: []string{
				"Set the context's server to the exact URL Grafana serves (scheme, host, port and sub-path, " +
					"Grafana's root_url): grafanapi config set contexts.<name>.grafana.server <url>",
				"Check the configured server: grafanapi config view",
			},
		}, true
	}

	serverErr := &config.ServerURLError{}
	if errors.As(err, &serverErr) {
		return &DetailedError{
			Parent:  serverErr,
			Summary: "Invalid Grafana server URL",
			Suggestions: []string{
				"Use the https:// address of your Grafana server, e.g. https://grafana.example.com",
				"Plain http:// is only accepted for a Grafana running on this machine (localhost, 127.0.0.1, ::1)",
			},
		}, true
	}

	return nil, false
}

func convertResourcesErrors(err error) (*DetailedError, bool) {
	invalidCommandErr := &resources.InvalidSelectorError{}
	if err != nil && errors.As(err, invalidCommandErr) {
		return &DetailedError{
			Parent:  err,
			Summary: "Could not parse resource(s) selector",
			Details: fmt.Sprintf("Failed to parse command '%s'", invalidCommandErr.Command),
			Suggestions: []string{
				"Make sure that your are passing in valid resource selectors",
			},
		}, true
	}

	return nil, false
}

func convertFSErrors(err error) (*DetailedError, bool) {
	pathErr := &fs.PathError{}

	if errors.Is(err, os.ErrNotExist) && errors.As(err, &pathErr) {
		return &DetailedError{
			Summary: "File not found",
			Details: fmt.Sprintf("could not read '%s'", pathErr.Path),
			Parent:  err,
			Suggestions: []string{
				"Check for typos in the command's arguments",
			},
		}, true
	}

	if errors.Is(err, os.ErrInvalid) && errors.As(err, &pathErr) {
		return &DetailedError{
			Summary: "Invalid path",
			Details: fmt.Sprintf("path '%s' is not valid", pathErr.Path),
			Parent:  err,
			Suggestions: []string{
				"Make sure that you are passing in a valid path",
				"If you are pulling resources make sure that the path is a directory",
			},
		}, true
	}

	if errors.Is(err, os.ErrPermission) && errors.As(err, &pathErr) {
		return &DetailedError{
			Summary: "Permission denied",
			Parent:  err,
			Suggestions: []string{
				"Review the permissions on the file",
			},
		}, true
	}

	return nil, false
}

// legacyAuthField reports whether err is a strict-decode "unknown field" error caused by one of
// the auth fields removed from GrafanaConfig (token, user, password), returning the offending
// field name. goccy/go-yaml's strict-mode error text has the shape `unknown field "<name>"`.
func legacyAuthField(err error) (string, bool) {
	if err == nil {
		return "", false
	}

	msg := err.Error()
	for _, name := range legacyAuthFieldNames {
		if strings.Contains(msg, fmt.Sprintf("unknown field %q", name)) {
			return name, true
		}
	}

	return "", false
}
