package clickhouse

import (
	"context"
	"encoding/json"
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

// MovementsByAssetBackfillMarker is the stellar.cap67_movements_watermark name written once the history copy is verified; until it exists, movements_by_asset holds only rows written since its MV was created.
const MovementsByAssetBackfillMarker = "movements_by_asset_backfill"

// AssetMovementRow is one movement of one asset from stellar.movements_by_asset, with participant rows collapsed to one from/to entry; an empty From or To is an unattributed side, never a guess.
type AssetMovementRow struct {
	Ledger          uint32
	LedgerCloseTime time.Time
	TxHash          string
	OpIndex         uint32
	LegIndex        uint32
	MovementKind    string
	Provenance      string
	Asset           string
	From            string
	To              string
	Amount          *big.Int
	Attributes      map[string]any
}

type assetMovementVersion struct {
	row        AssetMovementRow
	ingestedAt time.Time
}

const assetMovementCols = `address, ledger, ledger_close_time, tx_hash, op_index, leg_index, direction,
	movement_kind, provenance, counterparty, amount, attributes, ingested_at`

// assetMovementsSQL reads one asset's participant rows newest first at or below the ledger ceiling. The window form has no LIMIT 1 BY so
// ClickHouse can stop reading in key order (`native` spans billions of rows); exactDedup keeps only the newest row per movement.
func assetMovementsSQL(hasCursor, exactDedup bool) string {
	var sb strings.Builder
	sb.WriteString("SELECT " + assetMovementCols + " FROM stellar.movements_by_asset WHERE asset = ? AND ledger <= ?")
	if hasCursor {
		// The bare `ledger <= ?` lets the primary index cut the range; the tuple alone is not pruned.
		sb.WriteString(" AND ledger <= ? AND (ledger, tx_hash, op_index, leg_index) < (?, ?, ?, ?)")
	}
	if exactDedup {
		sb.WriteString(" ORDER BY ledger DESC, tx_hash DESC, op_index DESC, leg_index DESC, ingested_at DESC" +
			" LIMIT 1 BY ledger, tx_hash, op_index, leg_index LIMIT ?")
	} else {
		sb.WriteString(" ORDER BY ledger DESC, tx_hash DESC, op_index DESC, leg_index DESC LIMIT ?")
	}
	sb.WriteString(explorerScanSettings)
	return sb.String()
}

// AssetMovements returns one asset's movements, newest first, keyset-paged, never above maxLedger; the caller folds asset aliases, and the newest ingested_at wins per movement.
func (r *ExplorerReader) AssetMovements(ctx context.Context, asset string, limit int, cur AccountMovementCursor, maxLedger uint32) ([]AssetMovementRow, error) {
	if limit <= 0 || limit > 200 {
		limit = 25
	}
	args := []any{asset, maxLedger}
	if cur.IsSet() {
		args = append(args, cur.Ledger, cur.Ledger, cur.TxHash, cur.OpIndex, cur.LegIndex)
	}
	// Two participant rows per movement, each possibly with an un-merged duplicate.
	window := windowRows(limit, 2*windowFactorKeys)
	versions, err := r.queryAssetMovements(ctx, asset, assetMovementsSQL(cur.IsSet(), false), append(args, window))
	if err != nil {
		return nil, err
	}
	deduped, ok := dedupWindow(versions, window, limit,
		func(v assetMovementVersion) movementKey {
			return movementKey{v.row.Ledger, v.row.OpIndex, v.row.LegIndex, v.row.TxHash}
		},
		func(a, b assetMovementVersion) bool { return a.ingestedAt.After(b.ingestedAt) })
	if !ok {
		exactFallback("asset_movements")
		if deduped, err = r.queryAssetMovements(ctx, asset, assetMovementsSQL(cur.IsSet(), true), append(args, limit)); err != nil {
			return nil, err
		}
	}
	if len(deduped) > limit {
		deduped = deduped[:limit]
	}
	out := make([]AssetMovementRow, len(deduped))
	for i, v := range deduped {
		out[i] = v.row
	}
	return out, nil
}

func (r *ExplorerReader) queryAssetMovements(ctx context.Context, asset, q string, args []any) ([]assetMovementVersion, error) {
	rows, err := r.conn.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("clickhouse: asset %s movements: %w", asset, err)
	}
	defer func() { _ = rows.Close() }()
	var out []assetMovementVersion
	for rows.Next() {
		v, err := scanAssetMovementRow(rows, asset)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func scanAssetMovementRow(rows driver.Rows, asset string) (assetMovementVersion, error) {
	var v assetMovementVersion
	var address, direction, counterparty, attrs string
	var amt *big.Int
	if err := rows.Scan(&address, &v.row.Ledger, &v.row.LedgerCloseTime, &v.row.TxHash, &v.row.OpIndex,
		&v.row.LegIndex, &direction, &v.row.MovementKind, &v.row.Provenance, &counterparty, &amt, &attrs,
		&v.ingestedAt); err != nil {
		return v, fmt.Errorf("clickhouse: scan asset movement row: %w", err)
	}
	v.row.Asset = asset
	v.row.Amount = amt
	switch AccountMovementDirection(direction) {
	case AccountMovementSelf:
		v.row.From, v.row.To = address, address
	case AccountMovementReceived:
		v.row.From, v.row.To = counterparty, address
	default:
		v.row.From, v.row.To = address, counterparty
	}
	if attrs != "" && attrs != "{}" {
		if err := json.Unmarshal([]byte(attrs), &v.row.Attributes); err != nil {
			return v, fmt.Errorf("clickhouse: unmarshal asset movement attributes: %w", err)
		}
	}
	return v, nil
}

// AssetMovementsBackfilledThru returns the ledger through which the operator verified movements_by_asset's history copy (0 = not yet, or no marker table).
func (r *ExplorerReader) AssetMovementsBackfilledThru(ctx context.Context) (uint32, error) {
	const q = `SELECT max(thru_ledger) FROM stellar.cap67_movements_watermark WHERE name = ?`
	var thru uint32
	if err := r.conn.QueryRow(ctx, q, MovementsByAssetBackfillMarker).Scan(&thru); err != nil {
		if isSchemaAbsent(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("clickhouse: movements_by_asset backfill marker: %w", err)
	}
	return thru, nil
}
