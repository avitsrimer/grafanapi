// Package installskill implements the `grafanapi install-skill` command: it writes the bundled
// grafanapi Claude Code skill (embedded at build time via the skill package) into a .claude
// folder, so agents pick it up without needing network access or a source checkout.
//
// This package is a thin Cobra wiring layer: the embedded filesystem lives in skill, and there is
// no config/auth dependency (install-skill never touches a Grafana context).
package installskill

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	cmdio "github.com/grafana/grafanapi/cmd/grafanapi/io"
	"github.com/grafana/grafanapi/skill"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// skillName is both the directory embedded in skill.Files and the name of the directory it is
// installed under (<to>/skills/<skillName>).
const skillName = "grafanapi"

// dirPerm / filePerm are the permissions used when writing the installed skill tree: directories
// are traversable by the owner and group only, and files are readable/writable by the owner only
// — matching this project's config-file permission convention (see internal/config).
const (
	dirPerm  fs.FileMode = 0o750
	filePerm fs.FileMode = 0o600
)

// Command returns the `install-skill` command.
func Command() *cobra.Command {
	opts := &Options{}

	cmd := &cobra.Command{
		Use:   "install-skill",
		Args:  cobra.NoArgs,
		Short: "Install the bundled Claude Code skill",
		Long: `Install the bundled grafanapi Claude Code skill into a .claude folder.

The skill teaches Claude Code (or any agent reading the Claude Code skill format) how to use
grafanapi: authentication, discovering datasources, running ad-hoc queries, and managing
dashboards/folders as code. It is embedded in the grafanapi binary at build time, so this command
works offline and from any directory, independent of the source checkout.

Any existing installation at the destination is replaced.`,
		Example: "\n\tgrafanapi install-skill\n\tgrafanapi install-skill --to ~/projects/my-repo/.claude",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runInstallSkill(cmd, opts)
		},
	}

	opts.BindFlags(cmd.Flags())

	return cmd
}

// Options holds the flags accepted by `install-skill`.
type Options struct {
	To string
}

func (opts *Options) BindFlags(flags *pflag.FlagSet) {
	flags.StringVar(&opts.To, "to", "", "Path to a .claude folder (default ~/.claude)")
}

// runInstallSkill is the `install-skill` command's RunE body: resolve the destination
// (--to, or ~/.claude), replace whatever is currently at <destination>/skills/grafanapi, and
// write the embedded skill tree in its place.
func runInstallSkill(cmd *cobra.Command, opts *Options) error {
	claudeDir := opts.To
	if claudeDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return fmt.Errorf("install-skill: resolving home directory: %w", err)
		}

		claudeDir = filepath.Join(home, ".claude")
	}

	dest := filepath.Join(claudeDir, "skills", skillName)

	if err := os.RemoveAll(dest); err != nil {
		return fmt.Errorf("install-skill: removing existing %s: %w", dest, err)
	}

	if err := installFiles(dest); err != nil {
		return fmt.Errorf("install-skill: writing %s: %w", dest, err)
	}

	cmdio.Success(cmd.OutOrStdout(), "Installed grafanapi skill to %s", dest)

	return nil
}

// installFiles walks the embedded skill tree and writes it under dest, creating directories with
// dirPerm and files with filePerm.
func installFiles(dest string) error {
	return fs.WalkDir(skill.Files, skillName, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		rel, err := filepath.Rel(skillName, path)
		if err != nil {
			return err
		}

		target := filepath.Join(dest, rel)

		if d.IsDir() {
			return os.MkdirAll(target, dirPerm)
		}

		content, err := skill.Files.ReadFile(path)
		if err != nil {
			return err
		}

		return os.WriteFile(target, content, filePerm)
	})
}
