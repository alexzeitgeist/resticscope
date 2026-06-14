package model

import (
	"errors"
	"path"
	"strings"
	"time"
)

// browse.go holds the pure types and shared path/name helpers for the in-app
// snapshot file browser. It is part of the leaf model package, so — like
// everything here — it never carries credentials. Browse data is streamed from a
// single `restic ls --recursive` and indexed into a session-scoped, encrypted-at-
// rest SQLite database (see internal/browsedb); these types are the flat DTOs
// that cross the resticx→browsedb→app→tui boundary. The persistence/privacy
// contract lives at the browsedb/app/tui layers; this package stays free of any
// Save path on purpose.

// ErrBrowseDiskLimit is returned (wrapped) by the browse store when indexing a
// snapshot would push the session directory past its configured disk ceiling.
// It is a path-free typed sentinel so app/tui can classify the limit with
// errors.Is without importing the store package, and so the surfaced error
// never carries a filename.
var ErrBrowseDiskLimit = errors.New("browse index disk limit reached")

// BrowseScanSummary reports the outcome of one streamed namespace crawl:
// how many nodes the callback accepted and whether restic emitted the whole
// tree (clean EOF) versus stopping early (e.g. its index timeout fired).
type BrowseScanSummary struct {
	Entries    int
	IsComplete bool
}

// Node types as restic spells them in its JSON output ("dir"/"file"/"symlink"/
// special). BrowseNode.Type and BrowseEntry.Type carry these raw strings;
// every layer that branches on them compares against these constants.
const (
	NodeTypeFile = "file"
	NodeTypeDir  = "dir"
)

// BrowseNode is one flat node as decoded from `restic ls --json`. Path is the
// absolute path within the snapshot. OwnerKnown distinguishes a node that
// carried uid/gid (so UID:GID is meaningful, including a real root-owned 0:0)
// from one that omitted them (rendered as a missing value, never as 0:0). Type
// is restic's raw node type ("dir"/"file"/"symlink"/special); IsDir is derived
// from it for renderer/query convenience. LinkTarget is a symlink's target, "".
type BrowseNode struct {
	Path        string
	Name        string
	Type        string
	LinkTarget  string
	IsDir       bool
	Size        int64
	ModTime     time.Time
	Permissions string
	UID, GID    uint32
	OwnerKnown  bool
}

// BrowseEntry is one row of a directory listing served from the browse store: a
// flat DTO with no tree structure (the store answers parent→children queries
// directly). It carries the same metadata as BrowseNode plus the cleaned Path,
// so the TUI renders names, sizes, mtimes, perms, owner, and symlink targets
// without re-reading restic.
type BrowseEntry struct {
	Path        string
	Name        string
	Type        string
	LinkTarget  string
	IsDir       bool
	Size        int64
	ModTime     time.Time
	Permissions string
	UID, GID    uint32
	OwnerKnown  bool // the node carried uid/gid; distinguishes real 0:0 from missing
}

// CleanBrowsePath normalizes a node path to a rooted, lexically clean key so the
// namespace is indexed consistently regardless of how restic spelled the path (a
// missing leading slash or a trailing slash must not split a directory across
// two keys). An empty path is treated as the root. It is the one shared path
// rule that resticx-stream, browsedb, and the TUI agree on.
func CleanBrowsePath(p string) string {
	if p == "" {
		return "/"
	}
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	return path.Clean(p)
}

// JoinBrowsePath joins a cleaned parent directory and a child name into the
// child's absolute browse path — the exact inverse of the path.Dir split done in
// browsedb's Add at index time. Parent is always a cleaned, rooted path and name
// is non-empty (the root node is never stored), so a direct concat is provably
// exact and avoids a per-row re-Clean.
func JoinBrowsePath(parent, name string) string {
	if parent == "/" {
		return "/" + name
	}
	return parent + "/" + name
}

// BrowseName picks a display name for a node: its emitted name when present,
// else the base of its (already cleaned) path, with the root shown as "/".
func BrowseName(name, p string) string {
	if name != "" {
		return name
	}
	if p == "/" {
		return "/"
	}
	return path.Base(p)
}
