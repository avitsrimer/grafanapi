package alerts

import (
	"errors"

	cmdconfig "github.com/grafana/grafanapi/cmd/grafanapi/config"
	cmdio "github.com/grafana/grafanapi/cmd/grafanapi/io"
	"github.com/grafana/grafanapi/internal/alerts"
	"github.com/grafana/grafanapi/internal/format"
	"github.com/grafana/grafanapi/internal/grafana"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

const (
	// defaultAlertingPath is where a bundle is written when --path is not given. It mirrors
	// `resources pull`'s ./resources, one directory per kind of thing grafanapi manages as code.
	defaultAlertingPath = "./alerting"
	// defaultBundleFormat is the file format a bundle is written in unless -o says otherwise. YAML
	// rather than the repo-wide json default: a bundle is meant to be read, reviewed and edited by
	// hand before it is pushed back.
	defaultBundleFormat = format.YAML
)

// pullOptions holds the flags accepted by `alerts pull`.
//
// Unlike the read commands in this group, --output is a *file* format rather than a rendering:
// the table codec is deliberately not registered, so -o accepts exactly yaml and json - the two
// formats ReadBundle can read back.
type pullOptions struct {
	IO cmdio.Options

	Path          string
	FolderUIDs    []string
	MaxConcurrent int
}

func (opts *pullOptions) BindFlags(flags *pflag.FlagSet) {
	opts.IO.DefaultFormat(string(defaultBundleFormat))
	opts.IO.BindFlags(flags)

	flags.StringVarP(&opts.Path, "path", "p", defaultAlertingPath,
		"Path on disk in which the alerting bundle will be written")
	flags.StringArrayVar(&opts.FolderUIDs, "folder-uid", nil,
		"Only pull rule groups from this folder, matched against its UID (repeatable). "+
			"A UID no folder carries is an error; contact points and the notification policy tree are "+
			"org-wide and are pulled whatever this is set to")
	flags.IntVar(&opts.MaxConcurrent, "max-concurrent", alerts.DefaultMaxConcurrent,
		"Maximum number of concurrent operations")
}

// Validate checks everything that can be checked before a config load or a network call: the
// output format, the destination path and the concurrency bound. Same order and same messages as
// the `resources` commands, so an invalid flag costs nothing.
func (opts *pullOptions) Validate() error {
	if err := opts.IO.Validate(); err != nil {
		return err
	}

	if opts.Path == "" {
		return errors.New("--path is required")
	}

	if opts.MaxConcurrent <= 0 {
		return errors.New("max-concurrent must be greater than zero")
	}

	return nil
}

// pullOptions returns the internal/alerts selection this command asks Pull for. FolderUIDs are
// passed through as given: internal/alerts verifies each one exists before listing anything, so a
// typo is an error rather than a silently empty bundle.
func (opts *pullOptions) pullOptions() alerts.PullOptions {
	return alerts.PullOptions{
		FolderUIDs:    opts.FolderUIDs,
		MaxConcurrent: opts.MaxConcurrent,
	}
}

// writeOptions returns the internal/alerts options this command asks WriteBundle for, in the
// format the codec the user selected writes.
//
// KeepFolders names the folders whose stale rule-group files may be pruned: the selected folders
// for a filtered pull, nil (meaning "every folder") for a whole-instance one. A partial pull must
// not delete the files of the folders it never fetched.
func (opts *pullOptions) writeOptions(f format.Format) alerts.WriteOptions {
	keep := opts.FolderUIDs
	if len(keep) == 0 {
		keep = nil
	}

	return alerts.WriteOptions{Format: f, KeepFolders: keep}
}

// pullCmd returns the `alerts pull` command.
func pullCmd(configOpts *cmdconfig.Options) *cobra.Command {
	opts := &pullOptions{}

	cmd := &cobra.Command{
		Use:   "pull",
		Args:  cobra.NoArgs,
		Short: "Pull alerting configuration from Grafana to disk",
		Long: `Pull the alerting configuration of the current context into a directory on disk.

A bundle holds the rule groups, the contact points and the notification policy tree, laid out as:

	<path>/rules/<folder UID>/<group title>.<yaml|json>
	<path>/contact-points.<yaml|json>
	<path>/policies.<yaml|json>

Path components are sanitized and are NOT authoritative: the folderUid and title recorded inside
each group file are what a later push sends to Grafana. Do not hand-edit orgID - push overwrites
it from the remote group or from the context.

Files under rules/ that this pull did not produce are removed, so a group deleted in Grafana is
not resurrected by the next push. With --folder-uid that pruning is restricted to the named
folders, leaving a partial bundle's other folders untouched.

Secure contact-point settings arrive as "[REDACTED]" and must be left that way: pushing the
placeholder back keeps the stored secret, while replacing it sets a new one.

Reading needs the Viewer role. Pushing the result back needs Editor.`,
		Example: `
	grafanapi alerts pull

	grafanapi alerts pull -p ./alerting -o json

	grafanapi alerts pull --folder-uid folder-uid-1 --folder-uid folder-uid-2`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runPull(cmd, configOpts, opts)
		},
	}

	opts.BindFlags(cmd.Flags())

	return cmd
}

// runPull is the `alerts pull` command's RunE body: validate the flags, load the current context,
// fetch the bundle and write it out. Same order as the group's read commands, so a validation
// error never costs a config load or a network call.
//
// Errors are returned as they come: a dead session is still the unwrapped *runtime.APIError the
// exit-2 rendering needs, and a 403 the exit-1 permission one.
func runPull(cmd *cobra.Command, configOpts *cmdconfig.Options, opts *pullOptions) error {
	if err := opts.Validate(); err != nil {
		return err
	}

	ctx := cmd.Context()

	cfg, err := configOpts.LoadConfig(ctx)
	if err != nil {
		return err
	}

	gClient, err := grafana.ClientFromContext(cfg.GetCurrentContext())
	if err != nil {
		return err
	}

	codec, err := opts.IO.Codec()
	if err != nil {
		return err
	}

	bundle, err := alerts.Pull(ctx, gClient, opts.pullOptions())
	if err != nil {
		return err
	}

	if err := alerts.WriteBundle(opts.Path, bundle, opts.writeOptions(codec.Format())); err != nil {
		return err
	}

	// The tree is reported as a count so the summary reads uniformly across the three kinds. A
	// pull that fetched no tree wrote no file for it, and says 0 rather than claiming one exists.
	trees := 0
	if bundle.Policies != nil {
		trees = 1
	}

	cmdio.Success(cmd.OutOrStdout(), "%d rule group(s), %d contact point(s) and %d policy tree(s) written to %s",
		len(bundle.Groups), len(bundle.ContactPoints), trees, opts.Path)

	return nil
}
