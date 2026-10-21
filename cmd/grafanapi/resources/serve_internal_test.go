package resources

import (
	"bytes"
	"testing"

	cmdconfig "github.com/grafana/grafanapi/cmd/grafanapi/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestServeCmd_addressDefaultsToLoopback(t *testing.T) {
	flag := serveCmd(&cmdconfig.Options{}).Flags().Lookup("address")

	require.NotNil(t, flag)
	assert.Equal(t, "127.0.0.1", flag.DefValue)
}

func TestReportListening(t *testing.T) {
	const url = "http://127.0.0.1:8080/"

	t.Run("loopback address prints the URL and no warning", func(t *testing.T) {
		var stdout, stderr bytes.Buffer

		reportListening(&stdout, &stderr, "127.0.0.1", url)

		assert.Contains(t, stdout.String(), url)
		assert.Empty(t, stderr.String())
	})

	t.Run("wildcard address warns on stderr", func(t *testing.T) {
		var stdout, stderr bytes.Buffer

		reportListening(&stdout, &stderr, "0.0.0.0", url)

		assert.Contains(t, stdout.String(), url)
		assert.Contains(t, stderr.String(), "not a loopback address")
		assert.Contains(t, stderr.String(), "0.0.0.0")
		assert.NotContains(t, stdout.String(), "not a loopback address")
	})
}
