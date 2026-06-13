package browsedb

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"resticscope/internal/model"
)

// newTestDB opens an encrypted browse DB in a temp dir with a fresh random key.
// The pool is closed on cleanup; the temp dir (and DB file) is removed by the
// testing framework. maxDisk==0 means unlimited.
func newTestDB(t *testing.T, maxDisk int64) (*DB, string, []byte) {
	t.Helper()
	dir := t.TempDir()
	key := randomKey(t)
	db, err := Open(filepath.Join(dir, "db.sqlite"), key, maxDisk)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.pool.Close() })
	return db, dir, key
}

func randomKey(t *testing.T) []byte {
	t.Helper()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatalf("rand: %v", err)
	}
	return key
}

func mustAdd(t *testing.T, ctx context.Context, itx *IndexTx, n model.BrowseNode) {
	t.Helper()
	if err := itx.Add(ctx, n); err != nil {
		t.Fatalf("Add(%q): %v", n.Path, err)
	}
}

func mustIndex(t *testing.T, db *DB, repo, snap string, nodes []model.BrowseNode) {
	t.Helper()
	ctx := context.Background()
	itx, err := db.BeginIndex(ctx, repo, snap)
	if err != nil {
		t.Fatalf("BeginIndex: %v", err)
	}
	for _, n := range nodes {
		if err := itx.Add(ctx, n); err != nil {
			t.Fatalf("Add: %v", err)
		}
	}
	if err := itx.Commit(ctx); err != nil {
		t.Fatalf("Commit: %v", err)
	}
}

func names(entries []model.BrowseEntry) []string {
	out := make([]string, len(entries))
	for i, e := range entries {
		out[i] = e.Name
	}
	return out
}

func paths(entries []model.BrowseEntry) []string {
	out := make([]string, len(entries))
	for i, e := range entries {
		out[i] = e.Path
	}
	return out
}

func eqStrings(a, b []string) bool {
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

func assertErrPathFree(t *testing.T, err error, needles ...string) {
	t.Helper()
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
	for _, n := range needles {
		if strings.Contains(err.Error(), n) {
			t.Errorf("error leaked %q: %v", n, err)
		}
	}
}

func countDirs(t *testing.T, db *DB) int {
	t.Helper()
	var n int
	if err := db.pool.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM dirs`).Scan(&n); err != nil {
		t.Fatalf("count dirs: %v", err)
	}
	return n
}

func dirPaths(t *testing.T, db *DB, repo, snap string) []string {
	t.Helper()
	rows, err := db.pool.QueryContext(context.Background(),
		`SELECT d.path FROM dirs d JOIN snapshots s ON s.sid=d.sid WHERE s.repo=? AND s.snapshot=? ORDER BY d.path`,
		repo, snap)
	if err != nil {
		t.Fatalf("dir paths: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			t.Fatalf("dir path scan: %v", err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("dir path rows: %v", err)
	}
	return out
}

func TestOpenValidation(t *testing.T) {
	p := filepath.Join(t.TempDir(), "db.sqlite")
	if _, err := Open(p, make([]byte, 16), 0); !errors.Is(err, errInvalidKey) {
		t.Errorf("short key: want errInvalidKey, got %v", err)
	}
	if _, err := Open(p, make([]byte, 32), -1); !errors.Is(err, errInvalidDiskLimit) {
		t.Errorf("negative disk: want errInvalidDiskLimit, got %v", err)
	}
}

func TestListDirOrdering(t *testing.T) {
	db, _, _ := newTestDB(t, 0)
	ctx := context.Background()
	repo, snap := "repo", "snap"
	mustIndex(t, db, repo, snap, []model.BrowseNode{
		{Path: "/A", IsDir: true},
		{Path: "/a", IsDir: true},
		{Path: "/etc", IsDir: true},
		{Path: "/var", IsDir: true},
		{Path: "/Z"},
		{Path: "/b"},
		{Path: "/etc/passwd"},
		{Path: "/etc/hosts"},
	})

	root, err := db.ListDir(ctx, repo, snap, "/")
	if err != nil {
		t.Fatalf("ListDir(/): %v", err)
	}
	// Dirs first (case-insensitive, A before a by binary tie-break), then files.
	if want := []string{"A", "a", "etc", "var", "b", "Z"}; !eqStrings(names(root), want) {
		t.Errorf("ListDir(/) = %v, want %v", names(root), want)
	}

	etc, err := db.ListDir(ctx, repo, snap, "/etc")
	if err != nil {
		t.Fatalf("ListDir(/etc): %v", err)
	}
	if want := []string{"hosts", "passwd"}; !eqStrings(names(etc), want) {
		t.Errorf("ListDir(/etc) = %v, want %v", names(etc), want)
	}
}

func TestEmptySnapshot(t *testing.T) {
	db, _, _ := newTestDB(t, 0)
	ctx := context.Background()
	repo, snap := "repo", "snap"
	itx, err := db.BeginIndex(ctx, repo, snap)
	if err != nil {
		t.Fatalf("BeginIndex: %v", err)
	}
	if err := itx.Commit(ctx); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if ok, err := db.IsIndexed(ctx, repo, snap); err != nil || !ok {
		t.Errorf("IsIndexed = %v, %v; want true, nil", ok, err)
	}
	entries, err := db.ListDir(ctx, repo, snap, "/")
	if err != nil || len(entries) != 0 {
		t.Errorf("ListDir(/) = %v, %v; want empty", entries, err)
	}
	var n int64
	if err := db.pool.QueryRowContext(ctx,
		`SELECT entries FROM snapshots WHERE repo=? AND snapshot=? AND indexed_at_unix IS NOT NULL`, repo, snap).Scan(&n); err != nil {
		t.Fatalf("marker query: %v", err)
	}
	if n != 0 {
		t.Errorf("marker entries = %d, want 0", n)
	}
}

func TestListDirNeverIndexed(t *testing.T) {
	db, _, _ := newTestDB(t, 0)
	entries, err := db.ListDir(context.Background(), "nope", "nope", "/")
	if err != nil {
		t.Fatalf("ListDir: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("never-indexed ListDir = %v, want empty", entries)
	}
}

func TestPathCleaning(t *testing.T) {
	db, _, _ := newTestDB(t, 0)
	ctx := context.Background()
	repo, snap := "repo", "snap"
	itx, err := db.BeginIndex(ctx, repo, snap)
	if err != nil {
		t.Fatalf("BeginIndex: %v", err)
	}
	// All of these clean to "/" and must be skipped (root is never its own child).
	for _, p := range []string{"", ".", "/", "//"} {
		if err := itx.Add(ctx, model.BrowseNode{Path: p, IsDir: true}); err != nil {
			t.Fatalf("Add(%q): %v", p, err)
		}
	}
	mustAdd(t, ctx, itx, model.BrowseNode{Path: "/etc/", IsDir: true}) // -> /etc
	mustAdd(t, ctx, itx, model.BrowseNode{Path: "etc/passwd"})         // -> /etc/passwd
	mustAdd(t, ctx, itx, model.BrowseNode{Path: "/a/../b"})            // -> /b
	if err := itx.Commit(ctx); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	root, _ := db.ListDir(ctx, repo, snap, "/")
	if want := []string{"/etc", "/b"}; !eqStrings(paths(root), want) {
		t.Errorf("root paths = %v, want %v", paths(root), want)
	}
	// Uncleaned "etc" must resolve to the same parent as "/etc".
	etc, _ := db.ListDir(ctx, repo, snap, "etc")
	if want := []string{"/etc/passwd"}; !eqStrings(paths(etc), want) {
		t.Errorf("etc paths = %v, want %v", paths(etc), want)
	}
}

func TestNameFallback(t *testing.T) {
	db, _, _ := newTestDB(t, 0)
	ctx := context.Background()
	repo, snap := "repo", "snap"
	// No emitted Name: BrowseName falls back to path.Base.
	mustIndex(t, db, repo, snap, []model.BrowseNode{{Path: "/foo/bar.txt"}})
	entries, _ := db.ListDir(ctx, repo, snap, "/foo")
	if len(entries) != 1 || entries[0].Name != "bar.txt" {
		t.Errorf("name fallback = %v, want [bar.txt]", names(entries))
	}
}

func TestDuplicatePathsNotCollapsedWithinBatch(t *testing.T) {
	db, _, _ := newTestDB(t, 0)
	ctx := context.Background()
	repo, snap := "repo", "snap"
	itx, _ := db.BeginIndex(ctx, repo, snap)
	mustAdd(t, ctx, itx, model.BrowseNode{Path: "/etc/passwd", Size: 1})
	mustAdd(t, ctx, itx, model.BrowseNode{Path: "/etc//passwd", Size: 2}) // cleans to same path
	if err := itx.Commit(ctx); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	entries, _ := db.ListDir(ctx, repo, snap, "/etc")
	if len(entries) != 2 {
		t.Fatalf("want 2 rows (duplicate paths are out of contract), got %d", len(entries))
	}
	sizes := map[int64]bool{}
	for _, e := range entries {
		sizes[e.Size] = true
	}
	if !sizes[1] || !sizes[2] {
		t.Errorf("duplicate sizes = %+v, want both 1 and 2", entries)
	}
}

func TestCrossFlushDuplicatesNotCollapsed(t *testing.T) {
	// The unique constraint and the upsert are gone: restic emits each tree path
	// exactly once, so the plain INSERT trusts that. Duplicates are out of contract
	// and are NOT collapsed, even across flushes.
	db, _, _ := newTestDB(t, 0)
	ctx := context.Background()
	repo, snap := "repo", "snap"
	itx, _ := db.BeginIndex(ctx, repo, snap)
	mustAdd(t, ctx, itx, model.BrowseNode{Path: "/etc/dup", Size: 1})
	if err := itx.flush(ctx); err != nil { // force the first row into its own statement
		t.Fatalf("flush: %v", err)
	}
	mustAdd(t, ctx, itx, model.BrowseNode{Path: "/etc/dup", Size: 2})
	if err := itx.Commit(ctx); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	entries, _ := db.ListDir(ctx, repo, snap, "/etc")
	if len(entries) != 2 {
		t.Fatalf("want 2 rows (cross-flush dups are not collapsed), got %d", len(entries))
	}
}

func TestSameParentPath(t *testing.T) {
	tests := []struct {
		p      string
		parent string
		want   bool
	}{
		{p: "/a", parent: "/", want: true},
		{p: "/a/b", parent: "/", want: false},
		{p: "/a/b", parent: "/a", want: true},
		{p: "/a/b/c", parent: "/a", want: false},
		{p: "/ab", parent: "/a", want: false},
		{p: "/a", parent: "", want: false},
	}
	for _, tt := range tests {
		if got := sameParentPath(tt.p, tt.parent); got != tt.want {
			t.Errorf("sameParentPath(%q, %q) = %v, want %v", tt.p, tt.parent, got, tt.want)
		}
	}
}

func TestBatchFlush(t *testing.T) {
	db, _, _ := newTestDB(t, 0)
	ctx := context.Background()
	repo, snap := "repo", "snap"
	itx, _ := db.BeginIndex(ctx, repo, snap)
	for i := range batchRows {
		if err := itx.Add(ctx, model.BrowseNode{Path: fmt.Sprintf("/f%05d", i)}); err != nil {
			t.Fatalf("Add: %v", err)
		}
	}
	if len(itx.buf) != 0 {
		t.Errorf("buffer not flushed at batchRows: %d", len(itx.buf))
	}
	if itx.insertStmt == nil {
		t.Error("full node batch did not prepare the reusable insert statement")
	}
	if err := itx.Add(ctx, model.BrowseNode{Path: "/extra"}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if len(itx.buf) != 1 {
		t.Errorf("buffer after one more = %d, want 1", len(itx.buf))
	}
	if itx.Count() != batchRows+1 {
		t.Errorf("Count = %d, want %d", itx.Count(), batchRows+1)
	}
	if err := itx.Commit(ctx); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	entries, _ := db.ListDir(ctx, repo, snap, "/")
	if len(entries) != batchRows+1 {
		t.Errorf("entries = %d, want %d", len(entries), batchRows+1)
	}
	var n int64
	if err := db.pool.QueryRowContext(ctx,
		`SELECT entries FROM snapshots WHERE repo=? AND snapshot=? AND indexed_at_unix IS NOT NULL`, repo, snap).Scan(&n); err != nil {
		t.Fatalf("scan marker entries: %v", err)
	}
	if n != int64(batchRows+1) {
		t.Errorf("marker entries = %d, want %d", n, batchRows+1)
	}
}

func TestDirBatchFlushUsesPreparedStatement(t *testing.T) {
	db, _, _ := newTestDB(t, 0)
	ctx := context.Background()
	repo, snap := "repo", "snap"
	itx, err := db.BeginIndex(ctx, repo, snap)
	if err != nil {
		t.Fatalf("BeginIndex: %v", err)
	}
	// Dirs are now held until Commit (so their subtree sizes can be folded), so the
	// buffer is never trimmed mid-stream: reserving a full dirBatchRows worth leaves
	// root + dirBatchRows rows buffered.
	for i := range dirBatchRows {
		if _, err := itx.ensureCleanDir(ctx, fmt.Sprintf("/d%05d", i)); err != nil {
			t.Fatalf("ensureCleanDir: %v", err)
		}
	}
	if len(itx.dirBuf) != dirBatchRows+1 {
		t.Errorf("dir buffer = %d, want %d (held until Commit)", len(itx.dirBuf), dirBatchRows+1)
	}
	if err := itx.Commit(ctx); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	// The first commit-time chunk is a full dirBatchRows batch, so it prepares and
	// reuses the statement.
	if itx.dirInsertStmt == nil {
		t.Error("full dir batch did not prepare the reusable insert statement")
	}
	if n := countDirs(t, db); n != dirBatchRows+1 {
		t.Errorf("dirs persisted = %d, want %d", n, dirBatchRows+1)
	}
}

func TestMetadataRoundTrip(t *testing.T) {
	db, _, _ := newTestDB(t, 0)
	ctx := context.Background()
	repo, snap := "repo", "snap"
	zone := time.FixedZone("X", 2*3600) // non-UTC: +02:00
	mt := time.Date(2021, 3, 4, 5, 6, 7, 0, zone)
	mustIndex(t, db, repo, snap, []model.BrowseNode{
		{Path: "/dir", Type: "dir", IsDir: true, ModTime: mt, OwnerKnown: true, UID: 0, GID: 0, Permissions: "drwxr-xr-x"},
		{Path: "/file", Type: "file", Size: 42, ModTime: mt, OwnerKnown: true, UID: 1000, GID: 1000},
		{Path: "/link", Type: "symlink", LinkTarget: "/dir", OwnerKnown: true},
		{Path: "/sock", Type: "socket"}, // special node, owner unknown, no mtime
		{Path: "/notime", Type: "file"}, // zero ModTime (the em-dash case)
	})
	entries, _ := db.ListDir(ctx, repo, snap, "/")
	by := map[string]model.BrowseEntry{}
	for _, e := range entries {
		by[e.Path] = e
	}

	if d := by["/dir"]; !d.IsDir || d.Type != "dir" {
		t.Errorf("dir: IsDir=%v Type=%q", d.IsDir, d.Type)
	}
	// FixedZone preserves the displayed wall-clock minute, not the zone name.
	if got := by["/dir"].ModTime.Format("2006-01-02 15:04"); got != "2021-03-04 05:06" {
		t.Errorf("mtime wall clock = %q, want 2021-03-04 05:06", got)
	}
	if d := by["/dir"]; !d.OwnerKnown || d.UID != 0 || d.GID != 0 {
		t.Errorf("real 0:0 owner: known=%v uid=%d gid=%d", d.OwnerKnown, d.UID, d.GID)
	}
	if f := by["/file"]; f.IsDir || f.Type != "file" || f.Size != 42 || f.UID != 1000 {
		t.Errorf("file: %+v", f)
	}
	if l := by["/link"]; l.Type != "symlink" || l.LinkTarget != "/dir" || l.IsDir {
		t.Errorf("symlink: Type=%q LinkTarget=%q IsDir=%v", l.Type, l.LinkTarget, l.IsDir)
	}
	if s := by["/sock"]; s.Type != "socket" {
		t.Errorf("special type = %q, want socket", s.Type)
	}
	if s := by["/sock"]; s.OwnerKnown {
		t.Errorf("missing owner should be unknown, got known")
	}
	if n := by["/notime"]; !n.ModTime.IsZero() {
		t.Errorf("zero ModTime did not round-trip: %v", n.ModTime)
	}
}

func TestIsIndexedLifecycle(t *testing.T) {
	db, _, _ := newTestDB(t, 0)
	ctx := context.Background()
	repo, snap := "repo", "snap"

	itx, _ := db.BeginIndex(ctx, repo, snap)
	if err := itx.Add(ctx, model.BrowseNode{Path: "/x"}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := itx.Rollback(); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if ok, _ := db.IsIndexed(ctx, repo, snap); ok {
		t.Error("IsIndexed true after rollback")
	}
	// Idempotent rollback.
	if err := itx.Rollback(); err != nil {
		t.Errorf("second Rollback: %v", err)
	}

	// Re-index after rollback succeeds.
	mustIndex(t, db, repo, snap, []model.BrowseNode{{Path: "/x"}})
	if ok, _ := db.IsIndexed(ctx, repo, snap); !ok {
		t.Error("IsIndexed false after commit")
	}
}

func TestPoisonedTx(t *testing.T) {
	db, _, _ := newTestDB(t, 0)
	ctx := context.Background()
	repo, snap := "repo", "snap"
	const secret = "POISON_secret_filename_qux"

	itx, _ := db.BeginIndex(ctx, repo, snap)
	if err := itx.Add(ctx, model.BrowseNode{Path: "/" + secret, Name: secret}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	// Poison: roll back the underlying tx out from under the IndexTx so the next
	// flush hits a dead transaction.
	if err := itx.tx.Rollback(); err != nil {
		t.Fatalf("inner Rollback: %v", err)
	}
	err := itx.flush(ctx)
	assertErrPathFree(t, err, secret)

	// The sticky error is returned by later Add and Commit calls.
	if e2 := itx.Add(ctx, model.BrowseNode{Path: "/other"}); e2 == nil || e2.Error() != err.Error() {
		t.Errorf("Add after poison = %v, want %v", e2, err)
	}
	e3 := itx.Commit(ctx)
	if e3 == nil || e3.Error() != err.Error() {
		t.Errorf("Commit after poison = %v, want %v", e3, err)
	}
	assertErrPathFree(t, e3, secret)
	if ok, _ := db.IsIndexed(ctx, repo, snap); ok {
		t.Error("poisoned tx was marked indexed")
	}
	if err := itx.Rollback(); err != nil {
		t.Errorf("Rollback after poison: %v", err)
	}
}

func TestDiskLimit(t *testing.T) {
	// A 1-byte ceiling: the schema alone already exceeds it, so Commit's final
	// disk check trips ErrBrowseDiskLimit and rolls back.
	db, _, _ := newTestDB(t, 1)
	ctx := context.Background()
	repo, snap := "repo", "snap"
	const secret = "DISKLIMIT_secret_name_thud"

	itx, _ := db.BeginIndex(ctx, repo, snap)
	if err := itx.Add(ctx, model.BrowseNode{Path: "/" + secret, Name: secret}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	err := itx.Commit(ctx)
	if !errors.Is(err, model.ErrBrowseDiskLimit) {
		t.Fatalf("Commit error = %v, want ErrBrowseDiskLimit", err)
	}
	assertErrPathFree(t, err, secret)
	if ok, _ := db.IsIndexed(ctx, repo, snap); ok {
		t.Error("disk-limited snapshot was marked indexed")
	}
	if n := countDirs(t, db); n != 0 {
		t.Errorf("dirs remained after disk-limit rollback: %d", n)
	}
}

func TestDirSizeErrorPathFree(t *testing.T) {
	base := t.TempDir()
	const secret = "SECRET_cache_path_component"
	db := &DB{dir: filepath.Join(base, secret)}

	_, err := db.dirSize()
	if err == nil {
		t.Fatal("expected dirSize to fail for a missing directory")
	}
	assertErrPathFree(t, err, base, secret)
	if !errors.Is(err, errReadDBDir) {
		t.Errorf("dirSize error = %v, want errReadDBDir", err)
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("dirSize error = %v, want os.ErrNotExist", err)
	}
}

func TestMarkerConflict(t *testing.T) {
	db, _, _ := newTestDB(t, 0)
	ctx := context.Background()
	repo, snap := "repo", "snap"
	const secret = "CONFLICT_secret_name_grault"

	mustIndex(t, db, repo, snap, []model.BrowseNode{{Path: "/" + secret, Name: secret}})

	// A caller that bypasses IsIndexed and re-indexes an already-committed key is
	// turned away at BeginIndex with a path-free errAlreadyIndexed — not a silent
	// replace. The original index is untouched (the second run never opens a tx).
	itx, err := db.BeginIndex(ctx, repo, snap)
	assertErrPathFree(t, err, secret)
	if !errors.Is(err, errAlreadyIndexed) {
		t.Errorf("BeginIndex err = %v, want errAlreadyIndexed", err)
	}
	if itx != nil {
		t.Error("BeginIndex returned a non-nil tx for an already-indexed snapshot")
	}
	if ok, _ := db.IsIndexed(ctx, repo, snap); !ok {
		t.Error("original index lost after conflicting re-index")
	}
}

func TestDirIDRootReservationRollback(t *testing.T) {
	db, _, _ := newTestDB(t, 0)
	ctx := context.Background()
	repo, snap := "repo", "snap"

	itx, err := db.BeginIndex(ctx, repo, snap)
	if err != nil {
		t.Fatalf("BeginIndex: %v", err)
	}
	// The root dir is reserved in memory (its did + dirBuf row) but not written to
	// the DB until Commit, so check the in-memory reservation rather than the table.
	did := itx.dirs["/"]
	if did == 0 {
		t.Fatal("root did was not allocated")
	}
	if did != itx.firstDID {
		t.Fatalf("root did = %d, want firstDID %d", did, itx.firstDID)
	}
	if err := itx.Rollback(); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if ok, _ := db.IsIndexed(ctx, repo, snap); ok {
		t.Fatal("rolled-back snapshot was marked indexed")
	}
	if n := countDirs(t, db); n != 0 {
		t.Fatalf("dirs remained after rollback: %d", n)
	}
}

func TestDirIDsCreateParentDirectories(t *testing.T) {
	db, _, _ := newTestDB(t, 0)
	ctx := context.Background()
	repo, snap := "repo", "snap"

	mustIndex(t, db, repo, snap, []model.BrowseNode{{Path: "/a/b/file"}})
	if got, want := dirPaths(t, db, repo, snap), []string{"/", "/a", "/a/b"}; !eqStrings(got, want) {
		t.Fatalf("dir paths = %v, want %v", got, want)
	}
	rows, err := db.ListDir(ctx, repo, snap, "/a/b")
	if err != nil {
		t.Fatalf("ListDir(/a/b): %v", err)
	}
	if len(rows) != 1 || rows[0].Path != "/a/b/file" {
		t.Fatalf("ListDir(/a/b) = %+v, want /a/b/file", rows)
	}
}

func TestDirIDsOutOfOrderParentMetadata(t *testing.T) {
	db, _, _ := newTestDB(t, 0)
	ctx := context.Background()
	repo, snap := "repo", "snap"

	mustIndex(t, db, repo, snap, []model.BrowseNode{
		{Path: "/parent/file"},
		{Path: "/parent", IsDir: true},
	})
	root, err := db.ListDir(ctx, repo, snap, "/")
	if err != nil {
		t.Fatalf("ListDir(/): %v", err)
	}
	if got, want := paths(root), []string{"/parent"}; !eqStrings(got, want) {
		t.Fatalf("root paths = %v, want %v", got, want)
	}
	child, err := db.ListDir(ctx, repo, snap, "/parent")
	if err != nil {
		t.Fatalf("ListDir(/parent): %v", err)
	}
	if got, want := paths(child), []string{"/parent/file"}; !eqStrings(got, want) {
		t.Fatalf("child paths = %v, want %v", got, want)
	}
}

// dirSubtreeSize reads the persisted recursive subtree size for an interned
// directory path. The path always exists for a committed snapshot (including the
// root, which ListDir never returns), so any query error is fatal.
func dirSubtreeSize(t *testing.T, db *DB, repo, snap, p string) int64 {
	t.Helper()
	var size int64
	if err := db.pool.QueryRowContext(context.Background(),
		`SELECT d.subtree_size FROM dirs d JOIN snapshots s ON s.sid=d.sid WHERE s.repo=? AND s.snapshot=? AND d.path=?`,
		repo, snap, p).Scan(&size); err != nil {
		t.Fatalf("dir subtree size %q: %v", p, err)
	}
	return size
}

func byPath(entries []model.BrowseEntry) map[string]model.BrowseEntry {
	m := make(map[string]model.BrowseEntry, len(entries))
	for _, e := range entries {
		m[e.Path] = e
	}
	return m
}

func TestSubtreeSizesRollUp(t *testing.T) {
	db, _, _ := newTestDB(t, 0)
	ctx := context.Background()
	repo, snap := "repo", "snap"
	mustIndex(t, db, repo, snap, []model.BrowseNode{
		{Path: "/home", IsDir: true},
		{Path: "/home/alex", IsDir: true},
		{Path: "/home/alex/f.txt", Size: 42},
		{Path: "/home/alex/sub", IsDir: true},
		{Path: "/home/alex/sub/h.txt", Size: 8},
		{Path: "/home/bob", IsDir: true},
		{Path: "/home/bob/g.txt", Size: 100},
		{Path: "/home/empty", IsDir: true, Size: 999}, // empty dir with a non-zero raw node size
	})

	home, err := db.ListDir(ctx, repo, snap, "/home")
	if err != nil {
		t.Fatalf("ListDir(/home): %v", err)
	}
	h := byPath(home)
	if got := h["/home/alex"].Size; got != 50 { // 42 + 8
		t.Errorf("/home/alex size = %d, want 50", got)
	}
	if got := h["/home/bob"].Size; got != 100 {
		t.Errorf("/home/bob size = %d, want 100", got)
	}
	// An empty directory reports 0, never its raw node size (999).
	if got := h["/home/empty"].Size; got != 0 {
		t.Errorf("/home/empty size = %d, want 0", got)
	}

	alex, err := db.ListDir(ctx, repo, snap, "/home/alex")
	if err != nil {
		t.Fatalf("ListDir(/home/alex): %v", err)
	}
	a := byPath(alex)
	if got := a["/home/alex/f.txt"].Size; got != 42 { // file size is unchanged
		t.Errorf("/home/alex/f.txt size = %d, want 42", got)
	}
	if got := a["/home/alex/sub"].Size; got != 8 {
		t.Errorf("/home/alex/sub size = %d, want 8", got)
	}

	// /home rolls up everything beneath it (50 + 100 + 0).
	if got := dirSubtreeSize(t, db, repo, snap, "/home"); got != 150 {
		t.Errorf("/home subtree = %d, want 150", got)
	}
	// The root is not returned by ListDir (root nodes are skipped at index time), so
	// assert its total directly. It equals the whole tree's file bytes.
	if got := dirSubtreeSize(t, db, repo, snap, "/"); got != 150 {
		t.Errorf("/ subtree = %d, want 150", got)
	}
}

func TestSearchDirCarriesSubtreeSize(t *testing.T) {
	db, _, _ := newTestDB(t, 0)
	ctx := context.Background()
	repo, snap := "repo", "snap"
	mustIndex(t, db, repo, snap, []model.BrowseNode{
		{Path: "/home", IsDir: true},
		{Path: "/home/alex", IsDir: true},
		{Path: "/home/alex/f.txt", Size: 42},
		{Path: "/home/empty", IsDir: true, Size: 999},
	})

	find := func(query, wantPath string) model.BrowseEntry {
		t.Helper()
		res, err := db.Search(ctx, repo, snap, query, 0)
		if err != nil {
			t.Fatalf("Search(%q): %v", query, err)
		}
		for _, e := range res.Rows {
			if e.IsDir && e.Path == wantPath {
				return e
			}
		}
		t.Fatalf("Search(%q) did not return the %q directory", query, wantPath)
		return model.BrowseEntry{}
	}

	if got := find("alex", "/home/alex").Size; got != 42 {
		t.Errorf("matched dir /home/alex size = %d, want 42", got)
	}
	// A zero-subtree matched directory reports 0, not its raw nodes.size (999).
	if got := find("empty", "/home/empty").Size; got != 0 {
		t.Errorf("matched empty dir size = %d, want 0", got)
	}
}

// TestDirCommitChunkingExceedsParamCap indexes more unique directories than a
// single 4-column dir INSERT could bind (floor(32766/4) = 8191), pinning the
// commit-time flushDirs chunking so Option A can never silently become one
// oversized INSERT. The big root listing also exercises dirSizes path chunking.
func TestDirCommitChunkingExceedsParamCap(t *testing.T) {
	db, _, _ := newTestDB(t, 0)
	ctx := context.Background()
	repo, snap := "repo", "snap"
	const dirCount = 9000 // > 8191, and several dirBatchRows chunks
	itx, err := db.BeginIndex(ctx, repo, snap)
	if err != nil {
		t.Fatalf("BeginIndex: %v", err)
	}
	if err := itx.Add(ctx, model.BrowseNode{Path: "/base", IsDir: true}); err != nil {
		t.Fatalf("Add(/base): %v", err)
	}
	// Each subdirectory is a real node (so it lists under /base) holding one file,
	// so /base has dirCount children and ListDir(/base) drives dirSizes path
	// chunking past dirSizePathChunk.
	for i := range dirCount {
		if err := itx.Add(ctx, model.BrowseNode{Path: fmt.Sprintf("/base/d%05d", i), IsDir: true}); err != nil {
			t.Fatalf("Add(dir): %v", err)
		}
		if err := itx.Add(ctx, model.BrowseNode{Path: fmt.Sprintf("/base/d%05d/f", i), Size: int64(i + 1)}); err != nil {
			t.Fatalf("Add(file): %v", err)
		}
	}
	if err := itx.Commit(ctx); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if n := countDirs(t, db); n != dirCount+2 { // + root + /base
		t.Errorf("dirs persisted = %d, want %d", n, dirCount+2)
	}
	base, err := db.ListDir(ctx, repo, snap, "/base")
	if err != nil {
		t.Fatalf("ListDir(/base): %v", err)
	}
	if len(base) != dirCount {
		t.Fatalf("/base entries = %d, want %d", len(base), dirCount)
	}
	// Each dir holds exactly one file of size i+1, so /base/d00042 rolls up to 43.
	if got := byPath(base)["/base/d00042"].Size; got != 43 {
		t.Errorf("/base/d00042 size via ListDir = %d, want 43", got)
	}
	// /base rolls up the sum of 1..dirCount.
	if got := dirSubtreeSize(t, db, repo, snap, "/base"); got != dirCount*(dirCount+1)/2 {
		t.Errorf("/base subtree = %d, want %d", got, dirCount*(dirCount+1)/2)
	}
}

// assertNoPlaintext fails if any regular file in dir contains needle bytes.
func assertNoPlaintext(t *testing.T, dir string, needles ...string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	sawFile := false
	for _, e := range entries {
		if !e.Type().IsRegular() {
			continue
		}
		sawFile = true
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatalf("ReadFile %s: %v", e.Name(), err)
		}
		for _, n := range needles {
			if bytes.Contains(data, []byte(n)) {
				t.Fatalf("plaintext %q found on disk in %s", n, e.Name())
			}
		}
	}
	if !sawFile {
		t.Fatal("no regular files present to grep")
	}
}

func TestEncryptionAtRest(t *testing.T) {
	db, dir, key := newTestDB(t, 0)
	ctx := context.Background()
	repo, snap := "repo", "snap"
	const secret1 = "TOPSECRET_alpha_marker_xyzzy"
	const secret2 = "TOPSECRET_beta_marker_plugh"

	mustIndex(t, db, repo, snap, []model.BrowseNode{{Path: "/" + secret1, Name: secret1}})
	// Committed: the filename lives only in the encrypted db.sqlite.
	assertNoPlaintext(t, dir, secret1)

	// secret1's sid and root did back the manual node inserts below (the stored
	// path column is gone; a node row references its parent directory by did).
	var sid, rootDID int64
	if err := db.pool.QueryRowContext(ctx,
		`SELECT sid FROM snapshots WHERE repo=? AND snapshot=?`, repo, snap).Scan(&sid); err != nil {
		t.Fatalf("sid lookup: %v", err)
	}
	if err := db.pool.QueryRowContext(ctx,
		`SELECT did FROM dirs WHERE sid=? AND path='/'`, sid).Scan(&rootDID); err != nil {
		t.Fatalf("root did lookup: %v", err)
	}
	// Force a rollback journal to exist while we grep, so the encryption proof is
	// not a vacuous "no journal existed" pass. journal_mode=DELETE keeps the journal
	// on disk (and encrypted by the adiantum VFS); the manual INSERT adds a fresh
	// node and the UPDATE touches secret1's committed page, forcing its original
	// (ciphertext) content into the journal.
	tx, err := db.pool.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("manual BeginTx: %v", err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO nodes `+insertColumns+` VALUES `+rowPlaceholder,
		sid, rootDID, secret2, "file", 0, 0, 0, 0, 0, "", 0, 0, 0, "", strings.ToLower(secret2)); err != nil {
		t.Fatalf("manual insert: %v", err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE nodes SET size = size + 1 WHERE sid=?`, sid); err != nil {
		t.Fatalf("manual update: %v", err)
	}
	matches, _ := filepath.Glob(filepath.Join(dir, "db.sqlite-*"))
	if len(matches) == 0 {
		t.Fatal("expected a rollback journal to exist during an open write tx")
	}
	assertNoPlaintext(t, dir, secret1, secret2)
	if err := tx.Rollback(); err != nil {
		t.Fatalf("manual Rollback: %v", err)
	}

	// Close only the pool so the file persists for the reopen below, then prove a
	// different key cannot read it.
	if err := db.pool.Close(); err != nil {
		t.Fatalf("pool close: %v", err)
	}
	wrong := randomKey(t)
	if wdb, err := Open(filepath.Join(dir, "db.sqlite"), wrong, 0); err == nil {
		if _, err := wdb.IsIndexed(ctx, repo, snap); err == nil {
			t.Error("wrong key read succeeded; DB is not encrypted")
		}
		_ = wdb.pool.Close()
	}

	// The correct key reopens and reads.
	rdb, err := Open(filepath.Join(dir, "db.sqlite"), key, 0)
	if err != nil {
		t.Fatalf("reopen with correct key: %v", err)
	}
	if ok, err := rdb.IsIndexed(ctx, repo, snap); err != nil || !ok {
		t.Errorf("reopened IsIndexed = %v, %v; want true, nil", ok, err)
	}

	// Close closes the pool only; whole-directory teardown is the session
	// wrapper's job (covered by cmd's TestBrowseStoreCloseRemovesSessionDir).
	if err := rdb.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestSQLiteURIPathEscaping(t *testing.T) {
	cases := []struct{ in, want string }{
		{"/plain/path/db.sqlite", "/plain/path/db.sqlite"},
		{"/has?query", "/has%3Fquery"},
		{"/has#frag", "/has%23frag"},
		{"/has%literal", "/has%25literal"},
		{"/a?b#c%d", "/a%3Fb%23c%25d"},
		{"/already%3Fencoded", "/already%253Fencoded"}, // '%' must itself escape, no double-decode
	}
	for _, tc := range cases {
		if got := sqliteURIPath(tc.in); got != tc.want {
			t.Errorf("sqliteURIPath(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestEncryptionAtRestTrickyCacheDirPath proves the adiantum VFS is NOT dropped
// when the DB path contains characters SQLite treats specially in a "file:" URI.
// A dropped VFS would silently open the DB on the plaintext VFS, writing browsed
// filenames to disk in the clear (privacy-contract breach).
func TestEncryptionAtRestTrickyCacheDirPath(t *testing.T) {
	// '#' and '%' are valid filename chars on every supported OS; '?' is valid on
	// POSIX but not on Windows, so only add it off-Windows.
	seg := "weird#dir%name"
	if runtime.GOOS != "windows" {
		seg += "?q"
	}
	dir := filepath.Join(t.TempDir(), seg)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir tricky dir: %v", err)
	}
	key := randomKey(t)
	db, err := Open(filepath.Join(dir, "db.sqlite"), key, 0)
	if err != nil {
		t.Fatalf("Open under tricky path %q: %v", dir, err)
	}
	ctx := context.Background()
	repo, snap := "repo", "snap"
	const secret = "TOPSECRET_trickypath_marker_qux"
	mustIndex(t, db, repo, snap, []model.BrowseNode{{Path: "/" + secret, Name: secret}})

	// The DB must actually live where we asked (the '%'/'?'/'#' must round-trip,
	// not redirect the open elsewhere) and must be ciphertext on disk.
	if _, err := os.Stat(filepath.Join(dir, "db.sqlite")); err != nil {
		t.Fatalf("db.sqlite not created under tricky path: %v", err)
	}
	assertNoPlaintext(t, dir, secret)

	// Prove it is genuinely keyed: a different key cannot read it.
	if err := db.pool.Close(); err != nil {
		t.Fatalf("pool close: %v", err)
	}
	if wdb, err := Open(filepath.Join(dir, "db.sqlite"), randomKey(t), 0); err == nil {
		if _, err := wdb.IsIndexed(ctx, repo, snap); err == nil {
			t.Error("wrong key read succeeded; DB under tricky path is not encrypted")
		}
		_ = wdb.pool.Close()
	}
}

// TestPathFreeFSError is the durable anchor for the path-free invariant that both
// browsedb and the cmd-level session wrapper now route through (a filesystem error
// must never carry a filename). It is privilege-independent, unlike a forced
// RemoveAll/Remove failure, so it always exercises the stripping logic.
func TestPathFreeFSError(t *testing.T) {
	if got := PathFreeFSError(nil); got != nil {
		t.Errorf("PathFreeFSError(nil) = %v, want nil", got)
	}

	const secret = "SECRET_db_path_component"
	pe := &os.PathError{Op: "remove", Path: "/cache/" + secret + "/db.sqlite", Err: syscall.EACCES}
	got := PathFreeFSError(pe)
	if got == nil {
		t.Fatal("PathFreeFSError(*os.PathError) = nil, want a path-free error")
	}
	if strings.Contains(got.Error(), secret) {
		t.Errorf("PathFreeFSError leaked the path component %q: %v", secret, got)
	}
	if !errors.Is(got, syscall.EACCES) {
		t.Errorf("PathFreeFSError(*os.PathError) lost the inner errno: %v", got)
	}

	// A non-PathError (or a PathError whose inner Err is nil) collapses to the
	// path-free ErrFilesystem sentinel rather than echoing the original string.
	if got := PathFreeFSError(errors.New("boom /cache/" + secret)); !errors.Is(got, ErrFilesystem) {
		t.Errorf("PathFreeFSError(generic) = %v, want ErrFilesystem", got)
	}
}

func lockingSupported() bool {
	f, err := os.CreateTemp("", "browsedb-lock-probe")
	if err != nil {
		return false
	}
	defer func() { _ = os.Remove(f.Name()) }()
	defer func() { _ = f.Close() }()
	_, supported := tryLock(f)
	return supported
}

func TestCleanStaleSessions(t *testing.T) {
	cache := t.TempDir()
	old := time.Now().Add(-24 * time.Hour)

	recent := filepath.Join(cache, SessionPrefix+"recent")
	mkSession(t, recent)

	stale := filepath.Join(cache, SessionPrefix+"stale")
	mkSession(t, stale)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatal(err)
	}

	other := filepath.Join(cache, "not-a-session")
	if err := os.MkdirAll(other, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(other, old, old); err != nil {
		t.Fatal(err)
	}

	if err := CleanStaleSessions(cache); err != nil {
		t.Fatalf("CleanStaleSessions: %v", err)
	}

	if _, err := os.Stat(recent); err != nil {
		t.Error("recent session was removed")
	}
	if _, err := os.Stat(other); err != nil {
		t.Error("non-session directory was removed")
	}
	if lockingSupported() {
		if _, err := os.Stat(stale); !os.IsNotExist(err) {
			t.Error("stale unlocked session was not removed")
		}
	} else {
		if _, err := os.Stat(stale); err != nil {
			t.Error("stale session removed on a platform without advisory locking")
		}
	}
}

func TestCleanStaleSessionsSkipsLocked(t *testing.T) {
	if !lockingSupported() {
		t.Skip("advisory locking unsupported on this platform")
	}
	cache := t.TempDir()
	dir := filepath.Join(cache, SessionPrefix+"live")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	lock, err := LockSession(dir)
	if err != nil {
		t.Fatalf("LockSession: %v", err)
	}
	defer func() { _ = lock.Close() }()
	old := time.Now().Add(-24 * time.Hour)
	if err := os.Chtimes(dir, old, old); err != nil {
		t.Fatal(err)
	}
	if err := CleanStaleSessions(cache); err != nil {
		t.Fatalf("CleanStaleSessions: %v", err)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Error("a live (locked) session was removed")
	}
}

func TestLockSessionRejectsHeldLock(t *testing.T) {
	if !lockingSupported() {
		t.Skip("advisory locking unsupported on this platform")
	}
	dir := t.TempDir()
	lock, err := LockSession(dir)
	if err != nil {
		t.Fatalf("first LockSession: %v", err)
	}
	defer func() { _ = lock.Close() }()

	second, err := LockSession(dir)
	if err == nil {
		_ = second.Close()
		t.Fatal("second LockSession unexpectedly acquired an already-held lock")
	}
	if !errors.Is(err, errSessionLockUnavailable) {
		t.Fatalf("second LockSession err = %v, want errSessionLockUnavailable", err)
	}
	assertErrPathFree(t, err, dir)
}

func TestLockSessionOpenErrorPathFree(t *testing.T) {
	base := t.TempDir()
	const secret = "SECRET_missing_session_dir"
	dir := filepath.Join(base, secret)

	lock, err := LockSession(dir)
	if err == nil {
		_ = lock.Close()
		t.Fatal("expected LockSession to fail for a missing session directory")
	}
	assertErrPathFree(t, err, base, secret)
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("LockSession err = %v, want os.ErrNotExist", err)
	}
}

func TestCleanStaleSessionsMissingDir(t *testing.T) {
	if err := CleanStaleSessions(filepath.Join(t.TempDir(), "does-not-exist")); err != nil {
		t.Errorf("missing cache dir: want nil, got %v", err)
	}
}

func TestLockSession(t *testing.T) {
	dir := t.TempDir()
	lock, err := LockSession(dir)
	if err != nil {
		t.Fatalf("LockSession: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, lockName)); err != nil {
		t.Errorf("lock marker not created: %v", err)
	}
	if err := lock.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
	if err := lock.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
}

func mkSession(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, lockName), nil, 0o600); err != nil {
		t.Fatal(err)
	}
}

// SubtreeCounts walks by parent-directory path prefix: recursive totals under
// the requested dir with the dir itself excluded, symlinks in neither bucket,
// byte-exact prefix matching (a "_" sibling must not bleed in as a single-char
// wildcard, and case-variant siblings like /A must not match /a — SQLite LIKE
// would do both), and known=false for a never-indexed snapshot.
func TestSubtreeCounts(t *testing.T) {
	db, _, _ := newTestDB(t, 0)
	mustIndex(t, db, "repo-a", "snap1", []model.BrowseNode{
		{Path: "/A", Name: "A", Type: "dir", IsDir: true},
		{Path: "/A/q", Name: "q", Type: "file"},
		{Path: "/A/sub", Name: "sub", Type: "dir", IsDir: true},
		{Path: "/A/sub/x", Name: "x", Type: "file"},
		{Path: "/a", Name: "a", Type: "dir", IsDir: true},
		{Path: "/a/f1", Name: "f1", Type: "file"},
		{Path: "/a/f2", Name: "f2", Type: "file"},
		{Path: "/a/link", Name: "link", Type: "symlink"},
		{Path: "/a/sub", Name: "sub", Type: "dir", IsDir: true},
		{Path: "/a/sub/f3", Name: "f3", Type: "file"},
		{Path: "/a_b", Name: "a_b", Type: "dir", IsDir: true},
		{Path: "/a_b/g", Name: "g", Type: "file"},
		{Path: "/axb", Name: "axb", Type: "dir", IsDir: true},
		{Path: "/axb/h", Name: "h", Type: "file"},
	})
	ctx := context.Background()

	cases := []struct {
		dir         string
		files, dirs int
	}{
		{"/", 7, 6},
		{"/a", 3, 1}, // must not absorb /A/sub/x (LIKE's ASCII case folding)
		{"/A", 2, 1}, // and the reverse direction
		{"/a/sub", 1, 0},
		{"/a_b", 1, 0}, // a "_" wildcard would also match /axb/h
	}
	for _, tc := range cases {
		files, dirs, known, err := db.SubtreeCounts(ctx, "repo-a", "snap1", tc.dir)
		if err != nil {
			t.Fatalf("SubtreeCounts(%q): %v", tc.dir, err)
		}
		if !known || files != tc.files || dirs != tc.dirs {
			t.Errorf("SubtreeCounts(%q) = %d files, %d dirs, known=%v; want %d, %d, true",
				tc.dir, files, dirs, known, tc.files, tc.dirs)
		}
	}

	if _, _, known, err := db.SubtreeCounts(ctx, "repo-a", "never-indexed", "/"); err != nil || known {
		t.Errorf("never-indexed snapshot: known=%v err=%v, want false, nil", known, err)
	}
}
