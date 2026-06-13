package tui

import (
	"strings"
	"testing"

	"resticscope/internal/model"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

// chrome_test.go locks the unified TUI chrome: the frame() contract (footer
// pinned to the bottom terminal row in every view), the persistent `? help`
// chip on the title row (and its deliberate input-state suppression), the
// `view: context` title convention, and the contextual back chips that moved
// from the per-view headers into the footer key bar.

// chromeCase is one view state the frame tests sweep: a setup that drives the
// model into the view, and a key-bar chip proving the bottom row is the footer.
type chromeCase struct {
	name     string
	setup    func(t *testing.T) Model
	wantLast string
}

// chromeViews drives the model into every view reachable in the TUI, including
// two-row-footer list states (filter prompt, status notice). The diff fixture
// mirrors TestSnapshotDiffViewRenders; browse/find/info reuse their suites'
// fixtures.
func chromeViews() []chromeCase {
	diffModel := func(t *testing.T) Model {
		a := detailApp(t)
		a.Restic = stubRestic{
			snaps: []model.Snapshot{{Hostname: "h"}},
			diffEntries: []model.DiffEntry{
				{Path: "/etc/passwd", Modifier: "M", Type: model.ChangeModified, Kinds: model.KindModified},
			},
		}
		m := newTestModel(t, a)
		m = update(t, m, press("enter"))
		m = update(t, m, press("t"))
		m = update(t, m, press("j"))
		m = update(t, m, press("t"))
		next, cmd := m.Update(press("d"))
		m = next.(Model)
		return drivePastDiff(t, m, cmd)
	}
	return []chromeCase{
		{"list", func(t *testing.T) Model {
			return newTestModel(t, detailApp(t))
		}, "q quit"},
		{"list filtering", func(t *testing.T) Model {
			m := newTestModel(t, detailApp(t))
			m = update(t, m, press("/"))
			return typeFilter(t, m, "re")
		}, "esc clear"},
		{"list with status notice", func(t *testing.T) Model {
			m := newTestModel(t, detailApp(t))
			m.statusMsg = "cache write failed: disk full"
			return m
		}, "q quit"},
		{"detail", func(t *testing.T) Model {
			return update(t, newTestModel(t, detailApp(t)), press("enter"))
		}, "q back"},
		{"browse", func(t *testing.T) Model {
			return openBrowse(t, newTestModel(t, browseApp(t,
				bnode("/home", "home", true, 0),
				bnode("/home/report.txt", "report.txt", false, 10),
			)))
		}, "q back"},
		{"find versions", func(t *testing.T) Model {
			a, _ := findApp(t, nil, bnode("/hostname", "hostname", false, 12))
			return openFindVersions(t, newTestModel(t, a), "hostname")
		}, "q back"},
		{"snapshot diff", diffModel, "q back"},
		{"info", func(t *testing.T) Model {
			m := newTestModel(t, snapshotInfoApp(t))
			m = update(t, m, press("enter"))
			return update(t, m, press("i"))
		}, "q back"},
		{"help", func(t *testing.T) Model {
			return update(t, newTestModel(t, detailApp(t)), press("?"))
		}, "q back"},
		{"extract review", func(t *testing.T) Model {
			em, _ := newExtractFixture(t, dirReq())
			m := newTestModel(t, extractApp(t))
			m.extract = em
			m.view = extractView
			return m
		}, "q back"},
	}
}

// The frame pins the footer to the bottom terminal row in every view: at a
// size where every body fits, the rendered view is exactly terminal-height
// tall and the last line is the key bar. The two-row footer states (filter
// prompt, status notice) guard against "pinned normally but drifts when a
// prompt line appears"; extract review guards the shortest body (~7 lines).
func TestFooterPinnedToBottomRowAcrossViews(t *testing.T) {
	const width, height = 192, 51 // the screenshot-baseline terminal size
	for _, tc := range chromeViews() {
		t.Run(tc.name, func(t *testing.T) {
			m := update(t, tc.setup(t), tea.WindowSizeMsg{Width: width, Height: height})
			content := m.View().Content
			if got := lipgloss.Height(content); got != height {
				t.Fatalf("view height = %d, want %d\n---\n%s", got, height, content)
			}
			lines := strings.Split(content, "\n")
			last := stripANSI(lines[len(lines)-1])
			if !strings.Contains(last, tc.wantLast) {
				t.Errorf("bottom row should be the key bar containing %q, got %q", tc.wantLast, last)
			}
		})
	}
}

// The `? help` chip sits top-right in every non-input state — including the
// help view itself, where ? is a toggle and the chip stays a truthful
// affordance (a deliberate exception; do not "clean it up").
func TestHelpChipPresentInEveryNonInputState(t *testing.T) {
	for _, tc := range chromeViews() {
		if tc.name == "list filtering" {
			continue // input state, asserted absent below
		}
		t.Run(tc.name, func(t *testing.T) {
			m := update(t, tc.setup(t), tea.WindowSizeMsg{Width: 192, Height: 51})
			title := stripANSI(strings.SplitN(m.View().Content, "\n", 2)[0])
			if !strings.Contains(title, "? help") {
				t.Errorf("title row should carry the '? help' chip, got %q", title)
			}
		})
	}
}

// While a text input owns the keyboard, ? is literal query text
// (handleInputKey gives inputs first claim), so the chip would be a false
// affordance and is suppressed — nothing in the whole frame advertises it.
func TestHelpChipSuppressedWhileTyping(t *testing.T) {
	cases := []chromeCase{
		{"list filtering", func(t *testing.T) Model {
			m := newTestModel(t, detailApp(t))
			m = update(t, m, press("/"))
			return typeFilter(t, m, "re")
		}, ""},
		{"browse searching", func(t *testing.T) Model {
			m := openBrowse(t, newTestModel(t, browseApp(t, bnode("/home", "home", true, 0))))
			return openSearch(t, m)
		}, ""},
		{"diff searching", func(t *testing.T) Model {
			for _, c := range chromeViews() {
				if c.name == "snapshot diff" {
					return openDiffSearch(t, c.setup(t))
				}
			}
			t.Fatal("no snapshot diff chrome case")
			return Model{}
		}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := update(t, tc.setup(t), tea.WindowSizeMsg{Width: 192, Height: 51})
			if content := stripANSI(m.View().Content); strings.Contains(content, "? help") {
				t.Errorf("'? help' must be hidden while a text input is active\n---\n%s", content)
			}
		})
	}
}

// A long title must not evict the chip: titleRow clips the title into the
// remaining width so `? help` survives, and the composed line never exceeds
// the terminal width. Only when the terminal is too narrow for the chip itself
// does the title win.
func TestTitleRowChipSurvivesLongTitle(t *testing.T) {
	m := newTestModel(t, detailApp(t))
	m.view = findVersionsView
	// The find title carries the repo name (the queried path lives in the
	// body), so a long repo name is what stretches the title now.
	m.findRepo = "repo-" + strings.Repeat("x", 150)
	m.browseSnapshot = "id-newest"

	m.width, m.height = 80, 30
	row := stripANSI(m.titleRow(m.findTitle()))
	if !strings.Contains(row, "? help") {
		t.Errorf("chip should survive a long find title at width 80, got %q", row)
	}
	if !strings.Contains(row, "versions: ") {
		t.Errorf("title prefix lost at width 80, got %q", row)
	}
	if got := lipgloss.Width(row); got > 80 {
		t.Errorf("title row width = %d, want <= 80: %q", got, row)
	}

	m.width = 8 // narrower than the chip + gap: the title wins
	row = stripANSI(m.titleRow(m.findTitle()))
	if strings.Contains(row, "? help") {
		t.Errorf("chip should be dropped at width 8, got %q", row)
	}
	if !strings.Contains(row, "versions") {
		t.Errorf("title should survive at width 8, got %q", row)
	}
	if got := lipgloss.Width(row); got > 8 {
		t.Errorf("title row width = %d, want <= 8: %q", got, row)
	}
}

// After a diff search jump, q mirrors esc and reverses the jump instead of
// leaving the view (routing.go), so the footer must advertise the combined
// `esc/q previous` chip and drop the now-lying `q back` — and restore it once
// the jump is reversed.
func TestDiffJumpedFooterAdvertisesPrevious(t *testing.T) {
	var m Model
	for _, c := range chromeViews() {
		if c.name == "snapshot diff" {
			m = c.setup(t)
		}
	}
	m = update(t, m, tea.WindowSizeMsg{Width: 192, Height: 51})
	m = openDiffSearch(t, m)
	m = typeDiffSearch(t, m, "passwd")
	m = update(t, m, press("enter"))
	if !m.diffSearchJumped {
		t.Fatal("precondition: enter on a match should arm the jump state")
	}

	footer := stripANSI(m.footerView())
	if !strings.Contains(footer, "esc/q previous") {
		t.Errorf("jumped diff footer should advertise 'esc/q previous'\n---\n%s", footer)
	}
	if strings.Contains(footer, "q back") {
		t.Errorf("jumped diff footer must not claim 'q back' (q reverses the jump)\n---\n%s", footer)
	}

	m = update(t, m, press("esc")) // reverse the jump
	if m.diffSearchJumped {
		t.Fatal("esc should reverse the jump")
	}
	footer = stripANSI(m.footerView())
	if !strings.Contains(footer, "q back") {
		t.Errorf("normal 'q back' chip should return once the jump is reversed\n---\n%s", footer)
	}
	if strings.Contains(footer, "previous") {
		t.Errorf("'previous' chip should disappear once the jump is reversed\n---\n%s", footer)
	}
}

// Every key bar uses the condensed "↑/↓" movement chip — never the two-chip
// "↑/k up • ↓/j down" form (j/k and ctrl+j/k stay documented in the ? overlay)
// — and reused chips keep one relative order across views: move → enter → ⌫ →
// / → view actions → sort/group → shell/refresh → back/quit. The list bar is
// spot-checked chip by chip since it was the one that deviated.
func TestFooterMovementChipAndOrderUnified(t *testing.T) {
	for _, tc := range chromeViews() {
		t.Run(tc.name, func(t *testing.T) {
			m := update(t, tc.setup(t), tea.WindowSizeMsg{Width: 192, Height: 51})
			footer := stripANSI(m.footerView())
			if strings.Contains(footer, "k up") || strings.Contains(footer, "j down") {
				t.Errorf("footer should use the condensed ↑/↓ chip\n---\n%s", footer)
			}
		})
	}

	m := update(t, newTestModel(t, detailApp(t)), tea.WindowSizeMsg{Width: 192, Height: 51})
	footer := stripANSI(m.footerView())
	pos := -1
	for _, chip := range []string{
		"↑/↓ move", "enter detail", "/ filter", "o sort", "g group",
		"s shell", "r refresh", "q quit",
	} {
		i := strings.Index(footer, chip)
		if i < 0 {
			t.Fatalf("list footer missing %q\n---\n%s", chip, footer)
		}
		if i < pos {
			t.Errorf("list footer chip %q out of order\n---\n%s", chip, footer)
		}
		pos = i
	}
	if strings.Contains(footer, "? help") {
		t.Errorf("list footer should not duplicate the title row's ? help chip\n---\n%s", footer)
	}
}

// Every view's key bar fits the ~100-column budget moveHelp documents
// (keys.go): at an effectively unlimited terminal width the bubbles help model
// renders the bar unclipped and footerView's clip is a no-op, so the assertion
// measures the chips themselves rather than a truncation. Known outlier left
// alone: the browse search-suspended bar (110 cells, transient state, not a
// chromeViews fixture) — if it gets added here, it needs an exception or a trim.
func TestFooterBarsFitWidthBudget(t *testing.T) {
	for _, tc := range chromeViews() {
		t.Run(tc.name, func(t *testing.T) {
			m := update(t, tc.setup(t), tea.WindowSizeMsg{Width: 4000, Height: 51})
			footer := stripANSI(m.footerView())
			lines := strings.Split(footer, "\n")
			bar := lines[len(lines)-1]
			if got := lipgloss.Width(bar); got > 100 {
				t.Errorf("key bar width = %d, want <= 100\n---\n%s", got, bar)
			}
		})
	}
}

func TestStatusWord(t *testing.T) {
	for _, tc := range []struct {
		status model.Status
		want   string
	}{
		{model.StatusGreen, "on schedule"},
		{model.StatusAmber, "grace period"},
		{model.StatusRed, "overdue"},
		{model.StatusError, "failed"},
		{model.StatusGrey, "never refreshed"},
	} {
		if got := statusWord(tc.status); got != tc.want {
			t.Errorf("statusWord(%s) = %q, want %q", tc.status, got, tc.want)
		}
	}
}

// Title spot-checks for the `view: context` convention. The detail title's
// status must read as a semantic word, never the literal color name.
func TestViewTitleConvention(t *testing.T) {
	m := update(t, newTestModel(t, detailApp(t)), press("enter"))
	wantDetail := "detail: repo-a · " + statusGlyph(model.StatusGreen) + " on schedule"
	if got := stripANSI(m.detailTitle()); !strings.Contains(got, wantDetail) {
		t.Errorf("detail title = %q, want %q", got, wantDetail)
	}
	if got := stripANSI(m.detailTitle()); strings.Contains(got, "green") {
		t.Errorf("detail title must not show the literal color word, got %q", got)
	}

	if got := stripANSI(m.helpTitle()); got != "help: keybindings" {
		t.Errorf("help title = %q, want 'help: keybindings'", got)
	}

	a, _ := findApp(t, nil, bnode("/hostname", "hostname", false, 12))
	fm := openFindVersions(t, newTestModel(t, a), "hostname")
	// The queried path is deliberately absent from the title (browse-title
	// shape); it renders once, in the body's Path row.
	want := "versions: repo-a · " + shortID(fm.browseSnapshot)
	if got := stripANSI(fm.findTitle()); got != want {
		t.Errorf("versions title = %q, want %q", got, want)
	}
	fm = update(t, fm, tea.WindowSizeMsg{Width: 120, Height: 40})
	if body := stripANSI(fm.findBody()); !strings.Contains(body, "/hostname") {
		t.Errorf("versions body must carry the queried path in its Path row\n---\n%s", body)
	}

	im := newTestModel(t, snapshotInfoApp(t))
	im = update(t, im, press("enter"))
	im = update(t, im, press("i"))
	if got := stripANSI(im.infoTitle()); got != "info: repo-a · sf" {
		t.Errorf("info title = %q, want 'info: repo-a · sf'", got)
	}
}
