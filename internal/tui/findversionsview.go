package tui

import (
	"fmt"
	"strings"

	"github.com/alexzeitgeist/resticscope/internal/humanize"
	"github.com/alexzeitgeist/resticscope/internal/model"
)

// Version rendering labels rows from the result's host filter, never an in-flight
// request toggle.

const (
	findModWidth   = 16
	findSizeWidth  = 10
	findSnapsWidth = 5
	findLatestMin  = 18
	findHostMin    = 8
	findPermsWidth = 10
	findOwnerWidth = 11
	findMetaRows   = 2
	findAuxRows    = 2
)

// findTitle names the repository and origin snapshot; the queried path remains
// in the body.
func (m Model) findTitle() string {
	label := "versions: " + m.findRepo
	if id := shortID(m.browseSnapshot); id != "" {
		label += " · " + id
	}
	return m.styles.title.Render(label)
}

// findHostLabel describes the installed result filter, using an ellipsis before
// any successful response.
func (m Model) findHostLabel() string {
	if m.findResultAllHosts {
		return "all hosts"
	}
	if m.findResultHost != "" {
		return m.findResultHost
	}
	return "…"
}

func (m Model) findBody() string {
	w, _ := m.effSize()
	pathLine := m.pathLine("Path", m.findPath, w)
	summary := clip(m.styles.meta.Render("  "+m.findSummaryLine()), w)
	return strings.Join([]string{pathLine, summary, m.findList(w)}, "\n")
}

func (m Model) findSummaryLine() string {
	if m.findErr != "" {
		return m.findErr
	}
	if m.findLoading() {
		return "loading…"
	}
	if len(m.findRows) == 0 {
		// Keep the host filter visible when no rows match.
		return "(no matches) · host: " + m.findHostLabel()
	}
	snaps := 0
	for _, r := range m.findRows {
		snaps += len(r.Occurrences)
	}
	return fmt.Sprintf("%s across %s · host: %s",
		humanize.Count(len(m.findRows), "version", "versions"),
		humanize.Count(snaps, "snapshot", "snapshots"),
		m.findHostLabel())
}

// findColLayout always includes Modified, Size, and Snaps, then promotes Latest,
// Permissions, and Owner in that order.
type findColLayout struct {
	latest     int
	showLatest bool
	showPerms  bool
	showOwner  bool
}

func findLayout(width int) findColLayout {
	const indicator, gap = 2, 2
	baseFixed := indicator + findModWidth + findSizeWidth + findSnapsWidth + 2*gap

	var l findColLayout
	rest := width - baseFixed
	if rest >= findLatestMin+gap {
		l.showLatest = true
		// Add bounded room for a host suffix when available.
		l.latest = findLatestMin
		rest -= findLatestMin + gap
		if rest >= findHostMin+gap {
			l.latest += findHostMin
			rest -= findHostMin + gap
		}
	}
	if rest >= findPermsWidth+gap {
		l.showPerms = true
		rest -= findPermsWidth + gap
	}
	if rest >= findOwnerWidth+gap {
		l.showOwner = true
	}
	return l
}

func (m Model) findList(w int) string {
	tw := browseTableWidth(w)
	l := findLayout(tw)
	header := clip(m.styles.dim.Render(findHeaderRow(l)), tw)

	if m.findLoading() || m.findErr != "" || len(m.findRows) == 0 {
		// The summary owns loading, empty, and error text.
		return header
	}

	total := len(m.findRows)
	cur := clampCursor(m.findCursor, total)
	start, end := scrollWindow(cur, total, m.findVisible())
	lines := make([]string, 0, end-start+2)
	lines = append(lines, header)
	for i := start; i < end; i++ {
		lines = append(lines, m.findRow(&m.findRows[i], i == cur, l, tw))
	}
	if start > 0 || end < total {
		lines = append(lines, clip(m.styles.meta.Render(fmt.Sprintf("  showing %d–%d of %d", start+1, end, total)), tw))
	}
	return strings.Join(lines, "\n")
}

func findHeaderRow(l findColLayout) string {
	cells := []string{
		fmt.Sprintf("%-*s", findModWidth, "Modified"),
		fmt.Sprintf("%*s", findSizeWidth, "Size"),
		fmt.Sprintf("%*s", findSnapsWidth, "Snaps"),
	}
	if l.showLatest {
		cells = append(cells, fmt.Sprintf("%-*s", l.latest, "Latest"))
	}
	if l.showPerms {
		cells = append(cells, fmt.Sprintf("%-*s", findPermsWidth, "Perms"))
	}
	if l.showOwner {
		cells = append(cells, fmt.Sprintf("%*s", findOwnerWidth, "Owner"))
	}
	return "  " + strings.Join(cells, "  ")
}

func (m Model) findRow(v *model.FileVersion, selected bool, l findColLayout, tw int) string {
	mod := emDash
	if !v.ModTime.IsZero() {
		mod = v.ModTime.Format("2006-01-02 15:04")
	}
	cells := []string{
		fmt.Sprintf("%-*s", findModWidth, mod),
		fmt.Sprintf("%*s", findSizeWidth, humanize.Bytes(v.Size)),
		fmt.Sprintf("%*d", findSnapsWidth, len(v.Occurrences)),
	}
	if l.showLatest {
		latest := findLatestText(v, m.findResultAllHosts)
		cells = append(cells, padRight(truncateWidth(latest, l.latest), l.latest))
	}
	if l.showPerms {
		perms := emDash
		if v.Permissions != "" {
			perms = truncateWidth(v.Permissions, findPermsWidth)
		}
		cells = append(cells, fmt.Sprintf("%-*s", findPermsWidth, perms))
	}
	if l.showOwner {
		owner := emDash
		if v.OwnerKnown {
			owner = truncateWidth(fmt.Sprintf("%d:%d", v.UID, v.GID), findOwnerWidth)
		}
		cells = append(cells, fmt.Sprintf("%*s", findOwnerWidth, owner))
	}

	content := strings.Join(cells, "  ")
	indicator := "  "
	if selected {
		indicator = m.styles.gutter.Render("▎") + " "
		content = m.styles.selected.Render(content)
	}
	return clip(indicator+content, tw)
}

// findLatestText formats the newest occurrence and appends its host for all-host results.
func findLatestText(v *model.FileVersion, allHosts bool) string {
	if len(v.Occurrences) == 0 {
		return emDash
	}
	o := v.Occurrences[0]
	ts := emDash
	if !o.SnapTime.IsZero() {
		ts = o.SnapTime.Format("2006-01-02 15:04")
	}
	id := o.ShortID
	if id == "" {
		id = emDash
	}
	out := ts + " " + id
	if allHosts && o.Hostname != "" {
		out += " " + o.Hostname
	}
	return out
}

// findVisible returns row capacity after fixed UI rows, floored at one.
func (m Model) findVisible() int {
	_, h := m.effSize()
	overhead := headerRows + 2*gapRows + m.footerRows() + findMetaRows + findAuxRows
	if n := h - overhead; n >= 1 {
		return n
	}
	return 1
}
