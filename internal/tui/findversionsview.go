package tui

import (
	"fmt"
	"strings"

	"resticscope/internal/humanize"
	"resticscope/internal/model"
)

// findversionsview.go renders findVersionsView: a title naming the repo and
// origin snapshot, a Path line carrying the queried path, a fixed-shape table
// of distinct file versions, and a status line carrying the host-filter label. The host label
// is driven by findResultHost/findResultAllHosts (the filter that produced the
// rows on screen), never by the user-toggle findRequestAllHosts, so a
// mid-toggle re-run can never relabel the visible rows until the new response
// actually lands.

const (
	findModWidth   = 16 // "2006-01-02 15:04" — file mtime, the dedup key
	findSizeWidth  = 10 // right-aligned humanize.Bytes
	findSnapsWidth = 5  // right-aligned snapshot count
	findLatestMin  = 18 // "YYYY-MM-DD HH:MM xxxxxxxx"; trimmed when host promotes
	findHostMin    = 8  // shortest host fragment shown when promoted (truncated by truncateWidth)
	findPermsWidth = 10
	findOwnerWidth = 11 // "uid:gid"; matches browseOwnerWidth so both views feel consistent
	findMetaRows   = 2  // path + summary
	findAuxRows    = 2  // column header + the "showing N–M of T" scroll note
)

// findTitle names the find context: the repo and the snapshot the search was
// launched from (find is reached only from browse, so browseSnapshot is the
// origin) — the same `view: repo · id` shape as browseTitle. The queried path
// is NOT repeated here: it lives in the body's Path row, where it can render
// unclipped.
func (m Model) findTitle() string {
	label := "versions: " + m.findRepo
	if id := shortID(m.browseSnapshot); id != "" {
		label += " · " + id
	}
	return m.styles.title.Render(label)
}

// findHostLabel renders the host-filter description from the result-of-record
// fields, so a mid-toggle reload cannot relabel the visible rows until the new
// response arrives. The label is "all hosts" when the response's AllHosts is
// true, the hostname the response actually filtered by otherwise, and "…"
// before any successful response has landed (initial load or after an error).
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
		// The host filter explains an empty result, so it must stay visible
		// here even though the populated branch below also carries it.
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

// findColLayout describes the find-versions table's columns for a given
// width. Modified / Size / Snaps are always present. Latest, Permissions,
// and Owner promote in priority order — Latest first (so the user can see
// when the most recent backup carrying the version ran), then Permissions
// and Owner together for parity with the browse table.
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
		// Take as much as needed for "YYYY-MM-DD HH:MM xxxxxxxx" plus a small
		// host suffix when there is room. The flex is bounded so a wide
		// terminal does not strand the column.
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
		// The summary line already explains the empty state ("loading…",
		// "(no matches)", or the error), so render just the column header here.
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

// findLatestText formats the newest occurrence in a group: "YYYY-MM-DD HH:MM <shortid>",
// with the host appended when the result is across all hosts (so the user can
// disambiguate which machine the most recent copy lives on).
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

// findVisible is the number of version rows the table shows at once: the height
// minus the app header, gaps, footer, the two meta rows (path + summary), and
// the two table auxiliary lines. Floored at 1.
func (m Model) findVisible() int {
	_, h := m.effSize()
	overhead := headerRows + 2*gapRows + m.footerRows() + findMetaRows + findAuxRows
	if n := h - overhead; n >= 1 {
		return n
	}
	return 1
}
