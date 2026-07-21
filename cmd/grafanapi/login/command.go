// Package login implements the `grafanapi login` (and `login update`) commands: they collect a
// Grafana session cookie interactively (never via flag or environment variable), validate it
// against a live Grafana server, and — only on success — persist the non-secret context data
// (server, org-id/stack-id, TLS) to the configuration file and the cookie itself to the platform
// Keychain (internal/keychain).
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

// keychainStore is the platform Keychain used to persist the session cookie on successful login
// (and to overwrite it on `login update`). It is a package-level var, defaulting to the real
// store, so tests can substitute a fake keychain.Store via SetKeychainStore instead of touching
// the actual Keychain.
//
//nolint:gochecknoglobals // test seam for the platform Keychain; see SetKeychainStore.
var keychainStore keychain.Store = keychain.NewStore()

// activePrompter collects interactive input for login/login-update. It defaults to the
// production ttyPrompter; tests substitute a fake via SetPrompter.
//
//nolint:gochecknoglobals // test seam for interactive input; see SetPrompter.
var activePrompter prompter = ttyPrompter{}

// SetKeychainStore overrides the keychain.Store used by login/login update. It exists for tests;
// it returns a function that restores the previously configured store.
func SetKeychainStore(store keychain.Store) func() {
	previous := keychainStore
	keychainStore = store

	return func() { keychainStore = previous }
}

// SetPrompter overrides the prompter used by login/login update. It exists for tests; it returns
// a function that restores the previously configured prompter.
func SetPrompter(p prompter) func() {
	previous := activePrompter
	activePrompter = p

	return func() { activePrompter = previous }
}

// Command returns the `login` command.
func Command() *cobra.Command {
	opts := &Options{}

	cmd := &cobra.Command{
		Use:   "login",
		Args:  cobra.NoArgs,
		Short: "Authenticate to a Grafana instance using a session cookie",
		Long: `Authenticate to a Grafana instance using a browser session cookie (grafana_session).

The cookie is validated against the target server (GET /api/user) before anything is persisted.
It is never accepted as a command-line flag or environment variable — only via an interactive,
no-echo prompt. On success, the context (server, org-id/stack-id, TLS) is written to the
configuration file and the cookie itself is stored in the macOS Keychain, never in the plaintext
configuration file.`,
		Example: "\n\tgrafanapi login --server https://grafana.example.com",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runLogin(cmd, opts)
		},
	}

	opts.BindFlags(cmd.Flags())

	cmd.AddCommand(updateCommand())

	return cmd
}

// Options holds the flags accepted by `login`.
type Options struct {
	ConfigFile string
	Context    string
	Server     string
	OrgID      int64
	StackID    int64
}

func (opts *Options) BindFlags(flags *pflag.FlagSet) {
	flags.StringVar(&opts.ConfigFile, "config", "", "Path to the configuration file to use")
	flags.StringVar(&opts.Context, "context", "", "Name of the context to create or update (defaults to the current context, or \"default\")")
	flags.StringVar(&opts.Server, "server", "", "Grafana server URL; skips the interactive server prompt")
	flags.Int64Var(&opts.OrgID, "org-id", 0, "Organization ID, for on-prem Grafana; skips Grafana Cloud stack-id discovery")
	flags.Int64Var(&opts.StackID, "stack-id", 0, "Grafana Cloud stack ID; skips stack-id discovery")

	_ = cobra.MarkFlagFilename(flags, "config", "yaml", "yml")
}

func (opts *Options) configSource() config.Source {
	if opts.ConfigFile != "" {
		return config.ExplicitConfigFile(opts.ConfigFile)
	}

	return config.StandardLocation()
}

// contextName resolves which context login should create or update: the --context flag if set,
// otherwise the config file's current context, otherwise config.DefaultContextName.
func (opts *Options) contextName(cfg config.Config) string {
	if opts.Context != "" {
		return opts.Context
	}

	if cfg.CurrentContext != "" {
		return cfg.CurrentContext
	}

	return config.DefaultContextName
}

func runLogin(cmd *cobra.Command, opts *Options) error {
	ctx := cmd.Context()

	cfg, err := config.Load(ctx, opts.configSource())
	if err != nil {
		return err
	}

	name := opts.contextName(cfg)

	var existingGrafana config.GrafanaConfig
	if existing := cfg.Contexts[name]; existing != nil && existing.Grafana != nil {
		existingGrafana = *existing.Grafana
	}

	server := strings.TrimSpace(opts.Server)
	if server == "" {
		server, err = activePrompter.PromptLine("Grafana server URL: ")
		if err != nil {
			return fmt.Errorf("login: %w", err)
		}
		server = strings.TrimSpace(server)
	}

	if server == "" {
		return errors.New("login: a Grafana server URL is required (pass --server or enter one at the prompt)")
	}

	cookie, err := activePrompter.PromptSecret("Grafana session cookie: ")
	if err != nil {
		return fmt.Errorf("login: %w", err)
	}
	cookie = strings.TrimSpace(cookie)

	if cookie == "" {
		return errors.New("login: a session cookie is required")
	}

	orgID := opts.OrgID
	if orgID == 0 {
		orgID = existingGrafana.OrgID
	}

	stackID := opts.StackID
	if stackID == 0 {
		stackID = existingGrafana.StackID
	}

	grafanaCfg := config.GrafanaConfig{
		Server:        server,
		OrgID:         orgID,
		StackID:       stackID,
		TLS:           existingGrafana.TLS,
		SessionCookie: cookie,
	}

	// If the caller didn't pin an org-id or stack-id, best-effort discover the Grafana Cloud
	// stack ID using the freshly entered cookie. Discovery failure is not fatal here: `config
	// check`/other commands will surface a clear "missing org-id or stack-id" error later if
	// discovery keeps failing and neither was ever set.
	if grafanaCfg.OrgID == 0 && grafanaCfg.StackID == 0 {
		if discovered, discoverErr := config.DiscoverStackID(ctx, grafanaCfg); discoverErr == nil {
			grafanaCfg.StackID = discovered
		}
	}

	newContext := config.Context{Name: name, Grafana: &grafanaCfg}

	if err := session.VerifyCookie(ctx, &newContext); err != nil {
		return fmt.Errorf("login: could not validate session cookie against %s: %w", server, err)
	}

	makeCurrent := cfg.CurrentContext == ""
	cfg.SetContext(name, makeCurrent, newContext)

	// login can be re-run against an already-authenticated context (the existing org-id/stack-id/
	// TLS block above is preserved for exactly that flow), in which case a cookie already sits in
	// the Keychain for this account. Read it before overwriting it: if the subsequent config.Write
	// fails, we can restore the prior cookie instead of deleting the account outright, so a
	// previously-working credential is never destroyed just because persisting the new config
	// failed. keychainStore.Get returning keychain.ErrNotFound means there genuinely is no prior
	// cookie — nothing to lose or restore either way. Any other Get error is NOT the same as "no
	// prior cookie": it only means we failed to read whatever is there, so the rollback below
	// treats it conservatively instead of assuming it's safe to delete.
	priorCookie, priorErr := keychainStore.Get(keychain.Account(name))
	hadPriorCookie := priorErr == nil
	priorLookupFailed := priorErr != nil && !errors.Is(priorErr, keychain.ErrNotFound)

	// Store the cookie before writing the config: if Set fails, nothing has been persisted to
	// disk yet, so there's no partial state to clean up. If the subsequent config.Write fails
	// instead, best-effort roll back the Keychain item: restore the prior cookie if this context
	// already had one, delete the just-created entry if it didn't, or — if we couldn't even tell
	// whether a prior cookie existed — leave the item untouched rather than risk destroying a
	// working credential we simply failed to read.
	if err := keychainStore.Set(keychain.Account(name), cookie); err != nil {
		return fmt.Errorf("login: storing session cookie in keychain: %w", err)
	}

	if err := config.Write(ctx, opts.configSource(), cfg); err != nil {
		switch {
		case hadPriorCookie:
			_ = keychainStore.Set(keychain.Account(name), priorCookie)
		case priorLookupFailed:
			io.Warning(cmd.ErrOrStderr(), "could not determine whether context %q already had a session cookie; leaving the keychain item as stored rather than risk deleting an existing credential", name)
		default:
			_ = keychainStore.Delete(keychain.Account(name))
		}

		return fmt.Errorf("login: writing configuration: %w", err)
	}

	io.Success(cmd.OutOrStdout(), "Logged in to %s as context %q", server, name)

	return nil
}
