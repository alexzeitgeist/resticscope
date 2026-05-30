package tui

import (
	"strings"

	"charm.land/lipgloss/v2"
)

// optionalCol is one promotable column for promoteColumns: its width and the
// layout flag to flip on when it fits.
type optionalCol struct {
	width int
	on    *bool
}

// promoteColumns turns on as many of cols (priority order) as fit, each only
// while at least flexMin width remains for the table's flex area after
// reserving the column plus one two-space separator. Stops at the first that
// won't fit, so a lower-priority column never appears without a higher one.
// Returns total extra width reserved (widths + separators).
func promoteColumns(width, baseFixed, flexMin int, cols []optionalCol) (reservedExtra int) {
	for _, c := range cols {
		cost := c.width + 2
		if width-baseFixed-reservedExtra-cost < flexMin {
			break
		}
		reservedExtra += cost
		*c.on = true
	}
	return reservedExtra
}

// truncateWidth shortens s to at most max display cells, appending an ellipsis
// when it has to cut. Unlike truncate (which counts runes), it measures each
// rune's terminal width, so a filename with wide runes — CJK, emoji, or the
// fullwidth/small colon some apps substitute for ':' — still fits its column
// instead of shoving the metadata columns out of alignment. fmt's %-*s and
// rune-based truncate both miscount such names.
func truncateWidth(s string, max int) string {
	if max <= 0 {
		return ""
	}
	if lipgloss.Width(s) <= max {
		return s
	}
	budget := max - 1 // reserve one cell for the ellipsis
	var b strings.Builder
	w := 0
	for _, r := range s {
		rw := lipgloss.Width(string(r))
		if w+rw > budget {
			break
		}
		b.WriteRune(r)
		w += rw
	}
	return b.String() + "…"
}
