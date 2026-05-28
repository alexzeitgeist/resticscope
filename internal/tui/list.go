package tui

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"charm.land/lipgloss/v2"

	tea "charm.land/bubbletea/v2"

	"resticscope/internal/app"
	"resticscope/internal/config"
	"resticscope/internal/humanize"
	"resticscope/internal/model"
)

// rowMeta is the per-repo descriptive line shown under each list entry: the
// repo's region plus its labels. It is derived once from config.
type rowMeta struct {
	region string
	labels []string // label values, ordered by key for deterministic output
}

func buildMeta(cfg *config.Config) map[string]rowMeta {
	out := make(map[string]rowMeta, len(cfg.Repos))
	for _, r := range cfg.Repos {
		rm := rowMeta{region: r.Region}
		keys := make([]string, 0, len(r.Labels))
		for k := range r.Labels {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			rm.labels = append(rm.labels, r.Labels[k])
		}
		out[r.Name] = rm
	}
	return out
}

func (m Model) View() tea.View {
	if m.quitting {
		return tea.NewView("")
	}
	_, hasDetail := m.detailRow()
	var body string
	switch {
	case m.view == helpView:
		body = lipgloss.JoinVertical(lipgloss.Left,
			m.helpHeaderView(),
			"",
			m.helpBody(),
		)
	case m.view == browseView:
		body = lipgloss.JoinVertical(lipgloss.Left,
			m.browseHeaderView(),
			"",
			m.browseBody(),
		)
	case m.view == detailView && hasDetail:
		body = lipgloss.JoinVertical(lipgloss.Left,
			m.detailHeaderView(),
			"",
			m.detailBody(),
		)
	default:
		body = lipgloss.JoinVertical(lipgloss.Left,
			m.headerView(),
			"",
			m.listView(),
		)
	}
	content := lipgloss.JoinVertical(lipgloss.Left, body, "", m.footerView())
	v := tea.NewView(content)
	v.AltScreen = true
	return v
}

// headerView is the top line: the app name, the at-a-glance status badges, the
// repo count, the restic version, and (when active) the sort indicator. The
// badges count the visible rows so they agree with countLabel() under a filter.
// It is a plain line, not a bar — only the app name is bold and only the badges
// carry color.
func (m Model) headerView() string {
	parts := []string{m.styles.title.Render("resticscope")}
	if badges := m.statusBadges(statusCounts(m.visibleRows())); badges != "" {
		parts = append(parts, badges)
	}
	parts = append(parts, m.countLabel())
	if m.resticVer != "" {
		parts = append(parts, "restic "+m.resticVer)
	}
	if m.sortMode != sortConfig {
		parts = append(parts, "sort: "+m.sortMode.label())
	}
	w, _ := m.effSize()
	return clip(strings.Join(parts, " · "), w)
}

// statusCounts tallies the rows by status across all five buckets.
func statusCounts(rows []app.RepoStatus) map[model.Status]int {
	counts := make(map[model.Status]int, 5)
	for _, r := range rows {
		counts[r.Status]++
	}
	return counts
}

// statusBadges renders the header's count buckets, each in its status color and
// only when non-zero: green (●), amber (▲), failed (✕, red and error summed so ✕
// is never double-counted), and cold (…, grey).
func (m Model) statusBadges(counts map[model.Status]int) string {
	buckets := []struct {
		status model.Status
		n      int
	}{
		{model.StatusGreen, counts[model.StatusGreen]},
		{model.StatusAmber, counts[model.StatusAmber]},
		{model.StatusRed, counts[model.StatusRed] + counts[model.StatusError]},
		{model.StatusGrey, counts[model.StatusGrey]},
	}
	parts := make([]string, 0, len(buckets))
	for _, b := range buckets {
		if b.n == 0 {
			continue
		}
		parts = append(parts, m.styles.glyph[b.status].Render(fmt.Sprintf("%s%d", statusGlyph(b.status), b.n)))
	}
	return strings.Join(parts, "  ")
}

// spread lays left and right on one line, padding the gap so right sits flush
// against the right edge once the width is known. It expects already styled
// strings: lipgloss.Width discounts the styling escapes. Shared by every view's
// header.
func (m Model) spread(left, right string) string {
	w, _ := m.effSize()
	gap := "  "
	if n := w - lipgloss.Width(left) - lipgloss.Width(right); n > 2 {
		gap = strings.Repeat(" ", n)
	}
	return left + gap + right
}

func (m Model) listView() string {
	w, _ := m.effSize()
	if len(m.rows) == 0 {
		return clip(m.styles.meta.Render("no repositories configured"), w)
	}
	rows := m.visibleRows()
	if len(rows) == 0 {
		return clip(m.styles.meta.Render("no repositories match "+strconv.Quote(strings.TrimSpace(m.filter))), w)
	}

	// Clamp the cursor here too: the field isn't re-clamped when a filter shrinks
	// the set, and the window must center on a real row.
	cursor := m.cursor
	if cursor >= len(rows) {
		cursor = len(rows) - 1
	}
	if cursor < 0 {
		cursor = 0
	}
	start, end := listWindow(cursor, len(rows), m.visibleRepos())

	lines := make([]string, 0, end-start+1)
	for i := start; i < end; i++ {
		lines = append(lines, m.renderRow(rows[i], i == cursor, w))
	}
	if start > 0 || end < len(rows) {
		lines = append(lines, clip(m.styles.meta.Render(fmt.Sprintf("  showing %d–%d of %d", start+1, end, len(rows))), w))
	}
	return strings.Join(lines, "\n")
}

// countLabel describes how many repos the list is showing: the total normally,
// or "N of M" while a filter narrows the set.
func (m Model) countLabel() string {
	total := len(m.rows)
	if strings.TrimSpace(m.filter) == "" {
		return repoCount(total)
	}
	return fmt.Sprintf("%d of %d repos", len(m.visibleRows()), total)
}

// renderRow renders one repo as a main line (a left gutter, the status
// glyph/spinner, the name, and the status summary) plus an optional indented meta
// sub-line. The cursor row is marked with an accent gutter bar (▎) and an accent
// name rather than a full-width highlight, so selection reads as style, not as a
// status. Both lines are clipped to width so a long name or refresh error can't
// wrap and break the two-lines-per-repo budget visibleRepos relies on.
func (m Model) renderRow(row app.RepoStatus, selected bool, width int) string {
	gutter := "  "
	nameStyle := m.styles.name
	if selected {
		gutter = m.styles.gutter.Render("▎") + " "
		nameStyle = m.styles.selectedName
	}

	mark := m.styles.glyph[row.Status].Render(statusGlyph(row.Status))
	if m.pending[row.Name] {
		mark = m.spinner.View()
	}

	main := gutter + mark + " " + nameStyle.Render(truncate(row.Name, nameWidth)) + m.summary(row)
	main = clip(main, width)

	sub := m.metaLine(row.Name)
	if sub == "" {
		return main
	}
	return main + "\n" + clip("      "+m.styles.meta.Render(sub), width)
}

func (m Model) summary(row app.RepoStatus) string {
	switch row.Status {
	case model.StatusError:
		msg := "refresh failed"
		if row.State.LastError != "" {
			msg = "refresh failed: " + firstLine(row.State.LastError)
		}
		return m.styles.errText.Render(msg)
	case model.StatusGrey:
		return m.styles.meta.Render("never refreshed")
	default:
		took := truncate(tookDuration(row.State.Snapshots), 7)
		s := fmt.Sprintf("last: %-9s  snaps: %4d  took: %-7s",
			humanize.Ago(m.app.Clock.Now(), row.State.LastSnapshot),
			row.State.SnapshotCount,
			took,
		)
		if row.Stale {
			s += "  " + m.styles.meta.Render("(stale)")
		}
		return s
	}
}

func tookDuration(snaps []model.Snapshot) string {
	d, ok := model.LastBackupDuration(snaps)
	if !ok {
		return "—"
	}
	return humanize.Duration(d)
}

func (m Model) footerView() string {
	w, _ := m.effSize()
	help := clip(m.help.View(viewHelp{keys: m.keys, view: m.view, filtering: m.filtering}), w)
	switch {
	case m.filtering:
		// Show the live query (vim-style) with a block cursor so the input mode
		// is obvious. The "/<query>" stays unstyled so it reads as one token.
		return clip("/"+m.filter+m.styles.dim.Render("▏"), w) + "\n" + help
	case m.statusMsg != "":
		return clip(m.styles.errText.Render(m.statusMsg), w) + "\n" + help
	}
	return help
}

func (m Model) metaLine(name string) string {
	rm, ok := m.meta[name]
	if !ok {
		return ""
	}
	parts := make([]string, 0, 1+len(rm.labels))
	if rm.region != "" {
		parts = append(parts, rm.region)
	}
	parts = append(parts, rm.labels...)
	return strings.Join(parts, " · ")
}

func repoCount(n int) string {
	if n == 1 {
		return "1 repo"
	}
	return fmt.Sprintf("%d repos", n)
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
