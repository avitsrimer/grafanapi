// Package skill embeds the grafanapi Claude skill tree so the installed binary can
// write it from any directory, independent of the source checkout.
package skill

import "embed"

//go:embed grafanapi
var Files embed.FS
