package launchd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// cellarMarkers are the path substrings ResolveBinaryPath treats as a brew-managed, versioned
// install directory. A "brew upgrade" renames the directory these substrings appear in (bumping
// the version component), which would silently break a LaunchAgent whose plist embeds the exact
// path — hence preferring a stable symlink (stableSymlinkCandidates) whenever one resolves to the
// same binary.
//
//nolint:gochecknoglobals // read-only lookup table, never mutated after init
var cellarMarkers = []string{"/Cellar/", "/Caskroom/"}

// stableSymlinkCandidates are the well-known Homebrew "current" symlinks that keep pointing at the
// active install after "brew upgrade" rewrites the underlying /Cellar (or /Caskroom) path. Checked
// in this order: /opt/homebrew is the Apple Silicon prefix, /usr/local the Intel prefix; both are
// tried so ResolveBinaryPath works on either.
//
//nolint:gochecknoglobals // read-only lookup table, never mutated after init
var stableSymlinkCandidates = []string{
	"/opt/homebrew/bin/grafanapi",
	"/usr/local/bin/grafanapi",
}

// fs is the filesystem seam ResolveBinaryPath uses for os.Executable/filepath.EvalSymlinks. It
// defaults to the real OS; tests substitute a fake via SetFileSystem so every branch (plain path,
// Homebrew Cellar path with/without a matching stable symlink, os.Executable failure) is
// reproducible without touching the real filesystem or the real running binary's path.
//
//nolint:gochecknoglobals // test seam; see SetFileSystem
var fs fileSystem = osFileSystem{}

// ResolveBinaryPath returns the absolute path "session keepalive install" should embed in the
// LaunchAgent's plist as the program to run. launchd stores this path verbatim and re-reads it on
// every scheduled wake, so it must survive a Homebrew upgrade: a raw ".../Cellar/grafanapi/1.2.3/
// bin/grafanapi" path is renamed out from under the agent the moment a newer version is installed,
// silently breaking it.
//
// The resolution order is: os.Executable() (the path the running process was invoked with) →
// filepath.EvalSymlinks (follow through any wrapper symlink to the real file). If the resolved
// path is NOT under a Homebrew Cellar/Caskroom directory, it is returned as-is — it is already
// stable. Otherwise, each of stableSymlinkCandidates is checked in order; the first one that
// exists and resolves to the same real file is returned verbatim (the symlink itself, not its
// target — Homebrew repoints it at every upgrade, so it is the stable reference). If none match,
// the resolved real path is returned as a best-effort fallback.
func ResolveBinaryPath() (string, error) {
	execPath, err := fs.Executable()
	if err != nil {
		return "", fmt.Errorf("launchd: resolving binary path: %w", err)
	}

	resolved, err := fs.EvalSymlinks(execPath)
	if err != nil {
		return "", fmt.Errorf("launchd: resolving binary path: evaluating symlinks for %s: %w", execPath, err)
	}

	if !isCellarPath(resolved) {
		return resolved, nil
	}

	for _, candidate := range stableSymlinkCandidates {
		candidateResolved, evalErr := fs.EvalSymlinks(candidate)
		if evalErr != nil {
			// Candidate does not exist or is not resolvable — not installed via this prefix, try
			// the next one.
			continue
		}

		if candidateResolved == resolved {
			return candidate, nil
		}
	}

	return resolved, nil
}

// SetFileSystem overrides the filesystem seam used by ResolveBinaryPath. It exists for tests; it
// returns a function that restores the previously configured filesystem.
func SetFileSystem(f fileSystem) func() {
	previous := fs
	fs = f

	return func() { fs = previous }
}

// fileSystem abstracts the two OS calls ResolveBinaryPath needs so tests can script every branch
// (plain path passthrough, Cellar path with/without a matching stable symlink, an os.Executable
// failure) without touching the real filesystem or the real running binary's path.
type fileSystem interface {
	// Executable returns the path of the running process's executable, as os.Executable.
	Executable() (string, error)
	// EvalSymlinks returns path after resolving all symbolic links, as filepath.EvalSymlinks. It
	// errors if path does not exist or cannot be resolved.
	EvalSymlinks(path string) (string, error)
}

// osFileSystem is the real, production fileSystem: a thin pass-through to the os and filepath
// standard-library functions.
type osFileSystem struct{}

func (osFileSystem) Executable() (string, error) {
	return os.Executable()
}

func (osFileSystem) EvalSymlinks(path string) (string, error) {
	return filepath.EvalSymlinks(path)
}

// isCellarPath reports whether path falls under a Homebrew Cellar or Caskroom directory (see
// cellarMarkers) — the versioned install location a "brew upgrade" renames.
func isCellarPath(path string) bool {
	for _, marker := range cellarMarkers {
		if strings.Contains(path, marker) {
			return true
		}
	}

	return false
}
