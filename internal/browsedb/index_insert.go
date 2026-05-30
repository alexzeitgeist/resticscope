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

// rowPlaceholder is the "(?,?,...)" group bound for one node row, built once so
// the bind count cannot drift from insertBindParams.
var rowPlaceholder = "(" + strings.TrimSuffix(strings.Repeat("?,", insertBindParams), ",") + ")"

var dirRowPlaceholder = "(" + strings.TrimSuffix(strings.Repeat("?,", dirInsertBindParams), ",") + ")"

var fullInsertSQL = buildInsertSQL(batchRows)

var fullDirInsertSQL = buildDirInsertSQL(dirBatchRows)

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
	if len(rows) == batchRows {
		stmt, err := itx.fullInsertStmt(ctx)
		if err != nil {
			return err
		}
		if _, err := stmt.ExecContext(ctx, args...); err != nil {
			itx.failed = fmt.Errorf("browsedb index: %w", err)
			return itx.failed
		}
		itx.insertedRows += int64(len(rows))
		return nil
	}
	query := buildInsertSQL(len(rows))
	if _, err := itx.tx.ExecContext(ctx, query, args...); err != nil {
		itx.failed = fmt.Errorf("browsedb index: %w", err)
		return itx.failed
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
	if len(rows) == dirBatchRows {
		stmt, err := itx.fullDirInsertStmt(ctx)
		if err != nil {
			return err
		}
		if _, err := stmt.ExecContext(ctx, args...); err != nil {
			itx.failed = fmt.Errorf("browsedb dir: %w", err)
			return itx.failed
		}
		return nil
	}
	query := buildDirInsertSQL(len(rows))
	if _, err := itx.tx.ExecContext(ctx, query, args...); err != nil {
		itx.failed = fmt.Errorf("browsedb dir: %w", err)
		return itx.failed
	}
	return nil
}

func (itx *IndexTx) fullInsertStmt(ctx context.Context) (*sql.Stmt, error) {
	if itx.insertStmt != nil {
		return itx.insertStmt, nil
	}
	stmt, err := itx.tx.PrepareContext(ctx, fullInsertSQL)
	if err != nil {
		itx.failed = fmt.Errorf("browsedb prepare index: %w", err)
		return nil, itx.failed
	}
	itx.insertStmt = stmt
	return stmt, nil
}

func (itx *IndexTx) fullDirInsertStmt(ctx context.Context) (*sql.Stmt, error) {
	if itx.dirInsertStmt != nil {
		return itx.dirInsertStmt, nil
	}
	stmt, err := itx.tx.PrepareContext(ctx, fullDirInsertSQL)
	if err != nil {
		itx.failed = fmt.Errorf("browsedb prepare dir: %w", err)
		return nil, itx.failed
	}
	itx.dirInsertStmt = stmt
	return stmt, nil
}

func buildInsertSQL(n int) string {
	var b strings.Builder
	b.WriteString("INSERT INTO nodes ")
	b.WriteString(insertColumns)
	b.WriteString(" VALUES ")
	for i := 0; i < n; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(rowPlaceholder)
	}
	return b.String()
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

func buildDirInsertSQL(n int) string {
	var b strings.Builder
	b.WriteString("INSERT INTO dirs (did,sid,path,subtree_size) VALUES ")
	for i := 0; i < n; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(dirRowPlaceholder)
	}
	return b.String()
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
