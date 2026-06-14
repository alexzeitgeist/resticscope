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
		return fmt.Sprintf("%ds", d/time.Second)
	case d < time.Hour:
		return fmt.Sprintf("%dm%02ds", d/time.Minute, d%time.Minute/time.Second)
	default:
		return fmt.Sprintf("%dh%02dm", d/time.Hour, d%time.Hour/time.Minute)
	}
}

// Count renders a count with its noun, picking the singular form for exactly
// one ("1 file", "3 files", "0 files"). Callers pass both forms because
// English pluralization is irregular ("entry" / "entries"). The constraint
// covers the integer types presentation code actually carries — len() results
// and restic's uint64 summary counters — so no caller has to narrow; extend it
// as needed.
func Count[N ~int | ~int64 | ~uint64](n N, singular, plural string) string {
	if n == 1 {
		return "1 " + singular
	}
	return fmt.Sprintf("%d %s", n, plural)
}

// byteSuffixes are the IEC unit suffixes indexed by Bytes's division exponent.
var byteSuffixes = [...]string{"KiB", "MiB", "GiB", "TiB", "PiB", "EiB"}

// Bytes formats a byte count in IEC units (GiB, MiB, …), with one decimal place
// below 10 of a unit. A negative count renders as an em-dash because it is
// invalid.
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
	// exp counts 1024-divisions of an int64: the max (~8 EiB) gives exp == 5,
	// the last of six suffixes, so the index can never run off the end.
	suffix := byteSuffixes[exp]
	if value >= 10 {
		return fmt.Sprintf("%.0f %s", value, suffix)
	}
	return fmt.Sprintf("%.1f %s", value, suffix)
}
