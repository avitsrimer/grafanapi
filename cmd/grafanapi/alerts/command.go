// Package alerts implements the `grafanapi alerts` command group: Grafana Unified Alerting, read
// and written from the terminal.
//
// The read commands report alert rule definitions joined with their live evaluation state
// (`alerts rules list`) and notification contact points (`alerts contact-points list`). The write
// commands are `alerts pull` / `alerts push`, which move the whole alerting configuration between
// Grafana and a directory of files, and `alerts rules pause|resume|delete`, which change
// individual rules in place.
//
// Reading needs the Viewer role and writing needs Editor. Every write goes through the same
// provisioning API the read side uses; no command in this group performs an org-admin operation.
//
// This package is a thin Cobra wiring layer over internal/alerts, mirroring
// cmd/grafanapi/datasources and cmd/grafanapi/explore: flag binding, fast pre-network flag
// validation, the "table" format.Codec adapters and the plan renderer live here, while every API
// call, join, filter, sort, diff and write lives in internal/alerts and is unit-testable without
// Cobra.
package alerts

import (
	cmdconfig "github.com/grafana/grafanapi/cmd/grafanapi/config"
	"github.com/spf13/cobra"
)

// Command returns the `alerts` command group.
//
// The group itself (and each of its intermediate parents) has no RunE, so running it bare prints
// help and exits 0 - the same shape as `resources` and `session`. --config/--context are bound
// as persistent flags here so every subcommand inherits them.
func Command() *cobra.Command {
	configOpts := &cmdconfig.Options{}

	cmd := &cobra.Command{
		Use:   "alerts",
		Short: "Inspect and manage Grafana alert rules, contact points and notification policies",
		Long: `Inspect and manage Grafana Unified Alerting.

Alert rules are listed with both their definition and their live evaluation state; contact points
are listed as the server reports them, with their declared secure fields already redacted
server-side (a credential embedded in a URL or a custom header is not).

The configuration is also managed as code: ` + "`alerts pull`" + ` writes it to a directory of files and
` + "`alerts push`" + ` sends it back. Individual rules are changed in place with
` + "`alerts rules pause`" + `, ` + "`resume`" + ` and ` + "`delete`" + `, and edited one at a time with
` + "`alerts rules get`" + `, ` + "`alerts pull --rule-uid`" + ` and ` + "`alerts push --rule-uid`" + `.

A rule group is the unit of every rule write: pushing or pausing rewrites the whole group, so a
rule missing from a pushed group file is deleted from it. Objects another provisioner owns (file
provisioning, imported Prometheus rules, ...) are refused rather than overwritten, and Grafana
offers no optimistic concurrency here - the last writer wins. Every write command takes --dry-run;
run it first.

Reading needs the Viewer role and writing needs Editor. No command in this group performs an
org-admin operation.`,
	}

	configOpts.BindFlags(cmd.PersistentFlags())

	cmd.AddCommand(contactPointsCmd(configOpts))
	cmd.AddCommand(pullCmd(configOpts))
	cmd.AddCommand(pushCmd(configOpts))
	cmd.AddCommand(rulesCmd(configOpts))

	return cmd
}
