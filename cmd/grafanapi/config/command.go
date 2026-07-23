package config

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/caarlos0/env/v11"
	"github.com/grafana/grafanapi/cmd/grafanapi/fail"
	"github.com/grafana/grafanapi/cmd/grafanapi/io"
	"github.com/grafana/grafanapi/internal/config"
	"github.com/grafana/grafanapi/internal/format"
	"github.com/grafana/grafanapi/internal/grafana"
	"github.com/grafana/grafanapi/internal/keychain"
	"github.com/grafana/grafanapi/internal/launchd"
	"github.com/grafana/grafanapi/internal/resources/discovery"
	"github.com/grafana/grafanapi/internal/secrets"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// keychainStore resolves the Grafana session cookie from the platform Keychain during config
// loading (see Options.LoadConfig, Options.LoadRESTConfig, and checkCmd). It is a package-level
// var, defaulting to the real platform store, so tests can substitute a fake keychain.Store via
// SetKeychainStore instead of touching the actual Keychain.
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

// keepaliveController resolves the keep-alive LaunchAgent's launchctl-backed status (loaded?) for
// "config check"'s Keep-alive section (checkKeepAlive). It is a package-level var, defaulting to
// the real launchctl-backed controller, so tests can substitute a fake via
// SetKeepaliveController instead of touching real launchd state.
//
//nolint:gochecknoglobals // test seam; see SetKeepaliveController
var keepaliveController launchd.Controller = launchd.NewExecController()

// SetKeepaliveController overrides the launchd.Controller used by "config check"'s Keep-alive
// section. It exists for tests; it returns a function that restores the previously configured
// controller.
func SetKeepaliveController(controller launchd.Controller) func() {
	previous := keepaliveController
	keepaliveController = controller

	return func() { keepaliveController = previous }
}

type Options struct {
	ConfigFile string
	Context    string
}

func (opts *Options) BindFlags(flags *pflag.FlagSet) {
	flags.StringVar(&opts.ConfigFile, "config", "", "Path to the configuration file to use")
	flags.StringVar(&opts.Context, "context", "", "Name of the context to use")

	_ = cobra.MarkFlagFilename(flags, "config", "yaml", "yml")
}

// loadConfigTolerant loads the configuration file (default, or explicitly set via flags)
// and returns it without validation.
// This function should only be used by config-related commands, to allow the
// user to iterate on the configuration until it becomes valid.
func (opts *Options) loadConfigTolerant(ctx context.Context, extraOverrides ...config.Override) (config.Config, error) {
	overrides := []config.Override{
		// If Grafana-related env variables are set, use them to configure the
		// current context and Grafana config.
		func(cfg *config.Config) error {
			if cfg.CurrentContext == "" {
				cfg.CurrentContext = config.DefaultContextName
			}

			if !cfg.HasContext(cfg.CurrentContext) {
				cfg.SetContext(cfg.CurrentContext, true, config.Context{})
			}

			curCtx := cfg.Contexts[cfg.CurrentContext]

			if curCtx.Grafana == nil {
				curCtx.Grafana = &config.GrafanaConfig{}
			}

			if err := env.Parse(curCtx); err != nil {
				return err
			}

			return nil
		},
	}

	// The current context is being overridden by a flag. This must happen before
	// extraOverrides (credential resolution, validation, ...) so those act on the context the
	// user actually asked for, not the one recorded in the config file.
	if opts.Context != "" {
		overrides = append(overrides, func(cfg *config.Config) error {
			if !cfg.HasContext(opts.Context) {
				return config.ContextNotFound(opts.Context)
			}

			cfg.CurrentContext = opts.Context
			return nil
		})
	}

	overrides = append(overrides, extraOverrides...)

	return config.Load(ctx, opts.configSource(), overrides...)
}

// LoadConfig loads the configuration file (default, or explicitly set via flags), resolves the
// current context's session cookie from the Keychain, and validates it.
//
// Credential resolution runs before validation: Context.Validate ultimately calls
// GrafanaConfig.validateNamespace, which discovers the stack ID via an authenticated /bootdata
// request, so the cookie must already be populated by the time validation runs.
func (opts *Options) LoadConfig(ctx context.Context) (config.Config, error) {
	validator := func(cfg *config.Config) error {
		// Ensure that the current context actually exists.
		if !cfg.HasContext(cfg.CurrentContext) {
			return config.ContextNotFound(cfg.CurrentContext)
		}

		return cfg.GetCurrentContext().Validate()
	}

	return opts.loadConfigTolerant(ctx, config.ResolveSessionCookie(keychainStore), validator)
}

// LoadRESTConfig loads the configuration file (resolving the session cookie via LoadConfig) and
// constructs a REST config from it.
func (opts *Options) LoadRESTConfig(ctx context.Context) (config.NamespacedRESTConfig, error) {
	cfg, err := opts.LoadConfig(ctx)
	if err != nil {
		return config.NamespacedRESTConfig{}, err
	}

	return cfg.GetCurrentContext().ToRESTConfig(ctx), nil
}

func (opts *Options) configSource() config.Source {
	if opts.ConfigFile != "" {
		return config.ExplicitConfigFile(opts.ConfigFile)
	}

	return config.StandardLocation()
}

func Command() *cobra.Command {
	configOpts := &Options{}

	cmd := &cobra.Command{
		Use:   "config",
		Short: "View or manipulate configuration settings",
		Long: fmt.Sprintf(`View or manipulate configuration settings.

The configuration file to load is chosen as follows:

1. If the --config flag is set, then that file will be loaded. No other location will be considered.
2. If the $%[3]s environment variable is set, then that file will be loaded. No other location will be considered.
3. If the $XDG_CONFIG_HOME environment variable is set, then it will be used: $XDG_CONFIG_HOME/%[1]s/%[2]s
   Example: /home/user/.config/%[1]s/%[2]s
4. If the $HOME environment variable is set, then it will be used: $HOME/.config/%[1]s/%[2]s
   Example: /home/user/.config/%[1]s/%[2]s
5. If the $XDG_CONFIG_DIRS environment variable is set, then it will be used: $XDG_CONFIG_DIRS/%[1]s/%[2]s
   Example: /etc/xdg/%[1]s/%[2]s
`, config.StandardConfigFolder, config.StandardConfigFileName, config.ConfigFileEnvVar),
	}

	configOpts.BindFlags(cmd.PersistentFlags())

	cmd.AddCommand(checkCmd(configOpts))
	cmd.AddCommand(currentContextCmd(configOpts))
	cmd.AddCommand(setCmd(configOpts))
	cmd.AddCommand(unsetCmd(configOpts))
	cmd.AddCommand(useContextCmd(configOpts))
	cmd.AddCommand(viewCmd(configOpts))
	cmd.AddCommand(listContextsCmd(configOpts))

	return cmd
}

type viewOpts struct {
	IO io.Options

	Minify bool
	Raw    bool
}

func (opts *viewOpts) BindFlags(flags *pflag.FlagSet) {
	opts.IO.DefaultFormat("yaml")
	opts.IO.BindFlags(flags)

	// Override the default yaml codec to enable bytes ↔ base64 conversion
	opts.IO.RegisterCustomCodec("yaml", &format.YAMLCodec{
		BytesAsBase64: true,
	})

	flags.BoolVar(&opts.Minify, "minify", opts.Minify, "Remove all information not used by current-context from the output")
	flags.BoolVar(&opts.Raw, "raw", opts.Raw, "Display sensitive information")
}

func (opts *viewOpts) Validate() error {
	if err := opts.IO.Validate(); err != nil {
		return err
	}

	return nil
}

func viewCmd(configOpts *Options) *cobra.Command {
	opts := &viewOpts{}

	cmd := &cobra.Command{
		Use:     "view",
		Args:    cobra.NoArgs,
		Short:   "Display the current configuration",
		Example: "\n\tgrafanapi config view",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := opts.Validate(); err != nil {
				return err
			}

			cfg, err := configOpts.loadConfigTolerant(cmd.Context())
			if err != nil {
				return err
			}

			if opts.Minify {
				cfg, err = config.Minify(cfg)
				if err != nil {
					return err
				}
			}

			if !opts.Raw {
				if err := secrets.Redact(&cfg); err != nil {
					return fmt.Errorf("could not redact secrets from configuration: %w", err)
				}
			}

			codec, err := opts.IO.Codec()
			if err != nil {
				return err
			}

			return codec.Encode(cmd.OutOrStdout(), cfg)
		},
	}

	opts.BindFlags(cmd.Flags())

	return cmd
}

func currentContextCmd(configOpts *Options) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "current-context",
		Args:    cobra.NoArgs,
		Short:   "Display the current context name",
		Long:    "Display the current context name.",
		Example: "\n\tgrafanapi config current-context",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := configOpts.loadConfigTolerant(cmd.Context())
			if err != nil {
				return err
			}

			cmd.Println(cfg.CurrentContext)

			return nil
		},
	}

	return cmd
}

func listContextsCmd(configOpts *Options) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "list-contexts",
		Args:    cobra.NoArgs,
		Short:   "List the contexts defined in the configuration",
		Long:    "List the contexts defined in the configuration.",
		Example: "\n\tgrafanapi config list-contexts",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := configOpts.loadConfigTolerant(cmd.Context())
			if err != nil {
				return err
			}

			tab := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', tabwriter.TabIndent|tabwriter.DiscardEmptyColumns)

			fmt.Fprintf(tab, "CURRENT\tNAME\tGRAFANA SERVER\n")
			for _, context := range cfg.Contexts {
				server := " "
				if context.Grafana != nil {
					server = context.Grafana.Server
				}

				current := " "
				if cfg.CurrentContext == context.Name {
					current = "*"
				}

				fmt.Fprintf(tab, "%s\t%s\t%s\n", current, context.Name, server)
			}

			return tab.Flush()
		},
	}

	return cmd
}

func checkCmd(configOpts *Options) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "check",
		Args:    cobra.NoArgs,
		Short:   "Check the current configuration for issues",
		Long:    "Check the current configuration for issues.",
		Example: "\n\tgrafanapi config check",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := configOpts.loadConfigTolerant(cmd.Context())
			if err != nil {
				return err
			}

			stdout := cmd.OutOrStdout()

			io.Success(stdout, "Configuration file: %s", io.Green(cfg.Source))

			switch {
			case cfg.CurrentContext == "":
				io.Error(stdout, "Current context: %s", io.Red("<undefined>"))
			case !cfg.HasContext(cfg.CurrentContext):
				io.Error(stdout, "Current context: %s", io.Red(config.ContextNotFound(cfg.CurrentContext).Error()))
			default:
				io.Success(stdout, "Current context: %s", io.Green(cfg.CurrentContext))
			}

			cmd.Println()

			for _, gCtx := range cfg.Contexts {
				checkContext(cmd, gCtx)
			}

			checkKeepAlive(cmd, cfg)

			return nil
		},
	}

	return cmd
}

func checkContext(cmd *cobra.Command, gCtx *config.Context) {
	stdout := cmd.OutOrStdout()
	title := "Context: "
	titleLen := len(title) + len(gCtx.Name)
	title += io.Bold(gCtx.Name)

	summarizeError := func(err error) string {
		detailedErr := fail.ErrorToDetailedError(err)

		return fmt.Sprintf("%s: %s", detailedErr.Summary, err.Error())
	}

	printSuggestions := func(err error) {
		detailedErr := fail.ErrorToDetailedError(err)
		if len(detailedErr.Suggestions) != 0 {
			io.Info(stdout, "Suggestions:\n")
			for _, suggestion := range detailedErr.Suggestions {
				fmt.Fprintf(stdout, "  • %s\n", suggestion)
			}
			stdout.Write([]byte("\n"))
		}
	}

	cmd.Println(io.Yellow(title))
	cmd.Println(io.Yellow(strings.Repeat("=", titleLen)))

	// Resolve this context's session cookie before validating/probing it: validation may trigger
	// stack-ID discovery (an authenticated /bootdata request), and the connectivity/version
	// probes below always need the cookie. Unlike Options.LoadConfig (which only resolves the
	// current context via an Override), "config check" inspects every context, so it calls the
	// per-context helper directly.
	if err := config.ResolveContextSessionCookie(keychainStore, gCtx); err != nil {
		io.Warning(stdout, "Session cookie: %s\n", io.Yellow(summarizeError(err)))
	}

	if err := gCtx.Validate(); err != nil {
		io.Error(stdout, "Configuration: %s", io.Red(summarizeError(err)))
		io.Warning(stdout, "Connectivity: %s", io.Yellow("skipped"))
		io.Warning(stdout, "Grafana version: %s", io.Yellow("skipped")+"\n")

		printSuggestions(err)
		return
	}

	io.Success(stdout, "Configuration: %s", io.Green("valid"))

	if _, err := discovery.NewDefaultRegistry(cmd.Context(), config.NewNamespacedRESTConfig(cmd.Context(), *gCtx)); err != nil {
		io.Error(stdout, "Connectivity: %s", io.Red(summarizeError(err)))
		io.Warning(stdout, "Grafana version: %s", io.Yellow("skipped")+"\n")
		printSuggestions(err)
		return
	}

	io.Success(stdout, "Connectivity: %s", io.Green("online"))

	version, err := grafana.GetVersion(gCtx)
	if err != nil {
		io.Error(stdout, "Grafana version: %s", io.Red(summarizeError(err))+"\n")
		return
	}

	if version.Major() < 12 {
		io.Error(stdout, "Grafana version: %s", io.Red(version.String()+" (Grafana >= 12.0.0 is required)")+"\n")
		return
	}

	io.Success(stdout, "Grafana version: %s", io.Green(version.String())+"\n")
}

// checkKeepAlive renders "config check"'s Keep-alive section: whether the keep-alive LaunchAgent
// (internal/launchd; see "session keepalive") is installed and loaded, its interval and target
// binary, and, per context, its "live-window" opt-in and last-rotation age (from the Keychain
// item's modification time). Every launchd/Keychain inspection here is best-effort - an error
// degrades the affected line to "unknown"/"none" rather than aborting, so "config check" always
// exits 0 regardless of keep-alive status.
func checkKeepAlive(cmd *cobra.Command, cfg config.Config) {
	stdout := cmd.OutOrStdout()

	title := "Keep-alive"
	cmd.Println(io.Yellow(io.Bold(title)))
	cmd.Println(io.Yellow(strings.Repeat("=", len(title))))

	if _, err := os.Stat(launchd.PlistPath()); err != nil {
		io.Warning(stdout, "LaunchAgent: %s", io.Yellow("not installed"))
	} else {
		loaded := "no"
		if _, err := keepaliveController.Print(launchd.UserServiceTarget()); err == nil {
			loaded = "yes"
		}

		interval := "unknown"
		binary := "unknown"

		if spec, err := launchd.Inspect(launchd.PlistPath()); err == nil {
			interval = (time.Duration(spec.IntervalSeconds) * time.Second).String()
			binary = spec.BinaryPath
		} else {
			io.Warning(stdout, "LaunchAgent: installed but could not be inspected: %s", err)
		}

		io.Success(stdout, "LaunchAgent: %s (loaded: %s, interval: %s, binary: %s)",
			io.Green("installed"), loaded, interval, binary)
	}

	names := make([]string, 0, len(cfg.Contexts))
	for name := range cfg.Contexts {
		names = append(names, name)
	}

	sort.Strings(names)

	for _, name := range names {
		gCtx := cfg.Contexts[name]

		liveWindow := "not set"
		if gCtx != nil && gCtx.Grafana != nil && gCtx.Grafana.LiveWindow != "" {
			liveWindow = gCtx.Grafana.LiveWindow
		}

		age := "none"
		if modAt, err := keychainStore.ModifiedAt(keychain.Account(name)); err == nil {
			age = fmt.Sprintf("rotated %s ago", time.Since(modAt).Round(time.Second))
		}

		io.Info(stdout, "  %s: live-window=%s, last-rotation=%s", io.Bold(name), liveWindow, age)
	}

	cmd.Println()
}

func useContextCmd(configOpts *Options) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "use-context CONTEXT_NAME",
		Args:    cobra.ExactArgs(1),
		Aliases: []string{"use"},
		Short:   "Set the current context",
		Long:    "Set the current context and updates the configuration file.",
		Example: "\n\tgrafanapi config use-context dev-instance",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := configOpts.loadConfigTolerant(cmd.Context())
			if err != nil {
				return err
			}

			if !cfg.HasContext(args[0]) {
				return config.ContextNotFound(args[0])
			}

			cfg.CurrentContext = args[0]

			if err := config.Write(cmd.Context(), configOpts.configSource(), cfg); err != nil {
				return err
			}

			io.Success(cmd.OutOrStdout(), "Context set to \"%s\"", cfg.CurrentContext)
			return nil
		},
	}

	return cmd
}

func setCmd(configOpts *Options) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "set PROPERTY_NAME PROPERTY_VALUE",
		Args:  cobra.ExactArgs(2),
		Short: "Set an single value in a configuration file",
		Long: `Set an single value in a configuration file

PROPERTY_NAME is a dot-delimited reference to the value to unset. It can either represent a field or a map entry.

PROPERTY_VALUE is the new value to set.`,
		Example: `
	# Set the "server" field on the "dev-instance" context to "https://grafana-dev.example"
	grafanapi config set contexts.dev-instance.grafana.server https://grafana-dev.example

	# Disable the validation of the server's SSL certificate in the "dev-instance" context
	grafanapi config set contexts.dev-instance.grafana.insecure-skip-tls-verify true`,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := configOpts.loadConfigTolerant(cmd.Context())
			if err != nil {
				return err
			}

			if err := config.SetValue(&cfg, args[0], args[1]); err != nil {
				return err
			}

			return config.Write(cmd.Context(), configOpts.configSource(), cfg)
		},
	}

	return cmd
}

func unsetCmd(configOpts *Options) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "unset PROPERTY_NAME",
		Args:  cobra.ExactArgs(1),
		Short: "Unset an single value in a configuration file",
		Long: `Unset an single value in a configuration file.

PROPERTY_NAME is a dot-delimited reference to the value to unset. It can either represent a field or a map entry.`,
		Example: `
	# Unset the "foo" context
	grafanapi config unset contexts.foo

	# Unset the "insecure-skip-tls-verify" flag in the "dev-instance" context
	grafanapi config unset contexts.dev-instance.grafana.insecure-skip-tls-verify`,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := configOpts.loadConfigTolerant(cmd.Context())
			if err != nil {
				return err
			}

			if err := config.UnsetValue(&cfg, args[0]); err != nil {
				return err
			}

			if err := config.Write(cmd.Context(), configOpts.configSource(), cfg); err != nil {
				return err
			}

			// Unsetting an entire context removes it from the config file, but its stored
			// session cookie would otherwise remain in the Keychain forever with nothing left
			// to reference it. Best-effort clean it up too; a failure here (e.g. the item
			// simply never having existed) must not fail the command.
			if contextName, ok := removedContextName(args[0]); ok {
				if err := keychainStore.Delete(keychain.Account(contextName)); err != nil {
					io.Warning(cmd.OutOrStdout(), "Could not remove the session cookie for context %q from the Keychain: %s\n", contextName, err)
				}
			}

			return nil
		},
	}

	return cmd
}

// removedContextName reports whether path unsets an entire context ("contexts.<name>", as opposed
// to a nested field like "contexts.<name>.grafana.server"), returning the context name.
func removedContextName(path string) (string, bool) {
	parts := strings.Split(path, ".")
	if len(parts) == 2 && parts[0] == "contexts" {
		return parts[1], true
	}

	return "", false
}
