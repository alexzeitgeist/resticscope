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

// formatCoverageRollup writes the cross-repo coverage aggregate: a headline
// count plus one line per repo with an unmet expectation. It is appended to
// `resticscope status --coverage` after the per-repo table, and stays plain
// text so it remains scriptable (plan §9, §11).
func formatCoverageRollup(w io.Writer, r app.CoverageRollup) {
	fmt.Fprintf(w, "coverage: %d of %d repos fully covered\n", r.Covered, r.Total)
	if r.FullyCovered() {
		return
	}
	fmt.Fprintln(w)
	tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
	for _, g := range r.Gaps {
		fmt.Fprintf(tw, "  %s\t%s\n", g.Repo, g.Coverage.Summary())
	}
	tw.Flush()
}

// checkLine writes one aligned stage line for `resticscope check`, e.g.
//
//	config   ok      3 repos, 2 credentials
//	restic   FAILED  0.16.0 is older than the minimum supported 0.17.0
func checkLine(w io.Writer, stage, status, detail string) {
	fmt.Fprintf(w, "%-9s%-8s%s\n", stage, status, detail)
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
