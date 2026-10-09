package clickhouse

import (
	"context"
	"fmt"
	"math/big"
	"strings"
	"time"
)

// EntryHistoryBackfillMarker is the stellar.entry_history_watermark name the
// operator writes once ch-entry-history has derived from the lake's first
// ledger (deploy/clickhouse/entry_history.sql step 4). Until it exists the
// derive may have started above genesis.
const EntryHistoryBackfillMarker = "entry_history_backfill"

// AssetEntryChangeCursor is the keyset position of one asset_entry_changes
// row: its sort key after the asset.
type AssetEntryChangeCursor struct {
	Ledger      uint32
	TxHash      string
	OpIndex     int32
	ChangeIndex uint32
	Role        string
}

// IsSet reports whether this is a continuation page.
func (c AssetEntryChangeCursor) IsSet() bool { return c.Ledger > 0 }

type assetEntryChangeVersion struct {
	row        AssetEntryChange
	ingestedAt time.Time
}

type assetEntryChangeKey struct {
	ledger      uint32
	txHash      string
	opIndex     int32
	changeIndex uint32
	role        string
}

const assetEntryChangeCols = `ledger, close_time, tx_hash, op_index, change_index, role, intra_ledger_seq,
	entry_type, change_type, changed, account, balance, fields, ingested_at`

// assetEntryChangesSQL reads one asset's rows newest first at or below the
// ceiling. The window form has no LIMIT 1 BY so ClickHouse can stop reading
// in key order (native offers and claimable balances span billions of rows);
// exactDedup keeps only the newest re-derive per key.
func assetEntryChangesSQL(hasCursor, exactDedup bool) string {
	var sb strings.Builder
	sb.WriteString("SELECT " + assetEntryChangeCols + " FROM stellar.asset_entry_changes WHERE asset = ? AND ledger <= ?")
	if hasCursor {
		// The bare `ledger <= ?` lets the primary index cut the range; the tuple alone is not pruned.
		sb.WriteString(" AND ledger <= ? AND (ledger, tx_hash, op_index, change_index, role) < (?, ?, ?, ?, ?)")
	}
	const order = " ORDER BY ledger DESC, tx_hash DESC, op_index DESC, change_index DESC, role DESC"
	if exactDedup {
		sb.WriteString(order + ", ingested_at DESC LIMIT 1 BY ledger, tx_hash, op_index, change_index, role LIMIT ?")
	} else {
		sb.WriteString(order + " LIMIT ?")
	}
	sb.WriteString(explorerScanSettings)
	return sb.String()
}

// AssetEntryChanges returns one asset's ledger-entry changes (trustline,
// offer, claimable balance, liquidity pool) from stellar.asset_entry_changes,
// newest first, keyset-paged by the table's sort key, never above maxLedger
// (inclusive). asset is the id the table stores ("native", "CODE-ISSUER" or
// "pool:<hex>"); the caller folds aliases.
func (r *ExplorerReader) AssetEntryChanges(ctx context.Context, asset string, limit int, cur AssetEntryChangeCursor, maxLedger uint32) ([]AssetEntryChange, error) {
	if limit <= 0 || limit > 200 {
		limit = 25
	}
	args := []any{asset, maxLedger}
	if cur.IsSet() {
		args = append(args, cur.Ledger, cur.Ledger, cur.TxHash, cur.OpIndex, cur.ChangeIndex, cur.Role)
	}
	window := windowRows(limit, windowFactorKeys)
	versions, err := r.queryAssetEntryChanges(ctx, asset, assetEntryChangesSQL(cur.IsSet(), false), append(args, window))
	if err != nil {
		return nil, err
	}
	deduped, ok := dedupWindow(versions, window, limit,
		func(v assetEntryChangeVersion) assetEntryChangeKey {
			return assetEntryChangeKey{v.row.Ledger, v.row.TxHash, v.row.OpIndex, v.row.ChangeIndex, v.row.Role}
		},
		func(a, b assetEntryChangeVersion) bool { return a.ingestedAt.After(b.ingestedAt) })
	if !ok {
		if deduped, err = r.queryAssetEntryChanges(ctx, asset, assetEntryChangesSQL(cur.IsSet(), true), append(args, limit)); err != nil {
			return nil, err
		}
	}
	if len(deduped) > limit {
		deduped = deduped[:limit]
	}
	out := make([]AssetEntryChange, len(deduped))
	for i, v := range deduped {
		out[i] = v.row
	}
	return out, nil
}

func (r *ExplorerReader) queryAssetEntryChanges(ctx context.Context, asset, q string, args []any) ([]assetEntryChangeVersion, error) {
	rows, err := r.conn.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("clickhouse: asset %s entry changes: %w", asset, err)
	}
	defer func() { _ = rows.Close() }()
	var out []assetEntryChangeVersion
	for rows.Next() {
		var v assetEntryChangeVersion
		c := &v.row
		if err := rows.Scan(&c.Ledger, &c.CloseTime, &c.TxHash, &c.OpIndex, &c.ChangeIndex, &c.Role, &c.IntraLedgerSeq,
			&c.EntryType, &c.ChangeType, &c.Changed, &c.Account, &c.Balance, &c.Fields, &v.ingestedAt); err != nil {
			return nil, fmt.Errorf("clickhouse: scan asset entry change row: %w", err)
		}
		if c.Balance == nil {
			c.Balance = big.NewInt(0)
		}
		c.Asset = asset
		c.CloseTime = c.CloseTime.UTC()
		out = append(out, v)
	}
	return out, rows.Err()
}

// EntryHistoryCoverage returns the ch-entry-history derive watermark and the
// ledger the operator's from-genesis backfill marker records (0 = never
// derived / not yet verified, or the watermark table is not provisioned).
func (r *ExplorerReader) EntryHistoryCoverage(ctx context.Context) (watermark, backfilledThru uint32, err error) {
	const q = `SELECT maxIf(thru_ledger, name = 'entry_history'), maxIf(thru_ledger, name = ?)
		FROM stellar.entry_history_watermark`
	if err := r.conn.QueryRow(ctx, q, EntryHistoryBackfillMarker).Scan(&watermark, &backfilledThru); err != nil {
		if isSchemaAbsent(err) {
			return 0, 0, nil
		}
		return 0, 0, fmt.Errorf("clickhouse: entry-history coverage: %w", err)
	}
	return watermark, backfilledThru, nil
}
