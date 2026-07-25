package tui

import (
	"fmt"
	"maps"
	"slices"
	"sort"
	"strings"

	"github.com/alexzeitgeist/resticscope/internal/model"
)

// Snapshot grouping and tree-ID collapse form a pure display pipeline over the
// newest-first snapshot list. snapCursor indexes its flattened selectable
// nodes, excluding collapsed peers.

// snapGroupMode selects flat display or grouping by host, tags, or paths.
// Grouped displays include a fallback section for missing keys.
type snapGroupMode int

const (
	snapGroupOff snapGroupMode = iota
	snapGroupHost
	snapGroupTags
	snapGroupPaths
	snapGroupModeCount // Number of modes; used for cycling.
)

// label is the short noun used in the heading suffix ("· group: host").
func (m snapGroupMode) label() string {
	switch m {
	case snapGroupHost:
		return "host"
	case snapGroupTags:
		return "tags"
	case snapGroupPaths:
		return "paths"
	default:
		return ""
	}
}

// snapNode is one selectable snapshot row. peers contains collapsed snapshots
// so their marks and IDs can resolve to head; count controls the +N suffix.
type snapNode struct {
	head  model.Snapshot
	count int
	peers []model.Snapshot
}

// snapSection groups nodes under a heading. rawCount preserves the pre-collapse
// count for its badge, while noKey identifies the dimmed fallback bucket.
type snapSection struct {
	key      string
	title    string
	nodes    []snapNode
	rawCount int
	noKey    bool
}

// snapDisplay is the canonical render and action order. nodes is the flattened
// cursor order; sections is populated only when grouping, keeping selection,
// actions, and rendered marks aligned.
type snapDisplay struct {
	nodes    []snapNode
	sections []snapSection
}

// snapDisplay rebuilds the inexpensive display for each render or action.
// Grouping precedes collapse so runs cannot cross section boundaries.
func (m Model) snapDisplay() snapDisplay {
	return buildSnapDisplay(m.detailSnapshots(), m.snapGroupMode, m.snapCollapseTree)
}

func buildSnapDisplay(snaps []model.Snapshot, mode snapGroupMode, collapse bool) snapDisplay {
	if mode == snapGroupOff {
		nodes := snapNodes(snaps, collapse)
		return snapDisplay{nodes: nodes}
	}
	sections := snapSectionsBy(snaps, mode)
	if collapse {
		for i := range sections {
			sections[i].nodes = collapseTreeRuns(sectionRawSnaps(sections[i]))
		}
	}
	flat := make([]snapNode, 0, len(snaps))
	for _, s := range sections {
		flat = append(flat, s.nodes...)
	}
	return snapDisplay{nodes: flat, sections: sections}
}

// snapNodes builds flat nodes with optional tree-run collapse.
func snapNodes(snaps []model.Snapshot, collapse bool) []snapNode {
	if !collapse {
		out := make([]snapNode, len(snaps))
		for i, s := range snaps {
			out[i] = snapNode{head: s}
		}
		return out
	}
	return collapseTreeRuns(snaps)
}

// sectionRawSnaps recovers the section snapshots so collapse can rebuild its
// nodes within the section boundary.
func sectionRawSnaps(s snapSection) []model.Snapshot {
	out := make([]model.Snapshot, 0, len(s.nodes))
	for _, n := range s.nodes {
		out = append(out, n.head)
	}
	return out
}

// snapSectionsBy partitions snapshots by source key, preserves newest-first row
// order, and sorts sections case-insensitively with a byte-order tie-break.
// Tag grouping uses the complete sorted tag set; the missing-key bucket is last.
func snapSectionsBy(snaps []model.Snapshot, mode snapGroupMode) []snapSection {
	type bucket struct {
		title string
		snaps []model.Snapshot
	}
	byKey := make(map[string]*bucket)
	var fallback *bucket
	var fallbackTitle string
	switch mode {
	case snapGroupHost:
		fallbackTitle = "(no host)"
	case snapGroupTags:
		fallbackTitle = "(untagged)"
	case snapGroupPaths:
		fallbackTitle = "(no paths)"
	}

	for _, s := range snaps {
		key, title, missing := sectionKeyTitle(s, mode)
		if missing {
			if fallback == nil {
				fallback = &bucket{title: fallbackTitle}
			}
			fallback.snaps = append(fallback.snaps, s)
			continue
		}
		b, ok := byKey[key]
		if !ok {
			b = &bucket{title: title}
			byKey[key] = b
		}
		b.snaps = append(b.snaps, s)
	}

	keys := slices.Collect(maps.Keys(byKey))
	sort.SliceStable(keys, func(i, j int) bool {
		li, lj := strings.ToLower(keys[i]), strings.ToLower(keys[j])
		if li != lj {
			return li < lj
		}
		// Byte-order tie-break for case-only collisions ("Prod" vs "prod").
		return keys[i] < keys[j]
	})

	out := make([]snapSection, 0, len(keys)+1)
	for _, k := range keys {
		b := byKey[k]
		nodes := make([]snapNode, len(b.snaps))
		for i, s := range b.snaps {
			nodes[i] = snapNode{head: s}
		}
		out = append(out, snapSection{
			key: k, title: b.title, nodes: nodes, rawCount: len(b.snaps),
		})
	}
	if fallback != nil {
		nodes := make([]snapNode, len(fallback.snaps))
		for i, s := range fallback.snaps {
			nodes[i] = snapNode{head: s}
		}
		out = append(out, snapSection{
			key: "", title: fallback.title, nodes: nodes,
			rawCount: len(fallback.snaps), noKey: true,
		})
	}
	return out
}

// sectionKeyTitle returns a stable sort key, display title, and missing flag.
// NUL-delimited tag and path keys keep ambiguous display strings distinct.
func sectionKeyTitle(s model.Snapshot, mode snapGroupMode) (key, title string, missing bool) {
	switch mode {
	case snapGroupHost:
		if s.Hostname == "" {
			return "", "", true
		}
		return s.Hostname, s.Hostname, false
	case snapGroupTags:
		if len(s.Tags) == 0 {
			return "", "", true
		}
		tags := sortedCopy(s.Tags)
		return strings.Join(tags, "\x00"), strings.Join(tags, ", "), false
	case snapGroupPaths:
		if len(s.Paths) == 0 {
			return "", "", true
		}
		paths := sortedCopy(s.Paths)
		return strings.Join(paths, "\x00"), strings.Join(paths, ", "), false
	}
	return "", "", true
}

func sortedCopy(in []string) []string {
	out := slices.Clone(in)
	slices.Sort(out)
	return out
}

// collapseTreeRuns folds consecutive snapshots with the same non-empty tree and
// source. The newest member remains the head; older peers preserve input order.
func collapseTreeRuns(snaps []model.Snapshot) []snapNode {
	if len(snaps) == 0 {
		return nil
	}
	out := make([]snapNode, 0, len(snaps))
	cur := snapNode{head: snaps[0]}
	for i := 1; i < len(snaps); i++ {
		next := snaps[i]
		if collapsible(cur.head, next) {
			cur.peers = append(cur.peers, next)
			cur.count++
			continue
		}
		out = append(out, cur)
		cur = snapNode{head: next}
	}
	out = append(out, cur)
	return out
}

func collapsible(a, b model.Snapshot) bool {
	if a.Tree == "" || b.Tree == "" {
		return false
	}
	if a.Tree != b.Tree {
		return false
	}
	return sourceKey(a) == sourceKey(b)
}

// sourceKey identifies a snapshot by host and sorted tag and path sets. Tree
// equality alone cannot distinguish identical content from different origins.
func sourceKey(s model.Snapshot) string {
	tags := strings.Join(sortedCopy(s.Tags), "\x00")
	paths := strings.Join(sortedCopy(s.Paths), "\x00")
	return s.Hostname + "\x01" + tags + "\x01" + paths
}

// selectedNode returns the clamped detail selection, or false when empty.
func (m Model) selectedNode() (snapNode, bool) {
	d := m.snapDisplay()
	if len(d.nodes) == 0 {
		return snapNode{}, false
	}
	return d.nodes[clampCursor(m.snapCursor, len(d.nodes))], true
}

// indexOfSnap returns the node containing id, including collapsed peers. Peer
// lookup reanchors a pre-collapse selection to its absorbing head.
func (m Model) indexOfSnap(id string) int {
	if id == "" {
		return 0
	}
	for i, n := range m.snapDisplay().nodes {
		if n.head.ID == id {
			return i
		}
		for _, p := range n.peers {
			if p.ID == id {
				return i
			}
		}
	}
	return 0
}

// isNodeMarked reports whether the head or a peer is marked, keeping hidden
// peer marks visible before normalization.
func (m Model) isNodeMarked(n snapNode) bool {
	if m.isMarked(n.head.ID) {
		return true
	}
	for _, p := range n.peers {
		if m.isMarked(p.ID) {
			return true
		}
	}
	return false
}

// normalizeDetailMarks maps hidden peer marks to their head, preserves FIFO
// order while removing duplicates, and drops snapshots absent after refresh.
//
// Normalization is monotonic: expanding cannot recover peer marks merged into
// a head, which is safe because collapsed snapshots share tree and source.
func (m Model) normalizeDetailMarks(d snapDisplay) Model {
	if len(m.detailMarks) == 0 {
		return m
	}
	m.detailMarks = normalizedDetailMarks(m.detailMarks, d)
	return m
}

func normalizedDetailMarks(marks []model.Snapshot, d snapDisplay) []model.Snapshot {
	headByID := make(map[string]model.Snapshot, len(d.nodes))
	for _, n := range d.nodes {
		headByID[n.head.ID] = n.head
		for _, p := range n.peers {
			headByID[p.ID] = n.head
		}
	}
	out := make([]model.Snapshot, 0, len(marks))
	seen := make(map[string]bool, len(marks))
	for _, mark := range marks {
		mapped, ok := headByID[mark.ID]
		if !ok {
			continue
		}
		if seen[mapped.ID] {
			continue
		}
		seen[mapped.ID] = true
		out = append(out, mapped)
	}
	return out
}

// cycleSnapGroup advances grouping, reanchors the selected snapshot by head ID,
// and normalizes marks against the reordered display.
func (m Model) cycleSnapGroup() Model {
	var anchorID string
	if n, ok := m.selectedNode(); ok {
		anchorID = n.head.ID
	}
	m.snapGroupMode = (m.snapGroupMode + 1) % snapGroupModeCount
	d := m.snapDisplay()
	m = m.normalizeDetailMarks(d)
	m.snapCursor = clampCursor(m.indexOfSnap(anchorID), len(d.nodes))
	return m
}

// cycleSnapCollapse toggles collapse, reanchors selection, and normalizes marks.
func (m Model) cycleSnapCollapse() Model {
	var anchorID string
	if n, ok := m.selectedNode(); ok {
		anchorID = n.head.ID
	}
	m.snapCollapseTree = !m.snapCollapseTree
	d := m.snapDisplay()
	m = m.normalizeDetailMarks(d)
	m.snapCursor = clampCursor(m.indexOfSnap(anchorID), len(d.nodes))
	return m
}

// snapTokKind / snapTok mirror group.go's flattened token model so the
// grouped renderer can pre-window before lipgloss touches any discarded row.
type snapTokKind int

const (
	snapTokBlank snapTokKind = iota
	snapTokHeading
	snapTokRow
)

type snapTok struct {
	kind    snapTokKind
	section int
	node    int // section-local node index
	data    int // flat-display index of the row; -1 for non-row tokens
}

// buildSnapTokens flattens a grouped display before rendering and records
// heading positions for windowing. Non-first sections get a blank separator.
func buildSnapTokens(d snapDisplay) ([]snapTok, []int) {
	capLines := len(d.sections)
	if len(d.sections) > 1 {
		capLines += len(d.sections) - 1
	}
	for _, sec := range d.sections {
		capLines += len(sec.nodes)
	}
	lines := make([]snapTok, 0, capLines)
	headingPos := make([]int, len(d.sections))
	idx := 0
	for si, sec := range d.sections {
		if si > 0 {
			lines = append(lines, snapTok{kind: snapTokBlank, section: si, data: -1})
		}
		headingPos[si] = len(lines)
		lines = append(lines, snapTok{kind: snapTokHeading, section: si, data: -1})
		for ni := range sec.nodes {
			lines = append(lines, snapTok{kind: snapTokRow, section: si, node: ni, data: idx})
			idx++
		}
	}
	return lines, headingPos
}

// idCell formats a bare short ID or a collapsed "shortid +N" value. The suffix
// is right-aligned within snapCollapseSuffixWidth; an unusually wide count may
// widen only that row.
func idCell(shortID string, count int) string {
	if count <= 0 {
		return shortID
	}
	suffix := fmt.Sprintf("+%d", count)
	if len(suffix) < snapCollapseSuffixWidth {
		suffix = fmt.Sprintf("%*s", snapCollapseSuffixWidth, suffix)
	}
	return fmt.Sprintf("%-*s %s", snapIDWidth, shortID, suffix)
}
