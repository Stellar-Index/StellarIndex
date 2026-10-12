package clickhouse

import (
	"context"
	"errors"
	"fmt"
)

// OpTypePage is one type-filtered operations page; ScannedFrom is the lowest ledger covered, a short page continues strictly below it, and 0 means the scan reached genesis.
type OpTypePage struct {
	Rows        []OpRow
	ScannedFrom uint32
}

// RecentOperationsOfType is RecentOperations restricted to opTypes, scanning at most recentLedgersTailWindow ledgers per page (no op_type skip index), so a rare type yields a short page continuing at OpTypePage.ScannedFrom.
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
