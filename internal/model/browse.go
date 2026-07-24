package model

import (
	"errors"
	"path"
	"strings"
	"time"
)

// Browser data crosses resticx, browsedb, app, and tui as flat values. Storage
// and privacy remain the responsibility of those layers, not model.

// ErrBrowseDiskLimit is returned, possibly wrapped, when indexing would exceed
// its disk limit. The sentinel carries no filename.
var ErrBrowseDiskLimit = errors.New("browse index disk limit reached")

// BrowseScanSummary reports how many nodes a streamed crawl accepted and
// whether restic emitted the complete tree.
type BrowseScanSummary struct {
	Entries    int
	IsComplete bool
}

// Node types retain restic's JSON spellings across browser layers.
const (
	NodeTypeFile = "file"
	NodeTypeDir  = "dir"
)

// BrowseNode is one flat node decoded from `restic ls --json`. Path is absolute
// within the snapshot. OwnerKnown distinguishes a reported UID:GID, including
// 0:0, from omitted ownership.
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

// BrowseEntry is one flat directory-listing row, with a cleaned Path and the
// metadata needed to render it without reading restic again.
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
	OwnerKnown  bool
}

// CleanBrowsePath returns a rooted, lexically clean namespace key. Empty input
// denotes the root.
func CleanBrowsePath(p string) string {
	if p == "" {
		return "/"
	}
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	return path.Clean(p)
}

// JoinBrowsePath joins a cleaned, rooted parent and non-empty child name into
// the child's absolute browse path.
func JoinBrowsePath(parent, name string) string {
	if parent == "/" {
		return "/" + name
	}
	return parent + "/" + name
}

// BrowseName returns the emitted name when present, otherwise the path base. It
// returns "/" for the root.
func BrowseName(name, p string) string {
	if name != "" {
		return name
	}
	if p == "/" {
		return "/"
	}
	return path.Base(p)
}
