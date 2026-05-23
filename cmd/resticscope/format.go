package main

import (
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"resticscope/internal/app"
	"resticscope/internal/humanize"
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
				humanize.Ago(now, r.State.LastSnapshot),
				humanize.Bytes(r.State.TotalSize),
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
