// Package alerts implements the `grafanapi alerts` command group: read-only access to Grafana
// Unified Alerting - alert rule definitions joined with their live evaluation state
// (`alerts rules list`) and notification contact points (`alerts contact-points list`).
//
// This package is a thin Cobra wiring layer over internal/alerts, mirroring
// cmd/grafanapi/datasources and cmd/grafanapi/explore: flag binding, fast pre-network flag
// validation, and the "table" format.Codec adapters live here, while every API call, join,
// filter and sort lives in internal/alerts and is unit-testable without Cobra.
//
// Every command in the group is read-only by construction: no write method of the generated
// client's provisioning service is wired up anywhere in it.
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
		Short: "Inspect Grafana alert rules and contact points",
		Long: `Inspect Grafana Unified Alerting.

Alert rules are listed with both their definition and their live evaluation state; contact points
are listed as the server reports them, with their declared secure fields already redacted
server-side (a credential embedded in a URL or a custom header is not).

Every command in this group is read-only.`,
	}

	configOpts.BindFlags(cmd.PersistentFlags())

	cmd.AddCommand(contactPointsCmd(configOpts))
	cmd.AddCommand(rulesCmd(configOpts))

	return cmd
}
