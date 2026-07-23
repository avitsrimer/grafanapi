package launchd_test

import (
	"path/filepath"
	"testing"

	"github.com/grafana/grafanapi/internal/launchd"
	"github.com/stretchr/testify/assert"
)

// TestPathHelpers_HonorHOME asserts every well-known path is derived from $HOME, which is how
// "session keepalive" tests (Task 8) point the package at a temporary, isolated home directory
// rather than the real one.
func TestPathHelpers_HonorHOME(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	assert.Equal(t, filepath.Join(home, "Library", "LaunchAgents"), launchd.LaunchAgentsDir())
	assert.Equal(t, filepath.Join(home, "Library", "LaunchAgents", launchd.Label+".plist"), launchd.PlistPath())
	assert.Equal(t, filepath.Join(home, "Library", "Logs", "grafanapi"), launchd.LogDir())
	assert.Equal(t, filepath.Join(home, "Library", "Logs", "grafanapi", "keepalive.log"), launchd.LogPath())
}
