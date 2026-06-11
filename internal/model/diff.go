package model

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"path"
	"sort"
	"strings"
)

// diff.go holds the pure DTOs and helpers for the snapshot-diff feature: the
// streamed NDJSON parser, the virtual-tree builder, and the modifier-bitmask
// model. Like the rest of model/, it imports nothing from internal/ and never
// participates in persistence — diff data lives only in the active TUI session.

// ChangeType is the single primary kind chosen for color/glyph display.
type ChangeType uint8

const (
	ChangeUnknown      ChangeType = iota // synthetic ancestor / `?` marker
	ChangeAdded                          // `+`
	ChangeRemoved                        // `-`
	ChangeModified                       // `M`
	ChangeMetadataOnly                   // `U`
	ChangeTypeChanged                    // `T`
	ChangeBitrot                         // `?` reported by restic for bitrot
)

// ModifierKind is a bitmask of restic's per-change marker characters. Each
// kind takes one bit so the multi-char modifier (e.g. `MU`, `?M`) survives
// the parse without precedence dropouts; filter and aggregate-count semantics
// read this bitmask, not the singular ChangeType.
type ModifierKind uint8

const (
	KindAdded       ModifierKind = 1 << iota // `+`
	KindRemoved                              // `-`
	KindModified                             // `M`
	KindMetadata                             // `U`
	KindTypeChanged                          // `T`
	KindBitrot                               // `?`
)

// AllDiffKinds is the bitmask with every documented modifier bit set. The TUI
// filter mask defaults to this; toggling a kind off clears its bit.
const AllDiffKinds ModifierKind = KindAdded | KindRemoved | KindModified | KindMetadata | KindTypeChanged | KindBitrot

// DiffEntry is one decoded `change` line from `restic diff --json`. Modifier
// retains the raw multi-char marker for renderer use; Type is the primary
// kind chosen for color/glyph; Kinds is the full bitmask used for filter and
// aggregate counting. No Size field — the documented `change` schema carries
// only message_type/path/modifier (sourcing size belongs to the metadata-join
// follow-up).
type DiffEntry struct {
	Path     string
	Modifier string
	Type     ChangeType
	Kinds    ModifierKind
	IsDir    bool
}

// SnapshotDiff is the terminal result of one streamed restic diff. Entries
// are the parsed `change` records (the live stream forwards each one through
// the onEntry callback). ParseErrors counts malformed-line tolerance so the
// footer can surface it. No fields sourced from restic's `statistics` line —
// UI counts come from BuildDiffTree's root aggregate so the parser is decoupled
// from the statistics schema and survives restic-side changes to it.
type SnapshotDiff struct {
	Entries     []DiffEntry
	ParseErrors int
}

// DiffStats counts changes by kind for one directory's subtree. A multi-kind
// entry (e.g. `MU`) contributes to each of its set kinds; no precedence is
// applied.
type DiffStats struct {
	Added        int
	Removed      int
	Modified     int
	MetadataOnly int
	TypeChanged  int
	Bitrot       int
}

// Total is the sum of every counter, used to detect a dir's "all-filtered" state.
func (s DiffStats) Total() int {
	return s.Added + s.Removed + s.Modified + s.MetadataOnly + s.TypeChanged + s.Bitrot
}

// Empty reports whether the stats carry no counters.
func (s DiffStats) Empty() bool { return s.Total() == 0 }

// addKinds bumps every set-bit counter in k by 1.
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

// EnabledKinds reports which DiffStats counters are >0, restricted to the
// supplied filter mask. A dir row hides when this returns 0.
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

// DiffRow is one renderable row in a parent's listing. Type/Kinds mirror the
// underlying entry (synthetic ancestor dirs use ChangeUnknown / zero Kinds).
// Aggregate is the rollup info for dir rows, empty for files.
type DiffRow struct {
	Name      string
	Path      string
	Type      ChangeType
	Kinds     ModifierKind
	Modifier  string
	IsDir     bool
	Aggregate DiffStats
}

// DiffTree is the virtual tree built from a flat entry stream. Children is
// keyed by parent dir path; the listed rows are that directory's children.
// Aggregate carries the per-dir rollup counts used when the dir appears
// collapsed in its own parent's listing.
type DiffTree struct {
	Children  map[string][]DiffRow
	Aggregate map[string]DiffStats
}

// DiffRoot is the path that every absolute restic path traces back to. The TUI
// starts navigation here.
const DiffRoot = "/"

// ModifierString renders kinds as restic's modifier vocabulary in a fixed
// bit-position order (+, -, M, U, T, ?). Used when a caller needs a canonical
// rendering that depends only on the merged Kinds — e.g. a search dedup that
// OR-merged duplicate-path records and must not let stream order decide
// whether the marker reads "MU" or "UM". Single-record paths render restic's
// verbatim modifier string instead, so this is not a default formatter.
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

// PrimaryChangeType picks the dominant ChangeType for a kinds bitmask using
// the precedence Bitrot > TypeChanged > Removed > Added > Modified > MetadataOnly
// — rarer / more alarming events win the one-cell glyph. Callers that already
// hold a merged Kinds (e.g. a search dedup that OR-merged duplicate-path
// records) use this to re-derive the primary marker so it reflects the merged
// state instead of whichever record happened to arrive first.
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

// parseModifier maps restic's modifier characters to the kind bitmask and
// picks a deterministic primary for color/glyph via PrimaryChangeType. Rows
// still render the raw multi-char Modifier when the column has room, so the
// user always sees the full restic vocabulary.
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

// diffScanBufferMax bounds the per-line scanner buffer (~1 MiB) so a
// pathological single path errors cleanly rather than running away.
const diffScanBufferMax = 1 << 20

// diffProgressEvery coalesces onProgress callbacks: the parser ticks once per
// this many parsed entries, so the streaming caller can drive a smooth UI
// count without flooding it.
const diffProgressEvery = 256

// ScanDiffNDJSON reads NDJSON from r and forwards each `change` line to
// onEntry as it arrives. It polls ctx.Err() between lines so cancellation
// stops the scan promptly with whatever was parsed so far. onEntry returning
// an error aborts the scan (the verbatim error is returned). Unknown
// message_types (including the terminal `statistics` line) are skipped without
// error. Malformed lines (JSON parse failure or missing required fields) are
// counted into SnapshotDiff.ParseErrors and do not abort.
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

// ParseDiffNDJSON wraps ScanDiffNDJSON for unit tests: it runs over a byte
// buffer with context.Background() and accumulates entries internally.
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

// newDiffEntry produces a DiffEntry from a raw path and modifier. A trailing
// slash on the path marks a directory; it is stored stripped so all later
// path comparisons use the canonical (no-slash) form.
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

// BuildDiffTree synthesizes navigation-only ancestor dirs from the flat entry
// stream and computes per-dir aggregates. It is decoupled from restic's
// `statistics` line so the root aggregate is the authoritative top-level count.
func BuildDiffTree(entries []DiffEntry) DiffTree {
	// rows is keyed by full path so an explicit later entry can upgrade an
	// earlier synthetic placeholder in place; children is keyed by parent dir
	// and stores pointers into rows so the upgrade reflects in both views.
	rows := make(map[string]*DiffRow)
	children := make(map[string][]*DiffRow)

	ensure := func(p string, isDir bool, typ ChangeType, kinds ModifierKind, modifier string) *DiffRow {
		if existing, ok := rows[p]; ok {
			// Upgrade in place when an explicit entry arrives for a previously-
			// synthetic ancestor. Modified Type wins over the synthetic Unknown;
			// Kinds OR-merges so a multi-kind explicit entry keeps every bit.
			prevKinds := existing.Kinds
			if typ != ChangeUnknown {
				existing.Type = typ
			}
			existing.Kinds |= kinds
			if modifier != "" {
				existing.Modifier = modifier
			}
			// Duplicate-path merge (both sides carry kind bits): the row's
			// Type/Modifier must mirror the OR-merged Kinds so the marker stays
			// consistent with what filters and aggregates see — otherwise a
			// later `U` overwrites an earlier `M` even though Kinds is M|U.
			// Single-record paths keep restic's verbatim modifier string;
			// synthetic ancestor upgrades (kinds==0) don't trigger this.
			if prevKinds != 0 && kinds != 0 {
				existing.Type = PrimaryChangeType(existing.Kinds)
				existing.Modifier = ModifierString(existing.Kinds)
			}
			// Only a real (non-synthetic) entry rewrites IsDir. A synthetic
			// ancestor walk (typ==ChangeUnknown) for a child of /foo must not
			// promote a previously-recorded file /foo into a directory, but a
			// later explicit file entry must demote an earlier synthetic dir.
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
		// Walk every ancestor up to the root; ensure is idempotent so a later
		// explicit ancestor entry upgrades any synthetic placeholder.
		for anc := DiffParentOf(e.Path); anc != ""; anc = DiffParentOf(anc) {
			ensure(anc, true, ChangeUnknown, 0, "")
			if anc == DiffRoot {
				break
			}
		}
	}

	// Dedupe by path so two `change` lines for the same file don't double-count
	// ancestor stats. Kinds OR-merge across duplicates so a `M` + `U` pair for
	// the same path still contributes to both the Modified and MetadataOnly
	// counters on every ancestor exactly once.
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

	// Materialize by-value rows under each parent, sorted dirs-first by name,
	// after copying aggregates onto dir rows.
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

// DiffExtractSet is the outcome of selecting a diff subtree for extraction:
// the per-side restic include lists plus the per-side selected-path counts the
// extract review screen reports. First is the snapshot left of the directional
// arrow (where `-` content lives), Second the right (where `+` content lives);
// the both-sides kinds (M, U, T, ?) put a path on both. A side with no content
// under the filter has a nil list and a zero count — the caller skips it.
type DiffExtractSet struct {
	First, Second           []string
	FirstCount, SecondCount int
}

// diffFirstSideKinds / diffSecondSideKinds map modifier bits to the snapshot
// side(s) holding the content: a removed path exists only in the first
// snapshot, an added one only in the second, and every other kind describes a
// path present (in some form) on both sides.
const (
	diffFirstSideKinds  = KindRemoved | KindModified | KindMetadata | KindTypeChanged | KindBitrot
	diffSecondSideKinds = KindAdded | KindModified | KindMetadata | KindTypeChanged | KindBitrot
)

// DiffExtractIncludes selects the changed paths at or under root that pass the
// filter mask — the same any-enabled-bit predicate the diff view's row
// visibility uses, so what extracts is exactly what the user sees — and splits
// them onto the snapshot sides that hold their content. Counts are the raw
// per-side selections; the include lists are then collapsed: a path under a
// selected pure-added (resp. pure-removed) directory is dropped on that side,
// because restic restores an included directory recursively and everything
// under a pure-added dir is itself added, so the ancestor include covers the
// whole subtree without ever pulling an unchanged sibling. Directory entries
// carrying any other kind are never included themselves — a recursive include
// would drag their unchanged contents along — so only their changed children
// (which carry their own entries) extract; the dir node still materializes on
// each side as the children's restored ancestor. Duplicate-path records are
// OR-merged first, mirroring BuildDiffTree, so an `M`+`U` pair selects once.
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

	// Only a filter-passing pure dir collapses its descendants: a filtered-out
	// dir is not in the include list, so it covers nothing.
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
		// A directory that is not purely one-sided never becomes an include
		// itself; see the doc comment.
		dirNonPure := isDir[p] && merged[p] != KindAdded && merged[p] != KindRemoved
		if eff&diffFirstSideKinds != 0 {
			out.FirstCount++
			if !dirNonPure && !covered(p, pureFirst) {
				out.First = append(out.First, p)
			}
		}
		if eff&diffSecondSideKinds != 0 {
			out.SecondCount++
			if !dirNonPure && !covered(p, pureSecond) {
				out.Second = append(out.Second, p)
			}
		}
	}
	sort.Strings(out.First)
	sort.Strings(out.Second)
	return out
}

// DiffParentOf returns the parent directory path of p. It returns "" when p is
// already the diff root, signaling "stop walking ancestors". An invalid path
// (empty, no leading slash) also returns "" so a bad entry can't loop. Exported
// so TUI navigation can share this canonical walk and not maintain a parallel
// copy.
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

// diffNameOf is the last path component (or "/" for the root).
func diffNameOf(p string) string {
	if p == DiffRoot || p == "" {
		return DiffRoot
	}
	return path.Base(p)
}

// errInvalidModifier is returned by demand-parse helpers when restic emits a
// modifier with no recognized character. Kept private to model — the parser
// classes such lines as malformed and increments ParseErrors instead.
var errInvalidModifier = errors.New("diff modifier has no recognized marker")

// _ blocks the linter from complaining about an unused error; the symbol is
// retained for documentation and a future precondition check.
var _ = errInvalidModifier
