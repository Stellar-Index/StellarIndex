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
// 'updated') carries the full decoded LedgerEntry.
type EntryChange struct {
	Ledger      uint32
	ClosedAt    time.Time
	TxHash      string
	OpIndex     int32
	ChangeIndex uint32
	ChangeType  string
	Entry       *xdr.LedgerEntry
}

// StreamEntryChanges reads OP-SCOPED (op_index >= 0 — fee/tx-level
// changes at op_index=-1 are never relevant to a single op's
// movement reconstruction) ledger_entry_changes rows for [from,to]
// restricted to entryType ('liquidity_pool' or 'claimable_balance'
// for ADR-0047 Phase 4), invoking fn in
// (ledger_seq, tx_hash, op_index, change_index) order — the SAME
// per-op grouping order stellar-core's own Changes list uses (see
// extract_entry_changes.go), so a caller can walk one op's rows and
// treat the first as "before" and the last as "after" without an
// extra sort.
//
// Returns ZERO rows for a ledger range where ledger_entry_changes'
// per-op fidelity hasn't been backfilled yet (ADR-0047 research
// §3.2's pre-fidelity-floor legacy census feed stamps op_index=-1
// EXCLUSIVELY, which this reader's op_index>=0 filter already
// excludes) — indistinguishable at the SQL layer from "this window
// genuinely has no such entry changes." Callers MUST run their own
// window-level fidelity probe (CountOpScopedEntryChanges) before
// treating an empty per-op group as anything other than "fidelity
// absent for this window," per ADR-0047 D2's detect-and-skip-
// honestly discipline — see classic-movements-backfill's use of both
// functions together.
func StreamEntryChanges(ctx context.Context, addr string, from, to uint32, entryType string, fn func(EntryChange) error) error {
	if entryType == "" {
		return fmt.Errorf("clickhouse: StreamEntryChanges: entryType is empty")
	}
	conn, err := openRead(ctx, addr)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()

	const query = `
		SELECT ledger_seq, close_time, tx_hash, op_index, change_index, change_type, entry_xdr
		FROM stellar.ledger_entry_changes
		WHERE ledger_seq BETWEEN ? AND ?
		  AND op_index >= 0
		  AND entry_type = ?
		ORDER BY ledger_seq, tx_hash, op_index, change_index
	`
	rows, err := conn.Query(ctx, query, from, to, entryType)
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

// CountOpScopedEntryChanges returns how many op-scoped
// (op_index >= 0) ledger_entry_changes rows exist for [from,to] —
// the window-level fidelity probe ADR-0047 Phase 4 needs before
// trusting an "empty per-op group" signal from StreamEntryChanges.
// Mirrors research §3.2's exact boundary-probe shape: a cheap
// countIf(op_index >= 0) over a bounded range, run once per window
// rather than per-op.
//
// Zero (with a non-empty window) means "ledger_entry_changes'
// per-op fidelity backfill (Phase 0) hasn't reached this range yet"
// — treat every LiquidityPoolDeposit/Withdraw and CAP-0038-eligible
// AllowTrust/SetTrustLineFlags op in the window as
// entry-changes-unavailable without even querying StreamEntryChanges
// per op.
//
// Returns uint64 (not int64) to match ClickHouse's count()
// — the clickhouse-go driver rejects scanning a UInt64 column into
// *int64 (confirmed against a real r1 query during implementation;
// gate.go's TotalLedgers/rowCount follow the same uint64 convention).
func CountOpScopedEntryChanges(ctx context.Context, addr string, from, to uint32) (uint64, error) {
	conn, err := openRead(ctx, addr)
	if err != nil {
		return 0, err
	}
	defer func() { _ = conn.Close() }()

	const query = `
		SELECT count()
		FROM stellar.ledger_entry_changes
		WHERE ledger_seq BETWEEN ? AND ?
		  AND op_index >= 0
	`
	var n uint64
	if err := conn.QueryRow(ctx, query, from, to).Scan(&n); err != nil {
		return 0, fmt.Errorf("clickhouse: count op-scoped entry changes [%d,%d]: %w", from, to, err)
	}
	return n, nil
}
