package timescale

import (
	"context"
	"database/sql"
	"fmt"
)

// LendingLiquidationFill is one user-liquidation (0) or bad-debt (1) fill_auction. Bid and Lot are the event's
// filled_auction_data (the filled portion, auctions/auction.rs fill), asset C-strkey -> i128 decimal string; HasAmounts is
// false when the event carries none (Blend V1 pools), so the amounts are unknown, not zero.
type LendingLiquidationFill struct {
	LendingEventPos
	AuctionType uint32
	User        string
	Filler      string
	FillPercent string
	Block       *uint32
	HasAmounts  bool
	Bid         map[string]string
	Lot         map[string]string
}

// lendingLiquidationsSQL pages both tables' liquidation and bad-debt fills (a pool lives in one), newest first, each branch on its
// pool index.
const lendingLiquidationsSQL = `
        SELECT * FROM (
            (SELECT ts, ledger, tx_hash::text, op_index, event_index, auction_type::bigint, user_address,
                    COALESCE(filler, ''), COALESCE(fill_percent::text, ''), block::bigint,
                    (bid IS NOT NULL AND lot IS NOT NULL), COALESCE(bid::text, ''), COALESCE(lot::text, ''), false
               FROM blend_auctions
              WHERE pool = $1 AND event_kind = 'fill' AND auction_type IN (0, 1)
                AND (ts, ledger, tx_hash::text, op_index, event_index) < ($2::timestamptz, $3::integer, $4::text, $5::integer, $6::integer)
              ORDER BY ts DESC, ledger DESC, tx_hash DESC, op_index DESC, event_index DESC
              LIMIT $7)
            UNION ALL
            (SELECT ledger_close_time, ledger, tx_hash::text, op_index, event_index, (attributes->>'auction_type')::bigint, user_address,
                    COALESCE(attributes->>'filler', ''), COALESCE(attributes->>'fill_percent', ''),
                    (attributes#>>'{filled_auction_data,block}')::bigint,
                    COALESCE(jsonb_typeof(attributes->'filled_auction_data'->'bid') = 'object'
                         AND jsonb_typeof(attributes->'filled_auction_data'->'lot') = 'object', false),
                    COALESCE(attributes#>>'{filled_auction_data,bid}', ''), COALESCE(attributes#>>'{filled_auction_data,lot}', ''), true
               FROM blnt_events
              WHERE contract_id = $1 AND contract_role = 'pool' AND event_kind = 'fill_auction'
                AND (attributes->>'auction_type')::bigint IN (0, 1)
                AND (ledger_close_time, ledger, tx_hash::text, op_index, event_index) < ($2::timestamptz, $3::integer, $4::text, $5::integer, $6::integer)
              ORDER BY ledger_close_time DESC, ledger DESC, tx_hash DESC, op_index DESC, event_index DESC
              LIMIT $7)
        ) f
        ORDER BY 1 DESC, 2 DESC, 3 DESC, 4 DESC, 5 DESC
        LIMIT $7`

// LendingLiquidations returns a pool's user-liquidation and bad-debt fills newest first, at most limit after cur (Kind is ignored).
func (s *Store) LendingLiquidations(ctx context.Context, pool string, limit int, cur LendingConfigCursor) ([]LendingLiquidationFill, error) {
	after := LendingConfigCursor{Ts: endOfTime}
	if cur.IsSet() {
		after = cur
	}
	args := []any{pool, after.Ts.UTC(), int64(after.Ledger), after.TxHash, int64(after.OpIndex), int64(after.EventIndex), limit}
	return queryFold(ctx, s.db, "LendingLiquidations", lendingLiquidationsSQL, args, scanLendingLiquidation)
}

func scanLendingLiquidation(rows *sql.Rows) (LendingLiquidationFill, error) {
	var f LendingLiquidationFill
	var ledger, op, ev, typ int64
	var block sql.NullInt64
	var bid, lot string
	var blntShape bool
	if err := rows.Scan(&f.Ts, &ledger, &f.TxHash, &op, &ev, &typ, &f.User, &f.Filler, &f.FillPercent, &block,
		&f.HasAmounts, &bid, &lot, &blntShape); err != nil {
		return f, err
	}
	scanEventPos(&f.LendingEventPos, &ledger, &op, &ev)
	f.AuctionType = uint32(typ) //nolint:gosec // auction_type is 0 or 1
	f.Block = nullU32(block)
	if !f.HasAmounts {
		return f, nil
	}
	var err error
	if f.Bid, err = auctionAmounts(bid, blntShape); err != nil {
		return f, fmt.Errorf("bid: %w", err)
	}
	if f.Lot, err = auctionAmounts(lot, blntShape); err != nil {
		return f, fmt.Errorf("lot: %w", err)
	}
	return f, nil
}
