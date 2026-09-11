package alerts

import (
	"errors"
	"fmt"
	"io"

	cmdconfig "github.com/grafana/grafanapi/cmd/grafanapi/config"
	cmdio "github.com/grafana/grafanapi/cmd/grafanapi/io"
	"github.com/grafana/grafanapi/cmd/grafanapi/onerror"
	"github.com/grafana/grafanapi/internal/alerts"
	"github.com/grafana/grafanapi/internal/grafana"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// pushOptions holds the flags accepted by `alerts push`.
//
// There is no --output: like `resources push` this command reports what it did rather than
// rendering an object, so the only thing it prints is the plan (--dry-run) or one line per change.
type pushOptions struct {
	Path          string
	DryRun        bool
	UIEditable    bool
	OnError       onerror.Mode
	MaxConcurrent int
}

func (opts *pushOptions) BindFlags(flags *pflag.FlagSet) {
	// Single path, unlike `resources push`'s repeatable -p: a bundle is one directory tree with a
	// layout ReadBundle walks, not a set of files that can be collected from anywhere.
	flags.StringVarP(&opts.Path, "path", "p", defaultAlertingPath,
		"Path on disk from which the alerting bundle will be read")
	flags.BoolVar(&opts.DryRun, "dry-run", opts.DryRun,
		"If set, the push is planned and printed, without creating or updating anything in Grafana")
	flags.BoolVar(&opts.UIEditable, "ui-editable", opts.UIEditable,
		"If set, objects this push CREATES are left editable in the Grafana UI. It has no effect on an "+
			"update, which always mirrors the provenance the object already carries")
	onerror.BindFlag(flags, &opts.OnError)
	flags.IntVar(&opts.MaxConcurrent, "max-concurrent", alerts.DefaultMaxConcurrent,
		"Maximum number of concurrent operations")
}

// Validate checks everything that can be checked before a config load or a network call: the
// source path, the concurrency bound and the --on-error vocabulary. Same order and same messages
// as `alerts pull` and the `resources` commands, so an invalid flag costs nothing.
func (opts *pushOptions) Validate() error {
	if opts.Path == "" {
		return errors.New("--path is required")
	}

	if opts.MaxConcurrent <= 0 {
		return errors.New("max-concurrent must be greater than zero")
	}

	return opts.OnError.Validate()
}

// pushOptions returns the internal/alerts options this command asks Push for. --on-error is
// translated here rather than passed through: internal/alerts knows only "stop at the first
// failure", and the exit code the mode implies is applied to the result by reportPush.
func (opts *pushOptions) pushOptions(orgID int64) alerts.PushOptions {
	return alerts.PushOptions{
		DryRun:        opts.DryRun,
		StopOnError:   opts.OnError.StopOnError(),
		UIEditable:    opts.UIEditable,
		MaxConcurrent: opts.MaxConcurrent,
		OrgID:         orgID,
	}
}

// pushCmd returns the `alerts push` command.
func pushCmd(configOpts *cmdconfig.Options) *cobra.Command {
	opts := &pushOptions{}

	cmd := &cobra.Command{
		Use:   "push",
		Args:  cobra.NoArgs,
		Short: "Push an alerting configuration from disk to Grafana",
		Long: `Push the alerting bundle in a directory on disk to the current context.

The bundle is the layout ` + "`alerts pull`" + ` writes: rule groups under rules/<folder UID>/, the
contact points and the notification policy tree beside them. The folderUid and title recorded
inside each file - not its path - are what is sent to Grafana.

A rule group is written whole: Grafana replaces the group with what the file holds, so a rule
missing from the file is DELETED from that group. Run --dry-run first; it prints exactly that as a
"-" line. Groups, contact points and policies that exist only in Grafana are left alone: this
command never prunes.

Folders are never created. A group naming a folder that does not exist fails; push the folders
first with ` + "`grafanapi resources push`" + `.

Objects another provisioner owns (file provisioning, imported Prometheus rules, ...) are refused
rather than overwritten. New objects are claimed for the API unless --ui-editable says otherwise.

Secure contact-point settings must be left as the "[REDACTED]" placeholder a pull wrote: pushing
it back keeps the stored secret, while replacing it sets a new one. A contact point being CREATED
is the exception - there is no stored secret to keep - and is refused until the file carries the
real value, which is what pushing a bundle into a different instance runs into.

Grafana has no optimistic concurrency here: the last writer wins. Pushing needs the Editor role.`,
		Example: `
	grafanapi alerts push --dry-run

	grafanapi alerts push

	grafanapi alerts push -p ./alerting --on-error abort`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runPush(cmd, configOpts, opts)
		},
	}

	opts.BindFlags(cmd.Flags())

	return cmd
}

// reportPush prints one line per planned change and the summary that closes them, then turns the
// failures into the exit code --on-error asks for.
//
// Applied entries (including the Unchanged ones, which needed no request) come first and the
// failures last, each half already in plan order, so the thing the user has to act on is the last
// thing on screen rather than buried between successes.
//
// The returned error wraps the first failure instead of only counting them: a push that failed
// because Grafana refused the write (403) must still render as the permission-denied message,
// which cmd/grafanapi/fail derives from the underlying *runtime.APIError. Counting alone would
// flatten every cause into one opaque "N changes failed".
//
// Each line is assembled first and scrubbed whole (see cell), rather than having its ID scrubbed
// as an argument: an error text quotes the very same untrusted folder UID, group title or contact
// point name back, so scrubbing only the ID would still let a newline in one of them forge a "✔"
// line in output a human reads before approving a destructive push.
func reportPush(out io.Writer, result alerts.Result, mode onerror.Mode) error {
	for _, change := range result.Applied {
		cmdio.Success(out, "%s", cell(fmt.Sprintf("%s %s %s", change.Action, change.Kind, change.ID)))
	}

	for _, failed := range result.Failed {
		cmdio.Error(out, "%s", cell(fmt.Sprintf("%s %s %s: %v",
			failed.Change.Action, failed.Change.Kind, failed.Change.ID, failed.Err)))
	}

	printer := summaryPrinter(len(result.Applied), len(result.Failed))
	printer(out, "%d change(s) applied, %d error(s)", len(result.Applied), len(result.Failed))

	if mode.FailOnErrors() && len(result.Failed) != 0 {
		return fmt.Errorf("%d change(s) failed to push: %w", len(result.Failed), result.Failed[0].Err)
	}

	return nil
}

// reportPushResult renders one push's outcome: under --dry-run the plan plus the entries an apply
// would refuse, and otherwise one line per change plus the summary.
//
// A dry run exits 0 even when it found refusals - it changed nothing, and showing them before the
// apply hits them is what it is for.
func reportPushResult(out io.Writer, result alerts.Result, opts *pushOptions) error {
	if opts.DryRun {
		if err := renderPlan(out, result.Applied); err != nil {
			return err
		}

		reportRefusals(out, result.Failed)

		return nil
	}

	return reportPush(out, result, opts.OnError)
}

// reportRefusals prints the plan entries an apply would refuse before writing them: a folder that
// is missing or that this session may not read, an object another provisioner owns, a contact
// point being created whose secret is still the pull's "[REDACTED]" marker. They follow the plan
// table rather than appearing in it, since they are not work the push would do.
//
// Each line is scrubbed whole for the reason reportPush's are: these are the lines a user reads to
// decide whether to run the apply.
func reportRefusals(out io.Writer, failed []alerts.FailedChange) {
	for _, refusal := range failed {
		cmdio.Error(out, "%s", cell(fmt.Sprintf("would fail: %s %s %s: %v",
			refusal.Change.Action, refusal.Change.Kind, refusal.Change.ID, refusal.Err)))
	}
}

// runPush is the `alerts push` command's RunE body: validate the flags, load the current context,
// read the bundle from disk and apply it. Same order as the rest of the group, so a validation
// error never costs a config load or a network call, and an unreadable bundle never costs a write.
//
// What the push did is reported even when it was cut short - under --on-error abort, or by a 401
// found halfway through - so the contact points and rule groups already written are visible rather
// than swallowed by the error. The push's own error then wins, so a dead session is still the
// unwrapped *runtime.APIError the exit-2 rendering needs, and a 403 the exit-1 permission one.
//
// A push that failed *before* it applied anything - a 401 or a 500 during the fetch Plan needs, an
// ambiguous contact-point name - has nothing to report, and its zero Result would otherwise print
// "0 change(s) applied, 0 error(s)" (or a bare plan header under --dry-run) above the error that
// actually happened. That is why the report is conditional rather than unconditional.
func runPush(cmd *cobra.Command, configOpts *cmdconfig.Options, opts *pushOptions) error {
	if err := opts.Validate(); err != nil {
		return err
	}

	ctx := cmd.Context()

	cfg, err := configOpts.LoadConfig(ctx)
	if err != nil {
		return err
	}

	gCtx := cfg.GetCurrentContext()

	gClient, err := grafana.ClientFromContext(gCtx)
	if err != nil {
		return err
	}

	bundle, err := alerts.ReadBundle(opts.Path)
	if err != nil {
		return err
	}

	result, pushErr := alerts.Push(ctx, gClient, bundle, opts.pushOptions(gCtx.Grafana.OrgID))

	var reportErr error

	if pushErr == nil || len(result.Applied)+len(result.Failed) != 0 {
		reportErr = reportPushResult(cmd.OutOrStdout(), result, opts)
	}

	if pushErr != nil {
		return pushErr
	}

	return reportErr
}
