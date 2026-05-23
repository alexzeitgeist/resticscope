package tui

import (
	"fmt"
	"sort"
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
	content := lipgloss.JoinVertical(lipgloss.Left,
		m.headerView(),
		"",
		m.listView(),
		"",
		m.footerView(),
	)
	v := tea.NewView(content)
	v.AltScreen = true
	return v
}

func (m Model) headerView() string {
	left := "resticscope · " + repoCount(len(m.rows))
	if m.resticVer != "" {
		left += " · restic " + m.resticVer
	}
	right := m.app.Clock.Now().Format("15:04:05")

	gap := "  "
	if m.width > 0 {
		if n := m.width - lipgloss.Width(left) - lipgloss.Width(right); n > 2 {
			gap = strings.Repeat(" ", n)
		}
	}
	return m.styles.title.Render(left) + gap + m.styles.dim.Render(right)
}

func (m Model) listView() string {
	if len(m.rows) == 0 {
		return m.styles.meta.Render("no repositories configured")
	}
	lines := make([]string, 0, len(m.rows))
	for i, row := range m.rows {
		lines = append(lines, m.renderRow(i, row))
	}
	return strings.Join(lines, "\n")
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
		s := fmt.Sprintf("last: %-9s  size: %9s  snaps: %4d",
			humanize.Ago(m.app.Clock.Now(), row.State.LastSnapshot),
			humanize.Bytes(row.State.TotalSize),
			row.State.SnapshotCount,
		)
		if row.Stale {
			s += "  " + m.styles.meta.Render("(stale)")
		}
		return s
	}
}

func (m Model) footerView() string {
	helpView := m.help.View(m.keys)
	if m.statusMsg != "" {
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
