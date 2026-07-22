package installskill_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/grafana/grafanapi/cmd/grafanapi/installskill"
	"github.com/grafana/grafanapi/internal/testutils"
	"github.com/grafana/grafanapi/skill"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func Test_InstallSkillCommand_writesEmbeddedSkill(t *testing.T) {
	claudeDir := t.TempDir()

	testCase := testutils.CommandTestCase{
		Cmd:     installskill.Command(),
		Command: []string{"--to", claudeDir},
		Assertions: []testutils.CommandAssertion{
			testutils.CommandSuccess(),
			testutils.CommandOutputContains(filepath.Join(claudeDir, "skills", "grafanapi")),
		},
	}
	testCase.Run(t)

	dest := filepath.Join(claudeDir, "skills", "grafanapi")

	destInfo, err := os.Stat(dest)
	require.NoError(t, err)
	assert.True(t, destInfo.IsDir())
	assert.Equal(t, os.FileMode(0o750), destInfo.Mode().Perm())

	wantContent, err := skill.Files.ReadFile("grafanapi/SKILL.md")
	require.NoError(t, err)

	gotPath := filepath.Join(dest, "SKILL.md")
	gotInfo, err := os.Stat(gotPath)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), gotInfo.Mode().Perm())

	gotContent, err := os.ReadFile(gotPath)
	require.NoError(t, err)
	assert.Equal(t, wantContent, gotContent)
}

func Test_InstallSkillCommand_overwritesStaleDestination(t *testing.T) {
	claudeDir := t.TempDir()
	dest := filepath.Join(claudeDir, "skills", "grafanapi")

	require.NoError(t, os.MkdirAll(dest, 0o750))
	stalePath := filepath.Join(dest, "stale-leftover.md")
	require.NoError(t, os.WriteFile(stalePath, []byte("stale"), 0o600))

	testCase := testutils.CommandTestCase{
		Cmd:     installskill.Command(),
		Command: []string{"--to", claudeDir},
		Assertions: []testutils.CommandAssertion{
			testutils.CommandSuccess(),
		},
	}
	testCase.Run(t)

	_, err := os.Stat(stalePath)
	require.ErrorIs(t, err, os.ErrNotExist)

	_, err = os.Stat(filepath.Join(dest, "SKILL.md"))
	require.NoError(t, err)
}

func Test_InstallSkillCommand_unwritableDestination(t *testing.T) {
	claudeDir := t.TempDir()

	// Point --to at a regular file instead of a directory: "skills/grafanapi" cannot be created
	// underneath it, so install-skill must fail with a wrapped error instead of silently
	// succeeding or panicking.
	blocker := filepath.Join(claudeDir, "blocker")
	require.NoError(t, os.WriteFile(blocker, []byte("not a directory"), 0o600))

	testCase := testutils.CommandTestCase{
		Cmd:     installskill.Command(),
		Command: []string{"--to", blocker},
		Assertions: []testutils.CommandAssertion{
			testutils.CommandErrorContains("install-skill"),
		},
	}
	testCase.Run(t)
}

func Test_InstallSkillCommand_defaultsToHomeClaudeDir(t *testing.T) {
	home := t.TempDir()

	testCase := testutils.CommandTestCase{
		Cmd:     installskill.Command(),
		Command: []string{},
		Env:     map[string]string{"HOME": home},
		Assertions: []testutils.CommandAssertion{
			testutils.CommandSuccess(),
			testutils.CommandOutputContains(filepath.Join(home, ".claude", "skills", "grafanapi")),
		},
	}
	testCase.Run(t)

	_, err := os.Stat(filepath.Join(home, ".claude", "skills", "grafanapi", "SKILL.md"))
	require.NoError(t, err)
}
