package main

import (
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"resticscope/internal/app"
	"resticscope/internal/model"
)

// formatStatusTable writes one plain-text line per repo, columns aligned. It
// stays uncolored and scriptable on purpose (plan §9, recommendation 5).
func formatStatusTable(w io.Writer, rows []app.RepoStatus, now time.Time) {
	tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
	for _, r := range rows {
		switch r.Status {
		case model.StatusError:
			msg := "refresh failed"
			if r.State.LastError != "" {
				msg = "refresh failed: " + firstLine(r.State.LastError)
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\n", r.Name, r.Status, msg)
		case model.StatusGrey:
			fmt.Fprintf(tw, "%s\t%s\t%s\n", r.Name, r.Status, "never refreshed")
		default:
			line := fmt.Sprintf("%s\t%s\t%s\t%s\t%d snaps",
				r.Name, r.Status,
				humanizeAgo(now, r.State.LastSnapshot),
				humanizeBytes(r.State.TotalSize),
				r.State.SnapshotCount,
			)
			if r.Stale {
				line += "\t(stale cache)"
			}
			fmt.Fprintln(tw, line)
		}
	}
	tw.Flush()
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// humanizeAgo renders the elapsed time since t in a single coarse unit.
func humanizeAgo(now, t time.Time) string {
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

// humanizeBytes formats a byte count in IEC units (GiB, MiB, …), with one
// decimal place below 10 of a unit.
func humanizeBytes(n int64) string {
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
