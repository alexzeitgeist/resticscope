package tui

import (
	"strings"
	"testing"

	"github.com/alexzeitgeist/resticscope/internal/model"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

// These tests protect footer placement, title/help conventions, and contextual
// navigation hints across TUI views.

// chromeCase identifies a view and a footer chip expected on its final row.
type chromeCase struct {
	name     string
	setup    func(t *testing.T) Model
	wantLast string
}

// chromeViews covers every reachable view, including two-row footer states.
func chromeViews() []chromeCase {
	diffModel := func(t *testing.T) Model {
		t.Helper()
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
			t.Helper()
			return newTestModel(t, detailApp(t))
		}, "q quit"},
		{"list filtering", func(t *testing.T) Model {
			t.Helper()
			m := newTestModel(t, detailApp(t))
			m = update(t, m, press("/"))
			return typeFilter(t, m, "re")
		}, "esc clear"},
		{"list with status notice", func(t *testing.T) Model {
			t.Helper()
			m := newTestModel(t, detailApp(t))
			m.statusMsg = "cache write failed: disk full"
			return m
		}, "q quit"},
		{"detail", func(t *testing.T) Model {
			t.Helper()
			return update(t, newTestModel(t, detailApp(t)), press("enter"))
		}, "q back"},
		{"browse", func(t *testing.T) Model {
			t.Helper()
			return openBrowse(t, newTestModel(t, browseApp(t,
				bnode("/home", "home", true, 0),
				bnode("/home/report.txt", "report.txt", false, 10),
			)))
		}, "q back"},
		{"find versions", func(t *testing.T) Model {
			t.Helper()
			a, _ := findApp(t, nil, bnode("/hostname", "hostname", false, 12))
			return openFindVersions(t, newTestModel(t, a), "hostname")
		}, "q back"},
		{"snapshot diff", diffModel, "q back"},
		{"snapshot diff after search jump", func(t *testing.T) Model {
			t.Helper()
			m := typeDiffSearch(t, openDiffSearch(t, diffModel(t)), "passwd")
			m = update(t, m, press("enter"))
			if !m.diffSearchJumped {
				t.Fatal("precondition: enter should jump to the match")
			}
			return m
		}, "esc/q previous"},
		{"diff info", func(t *testing.T) Model {
			t.Helper()
			m := openInfoDiff(t, infoApp(t, stubRestic{treeNodes: infoNodes()}), "/etc/passwd")
			return pressInfo(t, m)
		}, "q back"},
		{"info", func(t *testing.T) Model {
			t.Helper()
			m := newTestModel(t, snapshotInfoApp(t))
			m = update(t, m, press("enter"))
			return update(t, m, press("i"))
		}, "q back"},
		{"help", func(t *testing.T) Model {
			t.Helper()
			return update(t, newTestModel(t, detailApp(t)), press("?"))
		}, "q back"},
		{"extract review", func(t *testing.T) Model {
			t.Helper()
			em, _ := newExtractFixture(t, dirReq())
			m := newTestModel(t, extractApp(t))
			m.extract = em
			m.view = extractView
			return m
		}, "q back"},
	}
}

// Every fitting view remains terminal-height with its key bar on the final row,
// including two-row footers and the short extraction view.
func TestFooterPinnedToBottomRowAcrossViews(t *testing.T) {
	const width, height = 192, 51
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

// The help chip remains visible in every non-input state, including help itself.
func TestHelpChipPresentInEveryNonInputState(t *testing.T) {
	for _, tc := range chromeViews() {
		if tc.name == "list filtering" {
			continue
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

// Text inputs consume `?` literally, so they suppress the help affordance.
func TestHelpChipSuppressedWhileTyping(t *testing.T) {
	cases := []chromeCase{
		{"list filtering", func(t *testing.T) Model {
			t.Helper()
			m := newTestModel(t, detailApp(t))
			m = update(t, m, press("/"))
			return typeFilter(t, m, "re")
		}, ""},
		{"browse searching", func(t *testing.T) Model {
			t.Helper()
			m := openBrowse(t, newTestModel(t, browseApp(t, bnode("/home", "home", true, 0))))
			return openSearch(t, m)
		}, ""},
		{"diff searching", func(t *testing.T) Model {
			t.Helper()
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

// Long titles preserve the help chip unless the chip itself cannot fit.
func TestTitleRowChipSurvivesLongTitle(t *testing.T) {
	m := newTestModel(t, detailApp(t))
	m.view = findVersionsView
	// The repository name, not the body-only path, stretches this title.
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

	m.width = 8
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

// A diff-search jump replaces q-back with esc/q-previous until reversed.
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

	m = update(t, m, press("esc"))
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

// Key bars share the condensed movement glyph and a consistent action order;
// the help overlay retains the expanded bindings.
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

// At unlimited render width, each ordinary key bar stays within its 100-column
// content budget. The transient suspended-search bar is intentionally excluded.
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

// Titles use `view: context`, with semantic status text instead of color names.
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
	// The queried path appears only in the body Path row.
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
