package model

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"path"
	"slices"
	"sort"
	"strings"
)

// Snapshot diff data is parsed into a virtual tree and modifier bitmasks. It
// remains in the active TUI session and is never persisted.

// ChangeType is the single primary kind chosen for color/glyph display.
type ChangeType uint8

// ChangeType values are ordered from unknown to bitrot. Inline comments name
// their restic markers.
const (
	ChangeUnknown      ChangeType = iota // synthetic ancestor / `?` marker
	ChangeAdded                          // `+`
	ChangeRemoved                        // `-`
	ChangeModified                       // `M`
	ChangeMetadataOnly                   // `U`
	ChangeTypeChanged                    // `T`
	ChangeBitrot                         // `?` reported by restic for bitrot
)

// ModifierKind is a bitmask of restic change markers, preserving every marker
// in concatenated changes such as `M?`.
type ModifierKind uint8

// The ModifierKind bits, one per restic marker character.
const (
	KindAdded       ModifierKind = 1 << iota // `+`
	KindRemoved                              // `-`
	KindModified                             // `M`
	KindMetadata                             // `U`
	KindTypeChanged                          // `T`
	KindBitrot                               // `?`
)

// AllDiffKinds contains every supported modifier bit.
const AllDiffKinds ModifierKind = KindAdded | KindRemoved | KindModified | KindMetadata | KindTypeChanged | KindBitrot

// DiffEntry is one decoded `change` line from `restic diff --json`. Modifier
// retains the raw marker, Type selects its primary display kind, and Kinds
// retains every marker for filtering and aggregation.
type DiffEntry struct {
	Path     string
	Modifier string
	Type     ChangeType
	Kinds    ModifierKind
	IsDir    bool
}

// SnapshotDiff is the result of a streamed restic diff. ParseErrors counts
// malformed lines; aggregate counts come from BuildDiffTree rather than
// restic's `statistics` line.
type SnapshotDiff struct {
	Entries     []DiffEntry
	ParseErrors int
}

// DiffStats counts changes by kind for one directory subtree. Multi-kind
// entries contribute to every set kind.
type DiffStats struct {
	Added        int
	Removed      int
	Modified     int
	MetadataOnly int
	TypeChanged  int
	Bitrot       int
}

// Total returns the sum of all counters.
func (s DiffStats) Total() int {
	return s.Added + s.Removed + s.Modified + s.MetadataOnly + s.TypeChanged + s.Bitrot
}

// Empty reports whether the stats contain no changes.
func (s DiffStats) Empty() bool { return s.Total() == 0 }

func (s *DiffStats) addKinds(k ModifierKind) {
	if k&KindAdded != 0 {
		s.Added++
	}
	if k&KindRemoved != 0 {
		s.Removed++
	}
	if k&KindModified != 0 {
		s.Modified++
	}
	if k&KindMetadata != 0 {
		s.MetadataOnly++
	}
	if k&KindTypeChanged != 0 {
		s.TypeChanged++
	}
	if k&KindBitrot != 0 {
		s.Bitrot++
	}
}

// EnabledKinds reports which nonzero counters are included by filter.
func (s DiffStats) EnabledKinds(filter ModifierKind) ModifierKind {
	var out ModifierKind
	if filter&KindAdded != 0 && s.Added > 0 {
		out |= KindAdded
	}
	if filter&KindRemoved != 0 && s.Removed > 0 {
		out |= KindRemoved
	}
	if filter&KindModified != 0 && s.Modified > 0 {
		out |= KindModified
	}
	if filter&KindMetadata != 0 && s.MetadataOnly > 0 {
		out |= KindMetadata
	}
	if filter&KindTypeChanged != 0 && s.TypeChanged > 0 {
		out |= KindTypeChanged
	}
	if filter&KindBitrot != 0 && s.Bitrot > 0 {
		out |= KindBitrot
	}
	return out
}

// DiffRow is one renderable row in a parent listing. Synthetic ancestors have
// unknown type and zero kinds; only directory rows have Aggregate values.
type DiffRow struct {
	Name      string
	Path      string
	Type      ChangeType
	Kinds     ModifierKind
	Modifier  string
	IsDir     bool
	Aggregate DiffStats
}

// DiffTree is the virtual tree built from flat entries. Children is keyed by
// parent path, while Aggregate holds each directory's subtree counts.
type DiffTree struct {
	Children  map[string][]DiffRow
	Aggregate map[string]DiffStats
}

// DiffRoot is the root of every absolute restic path.
const DiffRoot = "/"

// ModifierString renders kinds in canonical restic order: +, -, M, U, T, ?.
// It is intended for merged kinds; single records retain restic's raw marker.
func ModifierString(kinds ModifierKind) string {
	var b strings.Builder
	if kinds&KindAdded != 0 {
		b.WriteByte('+')
	}
	if kinds&KindRemoved != 0 {
		b.WriteByte('-')
	}
	if kinds&KindModified != 0 {
		b.WriteByte('M')
	}
	if kinds&KindMetadata != 0 {
		b.WriteByte('U')
	}
	if kinds&KindTypeChanged != 0 {
		b.WriteByte('T')
	}
	if kinds&KindBitrot != 0 {
		b.WriteByte('?')
	}
	return b.String()
}

// PrimaryChangeType returns the dominant kind using the precedence Bitrot,
// TypeChanged, Removed, Added, Modified, then MetadataOnly.
func PrimaryChangeType(kinds ModifierKind) ChangeType {
	switch {
	case kinds&KindBitrot != 0:
		return ChangeBitrot
	case kinds&KindTypeChanged != 0:
		return ChangeTypeChanged
	case kinds&KindRemoved != 0:
		return ChangeRemoved
	case kinds&KindAdded != 0:
		return ChangeAdded
	case kinds&KindModified != 0:
		return ChangeModified
	case kinds&KindMetadata != 0:
		return ChangeMetadataOnly
	}
	return ChangeUnknown
}

// parseModifier returns the complete bitmask, its primary display kind, and
// whether the input contained an unknown marker.
func parseModifier(s string) (primary ChangeType, kinds ModifierKind, unknown bool) {
	for _, r := range s {
		switch r {
		case '+':
			kinds |= KindAdded
		case '-':
			kinds |= KindRemoved
		case 'M':
			kinds |= KindModified
		case 'U':
			kinds |= KindMetadata
		case 'T':
			kinds |= KindTypeChanged
		case '?':
			kinds |= KindBitrot
		default:
			unknown = true
		}
	}
	return PrimaryChangeType(kinds), kinds, unknown
}

// changeMsg is the on-wire envelope for one restic diff `change` line.
type changeMsg struct {
	MessageType string `json:"message_type"`
	Path        string `json:"path"`
	Modifier    string `json:"modifier"`
}

// diffScanBufferMax caps one NDJSON line at approximately 1 MiB.
const diffScanBufferMax = 1 << 20

// diffProgressEvery coalesces progress callbacks to avoid flooding the UI.
const diffProgressEvery = 256

// ScanDiffNDJSON forwards each NDJSON `change` record to onEntry as it arrives.
// Cancellation returns the partial result and context error; an onEntry error
// aborts and is returned unchanged. Unknown message types are skipped, while
// malformed change lines increment SnapshotDiff.ParseErrors.
func ScanDiffNDJSON(ctx context.Context, r io.Reader, onEntry func(DiffEntry) error, onProgress func(seen int)) (SnapshotDiff, error) {
	var out SnapshotDiff
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), diffScanBufferMax)
	seen := 0
	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return out, err
		}
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var env changeMsg
		if err := json.Unmarshal(line, &env); err != nil {
			out.ParseErrors++
			continue
		}
		if env.MessageType != "change" {
			continue
		}
		if env.Path == "" || env.Path == DiffRoot || env.Modifier == "" || !strings.HasPrefix(env.Path, "/") {
			out.ParseErrors++
			continue
		}
		entry, unknownModifier := newDiffEntry(env.Path, env.Modifier)
		if unknownModifier || entry.Kinds == 0 {
			out.ParseErrors++
			continue
		}
		if onEntry != nil {
			if err := onEntry(entry); err != nil {
				return out, err
			}
		} else {
			out.Entries = append(out.Entries, entry)
		}
		seen++
		if onProgress != nil && seen%diffProgressEvery == 0 {
			onProgress(seen)
		}
	}
	if err := scanner.Err(); err != nil {
		return out, err
	}
	if err := ctx.Err(); err != nil {
		return out, err
	}
	if onProgress != nil && seen%diffProgressEvery != 0 {
		onProgress(seen)
	}
	return out, nil
}

// ParseDiffNDJSON parses buffered NDJSON and accumulates its change entries.
func ParseDiffNDJSON(b []byte) (SnapshotDiff, error) {
	var entries []DiffEntry
	out, err := ScanDiffNDJSON(context.Background(), bytes.NewReader(b),
		func(e DiffEntry) error { entries = append(entries, e); return nil }, nil)
	if err != nil {
		return out, err
	}
	out.Entries = entries
	return out, nil
}

// newDiffEntry strips a directory's trailing slash so later path comparisons
// use a canonical form.
func newDiffEntry(rawPath, modifier string) (DiffEntry, bool) {
	isDir := strings.HasSuffix(rawPath, "/")
	clean := rawPath
	if isDir && rawPath != "/" {
		clean = strings.TrimRight(rawPath, "/")
	}
	primary, kinds, unknown := parseModifier(modifier)
	return DiffEntry{
		Path:     clean,
		Modifier: modifier,
		Type:     primary,
		Kinds:    kinds,
		IsDir:    isDir,
	}, unknown
}

// BuildDiffTree synthesizes missing ancestor directories and computes subtree
// aggregates from flat diff entries.
func BuildDiffTree(entries []DiffEntry) DiffTree {
	// Shared row pointers let explicit entries upgrade synthetic ancestors in
	// both the path index and parent listings.
	rows := make(map[string]*DiffRow)
	children := make(map[string][]*DiffRow)

	ensure := func(p string, isDir bool, typ ChangeType, kinds ModifierKind, modifier string) *DiffRow {
		if existing, ok := rows[p]; ok {
			// Explicit entries replace synthetic type data and merge every kind.
			prevKinds := existing.Kinds
			if typ != ChangeUnknown {
				existing.Type = typ
			}
			existing.Kinds |= kinds
			if modifier != "" {
				existing.Modifier = modifier
			}
			// Re-derive display data after merging duplicate-path kinds so later
			// records cannot hide earlier markers.
			if prevKinds != 0 && kinds != 0 {
				existing.Type = PrimaryChangeType(existing.Kinds)
				existing.Modifier = ModifierString(existing.Kinds)
			}
			// Only explicit entries may change whether an existing row is a directory.
			if typ != ChangeUnknown {
				existing.IsDir = isDir
			}
			return existing
		}
		row := &DiffRow{
			Name:     diffNameOf(p),
			Path:     p,
			Type:     typ,
			Kinds:    kinds,
			Modifier: modifier,
			IsDir:    isDir,
		}
		rows[p] = row
		parent := DiffParentOf(p)
		children[parent] = append(children[parent], row)
		return row
	}

	for _, e := range entries {
		ensure(e.Path, e.IsDir, e.Type, e.Kinds, e.Modifier)
		// Ensure every ancestor up to the root.
		for anc := DiffParentOf(e.Path); anc != ""; anc = DiffParentOf(anc) {
			ensure(anc, true, ChangeUnknown, 0, "")
			if anc == DiffRoot {
				break
			}
		}
	}

	// Merge duplicate paths before aggregation so each kind contributes once
	// per path to every ancestor.
	mergedKinds := make(map[string]ModifierKind, len(entries))
	pathOrder := make([]string, 0, len(entries))
	for _, e := range entries {
		if _, seen := mergedKinds[e.Path]; !seen {
			pathOrder = append(pathOrder, e.Path)
		}
		mergedKinds[e.Path] |= e.Kinds
	}
	aggregate := make(map[string]DiffStats)
	for _, p := range pathOrder {
		k := mergedKinds[p]
		for anc := DiffParentOf(p); anc != ""; anc = DiffParentOf(anc) {
			s := aggregate[anc]
			s.addKinds(k)
			aggregate[anc] = s
			if anc == DiffRoot {
				break
			}
		}
	}

	// Materialize by-value rows after copying directory aggregates.
	out := DiffTree{
		Children:  make(map[string][]DiffRow, len(children)),
		Aggregate: aggregate,
	}
	for parent, kids := range children {
		dst := make([]DiffRow, 0, len(kids))
		for _, r := range kids {
			row := *r
			if row.IsDir {
				row.Aggregate = aggregate[row.Path]
			}
			dst = append(dst, row)
		}
		sort.SliceStable(dst, func(i, j int) bool {
			if dst[i].IsDir != dst[j].IsDir {
				return dst[i].IsDir // dirs first, matching browse's convention
			}
			return dst[i].Name < dst[j].Name
		})
		out.Children[parent] = dst
	}
	return out
}

// DiffExtractSet holds include lists and extractable-path counts for each side.
// Counts precede added/removed directory collapsing.
type DiffExtractSet struct {
	First, Second           []string
	FirstCount, SecondCount int
}

// These masks map modifier kinds to the snapshot sides containing the path.
const (
	diffFirstSideKinds  = KindRemoved | KindModified | KindMetadata | KindTypeChanged | KindBitrot
	diffSecondSideKinds = KindAdded | KindModified | KindMetadata | KindTypeChanged | KindBitrot
)

// DiffExtractIncludes selects visible changed paths at or under root and splits
// them by snapshot side. Counts precede include-list collapsing. Pure added or
// removed directories cover their descendants recursively; other changed
// directories are omitted to avoid restoring unchanged contents. Duplicate
// paths are merged before selection.
func DiffExtractIncludes(entries []DiffEntry, root string, filter ModifierKind) DiffExtractSet {
	if root == "" {
		root = DiffRoot
	}
	prefix := root
	if prefix != DiffRoot {
		prefix += "/"
	}
	under := func(p string) bool { return p == root || strings.HasPrefix(p, prefix) }

	merged := make(map[string]ModifierKind)
	isDir := make(map[string]bool)
	order := make([]string, 0, len(entries))
	for _, e := range entries {
		if !under(e.Path) {
			continue
		}
		if _, seen := merged[e.Path]; !seen {
			order = append(order, e.Path)
		}
		merged[e.Path] |= e.Kinds
		if e.IsDir {
			isDir[e.Path] = true
		}
	}

	// Only a selected pure directory can cover its descendants.
	pureFirst := make(map[string]bool)
	pureSecond := make(map[string]bool)
	for p, k := range merged {
		if !isDir[p] || k&filter == 0 {
			continue
		}
		if k == KindRemoved {
			pureFirst[p] = true
		}
		if k == KindAdded {
			pureSecond[p] = true
		}
	}
	covered := func(p string, pure map[string]bool) bool {
		for anc := DiffParentOf(p); anc != "" && under(anc); anc = DiffParentOf(anc) {
			if pure[anc] {
				return true
			}
		}
		return false
	}

	var out DiffExtractSet
	for _, p := range order {
		eff := merged[p] & filter
		if eff == 0 {
			continue
		}
		// Non-pure directories must not recursively include unchanged contents.
		dirNonPure := isDir[p] && merged[p] != KindAdded && merged[p] != KindRemoved
		if dirNonPure {
			continue
		}
		if eff&diffFirstSideKinds != 0 {
			out.FirstCount++
			if !covered(p, pureFirst) {
				out.First = append(out.First, p)
			}
		}
		if eff&diffSecondSideKinds != 0 {
			out.SecondCount++
			if !covered(p, pureSecond) {
				out.Second = append(out.Second, p)
			}
		}
	}
	slices.Sort(out.First)
	slices.Sort(out.Second)
	return out
}

// DiffParentOf returns the parent of an absolute diff path. It returns "" for
// the root or an invalid path, terminating ancestor walks.
func DiffParentOf(p string) string {
	if p == "" || p == DiffRoot {
		return ""
	}
	if !strings.HasPrefix(p, "/") {
		return ""
	}
	parent := path.Dir(p)
	if parent == "." {
		return DiffRoot
	}
	return parent
}

func diffNameOf(p string) string {
	if p == DiffRoot || p == "" {
		return DiffRoot
	}
	return path.Base(p)
}

// errInvalidModifier stays private to model: the parser classes an unrecognized
// modifier as malformed and increments ParseErrors instead of returning it.
var errInvalidModifier = errors.New("diff modifier has no recognized marker")

// Retained for documentation and a future precondition check; the blank
// assignment keeps the unused-symbol linters quiet.
var _ = errInvalidModifier
