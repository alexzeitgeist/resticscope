// Package humanize renders machine quantities (byte counts, elapsed time) as
// short human-readable strings. It is shared by every presentation layer — the
// plain `resticscope status` table and the TUI list view — so the two render
// the same value identically.
package humanize

import (
	"fmt"
	"time"
)

// Ago renders the elapsed time since t in a single coarse unit ("8h ago"). A
// zero time is "never"; a future time is clamped to "just now".
func Ago(now, t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	d := now.Sub(t)
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours())/24)
	}
}

// Duration renders an elapsed duration in coarse units: sub-second durations as
// "<1s", whole seconds below a minute ("28s"), minutes and seconds below an
// hour ("4m12s"), else hours and minutes ("1h03m"). A negative duration renders
// as an em-dash because it is invalid.
func Duration(d time.Duration) string {
	if d < 0 {
		return "—"
	}
	switch {
	case d < time.Second:
		return "<1s"
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
	default:
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	}
}

// Bytes formats a byte count in IEC units (GiB, MiB, …), with one decimal place
// below 10 of a unit.
func Bytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	value := float64(n) / float64(div)
	suffix := []string{"KiB", "MiB", "GiB", "TiB", "PiB", "EiB"}[exp]
	if value >= 10 {
		return fmt.Sprintf("%.0f %s", value, suffix)
	}
	return fmt.Sprintf("%.1f %s", value, suffix)
}
