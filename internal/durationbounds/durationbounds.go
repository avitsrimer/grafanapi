// Package durationbounds holds the single [1m, 6d] duration bound pair shared by two independent
// scheduling knobs that must always agree: a context's "live-window" configuration field
// (internal/config) and the keep-alive LaunchAgent's schedule interval (internal/launchd,
// cmd/grafanapi/session --interval). It also provides the day-aware duration parser both sides
// need: Go's time.ParseDuration deliberately has no "d" (day) unit, but "6d" reads far better than
// "144h" at the upper end of a multi-day bound. Keeping both the bounds and the parser in one leaf
// package - rather than two copies "kept in sync by comment" - is what makes them actually stay in
// sync.
package durationbounds

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

const (
	// Min is the smallest duration accepted wherever these bounds apply (live-window, --interval).
	Min = time.Minute
	// Max is the largest duration accepted wherever these bounds apply (live-window, --interval).
	Max = 6 * 24 * time.Hour
)

// ParseWithDays parses s as a time.Duration, additionally accepting a bare "d" (day) unit suffix
// (e.g. "6d", "1.5d") - a grafanapi extension beyond what time.ParseDuration supports, since a
// calendar day is not always exactly 24h once time zones/DST are involved; a day is treated as
// exactly 24h here, which is precise enough for a scheduling window measured in days. A day count
// that is not finite (NaN/Inf) or would overflow a time.Duration is rejected explicitly here,
// rather than silently converting to a saturated/garbage duration and relying on a caller's bounds
// check to incidentally reject it.
func ParseWithDays(s string) (time.Duration, error) {
	if d, err := time.ParseDuration(s); err == nil {
		return d, nil
	}

	days, ok := strings.CutSuffix(s, "d")
	if !ok {
		// Return time.ParseDuration's own error: it is more descriptive than a generic one, and
		// callers only see it when both the plain and the "d"-suffixed forms failed.
		_, err := time.ParseDuration(s)

		return 0, err
	}

	count, err := strconv.ParseFloat(days, 64)
	if err != nil {
		return 0, fmt.Errorf("time: invalid day count in duration %q: %w", s, err)
	}

	if math.IsNaN(count) || math.IsInf(count, 0) {
		return 0, fmt.Errorf("time: day count in duration %q must be finite", s)
	}

	nanos := count * float64(24*time.Hour)
	if nanos > math.MaxInt64 || nanos < math.MinInt64 {
		return 0, fmt.Errorf("time: day count in duration %q is out of range", s)
	}

	return time.Duration(nanos), nil
}
