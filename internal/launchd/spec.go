// Package launchd manages the macOS LaunchAgent that runs "grafanapi session refresh --due" on a
// schedule, keeping opted-in contexts' Grafana sessions from going stale. It generates and reads
// back the plist (plist.go), resolves the absolute binary path to embed in it so the agent
// survives a Homebrew upgrade (path.go), and shells out to launchctl through a small, fully fake-
// able seam (controller.go). No plist library and no third-party launchd wrapper are used: the
// plist is produced with text/template and read back with encoding/xml, both stdlib. The package
// is exercised entirely with fakes in unit tests; no test invokes real launchctl/launchd.
package launchd

import (
	"fmt"
	"time"
)

// Label is the fixed launchd service label for the grafanapi keep-alive LaunchAgent. It doubles
// as the plist filename stem (paths.go) and the trailing component of every launchctl service
// target (controller.go). It must never change across releases: a different label would strand
// any previously installed agent as an orphaned, un-managed LaunchAgent that "session keepalive
// uninstall" could no longer find.
const Label = "io.github.avitsrimer.grafanapi.keepalive"

const (
	// minInterval is the smallest StartInterval this package will generate or validate. It mirrors
	// config.minLiveWindow; the two are kept as separate unexported constants (rather than a shared
	// exported one) to avoid a launchd -> config import edge, but must stay in sync at [1m, 6d].
	minInterval = time.Minute
	// maxInterval is the largest StartInterval this package will generate or validate. See
	// minInterval.
	maxInterval = 6 * 24 * time.Hour
)

// AgentSpec fully describes one LaunchAgent: what to run, how often, and where its output goes.
// It is the single value threaded through plist generation (Generate), inspection (Inspect), and
// the derivation logic in "session keepalive install".
type AgentSpec struct {
	// Label is the launchd service label (see the package-level Label constant).
	Label string
	// BinaryPath is the absolute path to the grafanapi binary the agent executes (see
	// ResolveBinaryPath).
	BinaryPath string
	// Args are the arguments passed to BinaryPath after the binary path itself.
	Args []string
	// IntervalSeconds is the launchd StartInterval, in seconds: how often launchd wakes the agent.
	IntervalSeconds int
	// StdoutPath is the file launchd redirects the agent's standard output to.
	StdoutPath string
	// StderrPath is the file launchd redirects the agent's standard error to.
	StderrPath string
}

// ValidateInterval reports an error if d falls outside the bounds this package enforces for any
// launchd StartInterval it generates: [1m, 6d] (mirroring config.minLiveWindow/maxLiveWindow -
// kept as a separate constant pair to avoid a launchd -> config import edge; the two must stay in
// sync). "session keepalive install" calls this for an explicit --interval override; the value
// derived from the minimum live-window ("session keepalive install"'s min/2, clamped to
// [15m, 12h]) is always within these bounds by construction and need not be re-validated.
func ValidateInterval(d time.Duration) error {
	if d < minInterval || d > maxInterval {
		return fmt.Errorf("launchd: interval %s must be between %s and %s", d, minInterval, maxInterval)
	}

	return nil
}

// DefaultAgentSpec builds the AgentSpec for the grafanapi keep-alive agent: binaryPath, running
// "session refresh --due" every interval, logging both streams to LogPath(). "--due" (never
// "--all") is deliberate: each scheduled wake must re-check every context's own live-window and
// only rotate the ones that are actually due, not force-rotate everything on every wake.
func DefaultAgentSpec(binaryPath string, interval time.Duration) AgentSpec {
	logPath := LogPath()

	return AgentSpec{
		Label:           Label,
		BinaryPath:      binaryPath,
		Args:            []string{"session", "refresh", "--due"},
		IntervalSeconds: int(interval.Seconds()),
		StdoutPath:      logPath,
		StderrPath:      logPath,
	}
}
