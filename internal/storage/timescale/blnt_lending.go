package timescale

import (
	"context"
	"database/sql"
	"fmt"
)

// BLNT V3 lending reads over blnt_events (contract_role 'pool'): the pool list, reserve assets and
// configs behind /v1/lending, and the per-user fold behind /v1/accounts/{g}/positions. Event kinds
// and signs mirror the Blend readers; amount is underlying tokens and amount2 the b/d-token delta.

const (
	blntAuctionKinds    = `'new_auction','fill_auction','delete_auction'`
	blntSupplySideKinds = `'supply','supply_collateral','withdraw','withdraw_collateral','clawback'`
	blntPositionKinds   = blntSupplySideKinds + `,` + lendingBorrowSideKinds

	// A clawback burns the user's supply then collateral b-tokens for an exact underlying amount.
	blntSupplyNetExpr = `CASE WHEN event_kind IN ('supply','supply_collateral') THEN amount
	                          WHEN event_kind IN ('withdraw','withdraw_collateral','clawback') THEN -amount END`
	blntSupplyTokenExpr = `CASE WHEN event_kind IN ('supply','supply_collateral') THEN amount2
	                            WHEN event_kind IN ('withdraw','withdraw_collateral') THEN -amount2
	                            WHEN event_kind = 'clawback' THEN -((attributes->>'supply_burned')::numeric
	                                                              + (attributes->>'collateral_burned')::numeric) END`
	blntBorrowNetExpr = `CASE WHEN event_kind IN ('borrow','flash_loan') THEN amount
	                          WHEN event_kind = 'repay' THEN -amount END`
	blntBorrowTokenExpr = `CASE WHEN event_kind IN ('borrow','flash_loan') THEN amount2
	                            WHEN event_kind = 'repay' THEN -amount2 END`
)

// ListBLNTPools is ListBlendPools over blnt_events: one row per BLNT pool with position or auction events.
func (s *Store) ListBLNTPools(ctx context.Context) ([]BlendPoolSummary, error) {
	const q = `
        SELECT contract_id,
               COUNT(*) FILTER (WHERE event_kind IN (` + blntAuctionKinds + `) AND ledger_close_time > NOW() - INTERVAL '24 hours'),
               COUNT(*) FILTER (WHERE event_kind IN (` + blntAuctionKinds + `)) AS atot,
               GREATEST(
                 COUNT(DISTINCT user_address) FILTER (WHERE event_kind IN (` + blntAuctionKinds + `) AND ledger_close_time > NOW() - INTERVAL '30 days'),
                 COUNT(DISTINCT user_address) FILTER (WHERE event_kind IN (` + blntPositionKinds + `) AND ledger_close_time > NOW() - INTERVAL '30 days')),
               MAX(ledger_close_time),
               CASE WHEN COUNT(DISTINCT asset) FILTER (WHERE event_kind IN (` + blntPositionKinds + `) AND ledger_close_time > NOW() - INTERVAL '30 days') <= 1
                    THEN COALESCE(SUM(` + blntSupplyNetExpr + `) FILTER (WHERE ledger_close_time > NOW() - INTERVAL '30 days'), 0)::text END,
               CASE WHEN COUNT(DISTINCT asset) FILTER (WHERE event_kind IN (` + blntPositionKinds + `) AND ledger_close_time > NOW() - INTERVAL '30 days') <= 1
                    THEN COALESCE(SUM(` + blntBorrowNetExpr + `) FILTER (WHERE ledger_close_time > NOW() - INTERVAL '30 days'), 0)::text END
          FROM blnt_events
         WHERE contract_role = 'pool'
           AND event_kind IN (` + blntAuctionKinds + `,` + blntPositionKinds + `)
         GROUP BY contract_id
         ORDER BY atot DESC, contract_id ASC`
	return queryFold(ctx, s.db, "ListBLNTPools", q, nil, func(rows *sql.Rows) (BlendPoolSummary, error) {
		var p BlendPoolSummary
		var supplied, borrowed sql.NullString
		err := rows.Scan(&p.Pool, &p.Auctions24h, &p.AuctionsTotal, &p.UniqueUsers30d, &p.LastSeen, &supplied, &borrowed)
		p.NetSupplied30d, p.NetBorrowed30d = nullStringPtr(supplied), nullStringPtr(borrowed)
		return p, err
	})
}

// BLNTPoolAssets returns the reserve assets of a BLNT pool from its set_reserve and position events,
// busiest first; empty when the contract has emitted no BLNT pool event.
func (s *Store) BLNTPoolAssets(ctx context.Context, pool string) ([]string, error) {
	const q = `
        SELECT asset
          FROM blnt_events
         WHERE contract_id = $1 AND contract_role = 'pool' AND asset IS NOT NULL
           AND event_kind IN ('set_reserve',` + blntPositionKinds + `)
         GROUP BY asset
         ORDER BY count(*) DESC, asset`
	return queryFold(ctx, s.db, "BLNTPoolAssets", q, []any{pool}, func(rows *sql.Rows) (string, error) {
		var a string
		return a, rows.Scan(&a)
	})
}

// BLNTReserveConfigMetadata returns the latest APPLIED queue_set_reserve metadata JSON per asset (the same lifecycle rule as
// BlendReserveConfigs, same-ledger steps in apply order, by [lifecycleRankExpr] where it is unknown); parse with blnt.ParseReserveConfigMetadata.
func (s *Store) BLNTReserveConfigMetadata(ctx context.Context, pool string) (map[string][]byte, error) {
	const q = `
        WITH ranked AS (
            SELECT asset, event_kind, ledger_close_time, ledger, tx_hash, op_index, event_index, attributes->'metadata' AS metadata,
                   ` + txOrderExpr + ` AS tx_order, ` + lifecycleRankExpr + ` AS lifecycle_rank
              FROM blnt_events
             WHERE contract_id = $1 AND contract_role = 'pool' AND asset IS NOT NULL
               AND event_kind IN (` + reserveConfigKinds + `)
        ), chained AS (
            SELECT ranked.*,
                   LEAD(event_kind) OVER (PARTITION BY asset
                                          ORDER BY ledger_close_time, ledger, tx_order, lifecycle_rank, tx_hash, op_index, event_index) AS next_kind
              FROM ranked
        )
        SELECT DISTINCT ON (asset) asset, metadata::text
          FROM chained
         WHERE event_kind = 'queue_set_reserve' AND metadata IS NOT NULL AND next_kind = 'set_reserve'
         ORDER BY asset, ledger_close_time DESC, ledger DESC, tx_order DESC, lifecycle_rank DESC, tx_hash DESC, op_index DESC,
                  event_index DESC`
	rows, err := s.db.QueryContext(ctx, q, pool)
	if err != nil {
		return nil, fmt.Errorf("timescale: BLNTReserveConfigMetadata: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := map[string][]byte{}
	for rows.Next() {
		var asset, meta string
		if err := rows.Scan(&asset, &meta); err != nil {
			return nil, fmt.Errorf("timescale: BLNTReserveConfigMetadata scan: %w", err)
		}
		out[asset] = []byte(meta)
	}
	return out, rows.Err()
}

// blntMovesForUserSQL is blendAuctionMovesForUserSQL over blnt_events: a user-liquidation (0) fill moves the
// filled lot b-tokens and bid d-tokens from the auctioned user to the filler, a bad-debt (1) fill moves the
// bid d-tokens, and a bad_debt event moves the user's d-tokens to the backstop. filled_auction_data is the
// filled portion (auctions/auction.rs fill), its bid/lot JSON objects keyed by asset.
const blntMovesForUserSQL = `
		SELECT a.contract_id AS pool, x.asset, p.addr, x.side,
		       p.sign * x.amt::numeric AS delta, a.ledger, a.ledger_close_time AS ts
		  FROM blnt_events a
		 CROSS JOIN LATERAL (VALUES (a.user_address, -1), (a.attributes->>'filler', 1)) AS p(addr, sign)
		 CROSS JOIN LATERAL (
		        SELECT l.key, l.value, 'supply'::text FROM jsonb_each_text(
		               CASE WHEN (a.attributes->>'auction_type')::int = 0
		                     AND jsonb_typeof(a.attributes->'filled_auction_data'->'lot') = 'object'
		                    THEN a.attributes->'filled_auction_data'->'lot' END) l
		        UNION ALL
		        SELECT b.key, b.value, 'borrow'::text FROM jsonb_each_text(
		               CASE WHEN jsonb_typeof(a.attributes->'filled_auction_data'->'bid') = 'object'
		                    THEN a.attributes->'filled_auction_data'->'bid' END) b
		       ) AS x(asset, amt, side)
		 WHERE a.contract_role = 'pool' AND a.event_kind = 'fill_auction'
		   AND (a.attributes->>'auction_type')::int IN (0, 1)
		   AND (a.user_address = $1 OR a.attributes->>'filler' = $1)
		UNION ALL
		SELECT contract_id, asset, user_address, 'borrow'::text, -amount, ledger, ledger_close_time
		  FROM blnt_events
		 WHERE contract_role = 'pool' AND event_kind = 'bad_debt' AND user_address = $1
		   AND asset IS NOT NULL AND amount IS NOT NULL`

// blntPositionsByUserSQL is blendPositionsByUserSQL over blnt_events.
const blntPositionsByUserSQL = `
		WITH ev AS (
		  SELECT contract_id AS pool, asset,
		         COUNT(*) FILTER (WHERE event_kind IN (` + blntSupplySideKinds + `)) AS s_n,
		         COALESCE(SUM(` + blntSupplyNetExpr + `),0) AS s_net,
		         COALESCE(SUM(` + blntSupplyTokenExpr + `),0) AS s_tok,
		         MAX(ledger_close_time) FILTER (WHERE event_kind IN (` + blntSupplySideKinds + `)) AS s_ts,
		         MAX(ledger) FILTER (WHERE event_kind IN (` + blntSupplySideKinds + `)) AS s_ledger,
		         COUNT(*) FILTER (WHERE event_kind IN (` + lendingBorrowSideKinds + `)) AS b_n,
		         COALESCE(SUM(` + blntBorrowNetExpr + `),0) AS b_net,
		         COALESCE(SUM(` + blntBorrowTokenExpr + `),0) AS b_tok,
		         MAX(ledger_close_time) FILTER (WHERE event_kind IN (` + lendingBorrowSideKinds + `)) AS b_ts,
		         MAX(ledger) FILTER (WHERE event_kind IN (` + lendingBorrowSideKinds + `)) AS b_ledger
		    FROM blnt_events
		   WHERE user_address = $1 AND contract_role = 'pool' AND asset IS NOT NULL
		     AND event_kind IN (` + blntPositionKinds + `)
		   GROUP BY contract_id, asset
		), mv AS (
		  SELECT pool, asset,
		         COUNT(*) FILTER (WHERE side = 'supply') AS s_n,
		         COALESCE(SUM(delta) FILTER (WHERE side = 'supply'),0) AS s_tok,
		         MAX(ts) FILTER (WHERE side = 'supply') AS s_ts,
		         MAX(ledger) FILTER (WHERE side = 'supply') AS s_ledger,
		         COUNT(*) FILTER (WHERE side = 'borrow') AS b_n,
		         COALESCE(SUM(delta) FILTER (WHERE side = 'borrow'),0) AS b_tok,
		         MAX(ts) FILTER (WHERE side = 'borrow') AS b_ts,
		         MAX(ledger) FILTER (WHERE side = 'borrow') AS b_ledger
		    FROM (` + blntMovesForUserSQL + `) m
		   WHERE addr = $1
		   GROUP BY pool, asset
		)
		SELECT COALESCE(e.pool, m.pool), COALESCE(e.asset, m.asset),
		       COALESCE(e.s_n,0) + COALESCE(m.s_n,0) > 0,
		       COALESCE(e.s_net,0)::text,
		       COALESCE(m.s_n,0) > 0,
		       (COALESCE(e.s_tok,0) + COALESCE(m.s_tok,0))::text,
		       GREATEST(e.s_ts, m.s_ts), GREATEST(e.s_ledger, m.s_ledger),
		       COALESCE(e.b_n,0) + COALESCE(m.b_n,0) > 0,
		       COALESCE(e.b_net,0)::text,
		       COALESCE(m.b_n,0) > 0,
		       (COALESCE(e.b_tok,0) + COALESCE(m.b_tok,0))::text,
		       GREATEST(e.b_ts, m.b_ts), GREATEST(e.b_ledger, m.b_ledger)
		  FROM ev e
		  FULL JOIN mv m ON m.pool = e.pool AND m.asset = e.asset
		 ORDER BY 1, 2
		 LIMIT $2`

// BLNTPositionsByUser folds a user's BLNT pool events into one row per (pool, asset), with the same leg
// semantics as BlendPositionsByUser; sargable on blnt_events_user_ts_idx.
func (s *Store) BLNTPositionsByUser(ctx context.Context, address string) ([]BlendPositionFold, error) {
	return queryFold(ctx, s.db, "BLNTPositionsByUser", blntPositionsByUserSQL, []any{address, positionsVenueLimit}, scanLendingPositionFold)
}
