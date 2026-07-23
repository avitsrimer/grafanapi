package session

import (
	"errors"
	"fmt"
	"net/http"
	"sort"
	"time"

	"github.com/go-openapi/runtime"
	"github.com/grafana/grafanapi/cmd/grafanapi/io"
	"github.com/grafana/grafanapi/internal/config"
	"github.com/grafana/grafanapi/internal/keychain"
	"github.com/spf13/cobra"
)

// refreshOpAuth is the "op" argument used to build the runtime.APIError that carries a rejected
// rotation to fail's centralized stale-session rendering (see runSingle/aggregateOutcomes and
// cmd/grafanapi/fail/convert.go's convertSessionErrors, which recognizes any *runtime.APIError
// with Code == http.StatusUnauthorized regardless of op).
const refreshOpAuth = "session refresh"

// refreshCmd returns the `session refresh` command: it forces a Grafana session rotation now,
// reusing the exact rotation + Keychain-persist path already shipped for the automatic
// 401-triggered rotation (internal/config.SessionSource.Refresh). By default it targets a single
// context (--context, or the current context); --all targets every context unconditionally;
// --due (the scheduler entry point used by `session keepalive`) targets only the contexts whose
// `live-window` has elapsed since their last rotation.
func refreshCmd(opts *Options) *cobra.Command {
	var all bool
	var due bool

	cmd := &cobra.Command{
		Use:   "refresh",
		Args:  cobra.NoArgs,
		Short: "Force a Grafana session rotation now",
		Long: `Force a Grafana session rotation now, reusing the same rotation and Keychain-persist
path as the automatic 401-triggered rotation.

By default this targets a single context (--context, or the current context). --all targets
every context that has a stored session cookie, unconditionally. --due targets only the contexts
whose "live-window" configuration field has elapsed since their last rotation - this is the
scheduler entry point used by "session keepalive". --all and --due are mutually exclusive with
each other and with --context.

Contexts with no stored session cookie (never logged in) are skipped silently under --all/--due,
and reported as a clear error when targeted explicitly.`,
		Example: "\n\tgrafanapi session refresh\n\tgrafanapi session refresh --context staging\n" +
			"\tgrafanapi session refresh --all\n\tgrafanapi session refresh --due",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if all && due {
				return errors.New("session refresh: --all and --due cannot be used together")
			}
			if (all || due) && opts.Context != "" {
				return errors.New("session refresh: --context cannot be combined with --all or --due")
			}

			cfg, err := config.Load(cmd.Context(), opts.configSource())
			if err != nil {
				return err
			}

			switch {
			case due:
				return runDue(cmd, &cfg)
			case all:
				return runAll(cmd, &cfg)
			default:
				return runSingle(cmd, &cfg, opts.Context)
			}
		},
	}

	cmd.Flags().BoolVar(&all, "all", false, "Refresh every context that has a stored session cookie, unconditionally")
	cmd.Flags().BoolVar(&due, "due", false, "Refresh only contexts whose live-window has elapsed since their last rotation (the scheduler entry point)")

	return cmd
}

// refreshStatus classifies the outcome of refreshContext for a single context.
type refreshStatus int

const (
	// refreshSucceeded means the rotation completed and was persisted.
	refreshSucceeded refreshStatus = iota
	// refreshSkippedNoCookie means the context has no stored session cookie (never logged in, or
	// the Keychain item is gone): there is nothing to rotate.
	refreshSkippedNoCookie
	// refreshAuthRejected means the rotate endpoint itself rejected the stored cookie
	// (config.ErrRotateUnauthorized): the session is truly dead, not merely stale.
	refreshAuthRejected
	// refreshErrored means some other error prevented the rotation (Keychain read failure,
	// network error building/persisting the rotation, etc).
	refreshErrored
)

// refreshOutcome is the result of attempting to refresh one context.
type refreshOutcome struct {
	name   string
	status refreshStatus
	err    error
}

// refreshContext resolves gCtx's session cookie from the Keychain and, if one is stored, forces a
// rotation via SessionSource.Refresh - the same single-flight rotate + Keychain-persist path used
// by the automatic 401-triggered rotation. It never prints anything and never returns the cookie
// itself; callers render outcome.err (never the cookie) and classify outcome.status into an exit
// code.
func refreshContext(cmd *cobra.Command, gCtx *config.Context) refreshOutcome {
	outcome := refreshOutcome{name: gCtx.Name}

	if err := config.ResolveContextSessionCookie(keychainStore, gCtx); err != nil {
		outcome.status = refreshErrored
		outcome.err = fmt.Errorf("resolving session cookie: %w", err)

		return outcome
	}

	if gCtx.Grafana == nil || gCtx.Grafana.Session == nil {
		outcome.status = refreshSkippedNoCookie

		return outcome
	}

	if _, err := gCtx.Grafana.Session.Refresh(cmd.Context()); err != nil {
		if errors.Is(err, config.ErrRotateUnauthorized) {
			outcome.status = refreshAuthRejected
			outcome.err = err

			return outcome
		}

		outcome.status = refreshErrored
		outcome.err = err

		return outcome
	}

	outcome.status = refreshSucceeded

	return outcome
}

// runSingle refreshes exactly one context: contextName if set, otherwise cfg.CurrentContext.
func runSingle(cmd *cobra.Command, cfg *config.Config, contextName string) error {
	name := contextName
	if name == "" {
		name = cfg.CurrentContext
	}

	if name == "" {
		return errors.New("session refresh: no context specified and no current context is set (pass --context)")
	}

	gCtx := cfg.Contexts[name]
	if gCtx == nil {
		return config.ContextNotFound(name)
	}

	outcome := refreshContext(cmd, gCtx)

	switch outcome.status {
	case refreshSucceeded:
		io.Success(cmd.OutOrStdout(), "refreshed session for context %s", outcome.name)

		return nil
	case refreshSkippedNoCookie:
		return fmt.Errorf("session refresh: context %q has no stored session cookie (run: grafanapi login --context %s)", name, name)
	case refreshAuthRejected:
		return runtime.NewAPIError(refreshOpAuth, nil, http.StatusUnauthorized)
	case refreshErrored:
		fallthrough
	default:
		return fmt.Errorf("session refresh: context %q: %w", name, outcome.err)
	}
}

// runAll refreshes every context in cfg, sorted by name, unconditionally. Every cookie-bearing
// context is attempted even if an earlier one fails; contexts with no stored cookie are skipped
// silently. The aggregate result is exit-2 (via a *runtime.APIError) if any context's rotation was
// auth-rejected, exit-1 (a joined error) if any other error occurred with no auth rejection, and
// nil (exit 0) otherwise.
func runAll(cmd *cobra.Command, cfg *config.Config) error {
	names := sortedContextNames(cfg)

	contexts := make([]*config.Context, 0, len(names))
	for _, name := range names {
		contexts = append(contexts, cfg.Contexts[name])
	}

	return refreshAll(cmd, contexts)
}

// runDue refreshes only the contexts dueContexts selects: those whose live-window has elapsed
// since their last rotation, per keychainStore.ModifiedAt. Warnings (e.g. an invalid live-window)
// are printed but never fail the run - a bad context is skipped, not fatal. A run with nothing due
// is a success.
func runDue(cmd *cobra.Command, cfg *config.Config) error {
	due, warnings := dueContexts(cfg, time.Now(), keychainStore.ModifiedAt)

	for _, warning := range warnings {
		io.Warning(cmd.OutOrStdout(), "%s", warning)
	}

	if len(due) == 0 {
		io.Success(cmd.OutOrStdout(), "nothing due")

		return nil
	}

	return refreshAll(cmd, due)
}

// refreshAll attempts refreshContext for every context in contexts (assumed already
// deterministically ordered), rendering a ✔/✘ line per context, and aggregates the outcome: exit-2
// (a *runtime.APIError) if any rotation was auth-rejected, exit-1 (a joined error) if any other
// failure occurred with no auth rejection, nil otherwise. Skipped (no-cookie) contexts never count
// as a failure.
func refreshAll(cmd *cobra.Command, contexts []*config.Context) error {
	var authRejected bool
	var errs []error

	for _, gCtx := range contexts {
		outcome := refreshContext(cmd, gCtx)

		switch outcome.status {
		case refreshSucceeded:
			io.Success(cmd.OutOrStdout(), "refreshed session for context %s", outcome.name)
		case refreshSkippedNoCookie:
			// Silent: --all/--due never fail a run because some context was never logged in.
		case refreshAuthRejected:
			authRejected = true
			io.Error(cmd.OutOrStdout(), "context %s: %s", outcome.name, outcome.err)
		case refreshErrored:
			errs = append(errs, fmt.Errorf("context %s: %w", outcome.name, outcome.err))
			io.Error(cmd.OutOrStdout(), "context %s: %s", outcome.name, outcome.err)
		}
	}

	switch {
	case authRejected:
		return runtime.NewAPIError(refreshOpAuth, nil, http.StatusUnauthorized)
	case len(errs) > 0:
		return errors.Join(errs...)
	default:
		return nil
	}
}

// dueContexts is the pure due-selection function backing `session refresh --due`: for each
// context in cfg (visited in a deterministic, name-sorted order), it skips contexts with no
// Grafana config or an unset live-window, records a warning and skips a set-but-invalid
// live-window (never a hard error - one bad context must not fail the scheduled run for every
// other context), skips a context with no Keychain item (modAt returns any error, most commonly
// keychain.ErrNotFound - nothing to rotate yet), and selects a context iff its last rotation is at
// least its live-window old as of now. now and modAt are injected so tests can use a fake clock and
// fake modification times instead of touching the real Keychain or wall-clock time.
//
// Returns (due, warnings): due is the selected contexts, warnings is the set of "invalid
// live-window" messages recorded along the way (plain returns, not named - the loop body has no
// need for recover()-style named-return rewriting, unlike SessionSource.runRotate).
func dueContexts(cfg *config.Config, now time.Time, modAt func(account string) (time.Time, error)) ([]*config.Context, []string) {
	var due []*config.Context
	var warnings []string

	for _, name := range sortedContextNames(cfg) {
		gCtx := cfg.Contexts[name]
		if gCtx == nil || gCtx.Grafana == nil {
			continue
		}

		window, ok, err := gCtx.Grafana.ParsedLiveWindow()
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("context %q: %s (skipping)", name, err))

			continue
		}

		if !ok {
			// Not opted into keep-alive.
			continue
		}

		lastRotation, err := modAt(keychain.Account(name))
		if err != nil {
			// No stored cookie (keychain.ErrNotFound) - or any other lookup failure - means there
			// is nothing to rotate yet; skip silently rather than failing the scheduled run.
			continue
		}

		if now.Sub(lastRotation) >= window {
			due = append(due, gCtx)
		}
	}

	return due, warnings
}

// sortedContextNames returns cfg's context names in sorted order, so --all/--due iterate (and
// print) contexts deterministically instead of in Go's randomized map order.
func sortedContextNames(cfg *config.Config) []string {
	names := make([]string, 0, len(cfg.Contexts))
	for name := range cfg.Contexts {
		names = append(names, name)
	}

	sort.Strings(names)

	return names
}
