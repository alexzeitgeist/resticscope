package browsedb

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// insertBindParams is the number of columns bound for each node row.
const insertBindParams = 15

const dirInsertBindParams = 4

// batchRows limits each INSERT to 15,000 parameters, below the driver's 32,766
// variable limit.
const batchRows = 1000

const dirBatchRows = 1000

// insertColumns lists the nodes columns in bind order, sid first, no path. No
// upsert: restic emits each tree path once, so a plain INSERT is correct.
const insertColumns = `(sid,parent_did,name,type,is_dir,size,mtime_unix,mtime_offset_sec,mtime_known,perms,uid,gid,owner_known,link_target,name_ci)`

// dirInsertColumns lists directory columns in bind order.
const dirInsertColumns = "(did,sid,path,subtree_size)"

// rowPlaceholder derives one row's bind markers from insertBindParams.
var rowPlaceholder = "(" + strings.TrimSuffix(strings.Repeat("?,", insertBindParams), ",") + ")"

var dirRowPlaceholder = "(" + strings.TrimSuffix(strings.Repeat("?,", dirInsertBindParams), ",") + ")"

// batchInsertSpec configures the shared node and directory batch-insert path.
// fullSQL is precomputed for the reusable full-batch statement.
type batchInsertSpec struct {
	table, columns, placeholder, errPrefix string
	batchRows                              int
	fullSQL                                string
}

// newBatchInsertSpec precomputes a full-batch INSERT specification.
func newBatchInsertSpec(table, columns, placeholder, errPrefix string, batchRows int) batchInsertSpec {
	s := batchInsertSpec{table: table, columns: columns, placeholder: placeholder, errPrefix: errPrefix, batchRows: batchRows}
	s.fullSQL = buildBatchInsertSQL(s, batchRows)
	return s
}

var (
	nodeInsertSpec = newBatchInsertSpec("nodes", insertColumns, rowPlaceholder, "index", batchRows)
	dirInsertSpec  = newBatchInsertSpec("dirs", dirInsertColumns, dirRowPlaceholder, "dir", dirBatchRows)
)

// flush inserts buffered nodes. Full batches reuse a transaction-scoped
// statement; partial batches use size-specific SQL. Because nodes have no unique
// constraint, out-of-contract duplicate paths remain distinct.
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

// flushDirs writes buffered directories in parameter-safe chunks. Directories
// remain buffered until Commit computes their subtree sizes.
func (itx *IndexTx) flushDirs(ctx context.Context) error {
	if itx.failed != nil {
		return itx.failed
	}
	for len(itx.dirBuf) > 0 {
		n := min(len(itx.dirBuf), dirBatchRows)
		chunk := itx.dirBuf[:n]
		if err := itx.flushDirRows(ctx, chunk); err != nil {
			return err
		}
		itx.dirBuf = itx.dirBuf[n:]
	}
	itx.dirBuf = nil
	return nil
}

// flushDirRows writes one directory chunk, reusing the full-batch statement when
// possible.
func (itx *IndexTx) flushDirRows(ctx context.Context, rows []dirRow) error {
	args := buildDirInsertArgs(itx.sid, rows)
	return itx.execBatch(ctx, dirInsertSpec, &itx.dirInsertStmt, len(rows), args)
}

// buildBatchInsertSQL builds an n-row INSERT for spec.
func buildBatchInsertSQL(spec batchInsertSpec, n int) string {
	var b strings.Builder
	b.WriteString("INSERT INTO ")
	b.WriteString(spec.table)
	b.WriteByte(' ')
	b.WriteString(spec.columns)
	b.WriteString(" VALUES ")
	for i := range n {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(spec.placeholder)
	}
	return b.String()
}

// cachedFullStmt prepares at most one full-batch statement per transaction.
// Preparation failures become the transaction's sticky error.
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

// execBatch inserts materialized arguments for rowCount rows. Full batches reuse
// a prepared statement; partial batches build size-specific SQL. Execution
// failures become sticky, while preparation failures propagate unchanged.
// Callers retain responsibility for chunking and insertedRows accounting.
func (itx *IndexTx) execBatch(ctx context.Context, spec batchInsertSpec, cache **sql.Stmt, rowCount int, args []any) error {
	if rowCount == spec.batchRows {
		// database/sql closes the cached statement when the transaction finishes.
		stmt, err := itx.cachedFullStmt(ctx, spec, cache) //nolint:sqlclosecheck // tx-scoped cached stmt, auto-closed on tx finalize
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
