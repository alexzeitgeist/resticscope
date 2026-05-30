package browsedb

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// insertBindParams is the number of bound parameters per inserted node row — one
// per column of the nodes table (sid + 14 node fields). It is fixed by the schema
// below.
const insertBindParams = 15

const dirInsertBindParams = 4

// batchRows is how many node rows accumulate before a single multi-row INSERT is
// flushed inside the index transaction. The pinned driver's
// LIMIT_VARIABLE_NUMBER = 32766 caps a batch at floor(32766/15) = 2184 rows; 1000
// binds 15000 params/batch, ~4× fewer ExecContext round-trips than the old 250
// with comfortable headroom under that cap.
const batchRows = 1000

const dirBatchRows = 1000

// insertColumns lists the nodes columns in bind order, sid first, no path. The
// upsert is gone: restic emits each tree path once, so a plain INSERT is correct.
const insertColumns = `(sid,parent_did,name,type,is_dir,size,mtime_unix,mtime_offset_sec,mtime_known,perms,uid,gid,owner_known,link_target,name_ci)`

// dirInsertColumns lists the dirs columns in bind order. It is a const beside
// insertColumns (rather than inline in the SQL builder) so the two families read
// the same way and the column list cannot drift from dirInsertBindParams.
const dirInsertColumns = "(did,sid,path,subtree_size)"

// rowPlaceholder is the "(?,?,...)" group bound for one node row, built once so
// the bind count cannot drift from insertBindParams.
var rowPlaceholder = "(" + strings.TrimSuffix(strings.Repeat("?,", insertBindParams), ",") + ")"

var dirRowPlaceholder = "(" + strings.TrimSuffix(strings.Repeat("?,", dirInsertBindParams), ",") + ")"

// batchInsertSpec carries everything the shared batch-insert path needs that
// differs between the node and dir families: the table name and column list, the
// one-row placeholder group, the error-message prefix, the full-batch row count,
// and the full-batch INSERT SQL (precomputed once by newBatchInsertSpec). Two
// values exist (nodeInsertSpec, dirInsertSpec); the shared code never switches on
// which — all table-specific detail rides in the spec.
type batchInsertSpec struct {
	table, columns, placeholder, errPrefix string
	batchRows                              int
	fullSQL                                string
}

// newBatchInsertSpec builds a spec and precomputes its full-batch INSERT SQL once
// at package init, so cachedFullStmt prepares from a ready string rather than
// rebuilding it on the first full batch of every transaction.
func newBatchInsertSpec(table, columns, placeholder, errPrefix string, batchRows int) batchInsertSpec {
	s := batchInsertSpec{table: table, columns: columns, placeholder: placeholder, errPrefix: errPrefix, batchRows: batchRows}
	s.fullSQL = buildBatchInsertSQL(s, batchRows)
	return s
}

var nodeInsertSpec = newBatchInsertSpec("nodes", insertColumns, rowPlaceholder, "index", batchRows)
var dirInsertSpec = newBatchInsertSpec("dirs", dirInsertColumns, dirRowPlaceholder, "dir", dirBatchRows)

// flush writes the buffered rows as one multi-row plain INSERT. Full batches
// reuse a tx-scoped prepared statement; the final partial batch keeps its
// dynamically-sized SQL. restic emits each tree path once, so duplicate-path
// collapse is deliberately out of the hot path (there is no unique constraint to
// upsert against).
func (itx *IndexTx) flush(ctx context.Context) error {
	if itx.failed != nil {
		return itx.failed
	}
	if len(itx.buf) == 0 {
		return nil
	}
	rows := itx.buf
	itx.buf = itx.buf[:0]

	args := buildInsertArgs(itx.sid, rows)
	if err := itx.execBatch(ctx, nodeInsertSpec, &itx.insertStmt, len(rows), args); err != nil {
		return err
	}
	itx.insertedRows += int64(len(rows))
	return nil
}

// flushDirs writes every buffered dir row in chunks of at most dirBatchRows.
// Because all dir rows are held until Commit (so their subtree sizes can be
// folded), the buffer can far exceed one INSERT's parameter budget; chunking
// keeps each statement at most dirBatchRows*dirInsertBindParams params, well
// under the driver's LIMIT_VARIABLE_NUMBER.
func (itx *IndexTx) flushDirs(ctx context.Context) error {
	if itx.failed != nil {
		return itx.failed
	}
	for len(itx.dirBuf) > 0 {
		n := len(itx.dirBuf)
		if n > dirBatchRows {
			n = dirBatchRows
		}
		chunk := itx.dirBuf[:n]
		if err := itx.flushDirRows(ctx, chunk); err != nil {
			return err
		}
		itx.dirBuf = itx.dirBuf[n:]
	}
	itx.dirBuf = nil
	return nil
}

// flushDirRows writes exactly one chunk of dir rows. A full dirBatchRows chunk
// reuses the tx-scoped prepared statement; a shorter final chunk builds a partial
// INSERT sized to its row count.
func (itx *IndexTx) flushDirRows(ctx context.Context, rows []dirRow) error {
	args := buildDirInsertArgs(itx.sid, rows)
	return itx.execBatch(ctx, dirInsertSpec, &itx.dirInsertStmt, len(rows), args)
}

// buildBatchInsertSQL builds the multi-row INSERT for spec sized to n rows:
// "INSERT INTO <table> <columns> VALUES (…),(…),…". It collapses the two former
// per-table builders, which differed only in table/columns/placeholder.
func buildBatchInsertSQL(spec batchInsertSpec, n int) string {
	var b strings.Builder
	b.WriteString("INSERT INTO ")
	b.WriteString(spec.table)
	b.WriteByte(' ')
	b.WriteString(spec.columns)
	b.WriteString(" VALUES ")
	for i := 0; i < n; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(spec.placeholder)
	}
	return b.String()
}

// cachedFullStmt lazily prepares spec's full-batch INSERT (spec.fullSQL, built
// once at package init) through *cache and reuses it on later calls within the
// transaction, so the statement is prepared at most once per tx, not per batch.
// On prepare failure it sets the sticky itx.failed = "browsedb prepare
// <errPrefix>: %w" and returns it.
func (itx *IndexTx) cachedFullStmt(ctx context.Context, spec batchInsertSpec, cache **sql.Stmt) (*sql.Stmt, error) {
	if *cache != nil {
		return *cache, nil
	}
	stmt, err := itx.tx.PrepareContext(ctx, spec.fullSQL)
	if err != nil {
		itx.failed = fmt.Errorf("browsedb prepare %s: %w", spec.errPrefix, err)
		return nil, itx.failed
	}
	*cache = stmt
	return stmt, nil
}

// execBatch runs one batch of rowCount rows (already materialized into args) for
// spec. A full batch (rowCount == spec.batchRows) reuses the tx-scoped prepared
// statement via cachedFullStmt; a shorter batch builds a partial INSERT sized to
// its row count. ExecContext failures set the sticky itx.failed = "browsedb
// <errPrefix>: %w"; cachedFullStmt errors propagate unchanged (so a prepare
// failure stays "browsedb prepare <errPrefix>: %w" and is never double-wrapped).
// rowCount is passed explicitly (callers pass len(rows)) so bind-param counts stay
// out of the shared path. It owns neither the insertedRows counter nor any
// chunking — callers keep those.
func (itx *IndexTx) execBatch(ctx context.Context, spec batchInsertSpec, cache **sql.Stmt, rowCount int, args []any) error {
	if rowCount == spec.batchRows {
		stmt, err := itx.cachedFullStmt(ctx, spec, cache)
		if err != nil {
			return err
		}
		if _, err := stmt.ExecContext(ctx, args...); err != nil {
			itx.failed = fmt.Errorf("browsedb %s: %w", spec.errPrefix, err)
			return itx.failed
		}
		return nil
	}
	query := buildBatchInsertSQL(spec, rowCount)
	if _, err := itx.tx.ExecContext(ctx, query, args...); err != nil {
		itx.failed = fmt.Errorf("browsedb %s: %w", spec.errPrefix, err)
		return itx.failed
	}
	return nil
}

func buildInsertArgs(sid int64, rows []nodeRow) []any {
	args := make([]any, 0, len(rows)*insertBindParams)
	for _, r := range rows {
		args = append(args,
			sid, r.parentDID, r.name, r.typ, boolInt(r.isDir), r.size,
			r.mtimeUnix, r.mtimeOffsetSec, boolInt(r.mtimeKnown), r.perms,
			int64(r.uid), int64(r.gid), boolInt(r.ownerKnown), r.linkTarget, r.nameCI)
	}
	return args
}

func buildDirInsertArgs(sid int64, rows []dirRow) []any {
	args := make([]any, 0, len(rows)*dirInsertBindParams)
	for _, r := range rows {
		args = append(args, r.did, sid, r.path, r.subtreeSize)
	}
	return args
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
