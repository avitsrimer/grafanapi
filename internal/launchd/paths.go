package launchd

import (
	"os"
	"path/filepath"
)

// LaunchAgentsDir returns the per-user LaunchAgents directory launchd scans for GUI-domain agents:
// "~/Library/LaunchAgents". It honors $HOME (via os.UserHomeDir), which is how tests point the
// package at a temporary, isolated home directory.
func LaunchAgentsDir() string {
	return filepath.Join(homeDir(), "Library", "LaunchAgents")
}

// PlistPath returns the absolute path of the grafanapi keep-alive agent's plist file:
// "~/Library/LaunchAgents/<Label>.plist".
func PlistPath() string {
	return filepath.Join(LaunchAgentsDir(), Label+".plist")
}

// LogDir returns the directory grafanapi writes the keep-alive agent's stdout/stderr log to:
// "~/Library/Logs/grafanapi".
func LogDir() string {
	return filepath.Join(homeDir(), "Library", "Logs", "grafanapi")
}

// LogPath returns the absolute path of the keep-alive agent's combined stdout/stderr log file:
// "~/Library/Logs/grafanapi/keepalive.log". The log never contains the session cookie - only the
// same short success/failure lines "session refresh --due" would print to a terminal.
func LogPath() string {
	return filepath.Join(LogDir(), "keepalive.log")
}

// homeDir returns the current user's home directory, falling back to "." if it cannot be
// determined so the path helpers above never panic or error - a degraded "./Library/..." path is
// a harmless, obvious artifact that should not occur on a real macOS login session (the only
// supported environment for this package).
func homeDir() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "."
	}

	return home
}
