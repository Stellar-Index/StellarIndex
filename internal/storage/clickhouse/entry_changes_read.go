package clickhouse

import (
	"context"
	"fmt"
)

// EntryHistoryWatermark is the ledger ch-entry-history has derived the sponsorship lifecycle through (0 = never
// derived, or the watermark table is absent).
func (r *ExplorerReader) EntryHistoryWatermark(ctx context.Context) (uint32, error) {
	return entryWatermarkOn(ctx, r.conn, sponsorshipWatermarkName)
}

// EntryIndexWatermark is the ledger ch-entry-index has built the account and asset entry indexes through (0 = never
// built, or the watermark table not provisioned); the entry-changes routes answer 503 below it.
func (r *ExplorerReader) EntryIndexWatermark(ctx context.Context) (uint32, error) {
	return entryWatermarkOn(ctx, r.conn, entryIndexWatermarkName)
}

// AccountEntryLedgers lists, newest first, the ledgers at or below maxLedger in which the account (raw 32-byte key)
// had an entry change in one of the roles; an absent index table reads as no ledgers.
func (r *ExplorerReader) AccountEntryLedgers(ctx context.Context, account string, roles []string, maxLedger uint32, limit int) ([]uint32, error) {
	const q = `SELECT DISTINCT ledger FROM stellar.account_entry_ledgers
		WHERE account = ? AND role IN (?) AND ledger <= ?
		ORDER BY ledger DESC LIMIT ?` + explorerScanSettings
	return r.entryLedgers(ctx, "account entry ledgers", q, account, roles, maxLedger, limit)
}

// AssetEntryLedgers lists, newest first, the ledgers at or below maxLedger in which an entry of one of the types held
// the asset changed.
func (r *ExplorerReader) AssetEntryLedgers(ctx context.Context, asset string, entryTypes []string, maxLedger uint32, limit int) ([]uint32, error) {
	const q = `SELECT DISTINCT ledger FROM stellar.asset_entry_ledgers
		WHERE asset = ? AND entry_type IN (?) AND ledger <= ?
		ORDER BY ledger DESC LIMIT ?` + explorerScanSettings
	return r.entryLedgers(ctx, "asset entry ledgers", q, asset, entryTypes, maxLedger, limit)
}

func (r *ExplorerReader) entryLedgers(ctx context.Context, label, q, key string, in []string, maxLedger uint32, limit int) ([]uint32, error) {
	if len(in) == 0 || limit <= 0 {
		return nil, nil
	}
	rows, err := r.conn.Query(ctx, q, key, in, maxLedger, limit)
	if err != nil {
		if isSchemaAbsent(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("clickhouse: %s: %w", label, err)
	}
	defer func() { _ = rows.Close() }()
	out := make([]uint32, 0, limit)
	for rows.Next() {
		var l uint32
		if err := rows.Scan(&l); err != nil {
			return nil, fmt.Errorf("clickhouse: %s scan: %w", label, err)
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// entryChangesByLedgerQuery is the derive's source query over a ledger list instead of a range; the projector needs
// every tx-scoped classic row of a ledger in sort-key order to pair state pre-images with their changes.
const entryChangesByLedgerQuery = `SELECT ledger_seq, close_time, tx_hash, op_index, change_index, intra_ledger_seq,
		change_type, entry_type, key_xdr, entry_xdr
	FROM stellar.ledger_entry_changes
	WHERE ledger_seq IN (?)
	  AND entry_type IN ('account', 'trustline', 'offer', 'data', 'claimable_balance', 'liquidity_pool')
	ORDER BY ledger_seq, tx_hash, op_index, change_index` + explorerScanSettings

// StreamEntryChangesForLedgers streams the entry-history source rows of the listed ledgers in the projector's order.
func (r *ExplorerReader) StreamEntryChangesForLedgers(ctx context.Context, ledgers []uint32, fn func(EntryHistorySourceRow) error) error {
	if len(ledgers) == 0 {
		return nil
	}
	rows, err := r.conn.Query(ctx, entryChangesByLedgerQuery, ledgers)
	if err != nil {
		return fmt.Errorf("clickhouse: query entry changes for %d ledgers: %w", len(ledgers), err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var s EntryHistorySourceRow
		if err := rows.Scan(&s.Ledger, &s.CloseTime, &s.TxHash, &s.OpIndex, &s.ChangeIndex, &s.IntraLedgerSeq,
			&s.ChangeType, &s.EntryType, &s.KeyXDR, &s.EntryXDR); err != nil {
			return fmt.Errorf("clickhouse: scan entry change: %w", err)
		}
		s.CloseTime = s.CloseTime.UTC()
		if err := fn(s); err != nil {
			return err
		}
	}
	return rows.Err()
}
