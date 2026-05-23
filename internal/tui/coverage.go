package tui

import (
	"fmt"
	"strings"

	"resticscope/internal/app"
)

// coverageHeaderView is the title bar for the cross-repo coverage view, with the
// fully-covered count on the left and a back hint on the right.
func (m Model) coverageHeaderView(r app.CoverageRollup) string {
	left := m.styles.title.Render("resticscope · coverage") + "  " +
		m.styles.dim.Render(fmt.Sprintf("%d of %d repos fully covered", r.Covered, r.Total))
	right := m.styles.dim.Render("b back")
	return m.spread(left, right)
}

// coverageBody lists every repo with an unmet expectation, or a single
// all-clear line. The rollup spans all configured repos, independent of any
// list-view filter.
func (m Model) coverageBody(r app.CoverageRollup) string {
	if r.FullyCovered() {
		return "  " + m.styles.good.Render("✓ all repositories meet their expectations")
	}
	lines := make([]string, 0, len(r.Gaps))
	for _, g := range r.Gaps {
		// styles.name pads short names to a column for alignment; the explicit
		// gap guarantees separation even for names that overflow that column.
		lines = append(lines, "  "+m.styles.bad.Render("✕ ")+m.styles.name.Render(g.Repo)+"  "+g.Coverage.Summary())
	}
	return strings.Join(lines, "\n")
}
