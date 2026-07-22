// Package session implements the `grafanapi session` command group: session-lifecycle operations
// that span every configured context, starting with `session keepalive`.
//
// `session keepalive` proactively rotates the Grafana session cookie of every logged-in context
// (POST /api/user/auth-tokens/rotate, via the same SessionSource machinery the rotating transport
// uses on a 401) and persists each rotated cookie to the Keychain. Run periodically - manually,
// from cron, or via the launchd agent installed by --install-agent - it keeps sessions inside
// Grafana's inactive-lifetime window, so a single `grafanapi login` keeps working until the
// server's maximum session lifetime (or a real logout) forces a fresh login.
package session

import (
	"context"
	"errors"
	"fmt"
	"sort"

	cmdconfig "github.com/grafana/grafanapi/cmd/grafanapi/config"
	cmdio "github.com/grafana/grafanapi/cmd/grafanapi/io"
	"github.com/grafana/grafanapi/internal/config"
	"github.com/grafana/grafanapi/internal/keychain"
	"github.com/spf13/cobra"
)

// keychainStore resolves session cookies from the platform Keychain. It is a package-level var,
// defaulting to the real platform store, so tests can substitute a fake keychain.Store via
// SetKeychainStore instead of touching the actual Keychain (same seam pattern as
// cmd/grafanapi/config and cmd/grafanapi/login).
//
//nolint:gochecknoglobals // test seam for the platform Keychain; see SetKeychainStore.
var keychainStore keychain.Store = keychain.NewStore()

// SetKeychainStore overrides the keychain.Store used for session-cookie resolution. It exists for
// tests; it returns a function that restores the previously configured store.
func SetKeychainStore(store keychain.Store) func() {
	previous := keychainStore
	keychainStore = store

	return func() { keychainStore = previous }
}

// Command returns the `session` command group.
func Command() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "session",
		Short: "Manage Grafana login sessions across contexts",
		Long:  "Manage the Grafana login sessions stored for the configured contexts.",
	}

	cmd.AddCommand(keepaliveCmd())

	return cmd
}

// keepaliveCmd returns the `session keepalive` command.
func keepaliveCmd() *cobra.Command {
	configOpts := &cmdconfig.Options{}
	agentOpts := &agentOptions{}

	cmd := &cobra.Command{
		Use:   "keepalive",
		Args:  cobra.NoArgs,
		Short: "Rotate the session cookie of every logged-in context",
		Long: `Rotate the Grafana session cookie of every logged-in context and persist the rotated
cookies to the Keychain.

Grafana expires sessions that go unused for its inactive-lifetime window (7 days by default).
Running this command periodically keeps every logged-in session inside that window, so a single
"grafanapi login" keeps working until the server's maximum session lifetime (30 days by default)
or a real logout forces a fresh login.

Contexts that never completed "grafanapi login" are reported and skipped.

With --install-agent, instead of rotating now, install a launchd agent that runs
"grafanapi session keepalive" daily (--at, default 09:00) so sessions stay alive hands-off.`,
		Example: `
	grafanapi session keepalive
	grafanapi session keepalive --context staging
	grafanapi session keepalive --install-agent --at 07:30`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			hour, minute, err := agentOpts.validate(cmd)
			if err != nil {
				return err
			}

			if agentOpts.InstallAgent {
				return runInstallAgent(cmd, agentOpts, hour, minute)
			}

			return runKeepalive(cmd, configOpts)
		},
	}

	configOpts.BindFlags(cmd.Flags())
	agentOpts.bindFlags(cmd)

	return cmd
}

// runKeepalive is the `session keepalive` command's RunE body: load the configuration, resolve
// and rotate the session of every (or the --context selected) context, and report per-context
// outcomes. Contexts without a Grafana server or without a stored session are reported and
// skipped, never failed on.
func runKeepalive(cmd *cobra.Command, configOpts *cmdconfig.Options) error {
	ctx := cmd.Context()

	cfg, err := config.Load(ctx, configSource(configOpts))
	if err != nil {
		return err
	}

	names, err := contextNames(cfg, configOpts.Context)
	if err != nil {
		return err
	}

	var staleErr, otherErr error

	for _, name := range names {
		if err := keepaliveContext(ctx, cmd, cfg.Contexts[name]); err != nil {
			if errors.Is(err, config.ErrRotateUnauthorized) {
				staleErr = errors.Join(staleErr, err)
			} else {
				otherErr = errors.Join(otherErr, err)
			}
		}
	}

	// A stale session takes precedence: its dedicated exit code (2) is the signal automation
	// relies on to tell "a human needs to log in again" apart from transient failures.
	if staleErr != nil {
		return staleErr
	}

	return otherErr
}

// keepaliveContext rotates a single context's session and prints the outcome. A missing Grafana
// config or a context that never logged in is reported as skipped with a nil error.
func keepaliveContext(ctx context.Context, cmd *cobra.Command, gCtx *config.Context) error {
	stdout := cmd.OutOrStdout()

	if gCtx.Grafana == nil || gCtx.Grafana.Server == "" {
		cmdio.Warning(stdout, "Context %s: skipped (no Grafana server configured)", gCtx.Name)
		return nil
	}

	if err := config.ResolveContextSessionCookie(keychainStore, gCtx); err != nil {
		cmdio.Error(stdout, "Context %s: could not read the stored session: %v", gCtx.Name, err)
		return fmt.Errorf("context %q: %w", gCtx.Name, err)
	}

	if gCtx.Grafana.Session == nil {
		cmdio.Warning(stdout, "Context %s: skipped (not logged in - run `grafanapi login --context %s`)", gCtx.Name, gCtx.Name)
		return nil
	}

	_, gen := gCtx.Grafana.Session.Current()

	if _, err := gCtx.Grafana.Session.Rotate(ctx, gen); err != nil {
		if errors.Is(err, config.ErrRotateUnauthorized) {
			cmdio.Error(stdout, "Context %s: session is stale - run `grafanapi login update --context %s`", gCtx.Name, gCtx.Name)
		} else {
			cmdio.Error(stdout, "Context %s: %v", gCtx.Name, err)
		}

		return fmt.Errorf("context %q: %w", gCtx.Name, err)
	}

	cmdio.Success(stdout, "Context %s: session rotated", gCtx.Name)

	return nil
}

// contextNames returns the sorted context names keepalive should visit: every context, or just
// the one selected via --context (an unknown selection is an error).
func contextNames(cfg config.Config, selected string) ([]string, error) {
	if selected != "" {
		if !cfg.HasContext(selected) {
			return nil, config.ContextNotFound(selected)
		}

		return []string{selected}, nil
	}

	names := make([]string, 0, len(cfg.Contexts))
	for name := range cfg.Contexts {
		names = append(names, name)
	}

	sort.Strings(names)

	return names, nil
}

// configSource mirrors cmd/grafanapi/config.Options.configSource (private there): an explicit
// --config path wins, otherwise the standard location (including the GRAFANAPI_CONFIG override).
func configSource(configOpts *cmdconfig.Options) config.Source {
	if configOpts.ConfigFile != "" {
		return config.ExplicitConfigFile(configOpts.ConfigFile)
	}

	return config.StandardLocation()
}
