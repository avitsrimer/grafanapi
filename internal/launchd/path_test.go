package launchd_test

import (
	"errors"
	"testing"

	"github.com/grafana/grafanapi/internal/launchd"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeFileSystem scripts launchd's fileSystem seam: a fixed executable path (or error), plus a
// fixed symlink-resolution table keyed by input path. Any path not present in symlinks reports a
// "does not exist" error, mirroring filepath.EvalSymlinks on a missing file — this is how the
// "no matching stable symlink" branch of ResolveBinaryPath is exercised.
type fakeFileSystem struct {
	executablePath string
	executableErr  error
	symlinks       map[string]string
}

func (f fakeFileSystem) Executable() (string, error) {
	return f.executablePath, f.executableErr
}

func (f fakeFileSystem) EvalSymlinks(path string) (string, error) {
	resolved, ok := f.symlinks[path]
	if !ok {
		return "", errors.New("no such file or directory")
	}

	return resolved, nil
}

func TestResolveBinaryPath_PlainPathPassthrough(t *testing.T) {
	restore := launchd.SetFileSystem(fakeFileSystem{
		executablePath: "/usr/local/bin/grafanapi",
		symlinks: map[string]string{
			"/usr/local/bin/grafanapi": "/usr/local/bin/grafanapi",
		},
	})
	defer restore()

	got, err := launchd.ResolveBinaryPath()
	require.NoError(t, err)
	assert.Equal(t, "/usr/local/bin/grafanapi", got)
}

func TestResolveBinaryPath_CellarPathWithMatchingSymlink(t *testing.T) {
	const realBinary = "/opt/homebrew/Cellar/grafanapi/1.2.3/bin/grafanapi"

	restore := launchd.SetFileSystem(fakeFileSystem{
		executablePath: realBinary,
		symlinks: map[string]string{
			realBinary:                    realBinary,
			"/opt/homebrew/bin/grafanapi": realBinary,
		},
	})
	defer restore()

	got, err := launchd.ResolveBinaryPath()
	require.NoError(t, err)
	assert.Equal(t, "/opt/homebrew/bin/grafanapi", got, "must return the stable symlink itself, not its target")
}

func TestResolveBinaryPath_CellarPathWithoutMatchingSymlink(t *testing.T) {
	const realBinary = "/usr/local/Cellar/grafanapi/1.2.3/bin/grafanapi"

	restore := launchd.SetFileSystem(fakeFileSystem{
		executablePath: realBinary,
		symlinks: map[string]string{
			realBinary: realBinary,
			// Neither stable-symlink candidate resolves (absent from the map), so
			// ResolveBinaryPath must fall back to the resolved real path.
		},
	})
	defer restore()

	got, err := launchd.ResolveBinaryPath()
	require.NoError(t, err)
	assert.Equal(t, realBinary, got)
}

func TestResolveBinaryPath_CaskroomPathTreatedAsVersioned(t *testing.T) {
	const realBinary = "/usr/local/Caskroom/grafanapi/1.2.3/bin/grafanapi"

	restore := launchd.SetFileSystem(fakeFileSystem{
		executablePath: realBinary,
		symlinks: map[string]string{
			realBinary:                 realBinary,
			"/usr/local/bin/grafanapi": realBinary,
		},
	})
	defer restore()

	got, err := launchd.ResolveBinaryPath()
	require.NoError(t, err)
	assert.Equal(t, "/usr/local/bin/grafanapi", got)
}

func TestResolveBinaryPath_ExecutableErrorPropagates(t *testing.T) {
	wantErr := errors.New("boom: cannot determine executable")

	restore := launchd.SetFileSystem(fakeFileSystem{executableErr: wantErr})
	defer restore()

	_, err := launchd.ResolveBinaryPath()
	require.Error(t, err)
	assert.ErrorIs(t, err, wantErr)
}

func TestResolveBinaryPath_EvalSymlinksErrorPropagates(t *testing.T) {
	restore := launchd.SetFileSystem(fakeFileSystem{
		executablePath: "/does/not/exist/grafanapi",
		symlinks:       map[string]string{},
	})
	defer restore()

	_, err := launchd.ResolveBinaryPath()
	require.Error(t, err)
}
