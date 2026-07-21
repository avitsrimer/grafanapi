package config_test

import (
	"testing"

	"github.com/grafana/grafanapi/internal/config"
	"github.com/stretchr/testify/assert"
)

func TestSessionCookieName(t *testing.T) {
	assert.Equal(t, "grafana_session", config.SessionCookieName)
}

func TestCookieHeaderValue(t *testing.T) {
	tests := []struct {
		name   string
		cookie string
		want   string
	}{
		{
			name:   "simple cookie value",
			cookie: "abc123",
			want:   "grafana_session=abc123",
		},
		{
			name:   "empty cookie value",
			cookie: "",
			want:   "grafana_session=",
		},
		{
			name:   "cookie value containing special characters",
			cookie: "abc.123-XYZ_%3D",
			want:   "grafana_session=abc.123-XYZ_%3D",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, config.CookieHeaderValue(tt.cookie))
		})
	}
}
