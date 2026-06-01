package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"resticscope/internal/model"
)

// snapshotdiff_test.go covers the controller's three areas of interest: the
// 2-slot mark FIFO, diffPair's ordering and ok semantics, and the round-trip
// from detail through snapshotDiffView and back. The model package's diff_test
// already covers BuildDiffTree, ScanDiffNDJSON, and the filter predicate; here
// we exercise only the TUI-layer concerns.

// openDetail enters detailView on repo-a with the default 3-snapshot fixture
// from detailApp.
func openDetail(t *testing.T) Model {
	t.Helper()
	m := newTestModel(t, detailApp(t))
	m = update(t, m, press("enter"))
	if m.view != detailView {
		t.Fatalf("precondition: view = %d, want detailView", m.view)
	}
	return m
}

// drivePastDiff delivers the terminal snapshotDiffMsg produced by the
// dispatchSnapshotDiff Cmd so the model lands on a populated diff view in one
// step. The stub restic completes the stream synchronously, so the streamCmd
// returns a snapshotDiffMsg directly.
func drivePastDiff(t *testing.T, m Model, cmd tea.Cmd) Model {
	t.Helper()
	if cmd == nil {
		t.Fatal("expected a diff command")
	}
	// dispatchSnapshotDiff returns tea.Batch(streamCmd, waitForDiffProgress).
	// We want the snapshotDiffMsg (the terminal message), so unwrap the batch
	// and find the leaf that produces it.
	for _, leaf := range leafCmds(t, cmd) {
		out := leaf()
		if msg, ok := out.(snapshotDiffMsg); ok {
			return update(t, m, msg)
		}
	}
	t.Fatal("no snapshotDiffMsg in batch")
	return m
}

// snapshotIDs returns the 3 ids in detailApp's seeded order: oldest, middle, newest.
func snapshotIDs(t *testing.T, m Model) (older, middle, newer string) {
	t.Helper()
	snaps := m.detailSnapshots()
	if len(snaps) != 3 {
		t.Fatalf("precondition: %d snapshots, want 3", len(snaps))
	}
	// detailSnapshots sorts newest first.
	return snaps[2].ID, snaps[1].ID, snaps[0].ID
}

func TestToggleDetailMarkFIFO(t *testing.T) {
	m := openDetail(t)
	older, middle, newer := snapshotIDs(t, m)

	// Mark cursor (newest) → 1 entry.
	m = update(t, m, press("t"))
	if len(m.detailMarks) != 1 || m.detailMarks[0].ID != newer {
		t.Fatalf("first mark = %+v, want one entry with id %q", m.detailMarks, newer)
	}

	// Move cursor and mark middle → 2 entries (newer, middle), FIFO order.
	m = update(t, m, press("j"))
	m = update(t, m, press("t"))
	if len(m.detailMarks) != 2 ||
		m.detailMarks[0].ID != newer || m.detailMarks[1].ID != middle {
		t.Fatalf("two marks = %+v, want [%q, %q]", m.detailMarks, newer, middle)
	}

	// Move to oldest and mark → FIFO eviction: oldest mark (newer) drops, list
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

	// 0 marks → ok=false.
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

func TestDOpensDiffViewWithTwoMarks(t *testing.T) {
	a := detailApp(t)
	cap := &stubDiffCapture{}
	a.Restic = stubRestic{
		snaps:   []model.Snapshot{{Hostname: "h"}},
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

	if m.diffLoading {
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

func TestSnapshotDiffBackReturnsToDetailKeepingMarks(t *testing.T) {
	a := detailApp(t)
	a.Restic = stubRestic{snaps: []model.Snapshot{{Hostname: "h"}}}
	m := newTestModel(t, a)
	m = update(t, m, press("enter"))

	// Two marks → d → diff view.
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
	// effect we're exercising here).
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
	for _, want := range []string{"diff: repo-a", "s2", "s3", "etc/"} {
		if !strings.Contains(view, want) {
			t.Errorf("diff view missing %q\n---\n%s", want, view)
		}
	}
}

