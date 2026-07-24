package tui

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/alexzeitgeist/resticscope/internal/app"
	"github.com/alexzeitgeist/resticscope/internal/model"

	tea "charm.land/bubbletea/v2"
)

// These tests cover TUI diff marks, pair ordering, search, streaming, and view
// transitions; model-level tree and parser behavior is tested separately.

// openDetail enters repo-a detail with the default three-snapshot fixture.
func openDetail(t *testing.T) Model {
	t.Helper()
	m := newTestModel(t, detailApp(t))
	m = update(t, m, press("enter"))
	if m.view != detailView {
		t.Fatalf("precondition: view = %d, want detailView", m.view)
	}
	return m
}

// drivePastDiff delivers the synchronous stub's terminal message to populate
// the diff view.
func drivePastDiff(t *testing.T, m Model, cmd tea.Cmd) Model {
	t.Helper()
	if cmd == nil {
		t.Fatal("expected a diff command")
	}
	// Select the terminal-message leaf from the stream/progress batch.
	for _, leaf := range leafCmds(t, cmd) {
		out := leaf()
		if msg, ok := out.(snapshotDiffMsg); ok {
			return update(t, m, msg)
		}
	}
	t.Fatal("no snapshotDiffMsg in batch")
	return m
}

func openDiffSearch(t *testing.T, m Model) Model {
	t.Helper()
	m = update(t, m, press("/"))
	if !m.diffSearching {
		t.Fatal("/ should open diff search")
	}
	return m
}

func typeDiffSearch(t *testing.T, m Model, q string) Model {
	t.Helper()
	for _, r := range q {
		next, cmd := m.Update(press(string(r)))
		m = next.(Model)
		if cmd != nil {
			t.Fatalf("typing %q in diff search produced an unexpected command", string(r))
		}
	}
	return m
}

// snapshotIDs returns the fixture IDs in chronological order.
func snapshotIDs(t *testing.T, m Model) (older, middle, newer string) {
	t.Helper()
	snaps := m.detailSnapshots()
	if len(snaps) != 3 {
		t.Fatalf("precondition: %d snapshots, want 3", len(snaps))
	}
	return snaps[2].ID, snaps[1].ID, snaps[0].ID
}

func TestToggleDetailMarkFIFO(t *testing.T) {
	m := openDetail(t)
	older, middle, newer := snapshotIDs(t, m)

	// Mark cursor (newest) -> 1 entry.
	m = update(t, m, press("t"))
	if len(m.detailMarks) != 1 || m.detailMarks[0].ID != newer {
		t.Fatalf("first mark = %+v, want one entry with id %q", m.detailMarks, newer)
	}

	// Move cursor and mark middle -> 2 entries (newer, middle), FIFO order.
	m = update(t, m, press("j"))
	m = update(t, m, press("t"))
	if len(m.detailMarks) != 2 ||
		m.detailMarks[0].ID != newer || m.detailMarks[1].ID != middle {
		t.Fatalf("two marks = %+v, want [%q, %q]", m.detailMarks, newer, middle)
	}

	// Move to oldest and mark -> FIFO eviction: oldest mark (newer) drops, list
	// becomes [middle, older].
	m = update(t, m, press("j"))
	m = update(t, m, press("t"))
	if len(m.detailMarks) != 2 ||
		m.detailMarks[0].ID != middle || m.detailMarks[1].ID != older {
		t.Fatalf("after FIFO evict = %+v, want [%q, %q]", m.detailMarks, middle, older)
	}

	// Re-mark a marked row clears in place: cursor still on oldest, `t` removes it.
	m = update(t, m, press("t"))
	if len(m.detailMarks) != 1 || m.detailMarks[0].ID != middle {
		t.Fatalf("re-mark clear = %+v, want [%q]", m.detailMarks, middle)
	}
}

func TestDiffPairResolvesAndSortsChronologically(t *testing.T) {
	m := openDetail(t)
	older, _, newer := snapshotIDs(t, m)

	// 0 marks -> ok=false.
	if _, _, ok := m.diffPair(); ok {
		t.Error("0 marks: diffPair ok=true, want false")
	}

	// Mark the *newest* first, then mark the *oldest* second. The FIFO order is
	// [newest, oldest], but diffPair must sort chronologically so older.Time <=
	// newer.Time regardless of mark order.
	// Cursor starts on newest (newest-first ordering).
	m = update(t, m, press("t")) // mark newest
	m = update(t, m, press("j"))
	m = update(t, m, press("j")) // cursor on oldest
	m = update(t, m, press("t")) // mark oldest
	if len(m.detailMarks) != 2 {
		t.Fatalf("precondition: 2 marks, got %d", len(m.detailMarks))
	}

	o, n, ok := m.diffPair()
	if !ok {
		t.Fatal("2 marks: diffPair ok=false, want true")
	}
	if o.ID != older || n.ID != newer {
		t.Errorf("diffPair = (%q, %q), want (%q, %q) — chronological sort failed",
			o.ID, n.ID, older, newer)
	}
}

func TestDiffPairOneMarkUsesCursor(t *testing.T) {
	m := openDetail(t)
	older, _, newer := snapshotIDs(t, m)

	// Mark the oldest and place the cursor on the newest.
	m = update(t, m, press("j"))
	m = update(t, m, press("j")) // cursor on oldest
	m = update(t, m, press("t")) // mark oldest
	m = update(t, m, press("k"))
	m = update(t, m, press("k")) // cursor on newest

	o, n, ok := m.diffPair()
	if !ok {
		t.Fatal("1 mark + different cursor: ok=false, want true")
	}
	if o.ID != older || n.ID != newer {
		t.Errorf("diffPair = (%q, %q), want (%q, %q)", o.ID, n.ID, older, newer)
	}

	// Move the cursor back onto the marked row: pair is degenerate (same id), so
	// diffPair must report ok=false and the controller surfaces the hint.
	m = update(t, m, press("j"))
	m = update(t, m, press("j")) // cursor back on oldest (the marked row)
	if _, _, ok := m.diffPair(); ok {
		t.Error("1 mark + same cursor: ok=true, want false (degenerate pair)")
	}
}

func TestDKeyWithoutMarksShowsHint(t *testing.T) {
	m := openDetail(t)
	if len(m.detailMarks) != 0 {
		t.Fatalf("precondition: %d marks, want 0", len(m.detailMarks))
	}

	next, cmd := m.Update(press("d"))
	m = next.(Model)
	if m.view != detailView {
		t.Errorf("d with 0 marks should stay on detail, got view=%d", m.view)
	}
	if cmd != nil {
		t.Error("d with 0 marks should emit no command")
	}
	if !strings.Contains(m.statusMsg, "mark snapshots") {
		t.Errorf("statusMsg = %q, want the hint about marking", m.statusMsg)
	}
}

func TestRefreshPrunesStaleDetailMarksBeforeDiff(t *testing.T) {
	a := detailApp(t)
	cap := &stubDiffCapture{}
	a.Restic = stubRestic{diffCap: cap}
	m := newTestModel(t, a)
	m = update(t, m, press("enter"))

	m = update(t, m, press("t")) // mark newest
	if len(m.detailMarks) != 1 || m.detailMarks[0].ID != "id-newest" {
		t.Fatalf("precondition: detailMarks = %+v, want id-newest", m.detailMarks)
	}

	// Refresh repo-a with the marked snapshot removed. applyRefresh should prune
	// the now-stale mark so d cannot pass the vanished ID to restic diff.
	refreshed := app.RepoStatus{
		Name:   "repo-a",
		Status: model.StatusGreen,
		State: model.RepoState{
			Name:          "repo-a",
			RefreshedAt:   testNow,
			LastSnapshot:  testNow.Add(-2 * time.Hour),
			SnapshotCount: 2,
			Snapshots: []model.Snapshot{
				{ID: "id-oldest", ShortID: "s1", Time: testNow.Add(-3 * time.Hour), Hostname: "homeserver"},
				{ID: "id-middle", ShortID: "s2", Time: testNow.Add(-2 * time.Hour), Hostname: "homeserver"},
			},
		},
	}
	m = update(t, m, repoRefreshedMsg{name: "repo-a", row: refreshed})
	if len(m.detailMarks) != 0 {
		t.Fatalf("stale marks after refresh = %+v, want none", m.detailMarks)
	}

	next, cmd := m.Update(press("d"))
	m = next.(Model)
	if cmd != nil {
		t.Fatal("d after stale mark prune should not start a diff")
	}
	if m.view != detailView {
		t.Fatalf("view = %d, want detailView", m.view)
	}
	calls, _, _ := cap.snapshot()
	if calls != 0 {
		t.Fatalf("StreamDiff calls = %d, want 0", calls)
	}
}

func TestDOpensDiffViewWithTwoMarks(t *testing.T) {
	a := detailApp(t)
	cap := &stubDiffCapture{}
	a.Restic = stubRestic{
		snaps: []model.Snapshot{{Hostname: "h"}},
		diffEntries: []model.DiffEntry{
			{Path: "/etc/passwd", Modifier: "M", Type: model.ChangeModified, Kinds: model.KindModified},
			{Path: "/var/log/syslog", Modifier: "+", Type: model.ChangeAdded, Kinds: model.KindAdded},
		},
		diffCap: cap,
	}
	m := newTestModel(t, a)
	m = update(t, m, press("enter"))
	if m.view != detailView {
		t.Fatalf("precondition: view=%d, want detailView", m.view)
	}
	older, _, newer := snapshotIDs(t, m)

	// Mark newest, then oldest (deliberate reverse chronological order to prove
	// the sort happens before the restic call).
	m = update(t, m, press("t"))
	m = update(t, m, press("j"))
	m = update(t, m, press("j"))
	m = update(t, m, press("t"))

	next, cmd := m.Update(press("d"))
	m = next.(Model)
	if m.view != snapshotDiffView {
		t.Fatalf("d with 2 marks: view=%d, want snapshotDiffView", m.view)
	}
	m = drivePastDiff(t, m, cmd)

	if m.diffLoading() {
		t.Error("after the terminal msg lands, loading should be false")
	}
	if len(m.diffEntries) != 2 {
		t.Errorf("diffEntries = %d, want 2", len(m.diffEntries))
	}
	// The root aggregate counts both: 1 added + 1 modified.
	if m.diffStats.Added != 1 || m.diffStats.Modified != 1 {
		t.Errorf("root aggregate Added=%d Modified=%d, want 1/1", m.diffStats.Added, m.diffStats.Modified)
	}
	// The restic call must have received older first, newer second.
	calls, gotOlder, gotNewer := cap.snapshot()
	if calls != 1 {
		t.Errorf("StreamDiff calls = %d, want 1", calls)
	}
	if gotOlder != older || gotNewer != newer {
		t.Errorf("StreamDiff args = (%q, %q), want (%q, %q) — chronological sort missing",
			gotOlder, gotNewer, older, newer)
	}
}

func TestDOpensDiffViewSurfacesParseErrors(t *testing.T) {
	a := detailApp(t)
	a.Restic = stubRestic{
		snaps: []model.Snapshot{{Hostname: "h"}},
		diffEntries: []model.DiffEntry{
			{Path: "/etc/passwd", Modifier: "M", Type: model.ChangeModified, Kinds: model.KindModified},
		},
		diffParseErrors: 1,
	}
	m := newTestModel(t, a)
	m = update(t, m, press("enter"))
	m = update(t, m, press("t"))
	m = update(t, m, press("j"))
	m = update(t, m, press("t"))
	next, cmd := m.Update(press("d"))
	m = next.(Model)
	m = drivePastDiff(t, m, cmd)

	if m.diffParseErrs != 1 {
		t.Fatalf("diffParseErrs = %d, want 1", m.diffParseErrs)
	}
	if got := m.diffSummaryLine(); !strings.Contains(got, "1 malformed line ignored") {
		t.Errorf("summary = %q, want parse-error notice", got)
	}
}

// A mid-stream failure keeps parsed entries and a persistent warning; a
// transient status message could leave an incomplete tree looking complete.
func TestSnapshotDiffFirstTimeErrorWithPartialEntriesRetainsThem(t *testing.T) {
	a := detailApp(t)
	a.Restic = stubRestic{
		snaps: []model.Snapshot{{Hostname: "h"}},
		diffEntries: []model.DiffEntry{
			{Path: "/etc/passwd", Modifier: "M", Type: model.ChangeModified, Kinds: model.KindModified},
			{Path: "/var/log/syslog", Modifier: "+", Type: model.ChangeAdded, Kinds: model.KindAdded},
		},
		diffErr: errors.New("scanner: token too long"),
	}
	m := newTestModel(t, a)
	m = update(t, m, press("enter"))
	m = update(t, m, press("t"))
	m = update(t, m, press("j"))
	m = update(t, m, press("t"))
	next, cmd := m.Update(press("d"))
	m = next.(Model)
	m = drivePastDiff(t, m, cmd)

	if m.view != snapshotDiffView {
		t.Fatalf("partial-entry error: view = %d, want snapshotDiffView (no bail-to-detail)", m.view)
	}
	if m.diffLoading() {
		t.Error("partial-entry error: should clear loading")
	}
	if len(m.diffEntries) != 2 {
		t.Errorf("partial-entry error: diffEntries = %d, want 2 (entries retained)", len(m.diffEntries))
	}
	if m.diffStats.Added != 1 || m.diffStats.Modified != 1 {
		t.Errorf("partial-entry error: stats = %+v, want Added=1 Modified=1", m.diffStats)
	}
	if !strings.Contains(m.diffErr, "partial: scanner: token too long") {
		t.Errorf("partial-entry error: diffErr = %q, want sticky warning prefixed with partial:", m.diffErr)
	}
	// Keep both the warning and actionable statistics visible.
	summary := m.diffSummaryLine()
	if !strings.Contains(summary, "partial:") {
		t.Errorf("summary line missing partial warning: %q", summary)
	}
	if !strings.Contains(summary, "+1") || !strings.Contains(summary, "M1") {
		t.Errorf("summary line lost the stats next to the warning: %q", summary)
	}

	// Assert through View because search replaces the normal summary; the warning
	// must remain visible during navigation, filtering, and search.
	for _, k := range []string{"j", "k", "+", "/"} {
		next, _ := m.Update(press(k))
		m = next.(Model)
		if !strings.Contains(m.diffErr, "partial:") {
			t.Errorf("after pressing %q the partial warning was lost (diffErr = %q)", k, m.diffErr)
		}
		if view := m.View().Content; !strings.Contains(view, "partial:") {
			t.Errorf("after pressing %q the rendered view dropped the partial warning\n---\n%s", k, view)
		}
	}
}

// A failed first diff with no entries returns to detail with a status error.
func TestSnapshotDiffFirstTimeErrorWithNoEntriesBailsToDetail(t *testing.T) {
	a := detailApp(t)
	a.Restic = stubRestic{
		snaps:   []model.Snapshot{{Hostname: "h"}},
		diffErr: errors.New("wrong password"),
	}
	m := newTestModel(t, a)
	m = update(t, m, press("enter"))
	m = update(t, m, press("t"))
	m = update(t, m, press("j"))
	m = update(t, m, press("t"))
	next, cmd := m.Update(press("d"))
	m = next.(Model)
	m = drivePastDiff(t, m, cmd)

	if m.view != detailView {
		t.Fatalf("empty-result error: view = %d, want detailView (must bail)", m.view)
	}
	if len(m.diffEntries) != 0 {
		t.Errorf("empty-result error: diffEntries = %d, want 0 (cleared)", len(m.diffEntries))
	}
	if !strings.Contains(m.statusMsg, "diff: wrong password") {
		t.Errorf("empty-result error: statusMsg = %q, want failure notice", m.statusMsg)
	}
}

func TestSnapshotDiffSummaryShowsDirectionLegend(t *testing.T) {
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
	m = drivePastDiff(t, m, cmd)

	got := m.diffSummaryLine()
	for _, want := range []string{"+ in second snapshot", "- in first snapshot"} {
		if !strings.Contains(got, want) {
			t.Errorf("summary = %q, want %q", got, want)
		}
	}
}

// Filter key hints live in one footer chip; the active mask appends to the
// state-only summary without shifting existing fields.
func TestSnapshotDiffFilterHintInFooterAndStableSummary(t *testing.T) {
	a := detailApp(t)
	a.Restic = stubRestic{
		snaps: []model.Snapshot{{Hostname: "h"}},
		diffEntries: []model.DiffEntry{
			{Path: "/etc/passwd", Modifier: "M", Type: model.ChangeModified, Kinds: model.KindModified},
			{Path: "/etc/new", Modifier: "+", Type: model.ChangeAdded, Kinds: model.KindAdded},
		},
	}
	m := newTestModel(t, a)
	m = update(t, m, press("enter"))
	m = update(t, m, press("t"))
	m = update(t, m, press("j"))
	m = update(t, m, press("t"))
	next, cmd := m.Update(press("d"))
	m = next.(Model)
	m = drivePastDiff(t, m, cmd)

	footer := stripANSI(m.footerView())
	if !strings.Contains(footer, "+-MUTb filters") {
		t.Errorf("diff footer should advertise the filter-toggle group\n---\n%s", footer)
	}
	if strings.Contains(footer, "+ added") {
		t.Errorf("diff footer must not advertise a single toggle out of six\n---\n%s", footer)
	}

	// The grouped chip must leave the trailing q affordance visible at 100 columns.
	m = update(t, m, tea.WindowSizeMsg{Width: 100, Height: 40})
	narrow := stripANSI(m.footerView())
	for _, want := range []string{"+-MUTb filters", "q back"} {
		if !strings.Contains(narrow, want) {
			t.Errorf("80-column diff footer missing %q\n---\n%s", want, narrow)
		}
	}
	m = update(t, m, tea.WindowSizeMsg{Width: 192, Height: 51})

	before := stripANSI(m.diffSummaryLine())
	if strings.Contains(before, "toggle") {
		t.Errorf("summary line should carry no key hint, got %q", before)
	}
	if strings.Contains(before, "filter:") {
		t.Errorf("the all-on default mask should not render, got %q", before)
	}

	m = update(t, m, press("+")) // exclude added entries
	after := stripANSI(m.diffSummaryLine())
	if !strings.HasPrefix(after, before) {
		t.Errorf("toggling a filter must append the mask, not shift the line\nbefore: %q\nafter:  %q", before, after)
	}
	if !strings.Contains(after, "filter: -MUT?") {
		t.Errorf("summary should end with the active mask, got %q", after)
	}
}

func TestSnapshotDiffSwapRerunsReversedPair(t *testing.T) {
	a := detailApp(t)
	cap := &stubDiffCapture{}
	a.Restic = stubRestic{
		snaps: []model.Snapshot{{Hostname: "h"}},
		diffEntries: []model.DiffEntry{
			{Path: "/etc/group", Modifier: "+", Type: model.ChangeAdded, Kinds: model.KindAdded},
			{Path: "/etc/passwd", Modifier: "M", Type: model.ChangeModified, Kinds: model.KindModified},
		},
		diffCap: cap,
	}
	m := newTestModel(t, a)
	m = update(t, m, press("enter"))
	older, _, newer := snapshotIDs(t, m)

	m = update(t, m, press("t")) // mark newest
	m = update(t, m, press("j"))
	m = update(t, m, press("j"))
	m = update(t, m, press("t")) // mark oldest
	next, cmd := m.Update(press("d"))
	m = next.(Model)
	m = drivePastDiff(t, m, cmd)

	calls, gotOlder, gotNewer := cap.snapshot()
	if calls != 1 || gotOlder != older || gotNewer != newer {
		t.Fatalf("initial StreamDiff call = %d (%q, %q), want 1 (%q, %q)",
			calls, gotOlder, gotNewer, older, newer)
	}
	m = update(t, m, press("enter")) // descend from / into /etc
	if m.diffDir != "/etc" {
		t.Fatalf("precondition: diffDir = %q, want /etc", m.diffDir)
	}
	m = update(t, m, press("j")) // select /etc/passwd, not the first row.
	if r := m.selectedDiffRow(); r == nil || r.Path != "/etc/passwd" {
		t.Fatalf("precondition: selected row = %+v, want /etc/passwd", r)
	}

	next, cmd = m.Update(press("x"))
	m = next.(Model)
	if cmd == nil {
		t.Fatal("x should start a swapped diff command")
	}
	if !m.diffLoading() {
		t.Error("x should put the diff view back into loading state")
	}
	if m.diffOlder.ID != older || m.diffNewer.ID != newer {
		t.Errorf("loading swap model pair = (%q, %q), want still-loaded (%q, %q)",
			m.diffOlder.ID, m.diffNewer.ID, older, newer)
	}
	if m.diffDir != "/etc" || len(m.diffRows) != 2 {
		t.Errorf("swap should preserve the current loaded rows until the rerun succeeds, dir=%q rows=%d",
			m.diffDir, len(m.diffRows))
	}

	m = drivePastDiff(t, m, cmd)
	calls, gotOlder, gotNewer = cap.snapshot()
	if calls != 2 || gotOlder != newer || gotNewer != older {
		t.Errorf("swapped StreamDiff call = %d (%q, %q), want 2 (%q, %q)",
			calls, gotOlder, gotNewer, newer, older)
	}
	if m.diffOlder.ID != newer || m.diffNewer.ID != older {
		t.Errorf("landed swap model pair = (%q, %q), want (%q, %q)",
			m.diffOlder.ID, m.diffNewer.ID, newer, older)
	}
	if m.diffDir != "/etc" {
		t.Errorf("after swapped diff lands, diffDir = %q, want /etc", m.diffDir)
	}
	if len(m.diffRows) != 2 {
		t.Fatalf("after swapped diff lands, /etc rows = %+v, want 2 rows", m.diffRows)
	}
	if r := m.selectedDiffRow(); r == nil || r.Path != "/etc/passwd" {
		t.Errorf("after swapped diff lands, selected row = %+v, want /etc/passwd", r)
	}
}

func TestSnapshotDiffSwapErrorKeepsPreviousDiff(t *testing.T) {
	a := detailApp(t)
	a.Restic = stubRestic{
		snaps: []model.Snapshot{{Hostname: "h"}},
		diffEntries: []model.DiffEntry{
			{Path: "/etc/group", Modifier: "+", Type: model.ChangeAdded, Kinds: model.KindAdded},
			{Path: "/etc/passwd", Modifier: "M", Type: model.ChangeModified, Kinds: model.KindModified},
		},
	}
	m := newTestModel(t, a)
	m = update(t, m, press("enter"))
	older, _, newer := snapshotIDs(t, m)
	m = update(t, m, press("t"))
	m = update(t, m, press("j"))
	m = update(t, m, press("j"))
	m = update(t, m, press("t"))
	next, cmd := m.Update(press("d"))
	m = next.(Model)
	m = drivePastDiff(t, m, cmd)

	m = update(t, m, press("enter")) // descend into /etc
	m = update(t, m, press("j"))     // select /etc/passwd
	if r := m.selectedDiffRow(); r == nil || r.Path != "/etc/passwd" {
		t.Fatalf("precondition: selected row = %+v, want /etc/passwd", r)
	}

	a.Restic = stubRestic{diffErr: errors.New("context deadline exceeded")}
	next, cmd = m.Update(press("x"))
	m = next.(Model)
	if !m.diffLoading() {
		t.Fatal("x should put the diff view into loading state")
	}
	m = drivePastDiff(t, m, cmd)

	if m.view != snapshotDiffView {
		t.Fatalf("failed swap should stay in diff view, got %d", m.view)
	}
	if m.diffLoading() {
		t.Fatal("failed swap should clear loading")
	}
	if m.diffOlder.ID != older || m.diffNewer.ID != newer {
		t.Fatalf("failed swap pair = (%q, %q), want original (%q, %q)",
			m.diffOlder.ID, m.diffNewer.ID, older, newer)
	}
	if m.diffDir != "/etc" {
		t.Fatalf("failed swap dir = %q, want /etc", m.diffDir)
	}
	if len(m.diffRows) != 2 {
		t.Fatalf("failed swap rows = %+v, want previous /etc rows", m.diffRows)
	}
	if r := m.selectedDiffRow(); r == nil || r.Path != "/etc/passwd" {
		t.Fatalf("failed swap selected row = %+v, want /etc/passwd", r)
	}
	if !strings.Contains(m.statusMsg, "diff: context deadline exceeded") {
		t.Errorf("statusMsg = %q, want failed diff notice", m.statusMsg)
	}
}

func TestSnapshotDiffFooterHelpIncludesSwap(t *testing.T) {
	short := viewHelp{keys: defaultKeys(), view: snapshotDiffView}.ShortHelp()
	for _, b := range short {
		if h := b.Help(); h.Key == "x" && h.Desc == "swap" {
			return
		}
	}
	t.Fatalf("snapshot diff footer help should include x swap, got %+v", short)
}

func TestSnapshotDiffSearchTypeShowsChangedPaths(t *testing.T) {
	a := detailApp(t)
	a.Restic = stubRestic{
		snaps: []model.Snapshot{{Hostname: "h"}},
		diffEntries: []model.DiffEntry{
			{Path: "/etc/passwd", Modifier: "M", Type: model.ChangeModified, Kinds: model.KindModified},
			{Path: "/home/report.txt", Modifier: "+", Type: model.ChangeAdded, Kinds: model.KindAdded},
			{Path: "/var/log/syslog", Modifier: "-", Type: model.ChangeRemoved, Kinds: model.KindRemoved},
		},
	}
	m := newTestModel(t, a)
	m = update(t, m, press("enter"))
	m = update(t, m, press("t"))
	m = update(t, m, press("j"))
	m = update(t, m, press("t"))
	next, cmd := m.Update(press("d"))
	m = next.(Model)
	m = drivePastDiff(t, m, cmd)

	m = openDiffSearch(t, m)
	m = typeDiffSearch(t, m, "pass")

	if m.diffSearchTotal != 1 {
		t.Fatalf("diffSearchTotal = %d, want 1", m.diffSearchTotal)
	}
	if len(m.diffSearchRows) != 1 || m.diffSearchRows[0].Path != "/etc/passwd" {
		t.Fatalf("diff search rows = %+v, want /etc/passwd", m.diffSearchRows)
	}
	view := m.View().Content
	if !strings.Contains(view, "/etc/passwd") {
		t.Errorf("diff search view should render full changed path\n---\n%s", view)
	}
	if strings.Contains(view, "/var/log/syslog") {
		t.Errorf("non-matching changed path leaked into search view\n---\n%s", view)
	}
}

func TestSnapshotDiffSearchEnterJumpsAndEscReturns(t *testing.T) {
	a := detailApp(t)
	a.Restic = stubRestic{
		snaps: []model.Snapshot{{Hostname: "h"}},
		diffEntries: []model.DiffEntry{
			{Path: "/etc/passwd", Modifier: "M", Type: model.ChangeModified, Kinds: model.KindModified},
			{Path: "/var/log/syslog", Modifier: "-", Type: model.ChangeRemoved, Kinds: model.KindRemoved},
		},
	}
	m := newTestModel(t, a)
	m = update(t, m, press("enter"))
	m = update(t, m, press("t"))
	m = update(t, m, press("j"))
	m = update(t, m, press("t"))
	next, cmd := m.Update(press("d"))
	m = next.(Model)
	m = drivePastDiff(t, m, cmd)

	m = update(t, m, press("j")) // origin cursor on /var, not /etc.
	if r := m.selectedDiffRow(); r == nil || r.Path != "/var" {
		t.Fatalf("precondition: selected root row = %+v, want /var", r)
	}
	m = openDiffSearch(t, m)
	m = typeDiffSearch(t, m, "passwd")

	m = update(t, m, press("enter"))
	if m.diffSearching {
		t.Error("enter on a diff search match should close the search input")
	}
	if !m.diffSearchJumped {
		t.Error("enter on a diff search match should arm esc to return to the origin")
	}
	if m.diffDir != "/etc" {
		t.Fatalf("enter on /etc/passwd should jump to /etc, got %q", m.diffDir)
	}
	if r := m.selectedDiffRow(); r == nil || r.Path != "/etc/passwd" {
		t.Fatalf("selected row after jump = %+v, want /etc/passwd", r)
	}

	m = update(t, m, press("esc"))
	if m.diffSearchJumped || m.diffSearching {
		t.Fatalf("esc after a diff search jump should clear search state: searching=%v jumped=%v",
			m.diffSearching, m.diffSearchJumped)
	}
	if m.diffDir != model.DiffRoot {
		t.Fatalf("esc after jump should return to root, got %q", m.diffDir)
	}
	if r := m.selectedDiffRow(); r == nil || r.Path != "/var" {
		t.Errorf("esc after jump should restore the origin cursor, got %+v want /var", r)
	}
}

func TestDiffFilterAfterSearchJumpClearsOriginBookmark(t *testing.T) {
	a := detailApp(t)
	a.Restic = stubRestic{
		snaps: []model.Snapshot{{Hostname: "h"}},
		diffEntries: []model.DiffEntry{
			{Path: "/etc/passwd", Modifier: "M", Type: model.ChangeModified, Kinds: model.KindModified},
			{Path: "/var/log/syslog", Modifier: "-", Type: model.ChangeRemoved, Kinds: model.KindRemoved},
		},
	}
	m := newTestModel(t, a)
	m = update(t, m, press("enter"))
	m = update(t, m, press("t"))
	m = update(t, m, press("j"))
	m = update(t, m, press("t"))
	next, cmd := m.Update(press("d"))
	m = next.(Model)
	m = drivePastDiff(t, m, cmd)

	m = update(t, m, press("j")) // origin cursor on /var
	m = openDiffSearch(t, m)
	m = typeDiffSearch(t, m, "passwd")
	m = update(t, m, press("enter"))
	if !m.diffSearchJumped || m.diffDir != "/etc" {
		t.Fatalf("precondition: jump should land in /etc with jump state, dir=%q jumped=%v",
			m.diffDir, m.diffSearchJumped)
	}

	m = update(t, m, press("M"))
	if m.diffSearchJumped || m.diffSearchOrigin != "" {
		t.Fatalf("filter toggle after jump should clear origin bookmark: jumped=%v origin=%q",
			m.diffSearchJumped, m.diffSearchOrigin)
	}
	if m.diffDir != "/etc" {
		t.Fatalf("filter toggle should stay in current dir, got %q", m.diffDir)
	}

	m = update(t, m, press("q"))
	if m.view != detailView {
		t.Fatalf("q after filter-cleared jump should leave diff, got view=%d", m.view)
	}
}

func TestSnapshotDiffKeyClearsStaleStatusMsg(t *testing.T) {
	a := detailApp(t)
	a.Restic = stubRestic{
		snaps: []model.Snapshot{{Hostname: "h"}},
		diffEntries: []model.DiffEntry{
			{Path: "/etc/passwd", Modifier: "M", Type: model.ChangeModified, Kinds: model.KindModified},
			{Path: "/var/log/syslog", Modifier: "-", Type: model.ChangeRemoved, Kinds: model.KindRemoved},
		},
	}
	m := newTestModel(t, a)
	m = update(t, m, press("enter"))
	m = update(t, m, press("t"))
	m = update(t, m, press("j"))
	m = update(t, m, press("t"))
	next, cmd := m.Update(press("d"))
	m = next.(Model)
	m = drivePastDiff(t, m, cmd)

	m.statusMsg = "cache write failed: disk full"
	m = update(t, m, press("j"))
	if m.statusMsg != "" {
		t.Errorf("diff navigation should clear stale statusMsg, got %q", m.statusMsg)
	}
}

func TestSnapshotDiffSwapClearsSearchJumpState(t *testing.T) {
	a := detailApp(t)
	a.Restic = stubRestic{
		snaps: []model.Snapshot{{Hostname: "h"}},
		diffEntries: []model.DiffEntry{
			{Path: "/etc/passwd", Modifier: "M", Type: model.ChangeModified, Kinds: model.KindModified},
			{Path: "/var/log/syslog", Modifier: "-", Type: model.ChangeRemoved, Kinds: model.KindRemoved},
		},
	}
	m := newTestModel(t, a)
	m = update(t, m, press("enter"))
	older, _, newer := snapshotIDs(t, m)
	m = update(t, m, press("t"))
	m = update(t, m, press("j"))
	m = update(t, m, press("j"))
	m = update(t, m, press("t"))
	next, cmd := m.Update(press("d"))
	m = next.(Model)
	m = drivePastDiff(t, m, cmd)

	m = update(t, m, press("j")) // origin cursor on /var before search.
	m = openDiffSearch(t, m)
	m = typeDiffSearch(t, m, "passwd")
	m = update(t, m, press("enter"))
	if !m.diffSearchJumped || m.diffDir != "/etc" {
		t.Fatalf("precondition: search jump should land in /etc with jump state, dir=%q jumped=%v",
			m.diffDir, m.diffSearchJumped)
	}

	next, cmd = m.Update(press("x"))
	m = next.(Model)
	if cmd == nil {
		t.Fatal("x after search jump should start a swapped diff")
	}
	if m.diffSearchJumped || m.diffSearching || m.diffSearchOrigin != "" {
		t.Fatalf("x should clear stale search-jump state: searching=%v jumped=%v origin=%q",
			m.diffSearching, m.diffSearchJumped, m.diffSearchOrigin)
	}
	if m.diffOlder.ID != older || m.diffNewer.ID != newer {
		t.Errorf("loading swap model pair = (%q, %q), want still-loaded (%q, %q)",
			m.diffOlder.ID, m.diffNewer.ID, older, newer)
	}

	m = drivePastDiff(t, m, cmd)
	if m.diffOlder.ID != newer || m.diffNewer.ID != older {
		t.Errorf("landed swap model pair = (%q, %q), want (%q, %q)",
			m.diffOlder.ID, m.diffNewer.ID, newer, older)
	}
	if m.diffDir != "/etc" {
		t.Fatalf("swapped result should preserve the current dir, got %q", m.diffDir)
	}
	if r := m.selectedDiffRow(); r == nil || r.Path != "/etc/passwd" {
		t.Fatalf("swapped result should preserve selected row, got %+v", r)
	}

	m = update(t, m, press("esc"))
	if m.view != detailView {
		t.Errorf("esc after swapped search jump should leave diff, not restore stale origin; view=%d", m.view)
	}
}

func TestSnapshotDiffBackReturnsToDetailKeepingMarks(t *testing.T) {
	a := detailApp(t)
	a.Restic = stubRestic{snaps: []model.Snapshot{{Hostname: "h"}}}
	m := newTestModel(t, a)
	m = update(t, m, press("enter"))

	// Two marks -> d -> diff view.
	m = update(t, m, press("t"))
	m = update(t, m, press("j"))
	m = update(t, m, press("t"))
	next, cmd := m.Update(press("d"))
	m = next.(Model)
	m = drivePastDiff(t, m, cmd)
	if m.view != snapshotDiffView {
		t.Fatalf("precondition: view=%d, want snapshotDiffView", m.view)
	}

	// q from diff returns to detail with marks intact (the diff view is a
	// sub-context of detail).
	m = update(t, m, press("q"))
	if m.view != detailView {
		t.Errorf("q from diff: view=%d, want detailView", m.view)
	}
	if len(m.detailMarks) != 2 {
		t.Errorf("marks after diff→detail = %d, want 2 (sub-context survives)", len(m.detailMarks))
	}
	// And the diff fields are cleared.
	if m.diffRepo != "" || m.diffTree.Children != nil || m.diffEntries != nil {
		t.Errorf("clearSnapshotDiff did not zero diff fields: repo=%q tree=%+v entries=%v",
			m.diffRepo, m.diffTree, m.diffEntries)
	}
}

func TestGoBackFromDetailClearsMarks(t *testing.T) {
	m := openDetail(t)
	// Mark something, then go back to list.
	m = update(t, m, press("t"))
	if len(m.detailMarks) != 1 {
		t.Fatalf("precondition: 1 mark, got %d", len(m.detailMarks))
	}
	m = update(t, m, press("q"))
	if m.view != listView {
		t.Fatalf("q from detail: view=%d, want listView", m.view)
	}
	if len(m.detailMarks) != 0 {
		t.Errorf("marks after detail→list = %d, want 0 (goBack must clear)", len(m.detailMarks))
	}
}

func TestDiffFilterToggleHidesAndRestores(t *testing.T) {
	a := detailApp(t)
	// Two explicit entries directly under the diff root so the root listing has
	// two file rows (a row's Kinds bitmask drives the filter predicate; dir
	// rows would compare via their Aggregate instead, masking the filter
	// effect being exercised here).
	a.Restic = stubRestic{
		snaps: []model.Snapshot{{Hostname: "h"}},
		diffEntries: []model.DiffEntry{
			{Path: "/passwd", Modifier: "M", Type: model.ChangeModified, Kinds: model.KindModified},
			{Path: "/hosts", Modifier: "+", Type: model.ChangeAdded, Kinds: model.KindAdded},
		},
	}
	m := newTestModel(t, a)
	m = update(t, m, press("enter"))
	m = update(t, m, press("t"))
	m = update(t, m, press("j"))
	m = update(t, m, press("t"))
	next, cmd := m.Update(press("d"))
	m = next.(Model)
	m = drivePastDiff(t, m, cmd)

	// Two rows at the diff root, then `+` toggle drops the added entry but keeps
	// the modified one.
	if len(m.diffRows) != 2 {
		t.Fatalf("root listing = %d rows, want 2", len(m.diffRows))
	}
	m = update(t, m, press("+"))
	if m.diffFilters&model.KindAdded != 0 {
		t.Errorf("after + toggle, filter mask still has KindAdded: %b", m.diffFilters)
	}
	for _, r := range m.diffRows {
		if r.Kinds == model.KindAdded {
			t.Errorf("added-only row %q still visible after toggling Added off", r.Path)
		}
	}
	// Toggling `+` again restores the row.
	m = update(t, m, press("+"))
	if m.diffFilters != model.AllDiffKinds {
		t.Errorf("after second + toggle, filter mask = %b, want all bits set (%b)", m.diffFilters, model.AllDiffKinds)
	}
	if len(m.diffRows) != 2 {
		t.Errorf("rows after restore = %d, want 2", len(m.diffRows))
	}
}

func TestSnapshotDiffSearchDedupesDuplicatePaths(t *testing.T) {
	// Duplicate restic records must produce one search result and count.
	a := detailApp(t)
	a.Restic = stubRestic{
		snaps: []model.Snapshot{{Hostname: "h"}},
		diffEntries: []model.DiffEntry{
			{Path: "/etc/passwd", Modifier: "M", Type: model.ChangeModified, Kinds: model.KindModified},
			{Path: "/etc/passwd", Modifier: "M", Type: model.ChangeModified, Kinds: model.KindModified},
			{Path: "/var/log/syslog", Modifier: "-", Type: model.ChangeRemoved, Kinds: model.KindRemoved},
		},
	}
	m := newTestModel(t, a)
	m = update(t, m, press("enter"))
	m = update(t, m, press("t"))
	m = update(t, m, press("j"))
	m = update(t, m, press("t"))
	next, cmd := m.Update(press("d"))
	m = next.(Model)
	m = drivePastDiff(t, m, cmd)

	m = openDiffSearch(t, m)
	m = typeDiffSearch(t, m, "pass")

	if m.diffSearchTotal != 1 {
		t.Errorf("diffSearchTotal = %d, want 1 (duplicate /etc/passwd must count once)", m.diffSearchTotal)
	}
	if len(m.diffSearchRows) != 1 {
		t.Fatalf("diffSearchRows = %d, want 1, got %+v", len(m.diffSearchRows), m.diffSearchRows)
	}
	if m.diffSearchRows[0].Path != "/etc/passwd" {
		t.Errorf("search row path = %q, want /etc/passwd", m.diffSearchRows[0].Path)
	}
}

func TestSnapshotDiffSearchMergesMixedModifiersForSamePath(t *testing.T) {
	// Mixed modifiers must merge before ranking, independent of order and filter.
	a := detailApp(t)
	a.Restic = stubRestic{
		snaps: []model.Snapshot{{Hostname: "h"}},
		diffEntries: []model.DiffEntry{
			{Path: "/etc/passwd", Modifier: "M", Type: model.ChangeModified, Kinds: model.KindModified},
			{Path: "/etc/passwd", Modifier: "U", Type: model.ChangeMetadataOnly, Kinds: model.KindMetadata},
		},
	}
	m := newTestModel(t, a)
	m = update(t, m, press("enter"))
	m = update(t, m, press("t"))
	m = update(t, m, press("j"))
	m = update(t, m, press("t"))
	next, cmd := m.Update(press("d"))
	m = next.(Model)
	m = drivePastDiff(t, m, cmd)

	m = openDiffSearch(t, m)
	m = typeDiffSearch(t, m, "pass")

	if m.diffSearchTotal != 1 || len(m.diffSearchRows) != 1 {
		t.Fatalf("rows=%d total=%d, want 1/1", len(m.diffSearchRows), m.diffSearchTotal)
	}
	row := m.diffSearchRows[0]
	if row.Kinds != model.KindModified|model.KindMetadata {
		t.Errorf("merged Kinds = %b, want %b (M|U)",
			row.Kinds, model.KindModified|model.KindMetadata)
	}
	if row.Type != model.ChangeModified {
		t.Errorf("re-derived Type = %v, want ChangeModified (M wins over U)", row.Type)
	}
	if row.Modifier != "MU" {
		t.Errorf("merged Modifier = %q, want %q", row.Modifier, "MU")
	}

	// Reverse order must still render the canonical merged marker.
	aRev := detailApp(t)
	aRev.Restic = stubRestic{
		snaps: []model.Snapshot{{Hostname: "h"}},
		diffEntries: []model.DiffEntry{
			{Path: "/etc/passwd", Modifier: "U", Type: model.ChangeMetadataOnly, Kinds: model.KindMetadata},
			{Path: "/etc/passwd", Modifier: "M", Type: model.ChangeModified, Kinds: model.KindModified},
		},
	}
	mRev := newTestModel(t, aRev)
	mRev = update(t, mRev, press("enter"))
	mRev = update(t, mRev, press("t"))
	mRev = update(t, mRev, press("j"))
	mRev = update(t, mRev, press("t"))
	nextRev, cmdRev := mRev.Update(press("d"))
	mRev = nextRev.(Model)
	mRev = drivePastDiff(t, mRev, cmdRev)
	mRev = openDiffSearch(t, mRev)
	mRev = typeDiffSearch(t, mRev, "pass")
	if len(mRev.diffSearchRows) != 1 {
		t.Fatalf("reverse-order: rows = %d, want 1", len(mRev.diffSearchRows))
	}
	if got := mRev.diffSearchRows[0].Modifier; got != "MU" {
		t.Errorf("reverse-order Modifier = %q, want %q (canonical bit order)", got, "MU")
	}

	// A U-only filter must retain the row because filtering follows the merge.
	m = update(t, m, press("esc"))
	m = update(t, m, press("+"))
	m = update(t, m, press("-"))
	m = update(t, m, press("M"))
	m = update(t, m, press("T"))
	m = update(t, m, press("b"))
	if m.diffFilters != model.KindMetadata {
		t.Fatalf("precondition: filters = %b, want U-only (%b)", m.diffFilters, model.KindMetadata)
	}
	m = openDiffSearch(t, m)
	m = typeDiffSearch(t, m, "pass")
	if m.diffSearchTotal != 1 || len(m.diffSearchRows) != 1 {
		t.Fatalf("U-only filter: rows=%d total=%d, want 1/1", len(m.diffSearchRows), m.diffSearchTotal)
	}
	if m.diffSearchRows[0].Kinds != model.KindModified|model.KindMetadata {
		t.Errorf("U-filtered row Kinds = %b, want still M|U (merge happens before filter)",
			m.diffSearchRows[0].Kinds)
	}
}

func TestCancelDiffSearchFallbackIgnoresStaleOriginCursor(t *testing.T) {
	// When a missing origin falls back to a parent, its cursor belongs to the old
	// list and must not override the fallback directory's restoration.
	a := detailApp(t)
	a.Restic = stubRestic{
		snaps: []model.Snapshot{{Hostname: "h"}},
		diffEntries: []model.DiffEntry{
			{Path: "/etc/passwd", Modifier: "+", Type: model.ChangeAdded, Kinds: model.KindAdded},
			{Path: "/home/report.txt", Modifier: "+", Type: model.ChangeAdded, Kinds: model.KindAdded},
			{Path: "/var/cache/index", Modifier: "+", Type: model.ChangeAdded, Kinds: model.KindAdded},
			{Path: "/var/log/syslog", Modifier: "+", Type: model.ChangeAdded, Kinds: model.KindAdded},
		},
	}
	m := newTestModel(t, a)
	m = update(t, m, press("enter"))
	m = update(t, m, press("t"))
	m = update(t, m, press("j"))
	m = update(t, m, press("t"))
	next, cmd := m.Update(press("d"))
	m = next.(Model)
	m = drivePastDiff(t, m, cmd)

	// Root listing is /etc, /home, /var (3 dir rows). Move to /var at index 2
	// so descending records diffCache["/"] = 2.
	m = update(t, m, press("j"))
	m = update(t, m, press("j"))
	if r := m.selectedDiffRow(); r == nil || r.Path != "/var" {
		t.Fatalf("precondition: root selected = %+v, want /var", r)
	}
	m = update(t, m, press("enter"))
	if m.diffDir != "/var" {
		t.Fatalf("precondition: diffDir = %q, want /var", m.diffDir)
	}
	// Use a distinct /var cursor so stale restoration is observable at root.
	m = update(t, m, press("j"))
	if r := m.selectedDiffRow(); r == nil || r.Path != "/var/log" {
		t.Fatalf("precondition: /var selected = %+v, want /var/log", r)
	}

	m = openDiffSearch(t, m)
	if m.diffSearchOrigin != "/var" || m.diffSearchOrigCur != 1 {
		t.Fatalf("precondition: origin=%q origCur=%d, want /var/1",
			m.diffSearchOrigin, m.diffSearchOrigCur)
	}

	// Force the missing-origin fallback to root.
	m.diffSearchOrigin = "/nonexistent"

	m = update(t, m, press("esc"))

	if m.diffDir != model.DiffRoot {
		t.Fatalf("cancel fallback should land at root, got %q", m.diffDir)
	}
	// Root's cached cursor must win over the stale origin cursor.
	if m.diffCursor != 2 {
		t.Errorf("fallback cursor = %d, want 2 (diffCache restoration, not stale originCursor=1)",
			m.diffCursor)
	}
	if r := m.selectedDiffRow(); r == nil || r.Path != "/var" {
		t.Errorf("selected after fallback = %+v, want /var (cache restoration)", r)
	}
}

func TestExistingDiffDirTreatsDemotedSyntheticAncestorAsFile(t *testing.T) {
	tree := model.BuildDiffTree([]model.DiffEntry{
		{Path: "/foo/bar", Modifier: "+", Type: model.ChangeAdded, Kinds: model.KindAdded},
		{Path: "/foo", Modifier: "T", Type: model.ChangeTypeChanged, Kinds: model.KindTypeChanged},
	})

	if got := existingDiffDir(tree, "/foo"); got != model.DiffRoot {
		t.Fatalf("existingDiffDir(/foo) = %q, want root because /foo is an explicit file", got)
	}
	if got := existingDiffDir(tree, "/foo/bar"); got != model.DiffRoot {
		t.Fatalf("existingDiffDir(/foo/bar) = %q, want root because parent /foo is a file", got)
	}
}

func TestQAfterDiffSearchJumpRestoresOrigin(t *testing.T) {
	// q on the diff view after an accepted search jump must mirror esc: reverse
	// the jump first, leaving the view only on the second q. This is the only
	// way q and esc stay interchangeable as "step back one screen" everywhere.
	a := detailApp(t)
	a.Restic = stubRestic{
		snaps: []model.Snapshot{{Hostname: "h"}},
		diffEntries: []model.DiffEntry{
			{Path: "/etc/passwd", Modifier: "M", Type: model.ChangeModified, Kinds: model.KindModified},
			{Path: "/var/log/syslog", Modifier: "-", Type: model.ChangeRemoved, Kinds: model.KindRemoved},
		},
	}
	m := newTestModel(t, a)
	m = update(t, m, press("enter"))
	m = update(t, m, press("t"))
	m = update(t, m, press("j"))
	m = update(t, m, press("t"))
	next, cmd := m.Update(press("d"))
	m = next.(Model)
	m = drivePastDiff(t, m, cmd)

	m = update(t, m, press("j")) // origin cursor on /var, not /etc.
	if r := m.selectedDiffRow(); r == nil || r.Path != "/var" {
		t.Fatalf("precondition: selected root row = %+v, want /var", r)
	}
	m = openDiffSearch(t, m)
	m = typeDiffSearch(t, m, "passwd")
	m = update(t, m, press("enter"))
	if !m.diffSearchJumped || m.diffDir != "/etc" {
		t.Fatalf("precondition: jump should land in /etc with jump state, dir=%q jumped=%v",
			m.diffDir, m.diffSearchJumped)
	}

	m = update(t, m, press("q"))
	if m.view != snapshotDiffView {
		t.Fatalf("q after diff search jump should stay on diff view, got view=%d", m.view)
	}
	if m.diffSearchJumped || m.diffSearching {
		t.Fatalf("q after jump should clear search state: searching=%v jumped=%v",
			m.diffSearching, m.diffSearchJumped)
	}
	if m.diffDir != model.DiffRoot {
		t.Fatalf("q after jump should return to root, got %q", m.diffDir)
	}
	if r := m.selectedDiffRow(); r == nil || r.Path != "/var" {
		t.Errorf("q after jump should restore the origin cursor, got %+v want /var", r)
	}

	// A second q (no jump state armed) leaves the diff view back to detail,
	// confirming q's normal "step back" semantics resume once the jump is undone.
	m = update(t, m, press("q"))
	if m.view != detailView {
		t.Errorf("second q should leave diff for detail, got view=%d", m.view)
	}
}

// Smoke check that the snapshotDiffView renders without panicking and surfaces
// the (older, newer) pair label and at least one entry's name.
func TestSnapshotDiffViewRenders(t *testing.T) {
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
	m = drivePastDiff(t, m, cmd)

	view := m.View().Content
	for _, want := range []string{"diff: repo-a", "s2", "s3", "▸ etc/"} {
		if !strings.Contains(view, want) {
			t.Errorf("diff view missing %q\n---\n%s", want, view)
		}
	}
}

// Search distinguishes its empty-query prompt from a query with no matches.
func TestSnapshotDiffSearchSummaryEmptyStates(t *testing.T) {
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
	m = drivePastDiff(t, m, cmd)

	m = openDiffSearch(t, m)
	if got := m.diffSearchSummary(); got != "type to search" {
		t.Errorf("empty-query summary = %q, want %q", got, "type to search")
	}
	if view := m.View().Content; !strings.Contains(view, "type to search") {
		t.Errorf("rendered view missing the empty-query prompt\n---\n%s", view)
	}

	m = typeDiffSearch(t, m, "zzznomatch")
	if got := m.diffSearchSummary(); got != "(no matches)" {
		t.Errorf("no-match summary = %q, want %q", got, "(no matches)")
	}
	if view := m.View().Content; !strings.Contains(view, "(no matches)") {
		t.Errorf("rendered view missing the no-match notice\n---\n%s", view)
	}
}

// Hard quit must cancel an in-flight restic stream so it cannot outlive the
// password fd or continue consuming backend resources.
func TestSnapshotDiffHardQuitCancelsInFlightStream(t *testing.T) {
	a := detailApp(t)
	started := make(chan struct{})
	a.Restic = blockingRestic{started: started}
	m := newTestModel(t, a)

	m = update(t, m, press("enter"))
	m = update(t, m, press("t"))
	m = update(t, m, press("j"))
	m = update(t, m, press("t"))
	next, cmd := m.Update(press("d"))
	m = next.(Model)
	if !m.diffLoading() {
		t.Fatal("precondition: diff should be loading after d")
	}
	if m.view != snapshotDiffView {
		t.Fatalf("precondition: view = %d, want snapshotDiffView", m.view)
	}
	if cmd == nil {
		t.Fatal("expected a diff command")
	}

	// Run stream and progress leaves concurrently, forwarding only terminal output.
	done := make(chan tea.Msg, 1)
	for _, c := range leafCmds(t, cmd) {
		go func() {
			if msg, ok := c().(snapshotDiffMsg); ok {
				done <- msg
			}
		}()
	}

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("diff stream never reached restic")
	}

	// ctrl+c cancels m.ctx; dispatchSnapshotDiff's WithCancel child rides on top
	// of m.ctx so the cancel cascades into blockingRestic.StreamDiff's ctx.
	m.Update(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("diff stream did not unblock after ctrl+c cancelled the context")
	}
}

// diffRollup speaks the same change vocabulary as the summary cells and the
// filter mask: restic's letters (+ - M U T ?), not a private lowercase/tilde
// dialect. Regression for the `M114` (summary) vs `~10` (rollup) symbol split.
func TestDiffRollupUsesSummaryGlyphs(t *testing.T) {
	got := diffRollup(model.DiffStats{Added: 2, Removed: 1, Modified: 3, MetadataOnly: 4, TypeChanged: 5, Bitrot: 6})
	if want := "+2 -1 M3 U4 T5 ?6"; got != want {
		t.Errorf("diffRollup = %q, want %q", got, want)
	}
	glyphs := strings.Map(func(r rune) rune {
		if r >= '0' && r <= '9' || r == ' ' {
			return -1
		}
		return r
	}, got)
	if mask := diffFilterLabel(model.AllDiffKinds); glyphs != mask {
		t.Errorf("rollup glyphs %q diverge from the filter-mask vocabulary %q", glyphs, mask)
	}
}
