package launchd_test

import (
	"bytes"
	"encoding/xml"
	"os"
	"testing"
	"time"

	"github.com/grafana/grafanapi/internal/launchd"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// goldenSpec is the fixed AgentSpec used by TestGenerate_Golden: a 12h interval (43200 seconds)
// default spec with a plain, non-Homebrew-versioned binary path and a deterministic log path (not
// derived from the actual test machine's home directory, so the golden file is stable across
// environments).
func goldenSpec() launchd.AgentSpec {
	spec := launchd.DefaultAgentSpec("/opt/homebrew/bin/grafanapi", 12*time.Hour)
	spec.StdoutPath = "/Users/tester/Library/Logs/grafanapi/keepalive.log"
	spec.StderrPath = spec.StdoutPath

	return spec
}

func TestGenerate_Golden(t *testing.T) {
	spec := goldenSpec()
	require.Equal(t, 43200, spec.IntervalSeconds, "precondition: 12h must derive to 43200 seconds")

	var buf bytes.Buffer
	require.NoError(t, launchd.Generate(&buf, spec))

	golden, err := os.ReadFile("testdata/keepalive.plist.golden")
	require.NoError(t, err)

	assert.Equal(t, string(golden), buf.String())

	// The golden fixture itself must be valid, parseable XML - a golden file that happened to be
	// well-formed by accident would be a weak regression lock.
	require.NoError(t, xml.Unmarshal(golden, new(any)))
}

func TestGenerate_EscapesXMLSpecialCharacters(t *testing.T) {
	spec := launchd.DefaultAgentSpec(`/opt/homebrew/bin/gr&af<a>napi`, time.Hour)
	spec.StdoutPath = `/Users/te"ster/Library/Logs/grafanapi/keep&alive.log`
	spec.StderrPath = spec.StdoutPath

	var buf bytes.Buffer
	require.NoError(t, launchd.Generate(&buf, spec))

	rendered := buf.String()

	// The raw, unescaped special characters must not appear verbatim in the output.
	assert.NotContains(t, rendered, "gr&af<a>napi")
	assert.Contains(t, rendered, "gr&amp;af&lt;a&gt;napi")

	// And the result must still be valid, parseable XML.
	require.NoError(t, xml.Unmarshal(buf.Bytes(), new(any)), "escaped plist must remain valid XML")
}
