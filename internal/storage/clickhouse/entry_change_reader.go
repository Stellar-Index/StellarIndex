package clickhouse

import (
	"context"
	"fmt"
	"time"

	"github.com/stellar/go-stellar-sdk/xdr"
)

// EntryChange is one op-scoped ledger_entry_changes row: a snapshot of a LedgerEntry produced by
// ONE classic operation, decoded for ADR-0047's entry-changes-correlated movement
// reconstruction (LiquidityPoolDeposit/Withdraw and CAP-0038 revocation-edge arms).
//
// Entry is nil for a 'removed' change (the row carries only the key); every other ChangeType
// ('state'/'created'/'updated'/'restored') carries the full decoded LedgerEntry.
type EntryChange struct {
	Ledger      uint32
	ClosedAt    time.Time
	TxHash      string
	OpIndex     int32
	ChangeIndex uint32
	ChangeType  string
	Entry       *xdr.LedgerEntry
}

// streamEntryChangesQuery backs StreamEntryChanges. FINAL:
// stellar.ledger_entry_changes is ReplacingMergeTree(ingested_at); a re-derived window leaves
// un-merged duplicate parts until a background merge, and without dedup fn would run TWICE for
// one op-scoped change, over-counting the movement reconstruction. FINAL, not `LIMIT 1 BY`:
// this is an operator-run backfill, not a serving path; entry_xdr is KB-scale and `LIMIT n BY`
// has breached memory budgets on wide columns; and the `ledger_seq BETWEEN ? AND ?` window
// bounds FINAL's merge cost. The ORDER BY (the per-op grouping order callers rely on) is kept.
const streamEntryChangesQuery = `
	SELECT ledger_seq, close_time, tx_hash, op_index, change_index, change_type, entry_xdr
	FROM stellar.ledger_entry_changes FINAL
	WHERE ledger_seq BETWEEN ? AND ?
	  AND op_index >= 0
	  AND entry_type = ?
	ORDER BY ledger_seq, tx_hash, op_index, change_index
`

// StreamEntryChanges reads OP-SCOPED (op_index >= 0; fee/tx-level changes at op_index=-1 are
// irrelevant to movement reconstruction) rows for [from,to] restricted to entryType
// ('liquidity_pool' or 'claimable_balance'), invoking fn in (ledger_seq, tx_hash, op_index,
// change_index) order, stellar-core's own per-op order, so the first row of an op is "before"
// and the last "after" without a sort.
//
// Returns ZERO rows for a range whose per-op fidelity hasn't been backfilled (the legacy census
// feed stamps op_index=-1 only), indistinguishable in SQL from a window with no such changes.
// Callers MUST run CountOpScopedEntryChanges first (ADR-0047 D2: detect and skip honestly).
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

// CountOpScopedEntryChanges returns how many op-scoped (op_index >= 0) rows exist for [from,to]:
// the window-level fidelity probe to run, once per window, before trusting an "empty per-op
// group" from StreamEntryChanges.
//
// Zero (with a non-empty window) means the per-op backfill hasn't reached this range: treat every
// LiquidityPoolDeposit/Withdraw and CAP-0038-eligible AllowTrust/SetTrustLineFlags op in it as
// entry-changes-unavailable without querying per op.
//
// Returns uint64 because the clickhouse-go driver rejects scanning a UInt64 into *int64.
//
// uniqExact over the PRIMARY KEY tuple, not count(): the table is a ReplacingMergeTree, so
// un-merged duplicate parts would inflate the count into a false fidelity signal. Only four
// narrow key columns are touched.
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
