// Package session implements the `grafanapi session` command group: it lets grafanapi keep its
// own Grafana sessions alive proactively, on a schedule, instead of relying solely on the
// reactive 401-triggered rotation that already happens on every request (see
// internal/config/session_source.go). `session refresh` exposes that rotation as a first-class,
// force-now command (unconditionally, or restricted to whichever contexts are due per their
// `live-window`); `session keepalive` schedules `session refresh --due` through a macOS launchd
// LaunchAgent (see internal/launchd) so opted-in contexts stay warm without manual action.
package session

import (
	"fmt"

	"github.com/grafana/grafanapi/internal/config"
	"github.com/grafana/grafanapi/internal/keychain"
	"github.com/grafana/grafanapi/internal/launchd"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// keychainStore is the platform Keychain read by `session refresh`/`session keepalive` to resolve
// each context's stored session cookie and last-rotation time. It is a package-level var,
// defaulting to the real store, so tests can substitute a fake keychain.Store via
// SetKeychainStore instead of touching the actual Keychain.
//
//nolint:gochecknoglobals // test seam for the platform Keychain; see SetKeychainStore.
var keychainStore keychain.Store = keychain.NewStore()

// controller is the launchd.Controller used by `session keepalive install/status/uninstall` to
// bootstrap, boot out, and query the keep-alive LaunchAgent. It is a package-level var,
// defaulting to the real launchctl-backed controller, so tests can substitute a fake
// launchd.Controller via SetController instead of touching real launchd state.
//
//nolint:gochecknoglobals // test seam for launchctl; see SetController.
var controller launchd.Controller = launchd.NewExecController()

// SetKeychainStore overrides the keychain.Store used by the session command group. It exists for
// tests; it returns a function that restores the previously configured store.
func SetKeychainStore(store keychain.Store) func() {
	previous := keychainStore
	keychainStore = store

	return func() { keychainStore = previous }
}

// SetController overrides the launchd.Controller used by the session command group. It exists
// for tests; it returns a function that restores the previously configured controller.
func SetController(c launchd.Controller) func() {
	previous := controller
	controller = c

	return func() { controller = previous }
}

// Command returns the `session` command.
func Command() *cobra.Command {
	opts := &Options{}

	cmd := &cobra.Command{
		Use:   "session",
		Short: "Manage Grafana session keep-alive",
		Long: `Manage Grafana session keep-alive.

grafanapi authenticates to Grafana using a session cookie (see "grafanapi login"), which Grafana
rotates automatically on use and otherwise expires after a period of inactivity. The "session"
command group lets grafanapi keep a context's session alive proactively instead of relying solely
on that reactive, request-triggered rotation: "session refresh" forces a rotation now, and
"session keepalive" schedules that refresh through a macOS LaunchAgent for contexts that opt in
via the "live-window" configuration field.`,
	}

	opts.BindFlags(cmd.PersistentFlags())

	cmd.AddCommand(refreshCmd(opts))
	cmd.AddCommand(keepaliveCommand(opts))

	return cmd
}

// Options holds the flags shared by every `session` subcommand.
type Options struct {
	ConfigFile string
	Context    string
}

// BindFlags registers the flags shared by every `session` subcommand on flags.
func (opts *Options) BindFlags(flags *pflag.FlagSet) {
	flags.StringVar(&opts.ConfigFile, "config", "", "Path to the configuration file to use")
	flags.StringVar(&opts.Context, "context", "", "Name of the context to operate on (defaults to the current context)")

	_ = cobra.MarkFlagFilename(flags, "config", "yaml", "yml")
}

// configSource returns the config.Source `session` subcommands load configuration from: the
// explicit --config file if given, otherwise the standard XDG location.
func (opts *Options) configSource() config.Source {
	if opts.ConfigFile != "" {
		return config.ExplicitConfigFile(opts.ConfigFile)
	}

	return config.StandardLocation()
}

// keepaliveCommand returns the `session keepalive` command. This is a placeholder: the real
// install/status/uninstall subcommands land in keepalive.go in a later task.
func keepaliveCommand(_ *Options) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "keepalive",
		Short: "Schedule session refresh through a macOS LaunchAgent",
		Long: `Schedule "session refresh --due" through a macOS launchd LaunchAgent, so contexts that
opt in via the "live-window" configuration field stay warm without any manual action.`,
	}

	notImplemented := func(use, short string) *cobra.Command {
		return &cobra.Command{
			Use:   use,
			Short: short,
			RunE: func(*cobra.Command, []string) error {
				return fmt.Errorf("session keepalive %s: not yet implemented", use)
			},
		}
	}

	cmd.AddCommand(notImplemented("install", "Install the keep-alive LaunchAgent"))
	cmd.AddCommand(notImplemented("status", "Show the keep-alive LaunchAgent's status"))
	cmd.AddCommand(notImplemented("uninstall", "Remove the keep-alive LaunchAgent"))

	return cmd
}
