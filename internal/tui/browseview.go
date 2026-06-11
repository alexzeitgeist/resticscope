package tui

import (
	"fmt"
	"strings"

	"resticscope/internal/humanize"
	"resticscope/internal/model"
)

func (m Model) browseTitle() string {
	label := "browse: " + m.browseRepo
	if id := shortID(m.browseSnapshot); id != "" {
		label += " · " + id
	}
	return m.styles.title.Render(label)
}

func (m Model) browseBody() string {
	w, _ := m.effSize()
	pathLine := m.pathLine("Path", browseDirLabel(m.browseDir), w)
	summary := clip(m.styles.meta.Render("  "+m.browseSummaryLine()), w)
	if m.browseLoading && !m.browseIndexed {
		return strings.Join([]string{pathLine, summary}, "\n")
	}
	if m.browseSearching {
		return strings.Join([]string{pathLine, summary, m.browseSearchList(w)}, "\n")
	}
	return strings.Join([]string{pathLine, summary, m.browseList(w)}, "\n")
}

func (m Model) pathLine(label, value string, width int) string {
	return clip(m.styles.label.Render(label)+m.styles.name.UnsetWidth().Render(value), width)
}

// browseSummaryLine reports progress. While the one-time index runs it shows the
// running entry count and keeps the cancel affordance visible — a huge snapshot
// can take minutes, and the user must always see that esc/back aborts it.
// Otherwise it reports the current directory's entry count.
func (m Model) browseSummaryLine() string {
	if m.browseLoading && !m.browseIndexed {
		parts := []string{fmt.Sprintf("indexing… %d entries", m.browseIndexN)}
		if rate := browseIndexRateLabel(m.browseRate.rate); rate != "" {
			parts = append(parts, rate)
		}
		parts = append(parts, "esc/back cancels")
		return strings.Join(parts, " · ")
	}
	if m.browseSearching {
		return m.browseSearchSummary()
	}
	if m.browseNotice != "" {
		return m.browseNotice
	}
	parts := []string{fmt.Sprintf("%d entries", len(m.browseRows))}
	if m.browseSortMode != browseSortName {
		parts = append(parts, "sort: "+m.browseSortMode.label())
	}
	return strings.Join(parts, " · ")
}

// browseSearchSummary reports the state of the global filename search for the body
// summary line: a path-free error if the last search failed; "type to search"
// before anything is typed; "(no matches)" for an empty result; "showing N of
// Total matches" when the result cap trimmed the list; otherwise "N matches".
func (m Model) browseSearchSummary() string {
	if m.browseSearchErr != "" {
		return m.browseSearchErr
	}
	if m.browseSearchTotal == 0 {
		if strings.TrimSpace(m.browseSearchQuery) == "" {
			return "type to search"
		}
		return "(no matches)"
	}
	if shown := len(m.browseSearchRows); shown < m.browseSearchTotal {
		return fmt.Sprintf("showing %d of %d matches", shown, m.browseSearchTotal)
	}
	return fmt.Sprintf("%d matches", m.browseSearchTotal)
}

func browseIndexRateLabel(rate float64) string {
	if rate <= 0 {
		return ""
	}
	switch {
	case rate >= 1_000_000:
		return fmt.Sprintf("%.1fM/s", rate/1_000_000)
	case rate >= 1_000:
		return fmt.Sprintf("%.0fk/s", rate/1_000)
	case rate >= 10:
		return fmt.Sprintf("%.0f/s", rate)
	default:
		return fmt.Sprintf("%.1f/s", rate)
	}
}

// browseList renders the scrolling window of the current directory's rows as a
// responsive table: a dim column header, then rows marking the cursor with the
// accent gutter, and a window note when the list is scrolled. The header is shown
// for empty directories too so the table shape stays stable. The table renders at
// a capped working width (browseTableWidth) so a very wide terminal does not
// stretch the Name flex into a desert; the surrounding header/path/summary lines
// keep using the full terminal width.
func (m Model) browseList(w int) string {
	return m.browseTableList(w, m.browseRows, m.browseCursor, false, "(empty)")
}

// browseSearchList renders the ranked global-search matches with the same
// responsive table as browseList, but the flex column shows each match's full
// path (so results from anywhere in the snapshot are unambiguous) and it scrolls
// by the separate browseSearchCursor so esc can restore the directory listing
// untouched. The empty state is left to the summary line ("type to search",
// "(no matches)", or the error), so here an empty result is just the header.
func (m Model) browseSearchList(w int) string {
	return m.browseTableList(w, m.browseSearchRows, m.browseSearchCursor, true, "")
}

// browseTableList renders a window of rows as the responsive browse table shared
// by the directory listing and the global-search results. showPath swaps the flex
// column (and its header label) between the bare Name and the full Path, so the
// label can never disagree with the cell content. emptyNote is the meta line shown
// when there are no rows ("" renders just the header, as the search list wants).
func (m Model) browseTableList(w int, rows []model.BrowseEntry, cursor int, showPath bool, emptyNote string) string {
	tw := browseTableWidth(w)
	l := browseLayout(tw)
	flexLabel := "Name"
	if showPath {
		flexLabel = "Path"
	}
	header := clip(m.styles.dim.Render(browseHeaderRow(l, flexLabel, m.browseSortMode, !showPath)), tw)

	total := len(rows)
	if total == 0 {
		if emptyNote == "" {
			return header
		}
		return header + "\n" + clip(m.styles.meta.Render("  "+emptyNote), tw)
	}

	cur := clampCursor(cursor, total)
	start, end := scrollWindow(cur, total, m.browseVisible())

	lines := make([]string, 0, end-start+2)
	lines = append(lines, header)
	for i := start; i < end; i++ {
		lines = append(lines, m.browseRow(&rows[i], i == cur, l, tw, showPath))
	}
	if start > 0 || end < total {
		lines = append(lines, clip(m.styles.meta.Render(fmt.Sprintf("  showing %d–%d of %d", start+1, end, total)), tw))
	}
	return strings.Join(lines, "\n")
}

// browseTableMaxWidth bounds the file table's working width. Browse puts its only
// flexible column (Name) first, so left unbounded a wide terminal stretches Name
// and strands the metadata columns far to the right. (The snapshot table avoids
// this for free: its flex column, Tags, is last, so slack falls harmlessly at the
// trailing edge.) Capping the whole table — rather than just the Name column —
// states the decision once and keeps it correct if a future column is added.
const browseTableMaxWidth = 132

// browseTableWidth is the width the file table renders at: the terminal width,
// capped at browseTableMaxWidth so a wide terminal leaves trailing empty space
// rather than over-stretching the Name flex.
func browseTableWidth(w int) int {
	if w > browseTableMaxWidth {
		return browseTableMaxWidth
	}
	return w
}

// browseColLayout describes the browse table's variable geometry for a given
// width: the Name flex width and which of the optional Modified/Perms/Owner
// columns are promoted. Column promotion is shared with snapLayout via
// promoteColumns; only the flex distribution below is browse-specific.
type browseColLayout struct {
	name      int
	showMod   bool
	showPerms bool
	showOwner bool
}

const (
	browseSizeWidth  = 10 // right-aligned Size column (matches the old single size column)
	browseModWidth   = 16 // "2006-01-02 15:04"
	browsePermsWidth = 10 // os.FileMode.String() is usually 10 chars; longer (sticky/setuid) is clipped
	browseOwnerWidth = 11 // "uid:gid"; real uids are small, a pathological pair is clipped
	browseNameMin    = 16 // the Name flex must stay at least this wide for a column to be promoted
)

// browseLayout sizes the browse table's columns to the total width. A two-cell
// indicator, the Name flex, and the fixed Size column (plus their two-space gaps)
// are always reserved. Modified then Perms then Owner are promoted in priority
// order, each only while the Name flex would stay at least browseNameMin wide
// afterwards; promotion stops at the first that won't fit so a lower-priority
// column never appears without a higher one. Promotion is shared with
// snapshotLayout via promoteColumns; only the Name flex distribution below is
// browse-specific.
func browseLayout(width int) browseColLayout {
	const indicator, gap = 2, 2 // the gutter, and the one gap before Size
	baseFixed := indicator + browseSizeWidth + gap

	var l browseColLayout
	reservedExtra := promoteColumns(width, baseFixed, browseNameMin, []optionalCol{
		{browseModWidth, &l.showMod},
		{browsePermsWidth, &l.showPerms},
		{browseOwnerWidth, &l.showOwner},
	})

	l.name = width - baseFixed - reservedExtra
	if l.name < 1 {
		l.name = 1
	}
	return l
}

// browseCells formats one row's worth of columns — header or data — into the
// shared column order so both align: Name(l.name,left) · Size(10,right) ·
// [Modified(16,left)] · [Perms(10,left)] · [Owner(11,right)]. Callers join the
// result with two spaces. The Name cell carries arbitrary filenames, so it is
// truncated and padded by display width (filenames can hold wide runes); the
// fixed metadata cells are ASCII, so fmt's rune-count padding is exact for them.
func browseCells(l browseColLayout, name, size, mod, perms, owner string) []string {
	cells := []string{
		padRight(truncateWidth(name, l.name), l.name),
		fmt.Sprintf("%*s", browseSizeWidth, size),
	}
	if l.showMod {
		cells = append(cells, fmt.Sprintf("%-*s", browseModWidth, mod))
	}
	if l.showPerms {
		cells = append(cells, fmt.Sprintf("%-*s", browsePermsWidth, perms))
	}
	if l.showOwner {
		cells = append(cells, fmt.Sprintf("%*s", browseOwnerWidth, owner))
	}
	return cells
}

// browseHeaderRow is the dim column-label row, built from the same browseCells
// layout as the data rows (plus the two-cell gutter the rows get from their
// indicator) so labels line up with their values at every width. When showSort is
// set (the directory listing), markSortColumn flags the active sort column with a
// single down arrow (see browseSortArrow). A collapsed Modified column shows no
// arrow; the summary still names the sort.
func browseHeaderRow(l browseColLayout, flexLabel string, sort browseSortMode, showSort bool) string {
	name, size, mod := flexLabel, "Size", "Modified"
	if showSort {
		// Only Size is right-aligned in browseCells (%*s); Name and Modified are
		// left-aligned, so markSortColumn trails their arrow into the right padding.
		switch sort {
		case browseSortName:
			name = markSortColumn(name, false)
		case browseSortSize:
			size = markSortColumn(size, true)
		case browseSortModified:
			mod = markSortColumn(mod, false)
		}
	}
	return "  " + strings.Join(browseCells(l, name, size, mod, "Perms", "Owner"), "  ")
}

// browseSortArrow is the marker flagging the active sort column in the header. Each
// browse sort mode has a single fixed direction (name A→Z, size largest-first,
// modified newest-first), so one down arrow just flags which column the listing is
// ordered by; it is not a reversible ascending/descending indicator.
const browseSortArrow = "↓"

// markSortColumn appends browseSortArrow to a header label on its padding side, so
// the label text stays put when the marker appears instead of sliding over to make
// room: the arrow leads a right-aligned label (Size, kept pinned over its numbers)
// and trails a left-aligned one (growing into its right padding). Either way it only
// extends the label, never the column width (browseCells pads to the fixed layout),
// so the data rows stay aligned underneath.
func markSortColumn(label string, rightAligned bool) string {
	if rightAligned {
		return browseSortArrow + " " + label
	}
	return label + " " + browseSortArrow
}

// browseRow renders one entry as a table row: an accent gutter on the cursor row,
// then the width-promoted Name/Size/Modified/Perms/Owner cells. Directories show a
// trailing slash and their recursive subtree size (0 B when empty); missing
// metadata (no mtime, perms, or owner) renders as an em-dash. When showPath is set the flex column shows the
// entry's full path instead of its bare name (used by the global search list, whose
// matches come from anywhere in the snapshot). The whole row content is clipped to
// width so a long name or path can't wrap, and the selected style covers the row.
func (m Model) browseRow(e *model.BrowseEntry, selected bool, l browseColLayout, tw int, showPath bool) string {
	icon := "  "
	name := e.Name
	if showPath {
		name = e.Path
	}
	if e.IsDir {
		icon = "▸ "
		name += "/"
	}
	// The name cell is icon + name; browseCells truncates and pads it to the flex
	// width by display width so wide-rune names don't misalign the columns.
	nameCell := icon + name

	// Directories now carry a real recursive subtree size, so render every entry's
	// size unconditionally; an empty directory's 0 renders as "0 B".
	size := humanize.Bytes(e.Size)
	mod := "—"
	if !e.ModTime.IsZero() {
		mod = e.ModTime.Format("2006-01-02 15:04")
	}
	perms := "—"
	if e.Permissions != "" {
		perms = truncate(e.Permissions, browsePermsWidth)
	}
	owner := "—"
	if e.OwnerKnown {
		owner = truncate(fmt.Sprintf("%d:%d", e.UID, e.GID), browseOwnerWidth)
	}

	content := strings.Join(browseCells(l, nameCell, size, mod, perms, owner), "  ")
	indicator := "  "
	if selected {
		indicator = m.styles.gutter.Render("▎") + " "
		content = m.styles.selected.Render(content)
	}
	return clip(indicator+content, tw)
}

// browseAuxRows is the worst-case number of fixed lines browseList renders around
// the scrolling window: the column header, plus — for a scrolled directory — the
// "showing N–M of T" note. browseVisible reserves both so the footer is never
// overlapped.
const browseAuxRows = 2

// browseVisible is how many entry rows the browse list shows at once: the height
// minus the app header, gaps, footer, the two browse meta rows, and the two
// auxiliary table lines (column header + scroll note). Floored at 1.
func (m Model) browseVisible() int {
	_, h := m.effSize()
	overhead := headerRows + 2*gapRows + m.footerRows() + browseMetaRows + browseAuxRows
	if n := h - overhead; n >= 1 {
		return n
	}
	return 1
}

// browseDirLabel renders the current directory path for the header line, never
// empty so the label always has a value.
func browseDirLabel(p string) string {
	if p == "" {
		return "/"
	}
	return p
}

// shortID renders restic's 8-char short id from a full snapshot id, leaving an
// already-short id untouched.
func shortID(id string) string {
	if len(id) > snapIDWidth {
		return id[:snapIDWidth]
	}
	return id
}
