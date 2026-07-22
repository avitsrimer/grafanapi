package session

import (
	"errors"
)

// ErrUnauthorized wraps low-level 401 responses encountered while verifying a session cookie
// (see VerifyCookie). Other unauthorized errors (k8s StatusError, openapi APIError) are detected
// structurally by the caller (see cmd/grafanapi/fail.convertSessionErrors and convertAPIErrors)
// rather than by wrapping this sentinel.
var ErrUnauthorized = errors.New("unauthorized")
