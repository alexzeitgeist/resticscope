package tui

import (
	"fmt"
	"strings"

	"github.com/alexzeitgeist/resticscope/internal/humanize"
	"github.com/alexzeitgeist/resticscope/internal/model"
)

const (
	emDash         = "—"
	searchPrompt   = "type to search"
	noMatchesLabel = "(no matches)"
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
	if m.isBrowseLoading && !m.browseIndexed {
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

// browseSummaryLine reports index progress with a persistent cancel hint, or the
// current directory state when indexing is complete.
func (m Model) browseSummaryLine() string {
	if m.isBrowseLoading && !m.browseIndexed {
		parts := []string{"indexing… " + humanize.Count(m.browseIndexN, "entry", "entries")}
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
	parts := []string{humanize.Count(len(m.browseRows), "entry", "entries")}
	if m.browseSortMode != browseSortName {
		parts = append(parts, "sort: "+m.browseSortMode.label())
	}
	return strings.Join(parts, " · ")
}

// browseSearchSummary reports errors, empty-query state, and capped or complete
// match counts.
func (m Model) browseSearchSummary() string {
	if m.browseSearchErr != "" {
		return m.browseSearchErr
	}
	if m.browseSearchTotal == 0 {
		if strings.TrimSpace(m.browseSearchQuery) == "" {
			return searchPrompt
		}
		return noMatchesLabel
	}
	if shown := len(m.browseSearchRows); shown < m.browseSearchTotal {
		return fmt.Sprintf("showing %d of %s", shown, humanize.Count(m.browseSearchTotal, "match", "matches"))
	}
	return humanize.Count(m.browseSearchTotal, "match", "matches")
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

// browseList renders a capped-width directory table with a stable header and a
// scroll note when needed.
func (m Model) browseList(w int) string {
	return m.browseTableList(w, m.browseRows, m.browseCursor, false, "(empty)")
}

// browseSearchList renders ranked full paths with a separate cursor, leaving
// empty-state text to the summary.
func (m Model) browseSearchList(w int) string {
	return m.browseTableList(w, m.browseSearchRows, m.browseSearchCursor, true, "")
}

// browseTableList renders the shared responsive table. showPath selects full
// paths, and an empty emptyNote renders only the header.
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

// browseTableMaxWidth keeps the flexible name column from stranding metadata on
// wide terminals.
const browseTableMaxWidth = 132

// browseTableWidth caps the file table while allowing trailing terminal space.
func browseTableWidth(w int) int {
	if w > browseTableMaxWidth {
		return browseTableMaxWidth
	}
	return w
}

// browseColLayout holds the flexible name width and promoted metadata columns.
type browseColLayout struct {
	name      int
	showMod   bool
	showPerms bool
	showOwner bool
}

const (
	browseSizeWidth  = 10
	browseModWidth   = 16 // "2006-01-02 15:04"
	browsePermsWidth = 10
	browseOwnerWidth = 11
	browseNameMin    = 16
)

// browseLayout always reserves gutter, name, and size, then promotes Modified,
// Perms, and Owner while the name column remains at least browseNameMin wide.
func browseLayout(width int) browseColLayout {
	const indicator, gap = 2, 2
	baseFixed := indicator + browseSizeWidth + gap

	var l browseColLayout
	reservedExtra := promoteColumns(width, baseFixed, browseNameMin, []optionalCol{
		{browseModWidth, &l.showMod},
		{browsePermsWidth, &l.showPerms},
		{browseOwnerWidth, &l.showOwner},
	})

	l.name = max(width-baseFixed-reservedExtra, 1)
	return l
}

// browseCells aligns headers and data in shared column order. It sizes arbitrary
// names and paths by display width while preserving their basename; metadata
// uses fixed-width padding.
func browseCells(l browseColLayout, name, size, mod, perms, owner string) []string {
	cells := []string{
		padRight(truncatePathWidth(name, l.name), l.name),
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

// browseHeaderRow uses data-row geometry and optionally marks the active sort.
// A collapsed Modified column relies on the summary for its sort indicator.
func browseHeaderRow(l browseColLayout, flexLabel string, sort browseSortMode, showSort bool) string {
	name, size, mod := flexLabel, "Size", "Modified"
	if showSort {
		// Size is the only right-aligned sortable column.
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

// browseSortArrow marks the active column; each mode has a fixed direction.
const browseSortArrow = "↓"

// markSortColumn places the arrow on a label's padding side so alignment does not
// shift when the marker appears.
func markSortColumn(label string, rightAligned bool) string {
	if rightAligned {
		return browseSortArrow + " " + label
	}
	return label + " " + browseSortArrow
}

// browseRow renders one clipped entry with cursor gutter and promoted metadata.
// Directories show a trailing slash and recursive size; absent metadata uses an
// em dash, and showPath displays the full search-result path.
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
	// Display-width truncation keeps wide-rune names aligned.
	nameCell := icon + name

	// Recursive sizes make zero a meaningful "0 B" for directories too.
	size := humanize.Bytes(e.Size)
	mod := emDash
	if !e.ModTime.IsZero() {
		mod = e.ModTime.Format("2006-01-02 15:04")
	}
	perms := emDash
	if e.Permissions != "" {
		perms = truncateWidth(e.Permissions, browsePermsWidth)
	}
	owner := emDash
	if e.OwnerKnown {
		owner = truncateWidth(fmt.Sprintf("%d:%d", e.UID, e.GID), browseOwnerWidth)
	}

	content := strings.Join(browseCells(l, nameCell, size, mod, perms, owner), "  ")
	indicator := "  "
	if selected {
		indicator = m.styles.gutter.Render("▎") + " "
		content = m.styles.selected.Render(content)
	}
	return clip(indicator+content, tw)
}

// browseAuxRows reserves the header and optional scroll note outside the entry window.
const browseAuxRows = 2

// browseVisible returns the entry capacity after fixed UI rows, floored at one.
func (m Model) browseVisible() int {
	_, h := m.effSize()
	overhead := headerRows + 2*gapRows + m.footerRows() + browseMetaRows + browseAuxRows
	if n := h - overhead; n >= 1 {
		return n
	}
	return 1
}

// browseDirLabel substitutes root for an empty directory path.
func browseDirLabel(p string) string {
	if p == "" {
		return "/"
	}
	return p
}

// shortID truncates a snapshot ID to restic's short-ID width.
func shortID(id string) string {
	if len(id) > snapIDWidth {
		return id[:snapIDWidth]
	}
	return id
}
