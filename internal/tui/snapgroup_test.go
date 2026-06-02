package tui

import (
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"resticscope/internal/app"
	"resticscope/internal/model"
)

// snap is a terse fixture builder for the pipeline tests. ID and ShortID are
// derived from id so a test can name snapshots conversationally ("a", "b") and
// still assert on a deterministic short id.
func snap(id, host, tree string, minutesAgo int, tags, paths []string) model.Snapshot {
	return model.Snapshot{
		ID:       id,
		ShortID:  id,
		Time:     testNow.Add(-time.Duration(minutesAgo) * time.Minute),
		Hostname: host,
		Tree:     tree,
		Tags:     tags,
		Paths:    paths,
	}
}

func ids(nodes []snapNode) []string {
	out := make([]string, len(nodes))
	for i, n := range nodes {
		out[i] = n.head.ID
	}
	return out
}

// --- pipeline ---

func TestCollapseTreeRunsFoldsConsecutivePeers(t *testing.T) {
	// Newest-first: a3,a2,a1 all share tree T1 + (host, paths, tags), then b1 breaks
	// the run on tree, then a4 (same tree as a-run) does NOT re-merge because b1
	// sits between them.
	snaps := []model.Snapshot{
		snap("a3", "h", "T1", 1, nil, []string{"/data"}),
		snap("a2", "h", "T1", 2, nil, []string{"/data"}),
		snap("a1", "h", "T1", 3, nil, []string{"/data"}),
		snap("b1", "h", "T2", 4, nil, []string{"/data"}),
		snap("a4", "h", "T1", 5, nil, []string{"/data"}),
	}
	got := collapseTreeRuns(snaps)
	if len(got) != 3 {
		t.Fatalf("collapseTreeRuns produced %d nodes, want 3", len(got))
	}
	if got[0].head.ID != "a3" || got[0].count != 2 || len(got[0].peers) != 2 {
		t.Errorf("first node = %+v, want head a3 with 2 peers", got[0])
	}
	if got[0].peers[0].ID != "a2" || got[0].peers[1].ID != "a1" {
		t.Errorf("peer order = %v, want [a2 a1] (newest-first preserved)", []string{got[0].peers[0].ID, got[0].peers[1].ID})
	}
	if got[1].head.ID != "b1" || got[1].count != 0 {
		t.Errorf("second node = %+v, want bare b1", got[1])
	}
	if got[2].head.ID != "a4" || got[2].count != 0 {
		t.Errorf("third node = %+v, want bare a4 (non-contiguous tree never re-merges)", got[2])
	}
}

func TestCollapseTreeRunsRespectsSourceKey(t *testing.T) {
	// Same tree, different hosts must not collapse — the collapse rule is
	// tree + (host, sorted tags, sorted paths), not tree alone.
	snaps := []model.Snapshot{
		snap("x", "host-a", "T1", 1, nil, []string{"/data"}),
		snap("y", "host-b", "T1", 2, nil, []string{"/data"}),
	}
	got := collapseTreeRuns(snaps)
	if len(got) != 2 {
		t.Fatalf("nodes = %d, want 2 (different host blocks collapse)", len(got))
	}
}

func TestCollapseTreeRunsEmptyTreeNeverMerges(t *testing.T) {
	// An empty Tree (pre-tree fixture or unknown) must never collapse, even with
	// itself — otherwise unrelated pre-0.18 snapshots would silently merge.
	snaps := []model.Snapshot{
		snap("e1", "h", "", 1, nil, []string{"/d"}),
		snap("e2", "h", "", 2, nil, []string{"/d"}),
	}
	got := collapseTreeRuns(snaps)
	if len(got) != 2 {
		t.Fatalf("nodes = %d, want 2 (empty Tree never merges)", len(got))
	}
}

func TestCollapseTreeRunsHandlesEdgeSizes(t *testing.T) {
	if got := collapseTreeRuns(nil); got != nil {
		t.Errorf("collapseTreeRuns(nil) = %+v, want nil", got)
	}
	one := []model.Snapshot{snap("a", "h", "T", 1, nil, []string{"/d"})}
	got := collapseTreeRuns(one)
	if len(got) != 1 || got[0].count != 0 {
		t.Errorf("single-snapshot input = %+v, want one bare node", got)
	}
}

func TestSnapSectionsByHost(t *testing.T) {
	snaps := []model.Snapshot{
		snap("a", "Prod", "T1", 1, nil, nil),
		snap("b", "alpha", "T2", 2, nil, nil),
		snap("c", "", "T3", 3, nil, nil), // missing host -> fallback last
		snap("d", "prod", "T4", 4, nil, nil),
	}
	got := snapSectionsBy(snaps, snapGroupHost)
	if len(got) != 4 {
		t.Fatalf("sections = %d, want 4 (Prod, alpha, prod, fallback)", len(got))
	}
	// Case-insensitive primary order with byte-order tie-break ("Prod" < "prod"
	// because 'P' < 'p' bytewise); the noKey fallback always sorts last.
	wantTitles := []string{"alpha", "Prod", "prod", "(no host)"}
	for i, want := range wantTitles {
		if got[i].title != want {
			t.Errorf("section[%d].title = %q, want %q", i, got[i].title, want)
		}
	}
	if !got[3].noKey {
		t.Errorf("fallback section should set noKey=true")
	}
	if got[3].rawCount != 1 {
		t.Errorf("fallback rawCount = %d, want 1", got[3].rawCount)
	}
}

func TestSnapSectionsByTagsBucketByFullSet(t *testing.T) {
	// A snapshot tagged ["a","b"] must land in ONE "a, b" section, not in both
	// "a" and "b". The empty-tags snapshot falls into the fallback bucket.
	snaps := []model.Snapshot{
		snap("x", "h", "T1", 1, []string{"b", "a"}, nil),
		snap("y", "h", "T2", 2, []string{"a"}, nil),
		snap("z", "h", "T3", 3, nil, nil),
	}
	got := snapSectionsBy(snaps, snapGroupTags)
	if len(got) != 3 {
		t.Fatalf("sections = %d, want 3 (a, 'a, b', untagged)", len(got))
	}
	wantTitles := []string{"a", "a, b", "(untagged)"}
	for i, want := range wantTitles {
		if got[i].title != want {
			t.Errorf("section[%d].title = %q, want %q", i, got[i].title, want)
		}
	}
}

func TestSnapSectionsByPathsTitleIsSortedReadable(t *testing.T) {
	// Paths key is the sorted set NUL-joined for stable bucketing; title shows
	// the sorted set comma-joined for readability. "/etc, /var" and "/var, /etc"
	// must collapse into the same section.
	snaps := []model.Snapshot{
		snap("x", "h", "T1", 1, nil, []string{"/var", "/etc"}),
		snap("y", "h", "T2", 2, nil, []string{"/etc", "/var"}),
		snap("z", "h", "T3", 3, nil, nil),
	}
	got := snapSectionsBy(snaps, snapGroupPaths)
	if len(got) != 2 {
		t.Fatalf("sections = %d, want 2 ('/etc, /var' bucket + fallback)", len(got))
	}
	if got[0].title != "/etc, /var" {
		t.Errorf("section[0].title = %q, want '/etc, /var'", got[0].title)
	}
	if got[0].rawCount != 2 {
		t.Errorf("section[0].rawCount = %d, want 2", got[0].rawCount)
	}
	if got[1].title != "(no paths)" {
		t.Errorf("section[1].title = %q, want fallback", got[1].title)
	}
}

func TestSnapSectionsByPreservesNewestFirstWithinSection(t *testing.T) {
	// Section construction must not re-sort within a bucket; the caller's
	// newest-first order has to survive so collapse runs see consecutive peers.
	snaps := []model.Snapshot{
		snap("h1-new", "h1", "T1", 1, nil, nil),
		snap("h2-new", "h2", "T2", 2, nil, nil),
		snap("h1-old", "h1", "T1", 3, nil, nil),
	}
	got := snapSectionsBy(snaps, snapGroupHost)
	if len(got) != 2 {
		t.Fatalf("sections = %d, want 2", len(got))
	}
	h1 := got[0] // "h1" sorts before "h2"
	if h1.title != "h1" {
		t.Fatalf("first section = %q, want h1", h1.title)
	}
	if len(h1.nodes) != 2 || h1.nodes[0].head.ID != "h1-new" || h1.nodes[1].head.ID != "h1-old" {
		t.Errorf("h1 nodes preserved newest-first: %v", ids(h1.nodes))
	}
}

func TestSnapSectionsByDistinctKeysCollidingTitlesStaySeparate(t *testing.T) {
	// One snapshot tagged with the literal "a, b" must not collide with one
	// tagged ["a","b"] — they have distinct keys (NUL-joined sorted set) even
	// though their rendered titles look identical.
	snaps := []model.Snapshot{
		snap("p", "h", "T1", 1, []string{"a, b"}, nil),
		snap("q", "h", "T2", 2, []string{"a", "b"}, nil),
	}
	got := snapSectionsBy(snaps, snapGroupTags)
	if len(got) != 2 {
		t.Fatalf("sections = %d, want 2 distinct keys despite identical titles", len(got))
	}
}

func TestBuildSnapDisplayCollapseNeverCrossesSection(t *testing.T) {
	// Same tree across two hosts must split into two sections AND not collapse
	// across the section boundary — collapse runs strictly within a section.
	snaps := []model.Snapshot{
		snap("a-new", "host-a", "T1", 1, nil, []string{"/d"}),
		snap("b1", "host-b", "T1", 2, nil, []string{"/d"}),
		snap("b2", "host-b", "T1", 3, nil, []string{"/d"}),
		snap("a-old", "host-a", "T1", 4, nil, []string{"/d"}),
	}
	d := buildSnapDisplay(snaps, snapGroupHost, true)
	if len(d.sections) != 2 {
		t.Fatalf("sections = %d, want 2 (host-a, host-b)", len(d.sections))
	}
	// host-a's two snapshots are non-contiguous in the source slice (b1/b2 sit
	// between them); after sectioning they're contiguous so collapse folds them.
	hostA := d.sections[0]
	if hostA.title != "host-a" {
		t.Fatalf("section[0] = %q, want host-a", hostA.title)
	}
	if len(hostA.nodes) != 1 || hostA.nodes[0].count != 1 {
		t.Errorf("host-a should collapse to 1 head with 1 peer, got %+v", hostA.nodes)
	}
	hostB := d.sections[1]
	if len(hostB.nodes) != 1 || hostB.nodes[0].count != 1 {
		t.Errorf("host-b should collapse to 1 head with 1 peer, got %+v", hostB.nodes)
	}
}

func TestBuildSnapDisplayBothOffPassthrough(t *testing.T) {
	snaps := []model.Snapshot{
		snap("a", "h", "T1", 1, nil, nil),
		snap("b", "h", "T1", 2, nil, nil),
	}
	d := buildSnapDisplay(snaps, snapGroupOff, false)
	if d.sections != nil {
		t.Errorf("group off should leave sections nil, got %+v", d.sections)
	}
	if len(d.nodes) != 2 || d.nodes[0].count != 0 || d.nodes[1].count != 0 {
		t.Errorf("collapse off should keep one node per snapshot, got %v", ids(d.nodes))
	}
}

func TestBuildSnapDisplayGroupOffCollapseOn(t *testing.T) {
	snaps := []model.Snapshot{
		snap("a", "h", "T1", 1, nil, nil),
		snap("b", "h", "T1", 2, nil, nil),
		snap("c", "h", "T2", 3, nil, nil),
	}
	d := buildSnapDisplay(snaps, snapGroupOff, true)
	if d.sections != nil {
		t.Errorf("group off should leave sections nil")
	}
	if len(d.nodes) != 2 || d.nodes[0].count != 1 || d.nodes[1].count != 0 {
		t.Errorf("flat collapse = %v / counts=%d,%d, want a(+1), c", ids(d.nodes), d.nodes[0].count, d.nodes[1].count)
	}
}

// --- Model state & anchors ---

// snapGroupApp seeds repo-a with two hosts × multiple snapshots, with shared
// trees inside each host so collapse can fold and group can split. ID-time
// is staggered so the newest-first ordering is deterministic.
func snapGroupApp(t *testing.T) *app.App {
	t.Helper()
	a := testApp(map[string]model.RepoState{
		"repo-a": {
			Name: "repo-a", RefreshedAt: testNow, LastSnapshot: testNow.Add(-time.Minute),
			SnapshotCount: 5,
			Hosts:         []string{"host-a", "host-b"},
			Snapshots: []model.Snapshot{
				snap("a3", "host-a", "TA", 1, nil, []string{"/data"}), // newest a
				snap("a2", "host-a", "TA", 2, nil, []string{"/data"}),
				snap("a1", "host-a", "TA", 3, nil, []string{"/data"}),
				snap("b2", "host-b", "TB", 4, nil, []string{"/data"}),
				snap("b1", "host-b", "TB", 5, nil, []string{"/data"}),
			},
		},
	})
	return a
}

func openDetailWith(t *testing.T, a *app.App) Model {
	t.Helper()
	m := newTestModel(t, a)
	return update(t, m, press("enter"))
}

func TestDetailEntryDefaultsBothOff(t *testing.T) {
	m := openDetailWith(t, snapGroupApp(t))
	if m.snapGroupMode != snapGroupOff {
		t.Errorf("snapGroupMode = %d, want off", m.snapGroupMode)
	}
	if m.snapCollapseTree {
		t.Errorf("snapCollapseTree should be false on detail entry (collapse is opt-in)")
	}
	// With both off the display is the raw newest-first list — one node per
	// snapshot, no sections.
	d := m.snapDisplay()
	if d.sections != nil {
		t.Errorf("default display should have no sections, got %d", len(d.sections))
	}
	if len(d.nodes) != 5 {
		t.Errorf("default display nodes = %d, want 5 (no collapse)", len(d.nodes))
	}
}

func TestCycleSnapGroupPreservesSelection(t *testing.T) {
	// Default collapse OFF: raw flat order [a3,a2,a1,b2,b1]; b2 sits at
	// index 3. After cycling group to host the same snapshot must stay
	// selected even though the flat ordering changes.
	m := openDetailWith(t, snapGroupApp(t))
	m.snapCursor = 3
	got := m.selectedSnapshot()
	if got == nil || got.ID != "b2" {
		t.Fatalf("pre-cycle selection = %+v, want b2", got)
	}
	m = m.cycleSnapGroup() // off -> host
	if m.snapGroupMode != snapGroupHost {
		t.Fatalf("snapGroupMode = %d, want host", m.snapGroupMode)
	}
	got = m.selectedSnapshot()
	if got == nil || got.ID != "b2" {
		t.Errorf("post-cycle selection = %+v, want b2 still selected", got)
	}
}

func TestCycleSnapCollapsePreservesSelection(t *testing.T) {
	// Default collapse is OFF — the cursor sits on b2 in the raw flat order
	// [a3,a2,a1,b2,b1]. Toggling collapse ON folds the a-run and the b-run;
	// b2 is the b-section head so the same snapshot stays selected.
	m := openDetailWith(t, snapGroupApp(t))
	m.snapCursor = 3 // b2 in [a3,a2,a1,b2,b1]
	m = m.cycleSnapCollapse()
	if !m.snapCollapseTree {
		t.Fatalf("collapse should be on after toggle")
	}
	got := m.selectedSnapshot()
	if got == nil || got.ID != "b2" {
		t.Errorf("post-toggle selection = %+v, want b2", got)
	}
}

func TestCycleSnapCollapsePreservesPeerSelection(t *testing.T) {
	// Start with collapse OFF so every snapshot is its own node and a peer
	// (a2) is directly selectable. Then toggle collapse on: the cursor must
	// land on the absorbing head (a3) — indexOfSnap searches peers too.
	m := openDetailWith(t, snapGroupApp(t))
	m.snapCollapseTree = false
	m.snapCursor = 1 // a2 in the uncollapsed flat order [a3,a2,a1,b2,b1]
	pre := m.selectedSnapshot()
	if pre == nil || pre.ID != "a2" {
		t.Fatalf("pre-collapse selection = %+v, want a2", pre)
	}
	m = m.cycleSnapCollapse() // collapse on
	post := m.selectedSnapshot()
	if post == nil || post.ID != "a3" {
		t.Errorf("post-collapse selection = %+v, want absorbing head a3", post)
	}
}

func TestSnapCursorClampedAfterGroupShrink(t *testing.T) {
	// Start with group off + collapse off → 5 selectable nodes. Park the
	// cursor at the last one. Collapse-on shrinks the set to 2 nodes; the
	// cursor must clamp into bounds rather than index out of range.
	m := openDetailWith(t, snapGroupApp(t))
	m.snapCollapseTree = false
	m.snapCursor = 4 // last node in [a3,a2,a1,b2,b1]
	m = m.cycleSnapCollapse()
	if m.snapCursor >= m.snapCount() {
		t.Errorf("snapCursor=%d, snapCount=%d — cursor must be clamped", m.snapCursor, m.snapCount())
	}
	// Specifically: b1 is a peer of b2, so the anchor lands on b2 (index 1).
	if got := m.selectedSnapshot(); got == nil || got.ID != "b2" {
		t.Errorf("selection after shrink = %+v, want b2", got)
	}
}

// --- marks ---

func TestCollapseNormalizesPeerMarks(t *testing.T) {
	// Start collapse-off, mark a peer (a2), then turn collapse on. The mark
	// must map to the absorbing head (a3), and the FIFO must still hold one
	// mark (no duplicate from the eventual re-mark cycle).
	m := openDetailWith(t, snapGroupApp(t))
	m.snapCollapseTree = false
	// Mark a2 directly via the FIFO so the test doesn't depend on cursor
	// position semantics.
	m.detailMarks = []model.Snapshot{snap("a2", "host-a", "TA", 2, nil, []string{"/data"})}
	m = m.cycleSnapCollapse() // collapse on; a2 folds into a3
	if len(m.detailMarks) != 1 {
		t.Fatalf("marks after collapse = %d, want 1", len(m.detailMarks))
	}
	if m.detailMarks[0].ID != "a3" {
		t.Errorf("normalized mark = %q, want absorbing head a3", m.detailMarks[0].ID)
	}
}

func TestCollapsedPeerMarkRendersOnHead(t *testing.T) {
	// isNodeMarked checks head + peers, so a hidden peer mark stays visible on
	// the head row even BEFORE normalize folds it (defensive: covers a render
	// hop between cycle and normalize). Explicitly enable collapse since the
	// new default is off.
	m := openDetailWith(t, snapGroupApp(t))
	m.snapCollapseTree = true
	m.detailMarks = []model.Snapshot{snap("a1", "host-a", "TA", 3, nil, []string{"/data"})}
	d := m.snapDisplay()
	if !m.isNodeMarked(d.nodes[0]) {
		t.Errorf("hidden peer a1's mark must render on head a3")
	}
}

func TestCollapsedHeadPeerMarksDeduplicateKeepsFirst(t *testing.T) {
	// FIFO has [a2 (peer), a3 (head)] before collapse-on. After normalize,
	// both map to a3 — they must dedupe to one entry, keeping the FIRST
	// FIFO position (a2 came first; it folds into a3 at its original slot).
	m := openDetailWith(t, snapGroupApp(t))
	m.snapCollapseTree = false
	m.detailMarks = []model.Snapshot{
		snap("a2", "host-a", "TA", 2, nil, []string{"/data"}),
		snap("a3", "host-a", "TA", 1, nil, []string{"/data"}),
	}
	m = m.cycleSnapCollapse()
	if len(m.detailMarks) != 1 {
		t.Fatalf("marks = %d, want 1 deduped", len(m.detailMarks))
	}
	if m.detailMarks[0].ID != "a3" {
		t.Errorf("deduped mark = %q, want absorbing head a3", m.detailMarks[0].ID)
	}
}

func TestCollapseMarkNormalizationIsMonotonic(t *testing.T) {
	// Collapse-on folds a2 into a3; toggling collapse off must NOT
	// reconstruct the original a2 mark (folded peers share identity by
	// construction, so the loss is intentional and documented).
	m := openDetailWith(t, snapGroupApp(t))
	m.snapCollapseTree = false
	m.detailMarks = []model.Snapshot{snap("a2", "host-a", "TA", 2, nil, []string{"/data"})}
	m = m.cycleSnapCollapse() // collapse on -> mark becomes a3
	m = m.cycleSnapCollapse() // collapse off -> still a3 (not un-merged)
	if len(m.detailMarks) != 1 || m.detailMarks[0].ID != "a3" {
		t.Errorf("marks after collapse off again = %+v, want still [a3] (monotonic)", m.detailMarks)
	}
}

// --- render selection ---

func TestSnapshotDetailTracksGroupedSelection(t *testing.T) {
	// With grouping host + collapse on, the displayed nodes are [a3-head,
	// b2-head]. Moving the cursor to index 1 must put b2 in the selected
	// sub-panel — proving snapshotDetail goes through selectedSnapshot().
	// Collapse is opt-in (off by default) so the test sets it explicitly.
	m := openDetailWith(t, snapGroupApp(t))
	m.width, m.height = 120, 40
	m.snapCollapseTree = true
	m = m.cycleSnapGroup() // off -> host
	m.snapCursor = 1       // the b-section head
	if got := m.selectedSnapshot(); got == nil || got.ID != "b2" {
		t.Fatalf("selected = %+v, want b2 head", got)
	}
	view := stripANSI(m.View().Content)
	if !strings.Contains(view, "b2") {
		t.Errorf("view must show b2 in the selected sub-panel\n---\n%s", view)
	}
}

func TestSnapshotDetailUsesSelectedSnapshotNoRawSnapsParam(t *testing.T) {
	// snapshotDetail's new signature takes only width; calling it directly with
	// the current model must produce the same panel content the integrated
	// renderer does. Indirectly proves no raw snaps slice indexing remains.
	// Collapse is opt-in (off by default) so the test sets it explicitly.
	m := openDetailWith(t, snapGroupApp(t))
	m.width, m.height = 120, 40
	m.snapCollapseTree = true
	m = m.cycleSnapGroup()
	m.snapCursor = 1
	w, _ := m.effSize()
	got := stripANSI(m.snapshotDetail(w))
	if !strings.Contains(got, "b2") {
		t.Errorf("snapshotDetail(w) = %q, want b2 selected", got)
	}
	if strings.Contains(got, "a3") {
		t.Errorf("snapshotDetail(w) = %q, must not leak the other group's head", got)
	}
}

func TestGroupedSnapshotMaxOneRendersSelectedRow(t *testing.T) {
	// Squeeze the pane until detailSnapVisible == 1; the grouped renderer
	// must show the SELECTED data row (not a heading-only window).
	// Collapse is opt-in (off by default) so the test sets it explicitly
	// to land cursor index 1 on b2 head in [a3-head, b2-head].
	m := openDetailWith(t, snapGroupApp(t))
	m.snapCollapseTree = true
	m = m.cycleSnapGroup()
	m.snapCursor = 1 // b2 head
	// detailOverhead at the default test height is large; pick a height that
	// forces max=1. We bypass detailSnapVisible's floor by going as low as the
	// overhead arithmetic allows.
	m.width = 100
	m.height = m.detailOverhead(false, false) + 1
	out := m.snapshotTable()
	plain := stripANSI(out)
	if !strings.Contains(plain, "b2") {
		t.Errorf("max=1 grouped renderer must show the selected row b2\n---\n%s", plain)
	}
}

// --- goBack reset ---

func TestGoBackResetsSnapGroupState(t *testing.T) {
	m := openDetailWith(t, snapGroupApp(t))
	m = m.cycleSnapGroup()    // group: off -> host
	m = m.cycleSnapCollapse() // collapse: false -> true
	// Setup precondition: both fields have moved off the per-visit defaults
	// (group=off, collapse=false) so the goBack assertion below proves the
	// reset, not the absence of any cycle.
	if m.snapGroupMode == snapGroupOff || !m.snapCollapseTree {
		t.Fatalf("setup failed: group=%d collapse=%v", m.snapGroupMode, m.snapCollapseTree)
	}
	// q from detail returns to list AND resets transient state.
	next, _ := m.Update(press("q"))
	nm := next.(Model)
	if nm.view != listView {
		t.Fatalf("view after q = %d, want listView", nm.view)
	}
	if nm.snapGroupMode != snapGroupOff {
		t.Errorf("snapGroupMode after goBack = %d, want off", nm.snapGroupMode)
	}
	if nm.snapCollapseTree {
		t.Errorf("snapCollapseTree after goBack = true, want false")
	}
}

// --- meta count is raw ---

func TestSnapshotsMetaCountIsRaw(t *testing.T) {
	// Collapse on folds 5 snapshots into 2 nodes, but the meta "Snapshots: N"
	// row is sourced from row.State.SnapshotCount (raw), not snapCount().
	// Enable collapse explicitly since the per-visit default is off.
	m := openDetailWith(t, snapGroupApp(t))
	m.snapCollapseTree = true
	if got := m.snapCount(); got != 2 {
		t.Fatalf("snapCount = %d, want 2 (collapse on should fold 5→2)", got)
	}
	view := stripANSI(m.View().Content)
	// detailMeta renders "Snapshots" label + the SnapshotCount value.
	if !strings.Contains(view, "Snapshots") || !strings.Contains(view, "5") {
		t.Errorf("meta row should report raw count 5\n---\n%s", view)
	}
}

// --- help overlay ---

func TestHelpOverlayShowsSnapGroupingKeys(t *testing.T) {
	m := newTestModel(t, snapGroupApp(t))
	_, right := m.helpColumns()
	var detail helpSection
	for _, s := range right {
		if s.title == "Detail" {
			detail = s
			break
		}
	}
	if detail.title == "" {
		t.Fatal("help overlay missing Detail section")
	}
	hasKey := func(k string) bool {
		for _, e := range detail.entries {
			if e.keys == k {
				return true
			}
		}
		return false
	}
	if !hasKey("g") {
		t.Errorf("Detail help missing `g` entry: %+v", detail.entries)
	}
	if !hasKey("c") {
		t.Errorf("Detail help missing `c` entry: %+v", detail.entries)
	}
}

// --- heading suffix surfacing transient state ---

func TestSnapshotsHeadingShowsGroupAndCollapseState(t *testing.T) {
	m := openDetailWith(t, snapGroupApp(t))
	// Defaults (group off + collapse off): no suffix at all.
	if got := m.snapshotsHeadingText(); strings.Contains(got, "group:") || strings.Contains(got, "collapse") {
		t.Errorf("default heading should be clean, got %q", got)
	}
	m = m.cycleSnapGroup()
	if got := m.snapshotsHeadingText(); !strings.Contains(got, "group: host") {
		t.Errorf("heading after group cycle = %q, want '· group: host'", got)
	}
	m = m.cycleSnapCollapse()
	if got := m.snapshotsHeadingText(); !strings.Contains(got, "collapse on") {
		t.Errorf("heading after collapse toggle = %q, want '· collapse on'", got)
	}
}

// --- key wiring (g/c on detail) ---

func TestDetailGKeyCyclesSnapGroup(t *testing.T) {
	m := openDetailWith(t, snapGroupApp(t))
	m = update(t, m, press("g"))
	if m.snapGroupMode != snapGroupHost {
		t.Errorf("g on detail did not cycle group: mode=%d", m.snapGroupMode)
	}
}

func TestDetailCKeyTogglesCollapse(t *testing.T) {
	m := openDetailWith(t, snapGroupApp(t))
	if m.snapCollapseTree {
		t.Fatal("setup: collapse should default to false (opt-in)")
	}
	m = update(t, m, press("c"))
	if !m.snapCollapseTree {
		t.Errorf("c on detail did not toggle collapse on")
	}
}

// --- collapse-mode column alignment ---

func TestCollapsedRowAlignsWithUncollapsedRow(t *testing.T) {
	// When collapse is on the ID column reserves a "+N" suffix slot. The
	// uncollapsed row must pad blank space in that slot so every column past
	// ID (Time, Hostname, Size, …) lines up across rows. Verify by comparing
	// the byte offset of the Time value in two joined cell strings — they
	// must match.
	l := snapshotLayout(120, true)
	tm := "2024-01-01 12:00"
	collapsed := strings.Join(snapCells(l, snapRow{
		id: idCell("abc12345", 1), tm: tm, host: "h", size: "0 B",
	}), "  ")
	uncollapsed := strings.Join(snapCells(l, snapRow{
		id: idCell("def67890", 0), tm: tm, host: "h", size: "0 B",
	}), "  ")
	if got, want := strings.Index(uncollapsed, tm), strings.Index(collapsed, tm); got != want {
		t.Errorf("Time column misaligned (collapsed@%d vs uncollapsed@%d)\ncollapsed:   %q\nuncollapsed: %q",
			want, got, collapsed, uncollapsed)
	}
}

// Compile-time guard: signature drift on snapshotTable/snapshotDetail (e.g.
// re-introducing a raw snaps slice param) fails the build here, not at the
// first call site that breaks.
var (
	_ = (Model).snapshotTable
	_ = func(m Model, w int) string { return m.snapshotDetail(w) }
	_ = tea.KeyPressMsg{}
)
