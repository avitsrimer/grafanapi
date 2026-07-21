package session_test

import (
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/go-openapi/runtime"
	"github.com/grafana/grafanapi/internal/session"
	"github.com/stretchr/testify/assert"
	k8sapi "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func TestIsUnauthorized(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "nil error",
			err:  nil,
			want: false,
		},
		{
			name: "ErrUnauthorized",
			err:  session.ErrUnauthorized,
			want: true,
		},
		{
			name: "wrapped ErrUnauthorized",
			err:  fmt.Errorf("verifying cookie: %w", session.ErrUnauthorized),
			want: true,
		},
		{
			name: "StaleSessionError",
			err:  &session.StaleSessionError{Context: "default", Parent: errors.New("boom")},
			want: true,
		},
		{
			name: "k8s unauthorized status error",
			err:  k8sapi.NewUnauthorized("bad credentials"),
			want: true,
		},
		{
			name: "k8s generic 401 status error",
			err: &k8sapi.StatusError{ErrStatus: metav1.Status{
				Status: metav1.StatusFailure,
				Code:   http.StatusUnauthorized,
				Reason: metav1.StatusReasonUnknown,
			}},
			want: true,
		},
		{
			name: "k8s forbidden status error",
			err:  k8sapi.NewForbidden(schema.GroupResource{Resource: "dashboards"}, "foo", errors.New("nope")),
			want: false,
		},
		{
			name: "k8s not found status error",
			err:  k8sapi.NewNotFound(schema.GroupResource{Resource: "dashboards"}, "foo"),
			want: false,
		},
		{
			name: "openapi 401 API error",
			err:  &runtime.APIError{OperationName: "getUser", Code: http.StatusUnauthorized},
			want: true,
		},
		{
			name: "openapi 500 API error",
			err:  &runtime.APIError{OperationName: "getUser", Code: http.StatusInternalServerError},
			want: false,
		},
		{
			name: "unrelated error",
			err:  errors.New("network unreachable"),
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, session.IsUnauthorized(tt.err))
		})
	}
}

func TestStaleSessionError_Error(t *testing.T) {
	parent := errors.New("401 unauthorized")
	err := &session.StaleSessionError{Context: "prod", Parent: parent}

	assert.Contains(t, err.Error(), "prod")
	assert.Contains(t, err.Error(), "grafanapi login update")
	assert.Contains(t, err.Error(), parent.Error())
}

func TestStaleSessionError_Unwrap(t *testing.T) {
	parent := errors.New("401 unauthorized")
	err := &session.StaleSessionError{Context: "prod", Parent: parent}

	assert.Same(t, parent, errors.Unwrap(err))
	assert.ErrorIs(t, err, parent)
}
