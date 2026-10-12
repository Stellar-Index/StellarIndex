package clickhouse

import (
	"context"
	"fmt"
	"time"

	"github.com/stellar/go-stellar-sdk/xdr"
)

// EntryChange is one op-scoped ledger_entry_changes row decoded for ADR-0047 movement reconstruction; Entry is nil for a 'removed' change.
type EntryChange struct {
	Ledger      uint32
	ClosedAt    time.Time
	TxHash      string
	OpIndex     int32
	ChangeIndex uint32
	ChangeType  string
	Entry       *xdr.LedgerEntry
}

// streamEntryChangesQuery backs StreamEntryChanges. FINAL: ledger_entry_changes is ReplacingMergeTree(ingested_at) and a re-derived window
// leaves un-merged duplicates, so fn would run TWICE for one change, over-counting the movement reconstruction. FINAL, not `LIMIT 1 BY`:
// entry_xdr is KB-scale and `LIMIT n BY` has breached memory on wide columns; the `ledger_seq BETWEEN ? AND ?` window bounds FINAL's merge
// cost. The ORDER BY (the per-op grouping order callers rely on) is kept.
const streamEntryChangesQuery = `
	SELECT ledger_seq, close_time, tx_hash, op_index, change_index, change_type, entry_xdr
	FROM stellar.ledger_entry_changes FINAL
	WHERE ledger_seq BETWEEN ? AND ?
	  AND op_index >= 0
	  AND entry_type = ?
	ORDER BY ledger_seq, tx_hash, op_index, change_index
`

// StreamEntryChanges streams OP-SCOPED rows for [from,to] of one entryType in stellar-core per-op order; returns ZERO rows for a range not backfilled per-op, so callers MUST run CountOpScopedEntryChanges first.
func StreamEntryChanges(ctx context.Context, addr string, from, to uint32, entryType string, fn func(EntryChange) error) error {
	if entryType == "" {
		return fmt.Errorf("clickhouse: StreamEntryChanges: entryType is empty")
	}
	conn, err := openRead(ctx, addr)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()

	rows, err := conn.Query(ctx, streamEntryChangesQuery, from, to, entryType)
	if err != nil {
		return fmt.Errorf("clickhouse: query entry changes [%d,%d] entry_type=%s: %w", from, to, entryType, err)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var (
			ledger      uint32
			closeTime   time.Time
			txHash      string
			opIndex     int32
			changeIndex uint32
			changeType  string
			entryXDR    string
		)
		if err := rows.Scan(&ledger, &closeTime, &txHash, &opIndex, &changeIndex, &changeType, &entryXDR); err != nil {
			return fmt.Errorf("clickhouse: scan entry change: %w", err)
		}

		var entryPtr *xdr.LedgerEntry
		if entryXDR != "" {
			var entry xdr.LedgerEntry
			if err := xdr.SafeUnmarshalBase64(entryXDR, &entry); err != nil {
				return fmt.Errorf("clickhouse: unmarshal entry (ledger %d tx %s op %d change %d): %w",
					ledger, txHash, opIndex, changeIndex, err)
			}
			entryPtr = &entry
		}

		if err := fn(EntryChange{
			Ledger:      ledger,
			ClosedAt:    closeTime.UTC(),
			TxHash:      txHash,
			OpIndex:     opIndex,
			ChangeIndex: changeIndex,
			ChangeType:  changeType,
			Entry:       entryPtr,
		}); err != nil {
			return err
		}
	}
	return rows.Err()
}

// CountOpScopedEntryChanges counts op-scoped rows for [from,to] via uniqExact over the primary key (not count(), which unmerged duplicates inflate); zero in a non-empty window means per-op backfill has not reached it.
func CountOpScopedEntryChanges(ctx context.Context, addr string, from, to uint32) (uint64, error) {
	conn, err := openRead(ctx, addr)
	if err != nil {
		return 0, err
	}
	defer func() { _ = conn.Close() }()

	var n uint64
	if err := conn.QueryRow(ctx, countOpScopedEntryChangesQuery, from, to).Scan(&n); err != nil {
		return 0, fmt.Errorf("clickhouse: count op-scoped entry changes [%d,%d]: %w", from, to, err)
	}
	return n, nil
}

// countOpScopedEntryChangesQuery backs CountOpScopedEntryChanges — see its
// doc comment for the uniqExact-over-count() dedup rationale.
const countOpScopedEntryChangesQuery = `
	SELECT uniqExact(ledger_seq, tx_hash, op_index, change_index)
	FROM stellar.ledger_entry_changes
	WHERE ledger_seq BETWEEN ? AND ?
	  AND op_index >= 0
`
