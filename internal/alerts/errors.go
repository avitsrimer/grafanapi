package alerts

import (
	"errors"
	"fmt"

	"github.com/grafana/grafana-openapi-client-go/models"
)

// ErrRuleNotFound is what a rule operation records against a uid no rule carries. It is wrapped
// with that uid ("alerts: rule <uid> not found") and exported so cmd/grafanapi/fail can render a
// mistyped uid as its own tidy failure instead of under the generic "Unexpected error" heading -
// there is nothing unexpected about a typo.
var ErrRuleNotFound = errors.New("not found")

// messageError is a failure rendered through a message of our own - Grafana's validation text for
// a rejected write, "permission denied" for a folder this session may not read - while still
// carrying the error it came from, so errors.As keeps reaching the generated error type.
type messageError struct {
	message string
	err     error
}

// Error returns the rendered message, which has already been built from the operation and what it
// was refused for.
func (e *messageError) Error() string {
	return e.message
}

// Unwrap exposes the generated client's error, which is what the exit-code and session handling
// match on.
func (e *messageError) Unwrap() error {
	return e.err
}

// operationError renders one failed write the way the guide documents it - "alerts: <operation>
// <target>: <what went wrong>" - substituting Grafana's own validation message for the opaque 400
// the generated client would otherwise report.
//
// It is shared by the push phases and the rule operations, which differ only in what they call
// themselves and what they name: a kind and a plan ID for one, a rule or group for the other.
//
// The validation arm returns a wrapper rather than a flattened fmt.Errorf string: the message
// replaces the generated client's own rendering, but the response still has to be reachable
// through errors.As, which is how cmd/grafanapi/fail decides between the 401 and 403 renderings
// and how record recognises a dead session.
func operationError(operation, target string, err error) error {
	if message, ok := validationMessage(err); ok {
		return &messageError{message: fmt.Sprintf("alerts: %s %s: %s", operation, target, message), err: err}
	}

	return fmt.Errorf("alerts: %s %s: %w", operation, target, err)
}

// validationMessage extracts the message Grafana returns with a 400.
//
// There is no shared type to match on: the generated client declares a *per-operation* BadRequest
// struct for every write, and the only thing they have in common is the accessor below. Without
// this the user would see the generated client's opaque rendering of the response instead of
// "invalid alert rule: folder does not exist".
func validationMessage(err error) (string, bool) {
	var response interface {
		GetPayload() *models.ValidationError
	}
	if !errors.As(err, &response) {
		return "", false
	}

	payload := response.GetPayload()
	if payload == nil || payload.Message == "" {
		return "", false
	}

	return payload.Message, true
}
