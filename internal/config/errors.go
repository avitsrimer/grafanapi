package config

import (
	"errors"
	"fmt"
)

var ErrContextNotFound = errors.New("context not found")

var (
	// ErrSessionOriginMismatch is matched by a SessionBindingError whose stored session is bound to
	// a different server origin than the one the context currently targets.
	ErrSessionOriginMismatch = errors.New("session is bound to a different server")
	// ErrSessionUnbound is matched by a SessionBindingError for a stored session that carries no
	// origin and could not be bound to the current server automatically.
	ErrSessionUnbound = errors.New("session is not bound to a server")
)

type ValidationError struct {
	// File holds the path to the configuration file containing the error.
	File    string
	Message string
	// Path refers to the location of the error in the configuration file.
	// It is expressed as a YAMLPath, as described in https://pkg.go.dev/github.com/goccy/go-yaml#PathString
	Path            string
	AnnotatedSource string
	Suggestions     []string
}

func (e ValidationError) Error() string {
	return e.Message
}

type UnmarshalError struct {
	File string
	Err  error
}

func (e UnmarshalError) Error() string {
	return e.Err.Error()
}

// SessionBindingError reports that the stored session cookie of a context was not loaded because
// it is not bound to the server the context targets. It carries no cookie: only origins in
// ServerOrigin's format are rendered, and the type renders them as given without normalising.
type SessionBindingError struct {
	Context string
	// BoundOrigin is the origin the stored session is bound to; empty for a legacy (unbound) item.
	BoundOrigin string
	// ServerOrigin is the origin of the server the context currently targets.
	ServerOrigin string
}

func (e *SessionBindingError) Error() string {
	if e.BoundOrigin == "" {
		return fmt.Sprintf(
			"session for context %q is not bound to a server yet and %s was not taken from your own "+
				"configuration file (--config, GRAFANAPI_CONFIG, GRAFANA_SERVER or XDG_CONFIG_* is set, "+
				"or $HOME is not your account's home directory); the session cookie was not sent",
			e.Context, e.ServerOrigin)
	}

	return fmt.Sprintf(
		"session for context %q is bound to %s but the target server is %s; the session cookie was not sent",
		e.Context, e.BoundOrigin, e.ServerOrigin)
}

// Unwrap returns ErrSessionUnbound for an unbound session and ErrSessionOriginMismatch otherwise.
func (e *SessionBindingError) Unwrap() error {
	if e.BoundOrigin == "" {
		return ErrSessionUnbound
	}

	return ErrSessionOriginMismatch
}

func ContextNotFound(name string) error {
	return fmt.Errorf("invalid context \"%s\": %w", name, ErrContextNotFound)
}
