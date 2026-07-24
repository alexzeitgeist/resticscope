package tui

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/alexzeitgeist/resticscope/internal/app"
	"github.com/alexzeitgeist/resticscope/internal/model"
)

// snapshotdiffextract_test.go covers the diff view's `e` extract: the opener
// (per-side requests into one pair container, include sets honoring the live
// filter), the modal's sequential side queue, and the diff flavors of the
// review / done / terminal screens.

// Two concrete 64-hex snapshot IDs whose shorts are visually distinct. The
// fixture times make diffXFirstID the chronologically older snapshot, so it is
// the left ("first") side of the arrow on a fresh open.
const (
	diffXFirstID  = "aaaa1111000000000000000000000000000000000000000000000000000000ff"
	diffXSecondID = "bbbb2222000000000000000000000000000000000000000000000000000000ff"
)

// diffExtractEntries is the streamed diff between the pair:
//
//	/data/keep.txt  M  → both sides
//	/data/gone.txt  -  → first only
//	/data/new/      +  → pure-added dir on the second side
//	/data/new/img   +  →   …collapsed under it
func diffExtractEntries() []model.DiffEntry {
	return []model.DiffEntry{
		{Path: "/data/keep.txt", Modifier: "M", Type: model.ChangeModified, Kinds: model.KindModified},
		{Path: "/data/gone.txt", Modifier: "-", Type: model.ChangeRemoved, Kinds: model.KindRemoved},
		{Path: "/data/new", Modifier: "+", Type: model.ChangeAdded, Kinds: model.KindAdded, IsDir: true},
		{Path: "/data/new/img", Modifier: "+", Type: model.ChangeAdded, Kinds: model.KindAdded},
	}
}

// diffExtractModel drives the full open: list → detail → mark both snapshots →
// d → completed diff stream, cursor on the /data dir row.
func diffExtractModel(t *testing.T, entries []model.DiffEntry) Model {
	t.Helper()
	a := testApp(map[string]model.RepoState{
		"repo-a": {
			Name:        "repo-a",
			RefreshedAt: testNow,
			Snapshots: []model.Snapshot{
				{ID: diffXFirstID, ShortID: "aaaa1111", Time: testNow.Add(-2 * time.Hour), Hostname: "h"},
				{ID: diffXSecondID, ShortID: "bbbb2222", Time: testNow.Add(-1 * time.Hour), Hostname: "h"},
			},
		},
	})
	cfg, _ := extractCfg(t)
	a.Cfg.Extract = cfg
	a.Restic = stubRestic{
		snaps:       []model.Snapshot{{Hostname: "h"}},
		diffEntries: entries,
	}
	m := newTestModel(t, a)
	m = update(t, m, press("enter")) // list → detail
	m = update(t, m, press("t"))     // mark cursor row
	m = update(t, m, press("j"))
	m = update(t, m, press("t")) // mark second row
	next, cmd := m.Update(press("d"))
	m = drivePastDiff(t, next.(Model), cmd)
	if m.view != snapshotDiffView {
		t.Fatalf("view = %v, want snapshotDiffView", m.view)
	}
	return m
}

func TestDiffExtractOpenBuildsPairRequests(t *testing.T) {
	m := diffExtractModel(t, diffExtractEntries())
	if r := m.selectedDiffRow(); r == nil || r.Path != "/data" || !r.IsDir {
		t.Fatalf("selected row = %+v, want the /data dir", r)
	}
	m = update(t, m, press("e"))

	if m.view != extractView {
		t.Fatalf("view = %v, want extractView", m.view)
	}
	if m.extractReturn != snapshotDiffView {
		t.Errorf("extractReturn = %v, want snapshotDiffView", m.extractReturn)
	}
	em := m.extract
	if em.diff == nil {
		t.Fatal("extract sub-model is not in diff mode")
	}
	if em.diff.firstShort != "aaaa1111" || em.diff.secondShort != "bbbb2222" {
		t.Errorf("pair = %s → %s, want aaaa1111 → bbbb2222", em.diff.firstShort, em.diff.secondShort)
	}
	if !em.diff.sourceIsDir {
		t.Error("sourceIsDir = false, want true for a dir row")
	}
	if em.diff.firstCount != 2 || em.diff.secondCount != 3 {
		t.Errorf("counts = (%d,%d), want (2,3)", em.diff.firstCount, em.diff.secondCount)
	}

	const container = "diff-aaaa1111-bbbb2222"
	// Active request = first side, queue = second side.
	if em.req.SnapshotID != diffXFirstID || em.req.SnapshotShort != "aaaa1111" {
		t.Errorf("active side = %s, want the first snapshot", em.req.SnapshotShort)
	}
	if em.req.Source != "/data" || em.req.Mode != app.ExtractDirectoryTree || em.req.DiffContainer != container {
		t.Errorf("active req = %+v, want Source=/data tree-mode container=%s", em.req, container)
	}
	if want := []string{"/data/gone.txt", "/data/keep.txt"}; !stringSlicesEqual(em.req.IncludePaths, want) {
		t.Errorf("first-side includes = %q, want %q", em.req.IncludePaths, want)
	}
	if len(em.queue) != 1 {
		t.Fatalf("queue length = %d, want 1", len(em.queue))
	}
	q := em.queue[0]
	if q.SnapshotID != diffXSecondID || q.DiffContainer != container {
		t.Errorf("queued side = %+v, want the second snapshot in the same container", q)
	}
	// The pure-added dir collapses its child.
	if want := []string{"/data/keep.txt", "/data/new"}; !stringSlicesEqual(q.IncludePaths, want) {
		t.Errorf("second-side includes = %q, want %q", q.IncludePaths, want)
	}
	if !strings.HasSuffix(em.diff.containerDir, "/repo-a/"+container) {
		t.Errorf("containerDir = %q, want …/repo-a/%s", em.diff.containerDir, container)
	}
	// The Contains lookup is a plain-extract affordance; diff mode never fires it.
	if cmd := em.countsCmd(); cmd != nil {
		t.Error("countsCmd() must be nil in diff mode")
	}
}

func TestDiffExtractFilterScopesIncludes(t *testing.T) {
	m := diffExtractModel(t, diffExtractEntries())
	// Drop added and metadata-only entries from view: only M and - remain.
	m = update(t, m, press("+"))
	m = update(t, m, press("U"))
	m = update(t, m, press("e"))
	if m.view != extractView {
		t.Fatalf("view = %v, want extractView", m.view)
	}
	em := m.extract
	if want := []string{"/data/gone.txt", "/data/keep.txt"}; !stringSlicesEqual(em.req.IncludePaths, want) {
		t.Errorf("first-side includes = %q, want %q", em.req.IncludePaths, want)
	}
	if len(em.queue) != 1 {
		t.Fatalf("queue length = %d, want 1", len(em.queue))
	}
	// With + off the second side keeps only the modified file.
	if want := []string{"/data/keep.txt"}; !stringSlicesEqual(em.queue[0].IncludePaths, want) {
		t.Errorf("second-side includes = %q, want %q", em.queue[0].IncludePaths, want)
	}
	// The review surfaces the narrowed mask.
	body := stripANSI(m.extractBody())
	if !strings.Contains(body, "filter: -M") {
		t.Errorf("review body missing the filter mask\n---\n%s", body)
	}
}

func TestDiffExtractSkipsEmptySide(t *testing.T) {
	// Everything under /data is added: the first side has nothing to extract.
	m := diffExtractModel(t, []model.DiffEntry{
		{Path: "/data/new", Modifier: "+", Type: model.ChangeAdded, Kinds: model.KindAdded, IsDir: true},
		{Path: "/data/new/img", Modifier: "+", Type: model.ChangeAdded, Kinds: model.KindAdded},
	})
	m = update(t, m, press("e"))
	if m.view != extractView {
		t.Fatalf("view = %v, want extractView", m.view)
	}
	em := m.extract
	if len(em.queue) != 0 {
		t.Fatalf("queue length = %d, want 0 (single-side run)", len(em.queue))
	}
	if em.req.SnapshotShort != "bbbb2222" {
		t.Errorf("active side = %s, want the second snapshot only", em.req.SnapshotShort)
	}
	if em.diff.firstCount != 0 {
		t.Errorf("firstCount = %d, want 0", em.diff.firstCount)
	}
	body := stripANSI(m.extractBody())
	if !strings.Contains(body, "aaaa1111: nothing") {
		t.Errorf("review body should mark the empty side\n---\n%s", body)
	}
}

func TestDiffExtractReviewBodyRows(t *testing.T) {
	m := diffExtractModel(t, diffExtractEntries())
	m = update(t, m, press("e"))
	title := stripANSI(extractTitle(m.extract))
	if want := "extract: repo-a · aaaa1111 → bbbb2222"; title != want {
		t.Errorf("title = %q, want %q", title, want)
	}
	body := stripANSI(m.extractBody())
	for _, want := range []string{
		"Source", "▸ /data",
		"Diff", "aaaa1111 → bbbb2222",
		"Changes", "aaaa1111: 2 changed paths · bbbb2222: 3 changed paths",
		"Target", "diff-aaaa1111-bbbb2222",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("review body missing %q\n---\n%s", want, body)
		}
	}
	// The all-on default mask must not render (mirrors the diff summary rule).
	if strings.Contains(body, "filter:") {
		t.Errorf("review body must not render the all-on filter mask\n---\n%s", body)
	}
	footer := stripANSI(m.footerView())
	if !strings.Contains(footer, "enter extract") {
		t.Errorf("review footer missing enter extract\n---\n%s", footer)
	}
}

// TestDiffExtractRunsSidesSequentially drives both sides through the root
// Update loop: enter starts the first side, its completion starts the second
// (the Cmd returned through app.go routing), and the second completion lands
// on success with both sides published.
func TestDiffExtractRunsSidesSequentially(t *testing.T) {
	m := diffExtractModel(t, diffExtractEntries())
	m = update(t, m, press("e"))

	drv := &fakeExtractDriver{}
	drv.push(extractResp{result: app.ExtractResult{Files: 2, Dirs: 1, Bytes: 100, Elapsed: time.Second}})
	drv.push(extractResp{result: app.ExtractResult{Files: 3, Dirs: 2, Bytes: 200, Elapsed: 2 * time.Second}})
	m.extract.drv = drv

	next, cmd := m.Update(press("enter"))
	m = next.(Model)
	if m.extract.state != extractStateRunning {
		t.Fatalf("state = %v, want running", m.extract.state)
	}
	running := stripANSI(m.extractBody())
	if !strings.Contains(running, "aaaa1111 (1 of 2)") {
		t.Errorf("running body missing the side label\n---\n%s", running)
	}

	done1 := runCmd(t, cmd)
	if _, ok := done1.(extractRunDoneMsg); !ok {
		t.Fatalf("first worker msg = %T, want extractRunDoneMsg", done1)
	}
	next, cmd = m.Update(done1)
	m = next.(Model)
	if m.extract.state != extractStateRunning {
		t.Fatalf("after side 1: state = %v, want still running", m.extract.state)
	}
	if m.extract.req.SnapshotShort != "bbbb2222" {
		t.Errorf("after side 1: active side = %s, want bbbb2222", m.extract.req.SnapshotShort)
	}
	if cmd == nil {
		t.Fatal("side-1 completion must return the side-2 start Cmd")
	}
	running = stripANSI(m.extractBody())
	if !strings.Contains(running, "bbbb2222 (2 of 2)") {
		t.Errorf("running body missing the second side label\n---\n%s", running)
	}

	done2 := runCmd(t, cmd)
	next, _ = m.Update(done2)
	m = next.(Model)
	if m.extract.state != extractStateSuccess {
		t.Fatalf("after side 2: state = %v, want success", m.extract.state)
	}
	if len(m.extract.published) != 2 {
		t.Fatalf("published = %d sides, want 2", len(m.extract.published))
	}
	body := stripANSI(m.extractBody())
	for _, want := range []string{
		"extracted 5 files · 3 dirs", // summed tally
		"diff-aaaa1111-bbbb2222",
		"aaaa1111/  2 files · 1 dir",
		"bbbb2222/  3 files · 2 dirs",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("done body missing %q\n---\n%s", want, body)
		}
	}
	calls := drv.callsSnapshot()
	if len(calls) != 2 || calls[0].source != "/data" || calls[1].source != "/data" {
		t.Errorf("driver calls = %+v, want two /data restores", calls)
	}
}

// TestDiffExtractSecondSideFailureNotesFirst pins the partial-failure story:
// the first side stays published and the terminal body says so, alongside the
// failing side's id.
func TestDiffExtractSecondSideFailureNotesFirst(t *testing.T) {
	m := diffExtractModel(t, diffExtractEntries())
	m = update(t, m, press("e"))

	drv := &fakeExtractDriver{}
	drv.push(extractResp{result: app.ExtractResult{Files: 2, Dirs: 1}})
	drv.push(extractResp{err: errors.New("boom")})
	m.extract.drv = drv

	next, cmd := m.Update(press("enter"))
	m = next.(Model)
	next, cmd = m.Update(runCmd(t, cmd))
	m = next.(Model)
	next, _ = m.Update(runCmd(t, cmd))
	m = next.(Model)

	if m.extract.state != extractStateError {
		t.Fatalf("state = %v, want error", m.extract.state)
	}
	body := stripANSI(m.extractBody())
	for _, want := range []string{"boom", "bbbb2222 did not complete", "already extracted: aaaa1111"} {
		if !strings.Contains(body, want) {
			t.Errorf("terminal body missing %q\n---\n%s", want, body)
		}
	}
}

// TestDiffExtractBackReturnsToDiffView: leaving the modal lands back on the
// still-populated diff view (the diff state was never cleared by the launch).
func TestDiffExtractBackReturnsToDiffView(t *testing.T) {
	m := diffExtractModel(t, diffExtractEntries())
	rows := len(m.diffRows)
	m = update(t, m, press("e"))
	next, cmd := m.Update(press("esc"))
	m = next.(Model)
	if cmd == nil {
		t.Fatal("review esc must dispatch the back message")
	}
	next, _ = m.Update(cmd())
	m = next.(Model)
	if m.view != snapshotDiffView {
		t.Fatalf("view = %v, want snapshotDiffView", m.view)
	}
	if len(m.diffRows) != rows {
		t.Errorf("diff rows = %d, want %d (diff state must survive the round-trip)", len(m.diffRows), rows)
	}
}

func TestDiffViewFooterAdvertisesExtract(t *testing.T) {
	m := diffExtractModel(t, diffExtractEntries())
	footer := stripANSI(m.footerView())
	if !strings.Contains(footer, "e extract") {
		t.Errorf("diff footer missing \"e extract\"\n---\n%s", footer)
	}
}

func stringSlicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
