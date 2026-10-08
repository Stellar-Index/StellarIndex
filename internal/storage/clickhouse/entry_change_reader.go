package clickhouse

import (
	"context"
	"fmt"
	"time"

	"github.com/stellar/go-stellar-sdk/xdr"
)

// EntryChange is one op-scoped ledger_entry_changes row — a single
// before/after/created/removed snapshot of a LedgerEntry produced by
// ONE classic operation, decoded enough for ADR-0047 Phase 4's
// entry-changes-correlated movement reconstruction
// (internal/sources/classicmovements' LiquidityPoolDeposit/Withdraw
// + CAP-0038 revocation-edge decode arms).
//
// Entry is nil for a 'removed' change (the row only carries the key,
// per internal/storage/clickhouse/extract_entry_changes.go's
// entryChangeRow) — every other ChangeType ('state'/'created'/
// 'updated'/'restored') carries the full decoded LedgerEntry.
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
// stellar.ledger_entry_changes is ReplacingMergeTree(ingested_at); a
// re-derived/re-ingested window leaves un-merged duplicate PARTS —
// byte-identical rows bar ingested_at — until a background merge, and
// without dedup StreamEntryChanges invoked fn TWICE for the same op-scoped
// change (a genuine over-count into ADR-0047 Phase 4's movement
// reconstruction, not just a display artifact). FINAL, not `LIMIT 1 BY`:
// this is an operator-run backfill utility (classic-movements-backfill),
// not a per-request serving path, entry_xdr is KB-scale (this repo has
// measured `LIMIT n BY` breaching memory budgets specifically on wide
// columns — see sac_balance_seed.go's StreamSACBalanceSeedsFullHistory
// history), and the existing `ledger_seq BETWEEN ? AND ?` window already
// bounds FINAL's merge cost to that window, exactly like
// BackfillTxHashIndex's FINAL fix. Preserves the existing ORDER BY (the
// per-op grouping order callers rely on) unchanged.
const streamEntryChangesQuery = `
	SELECT ledger_seq, close_time, tx_hash, op_index, change_index, change_type, entry_xdr
	FROM stellar.ledger_entry_changes FINAL
	WHERE ledger_seq BETWEEN ? AND ?
	  AND op_index >= 0
	  AND entry_type = ?
	ORDER BY ledger_seq, tx_hash, op_index, change_index
`

// StreamEntryChanges reads OP-SCOPED (op_index >= 0; fee/tx-level changes at
// op_index=-1 are irrelevant to a single op's movement reconstruction)
// ledger_entry_changes rows for [from,to] restricted to entryType
// ('liquidity_pool' or 'claimable_balance' for ADR-0047 Phase 4), invoking
// fn in (ledger_seq, tx_hash, op_index, change_index) order — stellar-core's
// own per-op Changes order, so a caller can treat the first row of an op as
// "before" and the last as "after" without a sort.
//
// Returns ZERO rows for a range whose per-op fidelity hasn't been backfilled
// (the legacy census feed stamps op_index=-1 exclusively), which is
// indistinguishable at the SQL layer from a window with no such changes.
// Callers MUST run CountOpScopedEntryChanges first (ADR-0047 D2's
// detect-and-skip-honestly discipline).
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

// CountOpScopedEntryChanges returns how many op-scoped (op_index >= 0)
// ledger_entry_changes rows exist for [from,to] — the window-level fidelity
// probe ADR-0047 Phase 4 needs before trusting an "empty per-op group"
// signal from StreamEntryChanges. A cheap count over a bounded range, run
// once per window rather than per-op.
//
// Zero (with a non-empty window) means the per-op fidelity backfill hasn't
// reached this range yet: treat every LiquidityPoolDeposit/Withdraw and
// CAP-0038-eligible AllowTrust/SetTrustLineFlags op in it as
// entry-changes-unavailable without querying StreamEntryChanges per op.
//
// Returns uint64: the clickhouse-go driver rejects scanning a UInt64 column
// into *int64.
//
// uniqExact over the PRIMARY KEY tuple, not count(): the table is a
// ReplacingMergeTree, so un-merged duplicate parts would inflate the count
// and give a false fidelity signal. Same idiom as
// VerifyAccountMovementsWindow. Only four narrow key columns are touched.
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
