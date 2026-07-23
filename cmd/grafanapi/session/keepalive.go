package session

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	cmdio "github.com/grafana/grafanapi/cmd/grafanapi/io"
	"github.com/grafana/grafanapi/internal/config"
	"github.com/grafana/grafanapi/internal/durationbounds"
	"github.com/grafana/grafanapi/internal/format"
	"github.com/grafana/grafanapi/internal/launchd"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

const (
	// derivedIntervalMin is the smallest StartInterval "session keepalive install" will derive
	// automatically from the minimum opted-in live-window. A very short live-window (down to the
	// [1m,6d]-wide floor of 1m) is still clamped up to this so the agent does not wake needlessly
	// often - "session refresh --due" re-checks every context's own window on each wake, so a
	// coarser cadence than the window itself is sufficient.
	derivedIntervalMin = 15 * time.Minute
	// derivedIntervalMax is the largest StartInterval "session keepalive install" will derive
	// automatically. A very long live-window is still clamped down to this so a context does not
	// go further past its window than necessary before the next scheduled check.
	derivedIntervalMax = 12 * time.Hour
	// logTailLines is how many trailing lines of the keep-alive log "session keepalive status"
	// reports.
	logTailLines = 10
	// launchAgentsDirPerm is the permission used when creating the LaunchAgents and Logs
	// directories "session keepalive install" writes into. They are not secret-bearing (the plist
	// and log contain no cookie value - see LogPath's GoDoc), so the standard non-secret directory
	// mode is used, matching the LaunchAgents/Logs directories macOS itself creates.
	launchAgentsDirPerm = 0o755
)

// keepaliveCommand returns the `session keepalive` command, with its three subcommands: install,
// status, and uninstall. It schedules "session refresh --due" through a macOS launchd LaunchAgent
// (see internal/launchd) so contexts that opt in via the "live-window" configuration field stay
// warm without any manual action.
func keepaliveCommand(opts *Options) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "keepalive",
		Short: "Schedule session refresh through a macOS LaunchAgent",
		Long: `Schedule "session refresh --due" through a macOS launchd LaunchAgent, so contexts that
opt in via the "live-window" configuration field stay warm without any manual action.`,
	}

	cmd.AddCommand(keepaliveInstallCmd(opts))
	cmd.AddCommand(keepaliveStatusCmd(opts))
	cmd.AddCommand(keepaliveUninstallCmd(opts))

	return cmd
}

// keepaliveInstallCmd returns the `session keepalive install` command.
func keepaliveInstallCmd(opts *Options) *cobra.Command {
	var interval string

	cmd := &cobra.Command{
		Use:   "install",
		Args:  cobra.NoArgs,
		Short: "Install the keep-alive LaunchAgent",
		Long: `Install the keep-alive LaunchAgent: a macOS launchd LaunchAgent that periodically runs
"session refresh --due", so every context whose "live-window" is set stays warm without any manual
action.

At least one context must have "live-window" set (grafanapi config set contexts.<name>.grafana.
live-window 12h) before this command will install anything.

By default the agent's wake interval is derived from the minimum live-window across every opted-in
context (min/2, clamped to [15m, 12h]) - "session refresh --due" re-checks each context's own window
on every wake, so a modest, derived cadence is sufficient. --interval overrides that derivation: a
Go duration, or a bare day count with a "d" suffix such as "6d" (a grafanapi extension), validated
against [1m, 6d] and never clamped to the narrower derived range.

Installing is idempotent: running it again (e.g. after adding another opted-in context) replaces
the previous plist and reloads it.`,
		Example: "\n\tgrafanapi session keepalive install\n\tgrafanapi session keepalive install --interval 6h",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.Load(cmd.Context(), opts.configSource())
			if err != nil {
				return err
			}

			return installKeepAlive(cmd, &cfg, interval)
		},
	}

	cmd.Flags().StringVar(&interval, "interval", "",
		`How often the LaunchAgent wakes to check for due contexts: a Go duration, or a bare day `+
			`count with a "d" suffix such as "6d" (default: derived from the minimum live-window, `+
			`min/2 clamped to [15m, 12h])`)

	return cmd
}

// installKeepAlive is `session keepalive install`'s testable body: it derives (or parses/validates,
// if interval is non-empty) the LaunchAgent's StartInterval from cfg's opted-in contexts, writes the
// plist, and (re)loads it via launchctl. interval is the raw --interval flag value; an empty string
// means "derive from the minimum opted-in live-window".
func installKeepAlive(cmd *cobra.Command, cfg *config.Config, interval string) error {
	minWindow, ok, warnings := minLiveWindowAcrossContexts(cfg)
	for _, warning := range warnings {
		cmdio.Warning(cmd.OutOrStdout(), "%s", warning)
	}

	if !ok {
		return errors.New("session keepalive install: no context opts into keep-alive; " +
			"set one first, e.g. grafanapi config set contexts.<name>.grafana.live-window 12h")
	}

	effectiveInterval := deriveInterval(minWindow)
	if interval != "" {
		parsed, err := durationbounds.ParseWithDays(interval)
		if err != nil {
			return fmt.Errorf("session keepalive install: %w", err)
		}

		if err := launchd.ValidateInterval(parsed); err != nil {
			return fmt.Errorf("session keepalive install: %w", err)
		}

		effectiveInterval = parsed
	}

	binary, err := launchd.ResolveBinaryPath()
	if err != nil {
		return fmt.Errorf("session keepalive install: %w", err)
	}

	spec := launchd.DefaultAgentSpec(binary, effectiveInterval)

	if err := writePlist(spec); err != nil {
		return fmt.Errorf("session keepalive install: %w", err)
	}

	// Idempotent (re)install: boot the previous instance (if any) out first, ignoring a
	// "not loaded" failure (expected on a first install, or after a manual bootout). When
	// Bootout fails without being recognized as ErrNotLoaded (e.g. a launchctl version
	// whose wording internal/launchd hasn't seen), ClassifyLoadState asks Print directly
	// whether the service is actually still loaded before deciding:
	//   - NotLoaded: confirmed absent - treated the same as ErrNotLoaded, proceed silently.
	//   - Loaded: confirmed still loaded - surfaced as a warning, not an abort, since the
	//     plist rewrite and Bootstrap below may still succeed even if the old instance could
	//     not be booted out first.
	//   - Inconclusive: Print itself failed for an unrelated reason (permission denied,
	//     launchd unavailable, ...) - we genuinely don't know whether the old instance is
	//     still loaded, so this also warns (with a distinct message) rather than aborting.
	serviceTarget := launchd.UserServiceTarget()

	if err := controller.Bootout(serviceTarget); err != nil && !errors.Is(err, launchd.ErrNotLoaded) {
		switch launchd.ClassifyLoadState(controller, serviceTarget) {
		case launchd.LoadStateNotLoaded:
			// Confirmed absent - proceed exactly as for a directly-recognized ErrNotLoaded.
		case launchd.LoadStateLoaded:
			cmdio.Warning(cmd.OutOrStdout(),
				"could not boot out the previous LaunchAgent instance before reinstalling: %s", err)
		case launchd.LoadStateInconclusive:
			cmdio.Warning(cmd.OutOrStdout(),
				"could not boot out the previous LaunchAgent instance before reinstalling, and could "+
					"not determine whether it is still loaded (launchd error: %s)", err)
		}
	}

	if err := controller.Bootstrap(launchd.UserDomainTarget(), launchd.PlistPath()); err != nil {
		cmdio.Warning(cmd.OutOrStdout(),
			"wrote the LaunchAgent plist but could not load it via launchctl: %s\nYou can load it manually: launchctl bootstrap %s %s",
			err, launchd.UserDomainTarget(), launchd.PlistPath())

		return nil
	}

	cmdio.Success(cmd.OutOrStdout(),
		"installed keepalive LaunchAgent (polling every %s, honoring each context's live-window); logs: %s",
		effectiveInterval, launchd.LogPath())

	return nil
}

// keepaliveStatusOpts holds the flags for `session keepalive status`.
type keepaliveStatusOpts struct {
	IO cmdio.Options
}

func (opts *keepaliveStatusOpts) setup(flags *pflag.FlagSet) {
	opts.IO.RegisterCustomCodec("text", &statusTextCodec{})
	opts.IO.DefaultFormat("text")
	opts.IO.BindFlags(flags)
}

// keepaliveStatusCmd returns the `session keepalive status` command.
func keepaliveStatusCmd(_ *Options) *cobra.Command {
	opts := &keepaliveStatusOpts{}

	cmd := &cobra.Command{
		Use:     "status",
		Args:    cobra.NoArgs,
		Short:   "Show the keep-alive LaunchAgent's status",
		Long:    `Show whether the keep-alive LaunchAgent is installed and loaded, its interval and target binary, and a tail of its log (which never contains a session cookie).`,
		Example: "\n\tgrafanapi session keepalive status\n\tgrafanapi session keepalive status -o json",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := opts.IO.Validate(); err != nil {
				return err
			}

			codec, err := opts.IO.Codec()
			if err != nil {
				return err
			}

			return codec.Encode(cmd.OutOrStdout(), gatherStatus())
		},
	}

	opts.setup(cmd.Flags())

	return cmd
}

// keepaliveUninstallCmd returns the `session keepalive uninstall` command.
func keepaliveUninstallCmd(_ *Options) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "uninstall",
		Args:    cobra.NoArgs,
		Short:   "Remove the keep-alive LaunchAgent",
		Long:    `Remove the keep-alive LaunchAgent: boot it out of launchd and delete its plist. Idempotent - it succeeds even when no LaunchAgent is currently installed.`,
		Example: "\n\tgrafanapi session keepalive uninstall",
		RunE: func(cmd *cobra.Command, _ []string) error {
			// Ignore a "not loaded" failure - uninstall must be idempotent. When Bootout fails
			// without being recognized as ErrNotLoaded (e.g. a launchctl version whose wording
			// internal/launchd hasn't seen), ClassifyLoadState asks Print directly whether the
			// service is actually still loaded before failing:
			//   - NotLoaded: confirmed absent - treated the same as ErrNotLoaded, proceed and remove
			//     the plist.
			//   - Loaded: confirmed still loaded - a genuine problem; fail without deleting the
			//     plist or reporting success, since the agent may still be loaded.
			//   - Inconclusive: Print itself failed for an unrelated reason (permission denied,
			//     launchd unavailable, ...) - we genuinely don't know whether the agent is still
			//     loaded, so this also fails (with a distinct message) rather than risking a false
			//     "removed" - the exact false-success outcome this disambiguation exists to prevent.
			serviceTarget := launchd.UserServiceTarget()

			if err := controller.Bootout(serviceTarget); err != nil && !errors.Is(err, launchd.ErrNotLoaded) {
				switch launchd.ClassifyLoadState(controller, serviceTarget) {
				case launchd.LoadStateNotLoaded:
					// Confirmed absent - proceed exactly as for a directly-recognized ErrNotLoaded.
				case launchd.LoadStateLoaded:
					return fmt.Errorf(
						"session keepalive uninstall: could not boot out the LaunchAgent (it may still be loaded): %w", err)
				case launchd.LoadStateInconclusive:
					return fmt.Errorf(
						"session keepalive uninstall: could not boot out the LaunchAgent and could not determine "+
							"whether it is still loaded (launchd error: %w)", err)
				}
			}

			if err := os.Remove(launchd.PlistPath()); err != nil && !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("session keepalive uninstall: removing %s: %w", launchd.PlistPath(), err)
			}

			cmdio.Success(cmd.OutOrStdout(), "removed keepalive LaunchAgent")

			return nil
		},
	}

	return cmd
}

// statusReport is what `session keepalive status` renders, in text (statusTextCodec) or via the
// io.Options builtin json/yaml codecs.
type statusReport struct {
	// Installed reports whether the plist file exists on disk.
	Installed bool `json:"installed" yaml:"installed"`
	// Loaded reports whether launchctl currently has the agent loaded.
	Loaded bool `json:"loaded" yaml:"loaded"`
	// IntervalSeconds is the installed agent's StartInterval, read back from the plist. Zero when
	// not installed.
	IntervalSeconds int `json:"intervalSeconds,omitempty" yaml:"intervalSeconds,omitempty"`
	// Binary is the absolute path to the grafanapi binary the installed agent runs, read back from
	// the plist. Empty when not installed.
	Binary string `json:"binary,omitempty" yaml:"binary,omitempty"`
	// PlistPath is where the agent's plist lives (whether or not it is currently installed).
	PlistPath string `json:"plistPath" yaml:"plistPath"`
	// LogPath is where the agent's combined stdout/stderr is logged.
	LogPath string `json:"logPath" yaml:"logPath"`
	// LogTail is the last few lines of LogPath, verbatim (the log never contains a session cookie -
	// see LogPath's GoDoc). Empty if the log does not exist yet or could not be read.
	LogTail []string `json:"logTail,omitempty" yaml:"logTail,omitempty"`
}

// gatherStatus builds a statusReport from the current on-disk plist (os.Stat + launchd.Inspect),
// the real controller's Print, and a best-effort tail of the log file. Every inspection is
// best-effort: an error degrades the affected field to its zero value rather than failing the
// command - "session keepalive status" always succeeds.
func gatherStatus() statusReport {
	report := statusReport{
		PlistPath: launchd.PlistPath(),
		LogPath:   launchd.LogPath(),
	}

	if _, err := os.Stat(launchd.PlistPath()); err == nil {
		report.Installed = true
	}

	if report.Installed {
		if spec, err := launchd.Inspect(launchd.PlistPath()); err == nil {
			report.IntervalSeconds = spec.IntervalSeconds
			report.Binary = spec.BinaryPath
		}
	}

	if _, err := controller.Print(launchd.UserServiceTarget()); err == nil {
		report.Loaded = true
	}

	report.LogTail = tailLog(launchd.LogPath(), logTailLines)

	return report
}

// tailLog returns up to the last n lines of the file at path, or nil if it does not exist or
// cannot be read - a missing log (e.g. the agent has never run yet) is not an error.
func tailLog(path string, n int) []string {
	file, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer file.Close()

	var lines []string

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
		if len(lines) > n {
			lines = lines[1:]
		}
	}

	return lines
}

// statusTextCodec renders a statusReport as human-readable text - the default format for `session
// keepalive status`. -o json/-o yaml use the builtin codecs instead, serializing statusReport
// directly via its json/yaml tags.
type statusTextCodec struct{}

func (statusTextCodec) Format() format.Format { return "text" }

func (statusTextCodec) Encode(dst io.Writer, value any) error {
	report, ok := value.(statusReport)
	if !ok {
		return fmt.Errorf("expected statusReport, got %T", value)
	}

	if !report.Installed {
		fmt.Fprintf(dst, "installed: no\nplist:     %s (not present)\n", report.PlistPath)

		if report.Loaded {
			// Parity with the json/yaml codecs, which always report Loaded: the plist is gone (or
			// was never written by this tool) but launchctl still has the agent loaded - e.g. it
			// was bootstrapped from elsewhere, or the plist was deleted by hand after install.
			fmt.Fprintf(dst, "warning:   agent still loaded in launchd (%s) despite the plist being missing; "+
				"run: launchctl bootout %s\n", launchd.UserServiceTarget(), launchd.UserServiceTarget())
		}

		return nil
	}

	loaded := "no"
	if report.Loaded {
		loaded = "yes"
	}

	fmt.Fprintf(dst, "installed: yes\nloaded:    %s\ninterval:  %s\nbinary:    %s\nplist:     %s\nlog:       %s\n",
		loaded, time.Duration(report.IntervalSeconds)*time.Second, report.Binary, report.PlistPath, report.LogPath)

	if len(report.LogTail) > 0 {
		fmt.Fprintln(dst, "\nrecent log lines:")

		for _, line := range report.LogTail {
			fmt.Fprintln(dst, "  "+line)
		}
	}

	return nil
}

func (statusTextCodec) Decode(io.Reader, any) error {
	return errors.New("text codec does not support decoding")
}

// minLiveWindowAcrossContexts returns the smallest parsed, valid live-window across every context
// in cfg, and whether at least one was found. A context with no Grafana config or an unset
// live-window is silently skipped (not opted in); a set-but-invalid live-window is skipped with a
// recorded warning (mirroring dueContexts's "never fail the whole run for one bad context"
// philosophy) rather than aborting the install. The iterate/skip/parse-live-window/warn skeleton
// is shared with dueContexts via forEachLiveWindowContext; this function's own logic is just
// tracking the running minimum below.
func minLiveWindowAcrossContexts(cfg *config.Config) (time.Duration, bool, []string) {
	var (
		minWindow time.Duration
		found     bool
		warnings  []string
	)

	forEachLiveWindowContext(cfg,
		func(name string, err error) {
			warnings = append(warnings, fmt.Sprintf("context %q: %s (ignoring for interval derivation)", name, err))
		},
		func(_ string, _ *config.Context, window time.Duration) {
			if !found || window < minWindow {
				minWindow = window
				found = true
			}
		})

	return minWindow, found, warnings
}

// deriveInterval derives the LaunchAgent's StartInterval from the minimum opted-in live-window:
// minWindow/2, clamped to [derivedIntervalMin, derivedIntervalMax]. See keepaliveInstallCmd's Long
// help for the rationale.
func deriveInterval(minWindow time.Duration) time.Duration {
	interval := minWindow / 2

	switch {
	case interval < derivedIntervalMin:
		return derivedIntervalMin
	case interval > derivedIntervalMax:
		return derivedIntervalMax
	default:
		return interval
	}
}

// writePlist creates the LaunchAgents/Logs directories (if needed) and writes spec as the
// keep-alive agent's plist at launchd.PlistPath().
func writePlist(spec launchd.AgentSpec) error {
	if err := os.MkdirAll(launchd.LaunchAgentsDir(), launchAgentsDirPerm); err != nil {
		return fmt.Errorf("creating %s: %w", launchd.LaunchAgentsDir(), err)
	}

	if err := os.MkdirAll(launchd.LogDir(), launchAgentsDirPerm); err != nil {
		return fmt.Errorf("creating %s: %w", launchd.LogDir(), err)
	}

	plistFile, err := os.Create(launchd.PlistPath())
	if err != nil {
		return fmt.Errorf("writing %s: %w", launchd.PlistPath(), err)
	}

	genErr := launchd.Generate(plistFile, spec)
	closeErr := plistFile.Close()

	if genErr != nil {
		return fmt.Errorf("generating plist: %w", genErr)
	}

	if closeErr != nil {
		return fmt.Errorf("closing %s: %w", launchd.PlistPath(), closeErr)
	}

	return nil
}
