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

const (
	noServerOrigin = "no server"
	unknownOrigin  = "an unknown server"
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
--context-selected) context's server from the configuration file, then reads a new session cookie
— from an interactive, no-echo prompt, or from stdin with --cookie-stdin — validates it against
that stored server (GET /api/user), and — only on success — overwrites the cookie in the macOS
Keychain. The configuration file itself is never modified.

The session is bound to the server's origin (scheme, host, port). Moving a context to another server,
or binding a session stored by an older version from a non-default configuration, needs --rebind.

The stored server must use https://; plain http:// is only accepted for a loopback address (localhost, 127.x.x.x, ::1).`,
		Example: "\n\tgrafanapi login update\n\tgrafanapi login update --context staging\n" +
			"\tpbpaste | grafanapi login update --cookie-stdin",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runLoginUpdate(cmd, opts)
		},
	}

	opts.BindFlags(cmd.Flags())

	return cmd
}

// UpdateOptions holds the flags accepted by `login update`.
type UpdateOptions struct {
	ConfigFile  string
	Context     string
	CookieStdin bool
	Rebind      bool
}

func (opts *UpdateOptions) BindFlags(flags *pflag.FlagSet) {
	flags.StringVar(&opts.ConfigFile, "config", "", "Path to the configuration file to use")
	flags.StringVar(&opts.Context, "context", "", "Name of the context to update (defaults to the current context)")
	flags.BoolVar(&opts.CookieStdin, "cookie-stdin", false, "Read the session cookie from stdin instead of the interactive prompt")

	flags.BoolVar(&opts.Rebind, "rebind", false, "Allow binding this context's session to a different server than the one it is bound to; "+
		"only use it from a shell whose environment you trust")

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

	if err := config.ValidateServerURL(existing.Grafana.Server); err != nil {
		return fmt.Errorf("login update: %w", err)
	}

	newOrigin, err := config.ServerOrigin(existing.Grafana.Server)
	if err != nil {
		return fmt.Errorf("login update: %w", err)
	}

	io.Info(cmd.ErrOrStderr(), "Updating the session for context %q on %s", name, newOrigin)

	if err := guardRebind(cmd, opts, name, newOrigin); err != nil {
		return err
	}

	var cookie string
	if opts.CookieStdin {
		cookie, err = readCookieFromStdin(cmd)
		if err != nil {
			return fmt.Errorf("login update: %w", err)
		}
	} else {
		cookie, err = activePrompter.PromptSecret("Grafana session cookie: ")
		if err != nil {
			return fmt.Errorf("login update: %w", err)
		}
		cookie = strings.TrimSpace(cookie)

		if cookie == "" {
			return errors.New("login update: a session cookie is required")
		}
	}

	verifyGrafana := *existing.Grafana
	verifyGrafana.SessionCookie = cookie
	verifyContext := config.Context{Name: name, Grafana: &verifyGrafana}

	if err := session.VerifyCookie(ctx, &verifyContext); err != nil {
		return fmt.Errorf("login update: could not validate session cookie against %s: %w", existing.Grafana.Server, err)
	}

	secret, err := config.EncodeSessionCredential(cookie, existing.Grafana.Server)
	if err != nil {
		return fmt.Errorf("login update: %w", err)
	}

	if err := keychainStore.Set(keychain.Account(name), secret); err != nil {
		return fmt.Errorf("login update: storing session cookie in keychain: %w", err)
	}

	io.Success(cmd.OutOrStdout(), "Refreshed session for context %q", name)

	return nil
}

// guardRebind refuses, before any cookie is prompted for or sent, to bind the context's session to
// newOrigin when the stored item says it belongs elsewhere: the freshly pasted cookie is sent to
// newOrigin to validate it, so a configuration an attacker redirected must not get that far. A
// missing item, an item bound to newOrigin, and a legacy item under the user's own configuration
// pass; everything else (another origin, an untrusted legacy item, an unreadable item) needs --rebind.
func guardRebind(cmd *cobra.Command, opts *UpdateOptions, name, newOrigin string) error {
	oldOrigin, rebindNeeded := storedOriginNeedsRebind(name, newOrigin, legacyBindAllowed(opts.ConfigFile, false))
	if !rebindNeeded {
		return nil
	}

	if !opts.Rebind {
		if oldOrigin == noServerOrigin {
			return fmt.Errorf("login update: the session stored for context %q is not bound to a server yet, and its configuration "+
				"does not come from your own configuration file (--config, GRAFANAPI_CONFIG, XDG_CONFIG_HOME or "+
				"XDG_CONFIG_DIRS is set, or $HOME is not your home directory); refusing to prompt for a session cookie "+
				"(it would be sent to %s to validate it). Re-run from a shell without those overrides, which binds the "+
				"session to the server in your own configuration automatically; only if you really mean to bind it to %s, "+
				"re-run with --rebind",
				name, newOrigin, newOrigin)
		}

		return fmt.Errorf("login update: context %q is bound to %s but its configuration targets %s; "+
			"refusing to prompt for a session cookie (it would be sent to %s to validate it). "+
			"If you really moved this context, re-run from a shell without GRAFANAPI_CONFIG or --config overrides with --rebind",
			name, oldOrigin, newOrigin, newOrigin)
	}

	io.Warning(cmd.ErrOrStderr(), "re-binding context %q from %s to %s", name, oldOrigin, newOrigin)

	return nil
}

// storedOriginNeedsRebind reports whether the item stored for the context is not bound to newOrigin,
// together with how to describe what it is bound to.
func storedOriginNeedsRebind(name, newOrigin string, legacyTrusted bool) (string, bool) {
	raw, err := keychainStore.Get(keychain.Account(name))
	if errors.Is(err, keychain.ErrNotFound) {
		return "", false
	}

	if err != nil {
		return unknownOrigin, true
	}

	cred, err := keychain.DecodeCredential(raw)

	switch {
	case err != nil:
		return unknownOrigin, true
	case cred.Legacy():
		return noServerOrigin, !legacyTrusted
	default:
		return cred.Origin, cred.Origin != newOrigin
	}
}
