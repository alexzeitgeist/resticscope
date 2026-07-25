package main

import (
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/alexzeitgeist/resticscope/internal/app"
	"github.com/alexzeitgeist/resticscope/internal/humanize"
	"github.com/alexzeitgeist/resticscope/internal/model"
)

// formatStatusTable writes one plain-text line per repo, columns aligned. It
// stays uncolored and scriptable on purpose.
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
			line := fmt.Sprintf("%s\t%s\t%s\t%s\t%s",
				r.Name, r.Status,
				humanize.Ago(now, r.State.LastSnapshot),
				humanize.Count(r.State.SnapshotCount, "snap", "snaps"),
				lastBackupDuration(r.State.Snapshots),
			)
			if r.Stale {
				line += "\t(stale cache)"
			}
			fmt.Fprintln(tw, line)
		}
	}
	_ = tw.Flush()
}

func lastBackupDuration(snaps []model.Snapshot) string {
	d, ok := model.LastBackupDuration(snaps)
	if !ok {
		return "—"
	}
	return humanize.Duration(d)
}

// formatPruneResult writes an uncolored cache directory, per-repository rows,
// and summary. Dry runs use conditional wording.
func formatPruneResult(w io.Writer, res app.PruneResult, dryRun bool) {
	if res.Root == "" {
		fmt.Fprintln(w, "no cache directory configured; nothing to prune")
		return
	}
	if len(res.Entries) == 0 {
		fmt.Fprintf(w, "restic cache: %s\n\nno restic caches found; nothing to prune\n", res.Root)
		return
	}

	pruneVerb := "pruned"
	if dryRun {
		pruneVerb = "would prune"
	}

	fmt.Fprintf(w, "restic cache: %s\n\n", res.Root)
	var total int64
	tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
	for _, e := range res.Entries {
		total += e.Size
		action := "keep"
		if e.IsPruned {
			action = pruneVerb
			if e.IsOrphan {
				action += " (orphan)"
			}
		}
		fmt.Fprintf(tw, "  %s\t%s\t%s\n", e.Name, humanize.Bytes(e.Size), action)
	}
	_ = tw.Flush()

	fmt.Fprintln(w)
	switch n := res.Pruned(); {
	case n == 0:
		fmt.Fprintf(w, "nothing to prune (%s in %s); use --all to clear active caches too\n",
			humanize.Bytes(total), caches(len(res.Entries)))
	case dryRun:
		fmt.Fprintf(w, "would free %s across %d of %s\n",
			humanize.Bytes(res.Freed), n, caches(len(res.Entries)))
	default:
		fmt.Fprintf(w, "freed %s across %d of %s\n",
			humanize.Bytes(res.Freed), n, caches(len(res.Entries)))
	}
}

func caches(n int) string {
	return humanize.Count(n, "cache", "caches")
}

// checkLine writes one aligned check-stage row.
func checkLine(w io.Writer, stage, status, detail string) {
	fmt.Fprintf(w, "%-9s%-8s%s\n", stage, status, detail)
}

func firstLine(s string) string {
	if before, _, ok := strings.Cut(s, "\n"); ok {
		return before
	}
	return s
}
