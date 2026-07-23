package launchd_test

import (
	"testing"
	"time"

	"github.com/grafana/grafanapi/internal/launchd"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestValidateInterval(t *testing.T) {
	tests := []struct {
		name    string
		d       time.Duration
		wantErr bool
	}{
		{name: "below minimum", d: 30 * time.Second, wantErr: true},
		{name: "minimum boundary", d: time.Minute, wantErr: false},
		{name: "within range", d: 12 * time.Hour, wantErr: false},
		{name: "maximum boundary", d: 6 * 24 * time.Hour, wantErr: false},
		{name: "above maximum", d: 7 * 24 * time.Hour, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := launchd.ValidateInterval(tt.d)
			if tt.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestDefaultAgentSpec(t *testing.T) {
	spec := launchd.DefaultAgentSpec("/opt/homebrew/bin/grafanapi", 12*time.Hour)

	assert.Equal(t, launchd.Label, spec.Label)
	assert.Equal(t, "/opt/homebrew/bin/grafanapi", spec.BinaryPath)
	assert.Equal(t, []string{"session", "refresh", "--due"}, spec.Args)
	assert.Equal(t, 43200, spec.IntervalSeconds)
	assert.Equal(t, launchd.LogPath(), spec.StdoutPath)
	assert.Equal(t, launchd.LogPath(), spec.StderrPath)
}
