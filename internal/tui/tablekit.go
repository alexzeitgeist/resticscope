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
	return widthPrefix(s, max-1) + "…" // reserve one cell for the ellipsis
}

// widthPrefix returns the longest prefix of s that spans at most budget
// display cells, measuring rune widths like truncateWidth.
func widthPrefix(s string, budget int) string {
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
	return b.String()
}

// widthSuffix is widthPrefix's mirror: the longest suffix of s spanning at
// most budget display cells.
func widthSuffix(s string, budget int) string {
	r := []rune(s)
	w, i := 0, len(r)
	for i > 0 {
		rw := lipgloss.Width(string(r[i-1]))
		if w+rw > budget {
			break
		}
		i--
		w += rw
	}
	return string(r[i:])
}

// truncExtMax bounds the suffix nameExt treats as a file extension: a dot plus
// up to six characters covers real extensions (".pdf", ".jsonl", ".sqlite")
// while rejecting dotted prose that happens to end a long filename.
const truncExtMax = 7

// nameExt returns the extension of name's last path component (".pdf") when it
// has one worth carrying through truncation, else "". A leading dot (dotfiles)
// is not an extension, and neither is a suffix longer than truncExtMax or one
// containing spaces.
func nameExt(name string) string {
	dot := strings.LastIndexByte(name, '.')
	if dot <= strings.LastIndexByte(name, '/')+1 || len(name)-dot > truncExtMax {
		return ""
	}
	if strings.ContainsRune(name[dot+1:], ' ') {
		return ""
	}
	return name[dot:]
}

// truncateNameWidth shortens a filename to at most max display cells like
// truncateWidth, but keeps what identifies the entry through the cut. The
// extension and a directory's trailing slash survive at the end, so a column
// of truncated names still tells "….pdf" from "….mp4", and the stem is elided
// in the middle — generated and versioned names differ at their tail
// ("VID-…0012.mp4" beats "VID-202….mp4", which keeps only the shared prefix).
// The remaining stem budget splits evenly between head and tail, head taking
// the odd cell. Falls back to plain truncateWidth when the column is too
// narrow to fit even the suffix.
func truncateNameWidth(s string, max int) string {
	if max <= 0 || lipgloss.Width(s) <= max {
		return truncateWidth(s, max)
	}
	core, trail := s, ""
	if strings.HasSuffix(s, "/") {
		core, trail = s[:len(s)-1], "/"
	}
	ext := nameExt(core)
	budget := max - 1 - lipgloss.Width(ext+trail) // 1 for the ellipsis
	if budget < 1 {
		return truncateWidth(s, max)
	}
	stem := core[:len(core)-len(ext)]
	head := widthPrefix(stem, (budget+1)/2)
	tail := widthSuffix(stem, budget-lipgloss.Width(head))
	return head + "…" + tail + ext + trail
}

// truncatePathWidth shortens a path to at most max display cells, keeping the
// final component — the part that actually identifies the entry — intact and
// collapsing the elided middle of the directory chain to a single "…", so
// "/Android/media/com.whatsapp/…/IMG-1234.jpg" rather than the useless
// "/Android/media/com.wha…". Leading components are kept while they fit. When
// even "…/<base>" overflows, the basename itself is cut extension-preservingly
// via truncateNameWidth — which also handles a slash-free s, so this is safe
// for flex cells that hold bare names and full paths interchangeably.
func truncatePathWidth(s string, max int) string {
	if max <= 0 || lipgloss.Width(s) <= max {
		return truncateWidth(s, max)
	}
	core, trail := s, ""
	if strings.HasSuffix(s, "/") {
		core, trail = s[:len(s)-1], "/"
	}
	slash := strings.LastIndexByte(core, '/')
	if slash < 0 {
		return truncateNameWidth(s, max)
	}
	tail := "…" + core[slash:] + trail // "…/name.ext", or "…/name/" for a dir
	if lipgloss.Width(tail) > max {
		return truncateNameWidth(tail, max)
	}
	// Keep whole leading components while the result still fits. Each kept
	// prefix ends at its slash, which tail's "…" follows directly; prefixes
	// only grow, so stop at the first that overflows.
	best := tail
	for j, head := 0, core[:slash]; j < len(head); j++ {
		if head[j] != '/' {
			continue
		}
		cand := head[:j+1] + tail
		if lipgloss.Width(cand) > max {
			break
		}
		best = cand
	}
	return best
}
