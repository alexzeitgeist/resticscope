package tui

import (
	"fmt"
	"sort"
	"strings"

	"resticscope/internal/model"
)

// snapgroup.go owns the detail view's optional snapshot grouping (by host,
// tags, or paths) and the tree-ID collapse that folds consecutive same-tree
// snapshots into one "ID (+N)" row. Both are transient per-detail-visit state
// driven by `g` (cycle group) and `c` (toggle collapse). The pipeline is pure:
// it consumes the already-sorted newest-first snapshot slice from
// model.SortedSnapshotsNewestFirst and produces a snapDisplay the renderer
// indexes into. snapCursor indexes the flat selectable nodes; collapsed peers
// are unreachable by construction.

// snapGroupMode is the section grouping cycle position. snapGroupOff renders
// a single flat section; the other modes partition snapshots into per-key
// sections, plus a "(no host)" / "(untagged)" / "(no paths)" fallback when a
// snapshot lacks the key.
type snapGroupMode int

const (
	snapGroupOff snapGroupMode = iota
	snapGroupHost
	snapGroupTags
	snapGroupPaths
	snapGroupModeCount // sentinel: number of modes, for cycling
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

// snapNode is one selectable row in the detail view's snapshot table. count
// is the number of folded peers (zero means no "(+N)" suffix); peers carries
// those peer snapshots so the renderer can show their marks on the head row
// and so mark normalization can map a peer ID back to its absorbing head.
type snapNode struct {
	head  model.Snapshot
	count int
	peers []model.Snapshot
}

// snapSection groups nodes under a heading when grouping is active. rawCount
// is the snapshot count before collapse — used for the "(N)" badge on the
// heading so the user sees "host-a (12)" even when the 12 collapsed into 3
// rows. noKey marks the fallback bucket so the renderer can style it dim.
type snapSection struct {
	key      string
	title    string
	nodes    []snapNode
	rawCount int
	noKey    bool
}

// snapDisplay is the canonical render-and-action view of the snapshot list.
// nodes is the flat selectable order the cursor indexes; sections is nil in
// flat mode and non-nil under any non-off grouping mode. Every renderer,
// cursor clamp, and mark check resolves through this single view so the
// highlighted row, the acted-on snapshot, and the rendered marks can never
// diverge.
type snapDisplay struct {
	nodes    []snapNode
	sections []snapSection
}

// snapDisplay rebuilds the display from the detail repo's snapshots and the
// current grouping/collapse toggles. It is invoked per render and per action;
// the expected scale does not justify a cache. Pipeline order is fixed:
// section first, then collapse within each section so the collapse rule
// "consecutive same-tree" never crosses a section boundary.
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

// snapNodes is the flat-mode node builder: either one node per snapshot
// (collapse off) or a collapsed run.
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

// sectionRawSnaps returns the raw snapshots backing a section before collapse.
// snapSectionsBy seeds the section with one node per snapshot; this peels the
// heads back out so the collapse pass can re-fold within the section.
func sectionRawSnaps(s snapSection) []model.Snapshot {
	out := make([]model.Snapshot, 0, len(s.nodes))
	for _, n := range s.nodes {
		out = append(out, n.head)
	}
	return out
}

// snapSectionsBy partitions snaps by the configured source key into sections
// ordered case-insensitively by canonical key with a byte-order tie-break so
// distinct keys with colliding titles stay deterministic. The noKey fallback
// sorts last. Within each section the caller's newest-first order is
// preserved (no re-sort). Each snapshot lands in exactly one section: tag
// grouping buckets by the full sorted tag set, not by each individual tag.
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

	keys := make([]string, 0, len(byKey))
	for k := range byKey {
		keys = append(keys, k)
	}
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

// sectionKeyTitle resolves the (canonical key, display title, missing) tuple
// for a snapshot under the given mode. The key drives stable sorting; the
// title drives rendering. For tag/path modes the key joins the sorted set on
// NUL so distinct sets with the same comma-joined string sort apart.
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
	out := make([]string, len(in))
	copy(out, in)
	sort.Strings(out)
	return out
}

// collapseTreeRuns folds consecutive same-tree-and-source snapshots into a
// single node. Newest-first input means the head of a collapsed run is the
// newest member; peers carry the older, hidden snapshots in the same order.
// Two snapshots collapse iff both have non-empty Tree, share the same Tree,
// and share the same source key (Hostname + sorted Tags + sorted Paths).
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

// sourceKey is the cross-snapshot identity used by the collapse rule: same
// host, same sorted tag set, same sorted path set. Tree equality alone is
// not enough — restic can record identical trees across different hosts in
// theory, and grouping them together would silently misrepresent origin.
func sourceKey(s model.Snapshot) string {
	tags := strings.Join(sortedCopy(s.Tags), "\x00")
	paths := strings.Join(sortedCopy(s.Paths), "\x00")
	return s.Hostname + "\x01" + tags + "\x01" + paths
}

// selectedNode returns the node under the detail cursor, or false when the
// repo has no snapshots. The cursor is clamped on read so a refresh, a group
// cycle, or a collapse toggle that shrinks the selectable set can never index
// out of range.
func (m Model) selectedNode() (snapNode, bool) {
	d := m.snapDisplay()
	if len(d.nodes) == 0 {
		return snapNode{}, false
	}
	return d.nodes[clampCursor(m.snapCursor, len(d.nodes))], true
}

// indexOfSnap returns the cursor position of the node whose head or any peer
// carries id, or 0 if id is not present. Mirrors arrange.go's indexOf
// convention so the g/c anchor pattern can grab a pre-toggle head ID, rebuild
// the display, and re-seat the cursor without special-casing missing IDs.
// Searching peers makes a previously-selected peer map to its absorbing head
// when collapse turns on.
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

// isNodeMarked reports whether the node (head or any peer) is in the detail
// mark FIFO. Checking peers keeps a hidden peer's mark visible on its head
// even before normalization folds it.
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

// normalizeDetailMarks remaps the mark FIFO against d. A mark on a peer that
// becomes hidden maps to its absorbing head; duplicate marks that map to the
// same head collapse to one mark, preserving FIFO order by keeping the first
// occurrence. A mark whose ID is not present in d (head or peer) is dropped so
// refreshes that remove snapshots cannot leave a stale restic diff target.
//
// Normalization is monotonic: collapse-on can fold peer marks into a head
// mark; collapse-off cannot reconstruct the original peer identities after
// that fold. That is acceptable because folded snapshots share the same
// collapsible tree/source identity by definition.
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

// cycleSnapGroup advances the detail-view grouping cycle (off → host → tags
// → paths → off). Anchors the cursor on the selected node's head ID so the
// same snapshot stays selected across the reorder, then normalizes marks
// against the new display so a previously visible mark on a now-hidden peer
// shows on the absorbing head.
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

// cycleSnapCollapse toggles tree-ID collapse. Same anchor + normalize
// discipline as cycleSnapGroup; collapse-on can merge marks (handled by
// normalizeDetailMarks), collapse-off cannot un-merge them — that loss is
// documented and accepted because folded peers share an identity.
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

// buildSnapTokens flattens a grouped display into a render-free token stream
// and records each section heading's line index for the windowing math.
// Blank separators precede every non-first section.
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

// idCell formats the short-id column's text for a node. snapCells left-pads
// the result to l.idWidth, so:
//
//   - Uncollapsed: return the bare short-id; snapCells handles padding,
//     including the blank suffix slot when collapse mode is on.
//   - Collapsed head: return "shortid +N" with the "+N" right-aligned within
//     snapCollapseSuffixWidth so the digit lands in the reserved slot. A
//     count whose decimal width exceeds the slot widens that one row only
//     (snapCells's %-*s never truncates), accepted as a rare case.
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
