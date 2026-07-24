package tui

import (
	"strings"

	"charm.land/lipgloss/v2"
)

// optionalCol describes one promotable column and its layout flag.
type optionalCol struct {
	width int
	on    *bool
}

// promoteColumns enables columns in priority order while preserving flexMin.
// It stops at the first miss so lower-priority columns cannot appear alone and
// returns the width reserved for columns and separators.
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

// truncateWidth appends an ellipsis when cutting s to max display cells. It
// measures terminal width so wide runes cannot shift later columns.
func truncateWidth(s string, max int) string {
	if max <= 0 {
		return ""
	}
	if lipgloss.Width(s) <= max {
		return s
	}
	return widthPrefix(s, max-1) + "…" // reserve one cell for the ellipsis
}

// widthPrefix returns the longest prefix within a display-cell budget.
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

// widthSuffix returns the longest suffix within a display-cell budget.
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

// truncExtMax accepts a dot plus six extension characters while rejecting long
// dotted prose.
const truncExtMax = 7

// nameExt returns a short, space-free extension from the final path component.
// Leading dots do not count as extensions.
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

// truncateNameWidth preserves an extension, trailing slash, and both ends of a
// filename within max display cells. It falls back to end truncation when the
// suffix does not fit.
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

// truncatePathWidth preserves the final component, collapses middle directories,
// and retains leading components while they fit. If the basename still
// overflows, truncateNameWidth preserves its extension.
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
	tail := "…" + core[slash:] + trail // ".../name.ext", or ".../name/" for a dir
	if lipgloss.Width(tail) > max {
		return truncateNameWidth(tail, max)
	}
	// Keep complete leading components until the first overflow.
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
