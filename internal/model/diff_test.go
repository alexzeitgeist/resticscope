package model

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
)

func ndjsonLines(lines ...string) []byte {
	return []byte(strings.Join(lines, "\n") + "\n")
}

func TestParseDiffNDJSONBasic(t *testing.T) {
	in := ndjsonLines(
		`{"message_type":"change","path":"/a/added.txt","modifier":"+"}`,
		`{"message_type":"change","path":"/a/removed.txt","modifier":"-"}`,
		`{"message_type":"change","path":"/a/sub/","modifier":"+"}`,
		// statistics line with nested DiffStat objects: must be ignored cleanly,
		// no ParseErrors bump (the parser keys off message_type and never tries
		// to model the statistics envelope).
		`{"message_type":"statistics","added":{"files":1,"dirs":0,"bytes":42},"removed":{"files":1,"dirs":0,"bytes":17}}`,
	)
	out, err := ParseDiffNDJSON(in)
	if err != nil {
		t.Fatalf("ParseDiffNDJSON: %v", err)
	}
	if out.ParseErrors != 0 {
		t.Errorf("ParseErrors = %d, want 0 (statistics must be ignored without counting as malformed)", out.ParseErrors)
	}
	if len(out.Entries) != 3 {
		t.Fatalf("entries = %d, want 3", len(out.Entries))
	}
	if out.Entries[0].Type != ChangeAdded || out.Entries[0].Kinds != KindAdded {
		t.Errorf("first entry primary/kinds = %v/%v", out.Entries[0].Type, out.Entries[0].Kinds)
	}
	if !out.Entries[2].IsDir {
		t.Errorf("trailing-slash entry not marked IsDir: %+v", out.Entries[2])
	}
	if out.Entries[2].Path != "/a/sub" {
		t.Errorf("trailing slash not stripped: %q", out.Entries[2].Path)
	}
}

func TestParseDiffNDJSONMultiKindModifier(t *testing.T) {
	in := ndjsonLines(
		`{"message_type":"change","path":"/x/f1","modifier":"MU"}`,
		`{"message_type":"change","path":"/x/f2","modifier":"?M"}`,
	)
	out, err := ParseDiffNDJSON(in)
	if err != nil {
		t.Fatalf("ParseDiffNDJSON: %v", err)
	}
	if got := out.Entries[0].Kinds; got != KindModified|KindMetadata {
		t.Errorf("MU kinds = %b, want %b", got, KindModified|KindMetadata)
	}
	if got := out.Entries[0].Type; got != ChangeModified {
		t.Errorf("MU primary = %v, want ChangeModified", got)
	}
	if got := out.Entries[1].Kinds; got != KindModified|KindBitrot {
		t.Errorf("?M kinds = %b, want %b", got, KindModified|KindBitrot)
	}
	if got := out.Entries[1].Type; got != ChangeBitrot {
		t.Errorf("?M primary = %v, want ChangeBitrot", got)
	}
}

func TestParseDiffNDJSONUnknownMessageTypeIgnored(t *testing.T) {
	in := ndjsonLines(
		`{"message_type":"summary","files":42}`,
		`{"message_type":"change","path":"/a","modifier":"+"}`,
	)
	out, err := ParseDiffNDJSON(in)
	if err != nil {
		t.Fatalf("ParseDiffNDJSON: %v", err)
	}
	if out.ParseErrors != 0 || len(out.Entries) != 1 {
		t.Errorf("unknown msg type leaked: parseErrors=%d entries=%d", out.ParseErrors, len(out.Entries))
	}
}

func TestParseDiffNDJSONMalformedTolerated(t *testing.T) {
	in := ndjsonLines(
		`{"message_type":"change","path":"/a","modifier":"+"}`,
		`{this is not json}`,
		`{"message_type":"change","modifier":"+"}`, // missing path
		`{"message_type":"change","path":"/c"}`,    // missing modifier
		`{"message_type":"change","path":"/b","modifier":"-"}`,
	)
	out, err := ParseDiffNDJSON(in)
	if err != nil {
		t.Fatalf("ParseDiffNDJSON: %v", err)
	}
	if len(out.Entries) != 2 || out.ParseErrors != 3 {
		t.Errorf("entries=%d parseErrors=%d, want 2/3", len(out.Entries), out.ParseErrors)
	}
}

func TestParseDiffNDJSONUnknownModifierMalformed(t *testing.T) {
	in := ndjsonLines(
		`{"message_type":"change","path":"/a","modifier":"X"}`,
		`{"message_type":"change","path":"/b","modifier":"+"}`,
	)
	out, err := ParseDiffNDJSON(in)
	if err != nil {
		t.Fatalf("ParseDiffNDJSON: %v", err)
	}
	if len(out.Entries) != 1 || out.ParseErrors != 1 {
		t.Fatalf("entries=%d parseErrors=%d, want 1/1", len(out.Entries), out.ParseErrors)
	}
	if out.Entries[0].Path != "/b" {
		t.Errorf("parsed entry path = %q, want /b", out.Entries[0].Path)
	}
}

func TestParseDiffNDJSONMixedUnknownModifierMalformed(t *testing.T) {
	in := ndjsonLines(
		`{"message_type":"change","path":"/a","modifier":"MX"}`,
		`{"message_type":"change","path":"/b","modifier":"+"}`,
	)
	out, err := ParseDiffNDJSON(in)
	if err != nil {
		t.Fatalf("ParseDiffNDJSON: %v", err)
	}
	if len(out.Entries) != 1 || out.ParseErrors != 1 {
		t.Fatalf("entries=%d parseErrors=%d, want 1/1", len(out.Entries), out.ParseErrors)
	}
	if out.Entries[0].Path != "/b" {
		t.Errorf("parsed entry path = %q, want /b", out.Entries[0].Path)
	}
}

func TestParseDiffNDJSONRootPathRejected(t *testing.T) {
	in := ndjsonLines(
		`{"message_type":"change","path":"/","modifier":"+"}`,
		`{"message_type":"change","path":"/ok","modifier":"+"}`,
	)
	out, err := ParseDiffNDJSON(in)
	if err != nil {
		t.Fatalf("ParseDiffNDJSON: %v", err)
	}
	if len(out.Entries) != 1 || out.ParseErrors != 1 {
		t.Fatalf("entries=%d parseErrors=%d, want 1/1", len(out.Entries), out.ParseErrors)
	}
	if out.Entries[0].Path != "/ok" {
		t.Errorf("parsed entry path = %q, want /ok", out.Entries[0].Path)
	}
}

func TestParseDiffNDJSONLongLine(t *testing.T) {
	long := strings.Repeat("a", 800*1024)
	line := `{"message_type":"change","path":"/` + long + `","modifier":"+"}`
	out, err := ParseDiffNDJSON([]byte(line + "\n"))
	if err != nil {
		t.Fatalf("long line under 1MiB must parse: %v", err)
	}
	if len(out.Entries) != 1 || !strings.HasSuffix(out.Entries[0].Path, "aa") {
		t.Errorf("long-path entry not parsed: %+v", out.Entries)
	}
}

func TestParseDiffNDJSONOverLongLineErrors(t *testing.T) {
	huge := strings.Repeat("a", 2*1024*1024)
	line := `{"message_type":"change","path":"/` + huge + `","modifier":"+"}`
	_, err := ParseDiffNDJSON([]byte(line + "\n"))
	if err == nil {
		t.Fatal("expected an error for a line larger than the scanner cap")
	}
}

func TestScanDiffNDJSONContextCancellation(t *testing.T) {
	in := ndjsonLines(
		`{"message_type":"change","path":"/a","modifier":"+"}`,
		`{"message_type":"change","path":"/b","modifier":"+"}`,
		`{"message_type":"change","path":"/c","modifier":"+"}`,
	)
	ctx, cancel := context.WithCancel(context.Background())
	var got []DiffEntry
	out, err := ScanDiffNDJSON(ctx, bytes.NewReader(in), func(e DiffEntry) error {
		got = append(got, e)
		if len(got) == 2 {
			cancel()
		}
		return nil
	}, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if len(got) != 2 {
		t.Errorf("got = %d entries, want 2 (scan stops on the next line after cancel)", len(got))
	}
	_ = out
}

func TestScanDiffNDJSONProgressCadence(t *testing.T) {
	var lines []string
	for i := 0; i < diffProgressEvery*2+5; i++ {
		lines = append(lines, `{"message_type":"change","path":"/x","modifier":"+"}`)
	}
	in := ndjsonLines(lines...)
	var ticks []int
	if _, err := ScanDiffNDJSON(context.Background(), bytes.NewReader(in), nil,
		func(seen int) { ticks = append(ticks, seen) }); err != nil {
		t.Fatalf("ScanDiffNDJSON: %v", err)
	}
	if len(ticks) < 3 {
		t.Fatalf("expected at least 3 progress ticks, got %d: %v", len(ticks), ticks)
	}
	if ticks[0] != diffProgressEvery || ticks[1] != 2*diffProgressEvery {
		t.Errorf("first two ticks = %d/%d, want %d/%d", ticks[0], ticks[1], diffProgressEvery, 2*diffProgressEvery)
	}
	last := ticks[len(ticks)-1]
	if last != diffProgressEvery*2+5 {
		t.Errorf("final tick = %d, want exact final count", last)
	}
}

func TestScanDiffNDJSONOnEntryErrorAborts(t *testing.T) {
	in := ndjsonLines(
		`{"message_type":"change","path":"/a","modifier":"+"}`,
		`{"message_type":"change","path":"/b","modifier":"+"}`,
	)
	sentinel := errors.New("disk full")
	_, err := ScanDiffNDJSON(context.Background(), bytes.NewReader(in), func(DiffEntry) error {
		return sentinel
	}, nil)
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want %v", err, sentinel)
	}
}

func TestBuildDiffTreeSynthesizesAncestors(t *testing.T) {
	in := ndjsonLines(
		`{"message_type":"change","path":"/a/b/c/file","modifier":"+"}`,
	)
	out, err := ParseDiffNDJSON(in)
	if err != nil {
		t.Fatalf("ParseDiffNDJSON: %v", err)
	}
	tree := BuildDiffTree(out.Entries)
	// Every ancestor on the way to root must have its row in its parent's listing.
	expect := map[string]string{
		"/":      "a",
		"/a":     "b",
		"/a/b":   "c",
		"/a/b/c": "file",
	}
	for parent, wantName := range expect {
		kids, ok := tree.Children[parent]
		if !ok || len(kids) == 0 {
			t.Fatalf("parent %q has no listing", parent)
		}
		if kids[0].Name != wantName {
			t.Errorf("parent %q first row = %q, want %q", parent, kids[0].Name, wantName)
		}
	}
	// Root aggregate counts the single added file.
	if got := tree.Aggregate[DiffRoot]; got.Added != 1 || got.Total() != 1 {
		t.Errorf("root aggregate = %+v, want Added=1 only", got)
	}
}

func TestBuildDiffTreeMultiKindAggregates(t *testing.T) {
	in := ndjsonLines(
		`{"message_type":"change","path":"/x/a","modifier":"MU"}`,
		`{"message_type":"change","path":"/x/b","modifier":"?M"}`,
	)
	out, _ := ParseDiffNDJSON(in)
	tree := BuildDiffTree(out.Entries)
	got := tree.Aggregate["/x"]
	// MU contributes to Modified+MetadataOnly; ?M contributes to Modified+Bitrot.
	if got.Modified != 2 || got.MetadataOnly != 1 || got.Bitrot != 1 {
		t.Errorf("multi-kind aggregate = %+v, want Modified=2/Metadata=1/Bitrot=1", got)
	}
}

func TestBuildDiffTreeUpgradesSyntheticRow(t *testing.T) {
	// Restic might emit the leaf before the explicit directory entry; the dir
	// row must be upgraded in place rather than duplicated.
	in := ndjsonLines(
		`{"message_type":"change","path":"/a/b/leaf","modifier":"+"}`,
		`{"message_type":"change","path":"/a/b/","modifier":"+"}`,
	)
	out, _ := ParseDiffNDJSON(in)
	tree := BuildDiffTree(out.Entries)
	kids := tree.Children["/a"]
	if len(kids) != 1 {
		t.Fatalf("/a children = %d, want a single upgraded row, got %+v", len(kids), kids)
	}
	if kids[0].Type != ChangeAdded || kids[0].Modifier != "+" {
		t.Errorf("explicit dir entry did not upgrade the synthetic row: %+v", kids[0])
	}
}

func TestEnabledKindsFiltering(t *testing.T) {
	s := DiffStats{Modified: 1, MetadataOnly: 1}
	if got := s.EnabledKinds(KindModified); got != KindModified {
		t.Errorf("M-only filter on MU row = %b, want KindModified", got)
	}
	if got := s.EnabledKinds(KindMetadata); got != KindMetadata {
		t.Errorf("U-only filter on MU row = %b, want KindMetadata", got)
	}
	if got := s.EnabledKinds(KindAdded); got != 0 {
		t.Errorf("Added filter on MU row = %b, want 0 (row should hide)", got)
	}
}

func TestParseDiffNDJSONRelativePathRejected(t *testing.T) {
	// restic always emits absolute paths; a relative one is either a malformed
	// line or a parser bug upstream. Either way it must bump ParseErrors so the
	// footer surfaces the count, not silently leak into entries where every
	// later ancestor walk would diverge.
	in := ndjsonLines(
		`{"message_type":"change","path":"foo/bar","modifier":"+"}`,
		`{"message_type":"change","path":"/ok","modifier":"+"}`,
	)
	out, err := ParseDiffNDJSON(in)
	if err != nil {
		t.Fatalf("ParseDiffNDJSON: %v", err)
	}
	if len(out.Entries) != 1 || out.ParseErrors != 1 {
		t.Fatalf("entries=%d parseErrors=%d, want 1/1", len(out.Entries), out.ParseErrors)
	}
	if out.Entries[0].Path != "/ok" {
		t.Errorf("surviving entry path = %q, want /ok", out.Entries[0].Path)
	}
}

func TestBuildDiffTreeDedupesDuplicatePaths(t *testing.T) {
	// Two `change` lines for the same path (e.g. restic re-emitting a record
	// across a chunk boundary) must contribute to ancestor aggregates only once;
	// the OR-merge of their kinds is the merged set used for the single bump.
	in := ndjsonLines(
		`{"message_type":"change","path":"/x/a","modifier":"M"}`,
		`{"message_type":"change","path":"/x/a","modifier":"M"}`,
	)
	out, _ := ParseDiffNDJSON(in)
	tree := BuildDiffTree(out.Entries)
	if got := tree.Aggregate["/x"]; got.Modified != 1 || got.Total() != 1 {
		t.Errorf("/x aggregate = %+v, want Modified=1 only", got)
	}
	if got := tree.Aggregate[DiffRoot]; got.Modified != 1 || got.Total() != 1 {
		t.Errorf("root aggregate = %+v, want Modified=1 only", got)
	}
}

func TestBuildDiffTreeCanonicalizesDuplicatePathMarker(t *testing.T) {
	// Two `change` records for the same path with different modifiers (M then
	// U) must produce a row whose Type / Modifier mirror the OR-merged Kinds.
	// Without canonicalization the renderer trusts whichever record arrived
	// last (Modifier="U", Type=ChangeMetadataOnly), so an M-only filter keeps
	// the row visible (Kinds contains M) but the marker reads "U" with
	// metadata styling — silent disagreement between filter and marker.
	in := ndjsonLines(
		`{"message_type":"change","path":"/etc/passwd","modifier":"M"}`,
		`{"message_type":"change","path":"/etc/passwd","modifier":"U"}`,
	)
	out, _ := ParseDiffNDJSON(in)
	tree := BuildDiffTree(out.Entries)
	var row *DiffRow
	for i, r := range tree.Children["/etc"] {
		if r.Path == "/etc/passwd" {
			row = &tree.Children["/etc"][i]
			break
		}
	}
	if row == nil {
		t.Fatalf("/etc/passwd row missing from /etc listing: %+v", tree.Children["/etc"])
	}
	if row.Kinds != KindModified|KindMetadata {
		t.Errorf("Kinds = %b, want M|U", row.Kinds)
	}
	if row.Modifier != "MU" {
		t.Errorf("Modifier = %q, want canonical %q", row.Modifier, "MU")
	}
	if row.Type != ChangeModified {
		t.Errorf("Type = %v, want ChangeModified (M wins over U in precedence)", row.Type)
	}
}

func TestBuildDiffTreePreservesExplicitFileIsDir(t *testing.T) {
	// An explicit file row at /foo (T = type-changed) must not flip to IsDir
	// when a later /foo/bar entry walks its ancestors. The synthetic-ancestor
	// pass uses typ=ChangeUnknown; the guard in ensure() keeps real file rows
	// intact so they render with the file glyph instead of being promoted to
	// a directory listing.
	in := ndjsonLines(
		`{"message_type":"change","path":"/foo","modifier":"T"}`,
		`{"message_type":"change","path":"/foo/bar","modifier":"+"}`,
	)
	out, _ := ParseDiffNDJSON(in)
	tree := BuildDiffTree(out.Entries)
	kids := tree.Children[DiffRoot]
	var foo *DiffRow
	for i := range kids {
		if kids[i].Path == "/foo" {
			foo = &kids[i]
			break
		}
	}
	if foo == nil {
		t.Fatalf("/foo row missing from root listing: %+v", kids)
	}
	if foo.IsDir {
		t.Errorf("/foo IsDir promoted by synthetic ancestor walk: %+v", *foo)
	}
	if foo.Type != ChangeTypeChanged {
		t.Errorf("/foo Type = %v, want ChangeTypeChanged (explicit kind preserved)", foo.Type)
	}
}

func TestBuildDiffTreeDemotesSyntheticDirOnExplicitFile(t *testing.T) {
	// If a child appears before an explicit file entry for its parent path, the
	// ancestor walk creates /foo as a synthetic directory. The later real file row
	// must rewrite IsDir=false so /foo does not render as navigable.
	in := ndjsonLines(
		`{"message_type":"change","path":"/foo/bar","modifier":"+"}`,
		`{"message_type":"change","path":"/foo","modifier":"T"}`,
	)
	out, _ := ParseDiffNDJSON(in)
	tree := BuildDiffTree(out.Entries)
	kids := tree.Children[DiffRoot]
	var foo *DiffRow
	for i := range kids {
		if kids[i].Path == "/foo" {
			foo = &kids[i]
			break
		}
	}
	if foo == nil {
		t.Fatalf("/foo row missing from root listing: %+v", kids)
	}
	if foo.IsDir {
		t.Errorf("/foo IsDir stayed synthetic-dir after explicit file entry: %+v", *foo)
	}
	if foo.Type != ChangeTypeChanged {
		t.Errorf("/foo Type = %v, want ChangeTypeChanged", foo.Type)
	}
}

func TestParseModifierPrecedence(t *testing.T) {
	cases := []struct {
		in       string
		wantType ChangeType
		wantKind ModifierKind
	}{
		{"+", ChangeAdded, KindAdded},
		{"-", ChangeRemoved, KindRemoved},
		{"M", ChangeModified, KindModified},
		{"U", ChangeMetadataOnly, KindMetadata},
		{"T", ChangeTypeChanged, KindTypeChanged},
		{"?", ChangeBitrot, KindBitrot},
		{"MU", ChangeModified, KindModified | KindMetadata},
		{"MT", ChangeTypeChanged, KindModified | KindTypeChanged},
		{"?M", ChangeBitrot, KindModified | KindBitrot},
		{"", ChangeUnknown, 0},
	}
	for _, c := range cases {
		gotType, gotKind, gotUnknown := parseModifier(c.in)
		if gotType != c.wantType || gotKind != c.wantKind {
			t.Errorf("parseModifier(%q) = (%v,%b), want (%v,%b)", c.in, gotType, gotKind, c.wantType, c.wantKind)
		}
		if gotUnknown {
			t.Errorf("parseModifier(%q) unknown = true, want false", c.in)
		}
	}
}
