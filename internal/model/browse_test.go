package model

import (
	"testing"
	"time"
)

// scanOf builds a BrowseScan from a set of nodes with the given stopping reason
// and frontier, computing LoadedEntries from the node count.
func scanOf(reason PartialReason, frontier BrowseFrontier, nodes ...BrowseNode) BrowseScan {
	return BrowseScan{
		Nodes:         nodes,
		Reason:        reason,
		LoadedEntries: len(nodes),
		Frontier:      frontier,
	}
}

func dir(p, name string) BrowseNode { return BrowseNode{Path: p, Name: name, IsDir: true} }
func file(p, name string, size int64) BrowseNode {
	return BrowseNode{Path: p, Name: name, Size: size}
}

func childNames(e *BrowseEntry) []string {
	names := make([]string, len(e.Children))
	for i, c := range e.Children {
		names[i] = c.Name
	}
	return names
}

func eq(a, b []string) bool {
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

func TestBuildBrowseTreeParentChild(t *testing.T) {
	scan := scanOf(BrowseComplete, BrowseFrontier{},
		dir("/home", "home"),
		dir("/home/alex", "alex"),
		file("/home/alex/notes.txt", "notes.txt", 12),
		dir("/etc", "etc"),
	)
	res := BuildBrowseTree(scan, BrowseLimits{})
	tree := res.Tree

	if tree.Root.Path != "/" || !tree.Root.IsDir {
		t.Fatalf("root not a directory: %+v", tree.Root)
	}
	if tree.Root.Incomplete {
		t.Errorf("complete tree marked root incomplete")
	}
	// Root children: /etc and /home, dirs-first then name.
	if got := childNames(tree.Root); !eq(got, []string{"etc", "home"}) {
		t.Errorf("root children = %v, want [etc home]", got)
	}
	alex, ok := tree.ByPath["/home/alex"]
	if !ok {
		t.Fatalf("ByPath missing /home/alex")
	}
	if got := childNames(alex); !eq(got, []string{"notes.txt"}) {
		t.Errorf("/home/alex children = %v, want [notes.txt]", got)
	}
	notes := tree.ByPath["/home/alex/notes.txt"]
	if notes == nil || notes.IsDir || notes.Size != 12 {
		t.Errorf("notes.txt entry wrong: %+v", notes)
	}
	if res.LoadedEntries != 4 {
		t.Errorf("LoadedEntries = %d, want 4", res.LoadedEntries)
	}
}

func TestBuildBrowseTreeDirsFirstCaseInsensitive(t *testing.T) {
	scan := scanOf(BrowseComplete, BrowseFrontier{},
		file("/Zebra.txt", "Zebra.txt", 1),
		file("/apple.txt", "apple.txt", 1),
		dir("/Music", "Music"),
		dir("/docs", "docs"),
	)
	res := BuildBrowseTree(scan, BrowseLimits{})
	// dirs first (docs, Music — case-insensitive: d < m), then files (apple, Zebra).
	if got := childNames(res.Tree.Root); !eq(got, []string{"docs", "Music", "apple.txt", "Zebra.txt"}) {
		t.Errorf("sort order = %v, want [docs Music apple.txt Zebra.txt]", got)
	}
}

func TestBuildBrowseTreeFrontierOnDir(t *testing.T) {
	scan := scanOf(PartialEntryCap, BrowseFrontier{Path: "/home/alex/photos", IsDir: true},
		dir("/home", "home"),
		dir("/home/alex", "alex"),
		dir("/home/alex/photos", "photos"),
	)
	tree := BuildBrowseTree(scan, BrowseLimits{}).Tree

	for _, p := range []string{"/", "/home", "/home/alex", "/home/alex/photos"} {
		e := tree.ByPath[p]
		if e == nil || !e.Incomplete {
			t.Errorf("expected %s incomplete, got %+v", p, e)
		}
	}
}

func TestBuildBrowseTreeFrontierOnFile(t *testing.T) {
	scan := scanOf(PartialByteCap, BrowseFrontier{Path: "/home/alex/big.iso", IsDir: false},
		dir("/home", "home"),
		dir("/home/alex", "alex"),
		file("/home/alex/big.iso", "big.iso", 999),
	)
	tree := BuildBrowseTree(scan, BrowseLimits{}).Tree

	// Ancestors are incomplete; the frontier file itself is a known leaf.
	for _, p := range []string{"/", "/home", "/home/alex"} {
		if e := tree.ByPath[p]; e == nil || !e.Incomplete {
			t.Errorf("expected %s incomplete, got %+v", p, e)
		}
	}
	if e := tree.ByPath["/home/alex/big.iso"]; e == nil || e.Incomplete {
		t.Errorf("frontier file should not be incomplete: %+v", e)
	}
}

func TestBuildBrowseTreeSyntheticParent(t *testing.T) {
	// A node whose parent directory was never emitted must not panic; the parent
	// is synthesized and marked Incomplete.
	scan := scanOf(BrowseComplete, BrowseFrontier{},
		file("/var/log/syslog", "syslog", 5),
	)
	tree := BuildBrowseTree(scan, BrowseLimits{}).Tree

	for _, p := range []string{"/var", "/var/log"} {
		e := tree.ByPath[p]
		if e == nil {
			t.Fatalf("synthetic parent %s missing", p)
		}
		if !e.IsDir || !e.Incomplete {
			t.Errorf("synthetic parent %s should be an incomplete dir: %+v", p, e)
		}
	}
	if !tree.ByPath["/var/log"].HasMoreRow() {
		t.Errorf("incomplete dir should expose HasMoreRow")
	}
	syslog := tree.ByPath["/var/log/syslog"]
	if syslog == nil || syslog.Incomplete {
		t.Errorf("real leaf node should not be incomplete: %+v", syslog)
	}
}

func TestBuildBrowseTreeRealNodeClearsSynthetic(t *testing.T) {
	// Child arrives before its directory's own node, then the directory's real
	// node arrives: Incomplete must be cleared and metadata filled.
	scan := scanOf(BrowseComplete, BrowseFrontier{},
		file("/a/b/c.txt", "c.txt", 1),
		dir("/a/b", "b"),
		dir("/a", "a"),
	)
	tree := BuildBrowseTree(scan, BrowseLimits{}).Tree
	for _, p := range []string{"/a", "/a/b"} {
		if e := tree.ByPath[p]; e == nil || e.Incomplete {
			t.Errorf("%s should be complete after real node arrived: %+v", p, e)
		}
	}
}

func TestBuildBrowseTreeNormalizesPaths(t *testing.T) {
	// A path missing its leading slash or carrying a trailing one must land on the
	// same normalized key as its canonical spelling, so a directory is never split
	// across two keys (or duplicated as a child).
	scan := scanOf(BrowseComplete, BrowseFrontier{},
		dir("home", "home"),        // missing leading slash
		dir("/home/alex/", "alex"), // trailing slash
		file("/home/alex/notes.txt", "notes.txt", 7),
	)
	tree := BuildBrowseTree(scan, BrowseLimits{}).Tree

	for _, p := range []string{"/home", "/home/alex", "/home/alex/notes.txt"} {
		if tree.ByPath[p] == nil {
			t.Errorf("ByPath missing normalized key %s", p)
		}
	}
	for _, p := range []string{"home", "/home/alex/"} {
		if tree.ByPath[p] != nil {
			t.Errorf("un-normalized key %q should not exist in the tree", p)
		}
	}
	// One child each proves the spellings collapsed to a single directory rather
	// than duplicating it.
	if got := childNames(tree.ByPath["/home"]); !eq(got, []string{"alex"}) {
		t.Errorf("/home children = %v, want [alex]", got)
	}
	if got := childNames(tree.ByPath["/home/alex"]); !eq(got, []string{"notes.txt"}) {
		t.Errorf("/home/alex children = %v, want [notes.txt]", got)
	}
}

func TestByPathResolvesEveryNode(t *testing.T) {
	scan := scanOf(BrowseComplete, BrowseFrontier{},
		dir("/a", "a"),
		dir("/a/b", "b"),
		file("/a/b/c", "c", 1),
		file("/a/d", "d", 2),
	)
	tree := BuildBrowseTree(scan, BrowseLimits{}).Tree
	for _, p := range []string{"/", "/a", "/a/b", "/a/b/c", "/a/d"} {
		if tree.ByPath[p] == nil {
			t.Errorf("ByPath missing %s", p)
		}
	}
}

func TestHasMoreRow(t *testing.T) {
	dirInc := &BrowseEntry{IsDir: true, Incomplete: true}
	dirComplete := &BrowseEntry{IsDir: true}
	fileInc := &BrowseEntry{Incomplete: true}
	if !dirInc.HasMoreRow() {
		t.Error("incomplete dir should have more row")
	}
	if dirComplete.HasMoreRow() {
		t.Error("complete dir should not have more row")
	}
	if fileInc.HasMoreRow() {
		t.Error("file should never have more row")
	}
	var nilEntry *BrowseEntry
	if nilEntry.HasMoreRow() {
		t.Error("nil entry should not panic / should not have more row")
	}
}

func TestCanLoadMore(t *testing.T) {
	lim := BrowseLimits{
		MaxEntries: 100, MaxSessionEntries: 400,
		MaxJSONBytes: 1000, MaxSessionJSONBytes: 4000,
	}
	if !CanLoadMore(PartialEntryCap, lim) {
		t.Error("entry cap below ceiling should allow load more")
	}
	if !CanLoadMore(PartialByteCap, lim) {
		t.Error("byte cap below ceiling should allow load more")
	}
	if CanLoadMore(BrowseComplete, lim) {
		t.Error("complete must not load more")
	}
	if CanLoadMore(PartialTimeout, lim) {
		t.Error("timeout must not load more")
	}

	// At ceiling: doubling cannot exceed the current cap.
	atCeil := BrowseLimits{
		MaxEntries: 400, MaxSessionEntries: 400,
		MaxJSONBytes: 4000, MaxSessionJSONBytes: 4000,
	}
	if CanLoadMore(PartialEntryCap, atCeil) {
		t.Error("entry cap at ceiling must not load more")
	}
	if CanLoadMore(PartialByteCap, atCeil) {
		t.Error("byte cap at ceiling must not load more")
	}
}

func TestNextBrowseLimits(t *testing.T) {
	lim := BrowseLimits{
		MaxEntries: 100, MaxSessionEntries: 350,
		MaxJSONBytes: 1000, MaxSessionJSONBytes: 3500,
		Timeout: 30 * time.Second,
	}
	next := NextBrowseLimits(lim)
	if next.MaxEntries != 200 {
		t.Errorf("MaxEntries = %d, want 200", next.MaxEntries)
	}
	if next.MaxJSONBytes != 2000 {
		t.Errorf("MaxJSONBytes = %d, want 2000", next.MaxJSONBytes)
	}
	// Doubling again clamps to the session ceiling.
	next2 := NextBrowseLimits(next)
	if next2.MaxEntries != 350 {
		t.Errorf("clamped MaxEntries = %d, want 350", next2.MaxEntries)
	}
	if next2.MaxJSONBytes != 3500 {
		t.Errorf("clamped MaxJSONBytes = %d, want 3500", next2.MaxJSONBytes)
	}
	// Timeout and ceilings are untouched.
	if next.Timeout != 30*time.Second || next.MaxSessionEntries != 350 || next.MaxSessionJSONBytes != 3500 {
		t.Errorf("load-more raised timeout or ceiling: %+v", next)
	}
}

func TestApproxRetainedBytesTracksEntryCount(t *testing.T) {
	scan := scanOf(BrowseComplete, BrowseFrontier{},
		dir("/a", "a"),
		file("/a/b", "b", 9_000_000), // size must not affect the estimate
	)
	res := BuildBrowseTree(scan, BrowseLimits{})
	if want := int64(2) * approxBytesPerEntry; res.ApproxRetainedBytes != want {
		t.Errorf("ApproxRetainedBytes = %d, want %d", res.ApproxRetainedBytes, want)
	}
}

func TestPartialReasonString(t *testing.T) {
	cases := map[PartialReason]string{
		BrowseComplete:  "complete",
		PartialEntryCap: "partial: entry cap",
		PartialByteCap:  "partial: byte cap",
		PartialTimeout:  "partial: timeout",
	}
	for r, want := range cases {
		if got := r.String(); got != want {
			t.Errorf("%d.String() = %q, want %q", int(r), got, want)
		}
	}
}
