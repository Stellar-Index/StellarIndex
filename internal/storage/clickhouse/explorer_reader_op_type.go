package clickhouse

import (
	"context"
	"errors"
	"fmt"
)

// OpTypePage is one type-filtered operations page. ScannedFrom is the lowest
// ledger the read covered: a page shorter than its limit continues strictly
// below it, and 0 means the scan reached genesis.
type OpTypePage struct {
	Rows        []OpRow
	ScannedFrom uint32
}

// RecentOperationsOfType is RecentOperations restricted to opTypes (the lake's
// enum strings, e.g. "OperationTypePayment"), newest first, on the same keyset
// cursor.
//
// stellar.operations sorts on (ledger_seq, tx_index, op_index) with no op_type
// skip index, so the filter cannot prune. Each page instead scans at most
// recentLedgersTailWindow ledgers below its anchor (the tip on the first page)
// and never falls back to an unbounded read: a rare type yields a short or
// empty page whose continuation is OpTypePage.ScannedFrom. Measured on r1 with
// limit 200: at most 3.19M rows / 349 MiB / 238 ms per page.
func (r *ExplorerReader) RecentOperationsOfType(ctx context.Context, limit int, cur ExplorerCursor, opTypes []string) (OpTypePage, error) {
	if len(opTypes) == 0 {
		return OpTypePage{}, errors.New("clickhouse: recent operations of type: no op types")
	}
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	if !cur.IsSet() {
		var tip uint32
		if err := r.conn.QueryRow(ctx, `SELECT max(ledger_seq) FROM stellar.operations`).Scan(&tip); err != nil {
			return OpTypePage{}, fmt.Errorf("clickhouse: recent operations of type: tip: %w", err)
		}
		cur = ExplorerCursor{Ledger: tip + 1}
	}
	rows, err := r.recentOperationsPage(ctx, limit, cur, true, opTypes)
	if err != nil {
		return OpTypePage{}, err
	}
	return OpTypePage{Rows: rows, ScannedFrom: tailWindowFloor(cur.Ledger)}, nil
}

// tailWindowFloor is the lowest ledger a bounded cursor page reads. Clamped
// at 0: uint32 underflow near genesis would wrap and return nothing.
func tailWindowFloor(ledger uint32) uint32 {
	if ledger > uint32(recentLedgersTailWindow) {
		return ledger - uint32(recentLedgersTailWindow)
	}
	return 0
}
