package model

import (
	"path"
	"sort"
	"strings"
	"time"
)

// browse.go holds the pure types and tree builder for the in-app snapshot file
// browser. It is part of the leaf model package, so — like everything here — it
// never carries credentials and is safe to hold in memory. Browse data is built
// from a single streamed `restic ls --recursive`; the structures below are
// session-only and are deliberately NOT persisted to the cache or any log (the
// privacy contract of the browse feature lives at the app/tui layer, but this
// package is kept free of any Save path on purpose).

// approxBytesPerEntry is a coarse per-node memory estimate used only to give the
// user rough guidance on how much a tree is retaining. It is intentionally an
// order-of-magnitude figure (path + name strings + struct overhead), not a
// measured value, and is derived from the entry count rather than JSON bytes.
const approxBytesPerEntry = 256

// BrowseLimits are the five caps that bound one browse crawl. The initial caps
// (MaxEntries/MaxJSONBytes/Timeout) bound a single run; the session ceilings
// (MaxSessionEntries/MaxSessionJSONBytes) bound how far repeated load-more can
// raise the entry/byte caps. Timeout is never raised by load-more.
type BrowseLimits struct {
	MaxEntries          int
	MaxJSONBytes        int64
	Timeout             time.Duration
	MaxSessionEntries   int
	MaxSessionJSONBytes int64
}

// PartialReason records whether a scan returned the whole namespace or stopped
// early, and if so which cap stopped it. Only an entry or byte cap can be raised
// by load-more; a complete scan or a timeout cannot.
type PartialReason int

const (
	BrowseComplete  PartialReason = iota // restic emitted the whole tree (clean EOF)
	PartialEntryCap                      // stopped at MaxEntries
	PartialByteCap                       // stopped at MaxJSONBytes
	PartialTimeout                       // stopped at Timeout with at least one node
)

// String renders the reason as the short status the browse header/footer show.
func (r PartialReason) String() string {
	switch r {
	case PartialEntryCap:
		return "partial: entry cap"
	case PartialByteCap:
		return "partial: byte cap"
	case PartialTimeout:
		return "partial: timeout"
	default:
		return "complete"
	}
}

// Partial reports whether the scan stopped before the namespace was exhausted.
func (r PartialReason) Partial() bool { return r != BrowseComplete }

// BrowseNode is one flat node as decoded from `restic ls --json`. Path is the
// absolute path within the snapshot; the builder turns the flat stream into a
// tree keyed by Path.
type BrowseNode struct {
	Path  string
	Name  string
	IsDir bool
	Size  int64
}

// BrowseFrontier is the last node the scan decoded before it was cut off. It is
// used to mark which directories are known-incomplete: every ancestor of the
// frontier (and the frontier itself when it is a directory) may have unseen
// children. It is the zero value for a complete scan.
type BrowseFrontier struct {
	Path  string
	IsDir bool
}

// BrowseScan is resticx's raw streamed output for one crawl: the ordered nodes,
// why it stopped, how much it read, and the frontier. It carries no tree
// structure — BuildBrowseTree turns it into one.
type BrowseScan struct {
	Nodes         []BrowseNode
	Reason        PartialReason
	LoadedEntries int
	JSONBytes     int64
	Frontier      BrowseFrontier
}

// BrowseEntry is one node in the built tree. Incomplete marks a directory whose
// children are known to be only partially loaded (a synthetic parent, or an
// ancestor/frontier of a truncated crawl); the renderer shows such a directory
// with a "more entries not loaded" row so it is never presented as complete.
type BrowseEntry struct {
	Path       string
	Name       string
	IsDir      bool
	Size       int64
	Children   []*BrowseEntry
	Incomplete bool
}

// HasMoreRow reports whether listing this directory should append the synthetic
// "more entries not loaded" row. Only incomplete directories qualify.
func (e *BrowseEntry) HasMoreRow() bool {
	return e != nil && e.IsDir && e.Incomplete
}

// BrowseTree is the built structure: the root entry plus a path index. ByPath
// makes current-directory lookup, selection restore after a reload, and
// parent-fallback O(1).
type BrowseTree struct {
	Root   *BrowseEntry
	ByPath map[string]*BrowseEntry
}

// BrowseResult is the app-level outcome of one load: the built tree plus the
// outcome metadata the TUI needs to label status, decide whether load-more is
// possible, and show rough retained-memory guidance. BrowseTree is structure
// only; BrowseResult is structure plus outcome.
type BrowseResult struct {
	Tree                *BrowseTree
	Reason              PartialReason
	LoadedEntries       int
	JSONBytes           int64
	Limits              BrowseLimits
	Frontier            BrowseFrontier
	ApproxRetainedBytes int64
}

// BuildBrowseTree turns a flat, ordered scan into a tree keyed by path. It does
// not depend on restic's emission order: nodes are placed under their parent by
// path, missing parents are synthesized (and marked Incomplete so a truncated or
// malformed stream cannot panic or masquerade as complete), and each directory's
// children are sorted dirs-first then case-insensitively by name for display.
// When the scan is partial, the frontier rule marks the known-incomplete dirs.
func BuildBrowseTree(scan BrowseScan, limits BrowseLimits) BrowseResult {
	tree := &BrowseTree{ByPath: make(map[string]*BrowseEntry, len(scan.Nodes)+1)}
	tree.Root = &BrowseEntry{Path: "/", Name: "/", IsDir: true}
	tree.ByPath["/"] = tree.Root

	for _, n := range scan.Nodes {
		insertNode(tree, n)
	}
	for _, e := range tree.ByPath {
		sortChildren(e.Children)
	}
	if scan.Reason.Partial() {
		markFrontier(tree, scan.Frontier)
	}

	return BrowseResult{
		Tree:                tree,
		Reason:              scan.Reason,
		LoadedEntries:       scan.LoadedEntries,
		JSONBytes:           scan.JSONBytes,
		Limits:              limits,
		Frontier:            scan.Frontier,
		ApproxRetainedBytes: approxRetainedBytes(scan.LoadedEntries),
	}
}

// insertNode places one node under its parent directory, creating the entry if
// it is new or filling in real data over a previously synthesized placeholder.
func insertNode(tree *BrowseTree, n BrowseNode) {
	parent := ensureDir(tree, path.Dir(n.Path))
	name := n.Name
	if name == "" {
		name = path.Base(n.Path)
	}
	if e, ok := tree.ByPath[n.Path]; ok {
		// A child arrived before this node and synthesized it as a placeholder;
		// fill in the real metadata and clear the synthetic-incomplete flag.
		e.Name = name
		e.IsDir = n.IsDir
		e.Size = n.Size
		e.Incomplete = false
		return
	}
	e := &BrowseEntry{Path: n.Path, Name: name, IsDir: n.IsDir, Size: n.Size}
	tree.ByPath[n.Path] = e
	parent.Children = append(parent.Children, e)
}

// ensureDir returns the entry for path p, creating synthetic Incomplete parent
// directories up to the root for any missing ancestor. The root always exists
// (BuildBrowseTree seeds it), so recursion terminates there.
func ensureDir(tree *BrowseTree, p string) *BrowseEntry {
	if e, ok := tree.ByPath[p]; ok {
		return e
	}
	e := &BrowseEntry{Path: p, Name: path.Base(p), IsDir: true, Incomplete: true}
	tree.ByPath[p] = e
	parent := ensureDir(tree, path.Dir(p))
	parent.Children = append(parent.Children, e)
	return e
}

// sortChildren orders a directory's children for display: directories first,
// then case-insensitively by name. Stable so equal keys keep insertion order.
func sortChildren(children []*BrowseEntry) {
	sort.SliceStable(children, func(i, j int) bool {
		a, b := children[i], children[j]
		if a.IsDir != b.IsDir {
			return a.IsDir
		}
		return strings.ToLower(a.Name) < strings.ToLower(b.Name)
	})
}

// markFrontier marks the directories that a truncated crawl could not finish.
// restic ls --recursive emits a pre-order DFS, so when the stream is cut at the
// frontier, every ancestor directory of the frontier may still have unseen
// siblings, and the frontier itself — when it is a directory — has unseen
// children. A frontier file is a fully-known leaf, so only its ancestors are
// marked. An empty frontier (truncated before any node) marks the root.
func markFrontier(tree *BrowseTree, f BrowseFrontier) {
	if f.Path == "" || f.Path == "/" {
		tree.Root.Incomplete = true
		return
	}
	for _, d := range ancestorDirs(f.Path) {
		if e, ok := tree.ByPath[d]; ok {
			e.Incomplete = true
		}
	}
	if f.IsDir {
		if e, ok := tree.ByPath[f.Path]; ok {
			e.Incomplete = true
		}
	}
}

// ancestorDirs returns the directory paths strictly above p, root first:
// "/a/b/c.txt" -> ["/", "/a", "/a/b"]. It excludes p itself.
func ancestorDirs(p string) []string {
	dirs := []string{"/"}
	trimmed := strings.Trim(p, "/")
	if trimmed == "" {
		return dirs
	}
	parts := strings.Split(trimmed, "/")
	cur := ""
	for i := 0; i < len(parts)-1; i++ {
		cur += "/" + parts[i]
		dirs = append(dirs, cur)
	}
	return dirs
}

// approxRetainedBytes gives rough guidance on the memory a tree retains, derived
// from the loaded entry count rather than JSON bytes read (the JSON is streamed
// and discarded; what we keep is the node structs).
func approxRetainedBytes(loadedEntries int) int64 {
	return int64(loadedEntries) * approxBytesPerEntry
}

// CanLoadMore reports whether reloading with raised caps could return more than
// the current run did. Only the cap that actually stopped the run is considered,
// and only when doubling it (clamped to the session ceiling) would exceed the
// current cap. A complete scan, a timeout, or a cap already at its ceiling
// cannot load more.
func CanLoadMore(reason PartialReason, limits BrowseLimits) bool {
	switch reason {
	case PartialEntryCap:
		return min(limits.MaxEntries*2, limits.MaxSessionEntries) > limits.MaxEntries
	case PartialByteCap:
		return min(limits.MaxJSONBytes*2, limits.MaxSessionJSONBytes) > limits.MaxJSONBytes
	default:
		return false
	}
}

// NextBrowseLimits doubles both the entry and byte caps for a load-more, each
// clamped to its session ceiling. It never raises Timeout and never mutates the
// session ceilings; the caller passes the result back as the live limits.
func NextBrowseLimits(limits BrowseLimits) BrowseLimits {
	next := limits
	next.MaxEntries = min(limits.MaxEntries*2, limits.MaxSessionEntries)
	next.MaxJSONBytes = min(limits.MaxJSONBytes*2, limits.MaxSessionJSONBytes)
	return next
}
