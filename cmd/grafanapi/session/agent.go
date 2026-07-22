package session

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	cmdio "github.com/grafana/grafanapi/cmd/grafanapi/io"
	"github.com/spf13/cobra"
)

// agentLabel is both the launchd job label and the basename (plus .plist) of the file written to
// the LaunchAgents folder.
const agentLabel = "com.grafanapi.keepalive"

// agentDirPerm / agentFilePerm are the permissions used when writing the agent plist. The plist
// contains no secrets (the cookie stays in the Keychain), but 0o600 matches this project's
// config-file permission convention (see internal/config).
const (
	agentDirPerm  fs.FileMode = 0o755
	agentFilePerm fs.FileMode = 0o600
)

// plistTemplate is the launchd property list written by --install-agent. Placeholders: label,
// executable path, hour, minute.
const plistTemplate = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>%s</string>
    <key>ProgramArguments</key>
    <array>
        <string>%s</string>
        <string>session</string>
        <string>keepalive</string>
    </array>
    <key>StartCalendarInterval</key>
    <dict>
        <key>Hour</key>
        <integer>%d</integer>
        <key>Minute</key>
        <integer>%d</integer>
    </dict>
    <key>RunAtLoad</key>
    <false/>
</dict>
</plist>
`

// agentOptions holds the flags that drive --install-agent.
type agentOptions struct {
	InstallAgent bool
	At           string
	To           string
}

func (opts *agentOptions) bindFlags(cmd *cobra.Command) {
	flags := cmd.Flags()

	flags.BoolVar(&opts.InstallAgent, "install-agent", false,
		"Install a launchd agent that runs `session keepalive` daily, instead of running it now")
	flags.StringVar(&opts.At, "at", "09:00",
		"Time of day (HH:MM, 24h) the launchd agent runs at; only valid with --install-agent")
	flags.StringVar(&opts.To, "to", "",
		"Folder to write the launchd agent plist to (default ~/Library/LaunchAgents); only valid with --install-agent")
}

// validate enforces that the agent-only flags are not used without --install-agent, and that
// --at parses as a 24h HH:MM time. It returns the parsed hour and minute.
func (opts *agentOptions) validate(cmd *cobra.Command) (int, int, error) {
	if !opts.InstallAgent {
		if cmd.Flags().Changed("at") || cmd.Flags().Changed("to") {
			return 0, 0, errors.New("--at and --to require --install-agent")
		}

		return 0, 0, nil
	}

	at, err := time.Parse("15:04", opts.At)
	if err != nil {
		return 0, 0, fmt.Errorf("invalid --at value %q: expected HH:MM (24h), e.g. --at 07:30", opts.At)
	}

	return at.Hour(), at.Minute(), nil
}

// runInstallAgent writes the launchd agent plist and prints how to load it. The plist points at
// the currently running grafanapi binary, so a `brew upgrade` that replaces the binary in place
// keeps working, while a moved/deleted binary requires re-running --install-agent.
func runInstallAgent(cmd *cobra.Command, opts *agentOptions, hour, minute int) error {
	executable, err := os.Executable()
	if err != nil {
		return fmt.Errorf("install-agent: resolving the grafanapi binary path: %w", err)
	}

	agentsDir := opts.To
	if agentsDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return fmt.Errorf("install-agent: resolving home directory: %w", err)
		}

		agentsDir = filepath.Join(home, "Library", "LaunchAgents")
	}

	if err := os.MkdirAll(agentsDir, agentDirPerm); err != nil {
		return fmt.Errorf("install-agent: creating %s: %w", agentsDir, err)
	}

	target := filepath.Join(agentsDir, agentLabel+".plist")
	plist := fmt.Sprintf(plistTemplate, agentLabel, executable, hour, minute)

	if err := os.WriteFile(target, []byte(plist), agentFilePerm); err != nil {
		return fmt.Errorf("install-agent: writing %s: %w", target, err)
	}

	stdout := cmd.OutOrStdout()
	cmdio.Success(stdout, "Installed launchd agent to %s (daily at %02d:%02d)", target, hour, minute)
	cmdio.Info(stdout, "Load it now with:\n\n  launchctl unload %s 2>/dev/null; launchctl load %s\n", target, target)

	return nil
}
