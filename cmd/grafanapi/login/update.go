package login

import (
	"errors"
	"fmt"
	"strings"

	"github.com/grafana/grafanapi/cmd/grafanapi/io"
	"github.com/grafana/grafanapi/internal/config"
	"github.com/grafana/grafanapi/internal/keychain"
	"github.com/grafana/grafanapi/internal/session"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// updateCommand returns the `login update` subcommand.
func updateCommand() *cobra.Command {
	opts := &UpdateOptions{}

	cmd := &cobra.Command{
		Use:   "update",
		Args:  cobra.NoArgs,
		Short: "Refresh a stale Grafana session cookie",
		Long: `Refresh the Grafana session cookie stored for an existing context.

Unlike "grafanapi login", "login update" never re-asks for the server: it loads the current (or
--context-selected) context's server from the configuration file, prompts only for a new session
cookie, validates it against that stored server (GET /api/user), and — only on success — overwrites
the cookie in the macOS Keychain. The configuration file itself is never modified.`,
		Example: "\n\tgrafanapi login update\n\tgrafanapi login update --context staging",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runLoginUpdate(cmd, opts)
		},
	}

	opts.BindFlags(cmd.Flags())

	return cmd
}

// UpdateOptions holds the flags accepted by `login update`.
type UpdateOptions struct {
	ConfigFile string
	Context    string
}

func (opts *UpdateOptions) BindFlags(flags *pflag.FlagSet) {
	flags.StringVar(&opts.ConfigFile, "config", "", "Path to the configuration file to use")
	flags.StringVar(&opts.Context, "context", "", "Name of the context to update (defaults to the current context)")

	_ = cobra.MarkFlagFilename(flags, "config", "yaml", "yml")
}

func (opts *UpdateOptions) configSource() config.Source {
	if opts.ConfigFile != "" {
		return config.ExplicitConfigFile(opts.ConfigFile)
	}

	return config.StandardLocation()
}

func runLoginUpdate(cmd *cobra.Command, opts *UpdateOptions) error {
	ctx := cmd.Context()

	cfg, err := config.Load(ctx, opts.configSource())
	if err != nil {
		return err
	}

	name := opts.Context
	if name == "" {
		name = cfg.CurrentContext
	}

	if name == "" {
		return errors.New("login update: no context specified and no current context is set (pass --context)")
	}

	existing := cfg.Contexts[name]
	if existing == nil || existing.Grafana == nil || existing.Grafana.Server == "" {
		return fmt.Errorf("login update: context %q is not configured (run: grafanapi login --context %s)", name, name)
	}

	cookie, err := activePrompter.PromptSecret("Grafana session cookie: ")
	if err != nil {
		return fmt.Errorf("login update: %w", err)
	}
	cookie = strings.TrimSpace(cookie)

	if cookie == "" {
		return errors.New("login update: a session cookie is required")
	}

	verifyGrafana := *existing.Grafana
	verifyGrafana.SessionCookie = cookie
	verifyContext := config.Context{Name: name, Grafana: &verifyGrafana}

	if err := session.VerifyCookie(ctx, &verifyContext); err != nil {
		return fmt.Errorf("login update: could not validate session cookie against %s: %w", existing.Grafana.Server, err)
	}

	if err := keychainStore.Set(keychain.Account(name), cookie); err != nil {
		return fmt.Errorf("login update: storing session cookie in keychain: %w", err)
	}

	io.Success(cmd.OutOrStdout(), "Refreshed session for context %q", name)

	return nil
}
