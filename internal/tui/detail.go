package tui

import (
	"fmt"
	"sort"
	"strings"

	"resticscope/internal/app"
	"resticscope/internal/config"
	"resticscope/internal/humanize"
	"resticscope/internal/model"
)

// detailVisibleSnaps caps how many snapshot rows the detail view shows at once;
// the window scrolls to keep the selected snapshot in view.
const detailVisibleSnaps = 12

// repoConfig returns the config.Repo backing the named row. The bool is false
// only if config and rows somehow disagree, which validation prevents.
func (m Model) repoConfig(name string) (config.Repo, bool) {
	for _, r := range m.app.Cfg.Repos {
		if r.Name == name {
			return r, true
		}
	}
	return config.Repo{}, false
}

// detailRow returns the repo the detail view is pinned to (by name, set on
// enter). Resolving by name rather than cursor keeps the detail view stable when
// a size/staleness sort reorders the list after a background refresh.
func (m Model) detailRow() (app.RepoStatus, bool) {
	for _, r := range m.rows {
		if r.Name == m.detailName {
			return r, true
		}
	}
	return app.RepoStatus{}, false
}

// detailSnapshots returns the detail repo's snapshots ordered newest-first (the
// order the detail view and snapCursor both use). It copies before sorting so
// the cached state's slice order is left untouched.
func (m Model) detailSnapshots() []model.Snapshot {
	row, _ := m.detailRow()
	src := row.State.Snapshots
	snaps := make([]model.Snapshot, len(src))
	copy(snaps, src)
	sort.SliceStable(snaps, func(i, j int) bool { return snaps[i].Time.After(snaps[j].Time) })
	return snaps
}

func (m Model) snapCount() int {
	row, _ := m.detailRow()
	return len(row.State.Snapshots)
}

// selectedSnapshot returns the snapshot under the detail cursor, or nil when the
// repo has none. The cursor is clamped on read so a refresh that shrinks the
// list can never index out of range.
func (m Model) selectedSnapshot() *model.Snapshot {
	snaps := m.detailSnapshots()
	if len(snaps) == 0 {
		return nil
	}
	cur := m.snapCursor
	if cur >= len(snaps) {
		cur = len(snaps) - 1
	}
	return &snaps[cur]
}

func (m Model) detailHeaderView() string {
	row, _ := m.detailRow()
	left := m.styles.title.Render(row.Name) + "  " +
		m.styles.glyph[row.Status].Render(statusGlyph(row.Status)+" "+string(row.Status))
	right := m.styles.dim.Render("b back")
	return m.spread(left, right)
}

func (m Model) detailBody() string {
	row, _ := m.detailRow()
	repo, _ := m.repoConfig(row.Name)

	sections := []string{
		m.detailMeta(repo, row),
		m.styles.heading.Render("Snapshots") + "\n" + m.snapshotTable(),
	}
	return strings.Join(sections, "\n\n")
}

// detailMeta renders the fixed repo facts: where it lives and what was observed.
func (m Model) detailMeta(repo config.Repo, row app.RepoStatus) string {
	st := row.State

	last := humanize.Ago(m.app.Clock.Now(), st.LastSnapshot)
	if !st.LastSnapshot.IsZero() {
		last = st.LastSnapshot.Format("2006-01-02 15:04") + " (" + last + ")"
	}

	lines := []string{
		m.field("Endpoint", repo.Endpoint),
		m.field("Bucket", bucketLabel(repo)),
		m.field("Snapshots", fmt.Sprintf("%d", st.SnapshotCount)),
		m.field("Hosts", joinOrDash(st.Hosts)),
		m.field("Tags", joinOrDash(st.Tags)),
		m.field("Last", last),
	}
	return strings.Join(lines, "\n")
}

func (m Model) field(label, value string) string {
	return "  " + m.styles.label.Render(label) + value
}

// snapshotTable renders a scrolling window of snapshots, newest first, marking
// the selected row. Enter on the selection opens a shell scoped to it.
func (m Model) snapshotTable() string {
	snaps := m.detailSnapshots()
	if len(snaps) == 0 {
		return m.styles.meta.Render("  no snapshots")
	}

	cur := m.snapCursor
	if cur >= len(snaps) {
		cur = len(snaps) - 1
	}
	start, end := snapshotWindow(cur, len(snaps), detailVisibleSnaps)

	lines := make([]string, 0, end-start+1)
	for i := start; i < end; i++ {
		s := snaps[i]
		indicator := "  "
		if i == cur {
			indicator = "> "
		}
		size := "—"
		if s.Summary != nil {
			size = humanize.Bytes(s.Summary.TotalBytesProcessed)
		}
		line := fmt.Sprintf("%s%-16s  %-12s  %9s  %s",
			indicator,
			s.Time.Format("2006-01-02 15:04"),
			truncate(s.Hostname, 12),
			size,
			truncate(strings.Join(s.Tags, ","), 20),
		)
		if i == cur {
			line = m.styles.selected.Render(line)
		}
		lines = append(lines, line)
	}
	if start > 0 || end < len(snaps) {
		lines = append(lines, m.styles.meta.Render(fmt.Sprintf("  showing %d–%d of %d", start+1, end, len(snaps))))
	}
	return strings.Join(lines, "\n")
}

// snapshotWindow returns the [start, end) slice bounds for a scrolling window of
// the given size that keeps cursor visible and centered when possible.
func snapshotWindow(cursor, total, size int) (start, end int) {
	if total <= size {
		return 0, total
	}
	start = cursor - size/2
	if start < 0 {
		start = 0
	}
	end = start + size
	if end > total {
		end = total
		start = end - size
	}
	return start, end
}

func bucketLabel(repo config.Repo) string {
	if p := strings.Trim(repo.Path, "/"); p != "" {
		return repo.Bucket + "/" + p
	}
	return repo.Bucket
}

func joinOrDash(vals []string) string {
	if len(vals) == 0 {
		return "—"
	}
	return strings.Join(vals, ", ")
}

func truncate(s string, max int) string {
	if max <= 0 || len([]rune(s)) <= max {
		return s
	}
	r := []rune(s)
	if max == 1 {
		return "…"
	}
	return string(r[:max-1]) + "…"
}
