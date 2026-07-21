package session

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/go-openapi/runtime"
	k8sapi "k8s.io/apimachinery/pkg/api/errors"
)

// ErrUnauthorized wraps low-level 401 responses encountered while verifying a session cookie
// (see VerifyCookie). Other unauthorized errors (k8s StatusError, openapi APIError) are detected
// structurally by IsUnauthorized rather than by wrapping this sentinel.
var ErrUnauthorized = errors.New("unauthorized")

// IsUnauthorized reports whether err represents a 401 Unauthorized response, whether produced by
// the k8s dynamic client, the grafana-openapi-client-go client, VerifyCookie (ErrUnauthorized),
// or a StaleSessionError.
func IsUnauthorized(err error) bool {
	if err == nil {
		return false
	}

	if errors.Is(err, ErrUnauthorized) {
		return true
	}

	staleErr := &StaleSessionError{}
	if errors.As(err, &staleErr) {
		return true
	}

	statusErr := &k8sapi.StatusError{}
	if errors.As(err, &statusErr) && k8sapi.IsUnauthorized(statusErr) {
		return true
	}

	apiErr := &runtime.APIError{}
	if errors.As(err, &apiErr) && apiErr.Code == http.StatusUnauthorized {
		return true
	}

	return false
}

// StaleSessionError indicates that the stored session cookie for Context is no longer accepted
// by the Grafana server. It is produced by the login/login-update validation path and rendered
// centrally by cmd/grafanapi/fail.
type StaleSessionError struct {
	Context string
	Parent  error
}

func (e *StaleSessionError) Error() string {
	return fmt.Sprintf("session for context %q is stale — run `grafanapi login update`: %v", e.Context, e.Parent)
}

func (e *StaleSessionError) Unwrap() error {
	return e.Parent
}
