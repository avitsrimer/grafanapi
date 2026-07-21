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
	"github.com/grafana/grafanapi/internal/config"
	"github.com/grafana/grafanapi/internal/resources"
	"github.com/grafana/grafanapi/internal/session"
	k8sapi "k8s.io/apimachinery/pkg/api/errors"
)

// staleSessionSuggestion is shown whenever the Grafana session cookie is rejected or missing,
// pointing the user at the command that re-establishes it.
const staleSessionSuggestion = "Run: grafanapi login update"

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
		convertConfigErrors,    // Config-related
		convertFSErrors,        // FS-related
		convertResourcesErrors, // Resources-related
		convertNetworkErrors,   // Network-related errors
		convertSessionErrors,   // Stale/unauthorized Grafana session (openapi 401, StaleSessionError)
		convertAPIErrors,       // API-related errors (k8s dynamic-client StatusError)
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

// convertSessionErrors handles Grafana session-cookie authentication failures that do not surface
// as a k8s StatusError: a *session.StaleSessionError produced by login/login-update's own
// validation path, and a 401 from the grafana-openapi-client-go transport (e.g. `config check` /
// grafana.GetVersion). Both render as the same stale-session message with exit code 2. A raw k8s
// 401 is handled by convertAPIErrors instead, since it needs the *k8sapi.StatusError type assertion
// this function does not perform.
func convertSessionErrors(err error) (*DetailedError, bool) {
	staleErr := &session.StaleSessionError{}
	if errors.As(err, &staleErr) {
		return staleSessionError(err), true
	}

	apiErr := &runtime.APIError{}
	if errors.As(err, &apiErr) && apiErr.Code == http.StatusUnauthorized {
		return staleSessionError(err), true
	}

	return nil, false
}

// staleSessionError builds the user-facing DetailedError for any Grafana session-cookie
// authentication failure (stale/expired/missing cookie), regardless of which transport surfaced
// the underlying 401.
func staleSessionError(err error) *DetailedError {
	exitCode := 2

	return &DetailedError{
		Parent:      err,
		Summary:     "Grafana session is stale or unauthorized",
		Suggestions: []string{staleSessionSuggestion},
		ExitCode:    &exitCode,
	}
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
