package skill_test

import (
	"strings"
	"testing"

	"github.com/grafana/grafanapi/skill"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFiles_SkillMD(t *testing.T) {
	content, err := skill.Files.ReadFile("grafanapi/SKILL.md")
	require.NoError(t, err)

	body := string(content)

	assert.NotEmpty(t, body)
	assert.True(t, strings.HasPrefix(body, "---\n"), "SKILL.md must start with a frontmatter delimiter")

	frontmatter, _, found := strings.Cut(strings.TrimPrefix(body, "---\n"), "\n---")
	require.True(t, found, "SKILL.md must have a closing frontmatter delimiter")

	assert.Contains(t, frontmatter, "name: grafanapi")
	assert.Contains(t, frontmatter, "description:")
}
