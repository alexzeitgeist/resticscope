package tui

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/alexzeitgeist/resticscope/internal/app"
	"github.com/alexzeitgeist/resticscope/internal/model"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

// The detail fixture's pair diffs id-oldest (first) against id-newest (second).
const (
	infoFirstID  = "id-oldest"
	infoSecondID = "id-newest"
)

var infoTime = time.Date(2026, 4, 2, 9, 14, 7, 0, time.UTC)

// infoFile is a plain root-owned file record; tests vary one field at a time.
func infoFile(name string) model.TreeNode {
	return model.TreeNode{
		Name: name, Type: model.NodeTypeFile, Mode: 0o644, User: "root", Group: "root",
		ModTime: infoTime, AccessTime: infoTime, ChangeTime: infoTime,
		Inode: 1311, Size: 2873, Links: 1, Content: []string{"cc33"},
	}
}

// infoEntries holds one change of each shape the screen distinguishes.
func infoEntries() []model.DiffEntry {
	return []model.DiffEntry{
		{Path: "/etc", Modifier: "U", Type: model.ChangeMetadataOnly, Kinds: model.KindMetadata, IsDir: true},
		{Path: "/etc/passwd", Modifier: "U", Type: model.ChangeMetadataOnly, Kinds: model.KindMetadata},
		{Path: "/etc/wiki", Modifier: "+", Type: model.ChangeAdded, Kinds: model.KindAdded},
	}
}

// infoNodes gives /etc/passwd a chmod 600 between the snapshots and /etc only a
// new subtree.
func infoNodes() map[string]model.TreeNode {
	before, after := infoFile("passwd"), infoFile("passwd")
	after.Mode = 0o600
	after.ChangeTime = infoTime.Add(50 * 24 * time.Hour)
	dirBefore := model.TreeNode{Name: "etc", Type: model.NodeTypeDir, Mode: 0o20000000755, ModTime: infoTime, AccessTime: infoTime, ChangeTime: infoTime, Subtree: "aa"}
	dirAfter := dirBefore
	dirAfter.Subtree = "bb"
	return map[string]model.TreeNode{
		treeKey(infoFirstID, "/etc/passwd"):  before,
		treeKey(infoSecondID, "/etc/passwd"): after,
		treeKey(infoSecondID, "/etc/wiki"):   infoFile("wiki"),
		treeKey(infoFirstID, "/etc"):         dirBefore,
		treeKey(infoSecondID, "/etc"):        dirAfter,
	}
}

func infoApp(t *testing.T, r stubRestic) *app.App {
	t.Helper()
	a := detailApp(t)
	r.snaps = []model.Snapshot{{Hostname: "h"}}
	if r.diffEntries == nil {
		r.diffEntries = infoEntries()
	}
	a.Restic = r
	return a
}

// openInfoDiff loads the fixture pair's diff with the cursor on path.
func openInfoDiff(t *testing.T, a *app.App, path string) Model {
	t.Helper()
	m := newTestModel(t, a)
	m = update(t, m, press("enter"))
	m = update(t, m, press("t"))
	m = update(t, m, press("j"))
	m = update(t, m, press("j"))
	m = update(t, m, press("t"))
	next, cmd := m.Update(press("d"))
	m = drivePastDiff(t, next.(Model), cmd)
	if m.diffOlder.ID != infoFirstID || m.diffNewer.ID != infoSecondID {
		t.Fatalf("precondition: pair = (%q, %q)", m.diffOlder.ID, m.diffNewer.ID)
	}
	m = m.rebuildDiffRows(model.DiffParentOf(path), path)
	if r := m.selectedDiffRow(); r == nil || r.Path != path {
		t.Fatalf("precondition: selected %+v, want %s", r, path)
	}
	return m
}

// pressInfo opens the info screen and delivers the lookup's result.
func pressInfo(t *testing.T, m Model) Model {
	t.Helper()
	next, cmd := m.Update(press("i"))
	m = next.(Model)
	if m.view != diffInfoView || !m.diffInfoLoading() || cmd == nil {
		t.Fatalf("i should open a loading info screen: view=%d loading=%v cmd=%v", m.view, m.diffInfoLoading(), cmd != nil)
	}
	if body := stripANSI(m.diffInfoBody()); !strings.Contains(body, "loading… reading") {
		t.Errorf("loading body should say what it reads:\n%s", body)
	}
	msg, ok := cmd().(diffInfoMsg)
	if !ok {
		t.Fatal("info command should return a diffInfoMsg")
	}
	return update(t, m, msg)
}

func TestDiffInfoNamesChangedMetadata(t *testing.T) {
	cap := &stubTreeCapture{}
	m := openInfoDiff(t, infoApp(t, stubRestic{treeNodes: infoNodes(), treeCap: cap}), "/etc/passwd")
	m = pressInfo(t, m)

	lookups := cap.snapshot()
	slices.Sort(lookups)
	if want := []string{treeKey(infoSecondID, "/etc/passwd"), treeKey(infoFirstID, "/etc/passwd")}; !slices.Equal(lookups, want) {
		t.Errorf("lookups = %q, want %q", lookups, want)
	}
	body := stripANSI(m.diffInfoBody())
	for _, want := range []string{
		"Path       /etc/passwd",
		"U metadata only · differs in mode and ctime",
		"≠ Mode         -rw-r--r--",
		"-rw-------",
		"  Owner        root (0)",
		"≠ ctime        2026-04-02 09:14:07",
		"2026-05-22 09:14:07",
		"  Contents     identical",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q\n---\n%s", want, body)
		}
	}
	for _, unwanted := range []string{"atime", "≠ Owner", "loading"} {
		if strings.Contains(body, unwanted) {
			t.Errorf("body should not contain %q\n---\n%s", unwanted, body)
		}
	}
	if title := stripANSI(m.diffInfoTitle()); !strings.HasPrefix(title, "info: repo-a · s1 ") || !strings.Contains(title, " → s3 ") {
		t.Errorf("title = %q", title)
	}
}

// An added path exists only in the second snapshot, so the first is not read.
func TestDiffInfoAddedPathReadsOneSide(t *testing.T) {
	cap := &stubTreeCapture{}
	m := openInfoDiff(t, infoApp(t, stubRestic{treeNodes: infoNodes(), treeCap: cap}), "/etc/wiki")
	m = pressInfo(t, m)
	if got, want := cap.snapshot(), []string{treeKey(infoSecondID, "/etc/wiki")}; !slices.Equal(got, want) {
		t.Errorf("lookups = %q, want %q", got, want)
	}
	body := stripANSI(m.diffInfoBody())
	if !strings.Contains(body, "+ only in the second snapshot\n") {
		t.Errorf("verdict should restate + without comparing\n---\n%s", body)
	}
	if !strings.Contains(body, "  Type         —") || strings.Contains(body, "≠") || strings.Contains(body, "Contents") {
		t.Errorf("the absent side should show dashes and nothing compared\n---\n%s", body)
	}
}

// restic marks every directory above a change U; the screen says when that is
// the only reason.
func TestDiffInfoDirectoryMarkedForItsContents(t *testing.T) {
	m := openInfoDiff(t, infoApp(t, stubRestic{treeNodes: infoNodes()}), "/etc")
	m = pressInfo(t, m)
	body := stripANSI(m.diffInfoBody())
	for _, want := range []string{
		"Path       /etc/",
		"U metadata only · differs in contents",
		"≠ Contents     changed below",
		"restic marks a directory U when anything below it changes; its own metadata is unchanged.",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q\n---\n%s", want, body)
		}
	}
}

func TestDiffInfoLookupFailureShown(t *testing.T) {
	m := openInfoDiff(t, infoApp(t, stubRestic{treeErr: errors.New("restic cat failed: exit status 1\ndetail")}), "/etc/passwd")
	m = pressInfo(t, m)
	body := stripANSI(m.diffInfoBody())
	if !strings.Contains(body, "  s1: restic cat failed: exit status 1\n") || !strings.Contains(body, "  s3: restic cat failed") {
		t.Errorf("both failed sides should report their first error line\n---\n%s", body)
	}
	if strings.Contains(body, "differs in") {
		t.Errorf("nothing can be compared without records\n---\n%s", body)
	}
}

func TestDiffInfoMissingRecordShown(t *testing.T) {
	m := openInfoDiff(t, infoApp(t, stubRestic{}), "/etc/passwd")
	m = pressInfo(t, m)
	if body := stripANSI(m.diffInfoBody()); !strings.Contains(body, "  s1: no entry at this path") {
		t.Errorf("a missing record should be named\n---\n%s", body)
	}
}

// esc, q, and i all return to the listing, cancel the lookup, and drop its
// late result.
func TestDiffInfoCloseCancelsLookup(t *testing.T) {
	for _, k := range []string{"esc", "q", "i"} {
		t.Run(k, func(t *testing.T) {
			m := openInfoDiff(t, infoApp(t, stubRestic{treeNodes: infoNodes()}), "/etc/passwd")
			next, cmd := m.Update(press("i"))
			m = next.(Model)
			stale := cmd().(diffInfoMsg)

			m = update(t, m, press(k))
			if m.view != snapshotDiffView || m.diffInfoLoading() || m.diffInfoRow.Path != "" {
				t.Fatalf("%s: view=%d loading=%v row=%q, want the listing with info cleared", k, m.view, m.diffInfoLoading(), m.diffInfoRow.Path)
			}
			if r := m.selectedDiffRow(); r == nil || r.Path != "/etc/passwd" {
				t.Errorf("%s: cursor moved to %+v", k, r)
			}
			m = update(t, m, stale)
			if m.view != snapshotDiffView || m.diffInfoFirst.res.Found {
				t.Errorf("%s: a late result must not reopen or fill the screen", k)
			}
		})
	}
}

func TestDiffInfoCloseCancelsRunningRestic(t *testing.T) {
	a := infoApp(t, stubRestic{treeNodes: infoNodes()})
	m := openInfoDiff(t, a, "/etc/passwd")
	a.Restic = blockingRestic{started: make(chan struct{})}
	next, cmd := m.Update(press("i"))
	m = next.(Model)
	done := make(chan tea.Msg, 1)
	go func() { done <- cmd() }()
	if m = update(t, m, press("esc")); m.view != snapshotDiffView {
		t.Fatalf("esc should close info, view = %d", m.view)
	}
	select {
	case msg := <-done:
		if im := msg.(diffInfoMsg); !errors.Is(im.first.Err, context.Canceled) || !errors.Is(im.second.Err, context.Canceled) {
			t.Errorf("cancelled lookups returned %+v", im)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("esc did not cancel the running lookups")
	}
}

func TestDiffInfoIgnoredWithoutSelection(t *testing.T) {
	m := openInfoDiff(t, infoApp(t, stubRestic{}), "/etc/passwd")
	m.diffRows = nil
	next, cmd := m.Update(press("i"))
	if next.(Model).view != snapshotDiffView || cmd != nil {
		t.Error("i with no selected row should do nothing")
	}
}

// Leaving the diff from anywhere drops the records with the rest of its paths.
func TestClearSnapshotDiffDropsInfo(t *testing.T) {
	m := openInfoDiff(t, infoApp(t, stubRestic{treeNodes: infoNodes()}), "/etc/passwd")
	m = pressInfo(t, m)
	m = m.clearSnapshotDiff()
	if m.diffInfoRow.Path != "" || m.diffInfoFirst.res.Found || m.diffInfoSecond.res.Found {
		t.Error("clearSnapshotDiff should drop the info records")
	}
}

func TestDiffInfoScrollsOnShortTerminal(t *testing.T) {
	m := openInfoDiff(t, infoApp(t, stubRestic{treeNodes: infoNodes()}), "/etc/passwd")
	m = pressInfo(t, m)
	m = update(t, m, tea.WindowSizeMsg{Width: 100, Height: 10})
	if !m.diffInfoScrollable() {
		t.Fatal("precondition: body should overflow a 10-row terminal")
	}
	if footer := stripANSI(m.footerView()); !strings.Contains(footer, "↑/↓ scroll") || !strings.Contains(footer, "q back") {
		t.Errorf("footer = %q, want scroll and back", footer)
	}
	m = update(t, m, press("j"))
	if m.diffInfoScroll != 1 {
		t.Errorf("scroll = %d, want 1", m.diffInfoScroll)
	}
	if body := stripANSI(m.diffInfoBody()); !strings.Contains(body, "showing lines 2–") {
		t.Errorf("scrolled body should show its line range\n---\n%s", body)
	}
}

// The footer trades x swap for i info; swap stays in the full help.
func TestSnapshotDiffFooterAdvertisesInfo(t *testing.T) {
	h := viewHelp{keys: defaultKeys(), view: snapshotDiffView}
	hasKey := func(bs []key.Binding, k, desc string) bool {
		for _, b := range bs {
			if b.Help().Key == k && b.Help().Desc == desc {
				return true
			}
		}
		return false
	}
	if !hasKey(h.ShortHelp(), "i", "info") {
		t.Errorf("diff footer should include i info, got %+v", h.ShortHelp())
	}
	if hasKey(h.ShortHelp(), "x", "swap") {
		t.Error("x swap moved to the help overlay to keep the footer within 100 columns")
	}
	if !slices.ContainsFunc(h.FullHelp(), func(g []key.Binding) bool { return hasKey(g, "x", "swap") && hasKey(g, "i", "info") }) {
		t.Errorf("full help should keep x swap beside i info, got %+v", h.FullHelp())
	}
}

func TestDiffInfoFieldsTimes(t *testing.T) {
	a, b := infoFile("f"), infoFile("f")
	b.ModTime = a.ModTime.Add(250 * time.Millisecond)
	b.AccessTime = b.ModTime // restic's default without --with-atime
	fields := diffInfoFields(&a, &b)
	mtime := fieldNamed(t, fields, "mtime")
	if !mtime.changed || mtime.first != "2026-04-02 09:14:07.000000000" || mtime.second != "2026-04-02 09:14:07.250000000" {
		t.Errorf("sub-second mtime change = %+v, want nanosecond precision", mtime)
	}
	if ctime := fieldNamed(t, fields, "ctime"); ctime.first != "2026-04-02 09:14:07" {
		t.Errorf("unchanged ctime = %q, want seconds", ctime.first)
	}
	// With restic's default, atime copies mtime and is not worth a row.
	if slices.ContainsFunc(fields, func(f diffInfoField) bool { return f.name == "atime" }) {
		t.Error("atime equal to mtime should be hidden")
	}
	b.AccessTime = b.ModTime.Add(time.Hour)
	if f := fieldNamed(t, diffInfoFields(&a, &b), "atime"); !f.changed {
		t.Errorf("a recorded atime should show, got %+v", f)
	}
}

func TestDiffInfoFieldsOptionalRows(t *testing.T) {
	link := model.TreeNode{Type: "symlink", Mode: 0o777, LinkTarget: "/srv/a"}
	relinked := link
	relinked.LinkTarget = "/srv/b"
	fields := diffInfoFields(&link, &relinked)
	if f := fieldNamed(t, fields, "link target"); !f.changed || f.first != "/srv/a" || f.second != "/srv/b" {
		t.Errorf("link target = %+v", f)
	}
	for _, f := range fields {
		if f.name == "size" || f.name == "contents" || f.name == "inode" {
			t.Errorf("a symlink without those fields should not show %q", f.name)
		}
	}

	a, b := infoFile("f"), infoFile("f")
	b.Size++
	if f := fieldNamed(t, diffInfoFields(&a, &b), "size"); f.first != "2.8 KiB (2873 B)" || f.second != "2.8 KiB (2874 B)" {
		t.Errorf("sizes that round the same should show bytes, got %+v", f)
	}
	a.Xattrs = []model.ExtendedAttribute{{Name: "user.tag", Value: []byte("x")}}
	b.Xattrs = []model.ExtendedAttribute{{Name: "user.tag", Value: []byte("y")}}
	if f := fieldNamed(t, diffInfoFields(&a, &b), "xattrs"); !f.changed || f.first != "user.tag" {
		t.Errorf("xattrs = %+v", f)
	}
	notes := diffInfoNotes(model.DiffRow{Kinds: model.KindMetadata}, &a, &b)
	if !slices.Contains(notes, "Extended attribute values differ: user.tag") {
		t.Errorf("notes = %q, want the attribute whose value changed", notes)
	}
}

// A wide terminal keeps the second record next to the first instead of halfway
// across the screen, and a long second value keeps the room left to it.
func TestDiffInfoTableFitsFirstColumn(t *testing.T) {
	m := openInfoDiff(t, infoApp(t, stubRestic{treeNodes: infoNodes()}), "/etc/passwd")
	m = pressInfo(t, m)
	first, second := infoFile("passwd"), infoFile("passwd")
	second.LinkTarget = "/srv/" + strings.Repeat("x", 60)
	fields := diffInfoFields(&first, &second)
	for _, line := range m.diffInfoTable(fields, 250) {
		line = stripANSI(line)
		if strings.HasPrefix(line, "  Type") && line != "  Type         file                 file" {
			t.Errorf("type row = %q, want the second record 2 columns after the widest first value", line)
		}
		if strings.HasPrefix(line, "  Link target") && !strings.HasSuffix(line, second.LinkTarget) {
			t.Errorf("link target row = %q, want the full second value", line)
		}
	}
}

// ctime moves on any inode update, so a change to it alone gets an explanation,
// wrapped to the terminal.
func TestDiffInfoNotesCtimeOnly(t *testing.T) {
	a, b := infoFile("f"), infoFile("f")
	b.ChangeTime = b.ChangeTime.Add(time.Hour)
	notes := diffInfoNotes(model.DiffRow{Kinds: model.KindMetadata}, &a, &b)
	if len(notes) != 1 || !strings.HasPrefix(notes[0], "Only ctime changed") {
		t.Fatalf("notes = %q, want the ctime explanation", notes)
	}
	b.Mode = 0o600
	if notes := diffInfoNotes(model.DiffRow{Kinds: model.KindMetadata}, &a, &b); len(notes) != 0 {
		t.Errorf("a mode change explains itself, got %q", notes)
	}

	nodes := infoNodes()
	ctimeOnly := nodes[treeKey(infoFirstID, "/etc/passwd")]
	ctimeOnly.ChangeTime = ctimeOnly.ChangeTime.Add(time.Hour)
	nodes[treeKey(infoSecondID, "/etc/passwd")] = ctimeOnly
	m := openInfoDiff(t, infoApp(t, stubRestic{treeNodes: nodes}), "/etc/passwd")
	m = pressInfo(t, m)
	body := stripANSI(m.diffInfoBody())
	if !strings.Contains(body, "  Only ctime changed, so something touched the inode") || !strings.Contains(body, "\n  a chmod or chown to the values it already had,") {
		t.Errorf("the note should wrap inside the 100-column terminal\n---\n%s", body)
	}
	for l := range strings.SplitSeq(body, "\n") {
		if w := lipgloss.Width(l); w > 100 {
			t.Errorf("line wider than the terminal (%d): %q", w, l)
		}
	}
}

// The comparison ignores stored order, as restic does, so the table must too:
// a reorder alone neither reads as different nor earns a note.
func TestDiffInfoXattrsSortedLikeTheComparison(t *testing.T) {
	a, b := infoFile("f"), infoFile("f")
	a.Xattrs = []model.ExtendedAttribute{{Name: "user.b", Value: []byte("2")}, {Name: "user.a", Value: []byte("1")}}
	b.Xattrs = []model.ExtendedAttribute{{Name: "user.a", Value: []byte("1")}, {Name: "user.b", Value: []byte("2")}}
	f := fieldNamed(t, diffInfoFields(&a, &b), "xattrs")
	if f.changed || f.first != "user.a, user.b" || f.second != f.first {
		t.Errorf("reordered xattrs = %+v, want identical sorted cells", f)
	}
	if notes := diffInfoNotes(model.DiffRow{Kinds: model.KindMetadata}, &a, &b); len(notes) != 0 {
		t.Errorf("a reorder is not a change, got %q", notes)
	}
}

// Generic attributes show names only, so a value-only change is named in a
// note, never printed.
func TestDiffInfoNamesChangedGenericAttribute(t *testing.T) {
	a, b := infoFile("f"), infoFile("f")
	a.GenericAttrs = map[string]json.RawMessage{
		"windows.file_attributes": json.RawMessage(`32`),
		"windows.creation_time":   json.RawMessage(`"secret-bytes-a"`),
	}
	b.GenericAttrs = map[string]json.RawMessage{
		"windows.file_attributes": json.RawMessage(`32`),
		"windows.creation_time":   json.RawMessage(`"secret-bytes-b"`),
	}
	f := fieldNamed(t, diffInfoFields(&a, &b), "attributes")
	if !f.changed || f.first != "windows.creation_time, windows.file_attributes" || f.second != f.first {
		t.Errorf("attributes = %+v, want matching sorted names marked changed", f)
	}
	notes := diffInfoNotes(model.DiffRow{Kinds: model.KindMetadata}, &a, &b)
	if !slices.Equal(notes, []string{"Attribute values differ: windows.creation_time"}) {
		t.Errorf("notes = %q, want the changed attribute named", notes)
	}
	if joined := strings.Join(notes, "\n") + f.first + f.second; strings.Contains(joined, "secret-bytes") {
		t.Errorf("attribute values must never be shown: %q", joined)
	}
}

func TestDiffInfoNotesBitrot(t *testing.T) {
	a, b := infoFile("f"), infoFile("f")
	b.Content = []string{"dd44"}
	row := model.DiffRow{Kinds: model.KindModified | model.KindBitrot}
	if notes := diffInfoNotes(row, &a, &b); len(notes) != 1 || !strings.Contains(notes[0], "bitrot") {
		t.Errorf("notes = %q, want the bitrot explanation", notes)
	}
	b.ModTime = b.ModTime.Add(time.Second)
	if notes := diffInfoNotes(row, &a, &b); len(notes) != 0 {
		t.Errorf("a changed mtime rules out bitrot, got %q", notes)
	}
}

func TestJoinWords(t *testing.T) {
	for _, tc := range []struct {
		in   []string
		want string
	}{
		{nil, ""},
		{[]string{"mode"}, "mode"},
		{[]string{"mode", "ctime"}, "mode and ctime"},
		{[]string{"mode", "owner", "ctime"}, "mode, owner, and ctime"},
	} {
		if got := joinWords(tc.in); got != tc.want {
			t.Errorf("joinWords(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func fieldNamed(t *testing.T, fields []diffInfoField, name string) diffInfoField {
	t.Helper()
	for _, f := range fields {
		if f.name == name {
			return f
		}
	}
	t.Fatalf("no %q field in %+v", name, fields)
	return diffInfoField{}
}
