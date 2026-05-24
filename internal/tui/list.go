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
// credential's region plus the repo's labels. It is derived once from config.
type rowMeta struct {
	region string
	labels []string // label values, ordered by key for deterministic output
}

func buildMeta(cfg *config.Config) map[string]rowMeta {
	out := make(map[string]rowMeta, len(cfg.Repos))
	for _, r := range cfg.Repos {
		var rm rowMeta
		if cred, ok := cfg.Credential(r.Credential); ok {
			rm.region = cred.Region
		}
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

func (m Model) headerView() string {
	left := "resticscope · " + m.countLabel()
	if m.resticVer != "" {
		left += " · restic " + m.resticVer
	}
	if m.sortMode != sortConfig {
		left += " · sort: " + m.sortMode.label()
	}
	right := m.app.Clock.Now().Format("15:04:05")
	return m.spread(m.styles.title.Render(left), m.styles.dim.Render(right))
}

// spread lays left and right on one line, padding the gap so right sits flush
// against the terminal's right edge once the width is known. It expects already
// styled strings: lipgloss.Width discounts the styling escapes. Shared by every
// view's header bar.
func (m Model) spread(left, right string) string {
	gap := "  "
	if m.width > 0 {
		if n := m.width - lipgloss.Width(left) - lipgloss.Width(right); n > 2 {
			gap = strings.Repeat(" ", n)
		}
	}
	return left + gap + right
}

func (m Model) listView() string {
	if len(m.rows) == 0 {
		return m.styles.meta.Render("no repositories configured")
	}
	rows := m.visibleRows()
	if len(rows) == 0 {
		return m.styles.meta.Render("no repositories match " + strconv.Quote(strings.TrimSpace(m.filter)))
	}
	lines := make([]string, 0, len(rows))
	for i, row := range rows {
		lines = append(lines, m.renderRow(i, row))
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

func (m Model) renderRow(i int, row app.RepoStatus) string {
	indicator := "  "
	if i == m.cursor {
		indicator = "> "
	}

	mark := m.styles.glyph[row.Status].Render(statusGlyph(row.Status))
	if m.pending[row.Name] {
		mark = m.spinner.View()
	}

	nameStyle := m.styles.name
	if i == m.cursor {
		nameStyle = nameStyle.Bold(true)
	}

	main := indicator + mark + " " + nameStyle.Render(row.Name) + m.summary(row)

	sub := m.metaLine(row.Name)
	if sub == "" {
		return main
	}
	return main + "\n      " + m.styles.meta.Render(sub)
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
		s := fmt.Sprintf("last: %-9s  snaps: %4d",
			humanize.Ago(m.app.Clock.Now(), row.State.LastSnapshot),
			row.State.SnapshotCount,
		)
		if row.Stale {
			s += "  " + m.styles.meta.Render("(stale)")
		}
		return s
	}
}

func (m Model) footerView() string {
	helpView := m.help.View(viewHelp{keys: m.keys, view: m.view, filtering: m.filtering})
	switch {
	case m.filtering:
		// Show the live query (vim-style) with a block cursor so the input mode
		// is obvious. The "/<query>" stays unstyled so it reads as one token.
		return "/" + m.filter + m.styles.dim.Render("▏") + "\n" + helpView
	case m.statusMsg != "":
		return m.styles.errText.Render(m.statusMsg) + "\n" + helpView
	}
	return helpView
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
