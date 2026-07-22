package session_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/grafana/grafanapi/cmd/grafanapi/session"
	"github.com/grafana/grafanapi/internal/testutils"
	"github.com/stretchr/testify/require"
)

func Test_KeepaliveCommand_installAgentWritesPlist(t *testing.T) {
	agentsDir := t.TempDir()

	testCase := testutils.CommandTestCase{
		Cmd:     session.Command(),
		Command: []string{"keepalive", "--install-agent", "--at", "07:30", "--to", agentsDir},
		Assertions: []testutils.CommandAssertion{
			testutils.CommandSuccess(),
			testutils.CommandOutputContains("Installed launchd agent"),
			testutils.CommandOutputContains("launchctl"),
		},
	}

	testCase.Run(t)

	plist, err := os.ReadFile(filepath.Join(agentsDir, "com.grafanapi.keepalive.plist"))
	require.NoError(t, err)

	content := string(plist)
	require.Contains(t, content, "<string>com.grafanapi.keepalive</string>")
	require.Contains(t, content, "<string>session</string>")
	require.Contains(t, content, "<string>keepalive</string>")
	require.Contains(t, content, "<key>Hour</key>")
	require.Contains(t, content, "<integer>7</integer>")
	require.Contains(t, content, "<key>Minute</key>")
	require.Contains(t, content, "<integer>30</integer>")

	executable, err := os.Executable()
	require.NoError(t, err)
	require.Contains(t, content, "<string>"+executable+"</string>")
}

func Test_KeepaliveCommand_installAgentDefaultsTo9AM(t *testing.T) {
	agentsDir := t.TempDir()

	testCase := testutils.CommandTestCase{
		Cmd:     session.Command(),
		Command: []string{"keepalive", "--install-agent", "--to", agentsDir},
		Assertions: []testutils.CommandAssertion{
			testutils.CommandSuccess(),
		},
	}

	testCase.Run(t)

	plist, err := os.ReadFile(filepath.Join(agentsDir, "com.grafanapi.keepalive.plist"))
	require.NoError(t, err)
	require.Contains(t, string(plist), "<integer>9</integer>")
	require.Contains(t, string(plist), "<integer>0</integer>")
}

func Test_KeepaliveCommand_installAgentRejectsMalformedAt(t *testing.T) {
	testCase := testutils.CommandTestCase{
		Cmd:     session.Command(),
		Command: []string{"keepalive", "--install-agent", "--to", t.TempDir(), "--at", "quarter past nine"},
		Assertions: []testutils.CommandAssertion{
			testutils.CommandErrorContains("--at"),
		},
	}

	testCase.Run(t)
}

func Test_KeepaliveCommand_atWithoutInstallAgentErrors(t *testing.T) {
	testCase := testutils.CommandTestCase{
		Cmd:     session.Command(),
		Command: []string{"keepalive", "--at", "07:30"},
		Assertions: []testutils.CommandAssertion{
			testutils.CommandErrorContains("--install-agent"),
		},
	}

	testCase.Run(t)
}
