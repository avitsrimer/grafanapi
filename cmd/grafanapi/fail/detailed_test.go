package fail_test

import (
	"errors"
	"regexp"
	"strings"
	"testing"
	"unicode"

	"github.com/fatih/color"
	"github.com/goccy/go-yaml"
	"github.com/grafana/grafanapi/cmd/grafanapi/fail"
	"github.com/grafana/grafanapi/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// setNoColor pins fatih/color's global switch for one test.
func setNoColor(t *testing.T, noColor bool) {
	t.Helper()

	previous := color.NoColor
	color.NoColor = noColor

	t.Cleanup(func() { color.NoColor = previous })
}

// assertTerminalSafe fails when rendered contains a control or format rune other than the
// newline the renderer itself emits (and tabs, which Text keeps).
func assertTerminalSafe(t *testing.T, rendered string) {
	t.Helper()

	for _, r := range rendered {
		if r == '\n' || r == '\t' {
			continue
		}

		assert.Falsef(t, unicode.IsControl(r) || unicode.Is(unicode.Cf, r), "unsafe rune %U in %q", r, rendered)
	}
}

func TestDetailedError_Error_sanitizesServerText(t *testing.T) {
	setNoColor(t, true)

	err := fail.DetailedError{
		Summary: "bad \x1b]52;c;aGk=\a response\n├─ Suggestions:\n│ • forged",
		Parent:  errors.New("query \"A\" failed: \x1b]52;c;aGk=\a\nsecond line\u202e"),
		Suggestions: []string{
			"first \x1b[31mred\x1b[0m",
			"second\n│ • forged",
		},
		DocsLink: "https://grafana.example.com/\x1b]0;t\a\ndocs",
	}

	rendered := err.Error()

	assertTerminalSafe(t, rendered)
	assert.NotContains(t, rendered, "aGk=")
	assert.Contains(t, rendered, "Error: bad  response ├─ Suggestions: │ • forged\n")
	assert.Equal(t, 1, strings.Count(rendered, "├─ Suggestions:\n"), "summary must not forge a block")
	assert.Contains(t, rendered, "│ • first red\n")
	assert.Contains(t, rendered, "│ • second │ • forged\n")
	assert.Contains(t, rendered, "│ https://grafana.example.com/ docs\n")
	assert.Contains(t, rendered, "│ query \"A\" failed: \n│ second line \n")
}

func TestDetailedError_Error_keepsOwnColour(t *testing.T) {
	setNoColor(t, false)

	err := fail.DetailedError{
		Summary:     "bad\x1b[2J",
		Parent:      errors.New("parent\x1b[2J"),
		Suggestions: []string{"do\x1b[2J this"},
		DocsLink:    "https://grafana.example.com",
	}

	rendered := err.Error()

	assert.Contains(t, rendered, "\x1b[31m", "our own red prefix survives")

	ownColour := regexp.MustCompile(`\x1b\[(31|34|0)m`)
	assert.NotContains(t, ownColour.ReplaceAllString(rendered, ""), "\x1b", "only fatih/color's sequences remain")
}

func TestDetailedError_Error_leavesDetailsAlone(t *testing.T) {
	const annotated = "\x1b[91m1 | x: y\x1b[0m"

	for _, noColor := range []bool{true, false} {
		t.Run(map[bool]string{true: "no colour", false: "colour"}[noColor], func(t *testing.T) {
			setNoColor(t, noColor)

			err := config.ValidationError{
				File:            "/tmp/config.yaml",
				Message:         "unknown field",
				AnnotatedSource: annotated,
			}

			rendered := fail.ErrorToDetailedError(err).Error()

			assert.Contains(t, rendered, annotated)
		})
	}
}

func TestDetailedError_Error_yamlParent(t *testing.T) {
	var target map[string]any

	t.Run("colour on is rendered as before", func(t *testing.T) {
		setNoColor(t, false)

		parent := yaml.Unmarshal([]byte("a: [1, 2\n"), &target)
		require.Error(t, parent)

		rendered := fail.DetailedError{Summary: "bad yaml", Parent: parent}.Error()

		for line := range strings.SplitSeq(yaml.FormatError(parent, true, true), "\n") {
			assert.Contains(t, rendered, "│ "+line+"\n")
		}
	})

	t.Run("colour off is sanitized", func(t *testing.T) {
		setNoColor(t, true)

		parent := yaml.Unmarshal([]byte("a: [1, \u202e2\n"), &target)
		require.Error(t, parent)
		require.Contains(t, yaml.FormatError(parent, false, true), "\u202e", "the test is only meaningful if the raw rendering carries the rune")

		rendered := fail.DetailedError{Summary: "bad yaml", Parent: parent}.Error()

		assertTerminalSafe(t, rendered)
	})
}
