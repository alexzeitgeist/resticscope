// Package humanize formats machine quantities consistently across CLI and TUI
// presentation.
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
	d := max(now.Sub(t), 0)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", d/time.Minute)
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", d/time.Hour)
	default:
		return fmt.Sprintf("%dd ago", d/(24*time.Hour))
	}
}

// Duration renders elapsed time as "<1s", seconds, minutes and seconds, or hours
// and minutes. Negative values render as an em-dash.
func Duration(d time.Duration) string {
	if d < 0 {
		return "—"
	}
	switch {
	case d < time.Second:
		return "<1s"
	case d < time.Minute:
		return fmt.Sprintf("%ds", d/time.Second)
	case d < time.Hour:
		return fmt.Sprintf("%dm%02ds", d/time.Minute, d%time.Minute/time.Second)
	default:
		return fmt.Sprintf("%dh%02dm", d/time.Hour, d%time.Hour/time.Minute)
	}
}

// Count renders n with the singular form only for one. Callers provide both
// forms for irregular plurals.
func Count[N ~int | ~int64 | ~uint64](n N, singular, plural string) string {
	if n == 1 {
		return "1 " + singular
	}
	return fmt.Sprintf("%d %s", n, plural)
}

// byteSuffixes are the IEC unit suffixes indexed by Bytes's division exponent.
var byteSuffixes = [...]string{"KiB", "MiB", "GiB", "TiB", "PiB", "EiB"}

// Bytes formats nonnegative byte counts in IEC units, using one decimal below
// 10 of a unit. Negative counts render as an em-dash.
func Bytes(n int64) string {
	const unit = 1024
	if n < 0 {
		return "—"
	}
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	value := float64(n) / float64(div)
	// int64 values require at most all six suffixes, so exp stays in bounds.
	suffix := byteSuffixes[exp]
	if value >= 10 {
		return fmt.Sprintf("%.0f %s", value, suffix)
	}
	return fmt.Sprintf("%.1f %s", value, suffix)
}
