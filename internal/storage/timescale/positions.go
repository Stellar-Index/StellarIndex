package timescale

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// This file is the read side for GET /v1/accounts/{g_strkey}/positions: six per-protocol fold
// queries, one per venue table. Each is a `WHERE <user_col> = $1 GROUP BY <venue_col...>` bounded
// by positionsVenueLimit and served by a user-leading index.
//
// Net amounts are a per-venue SUM of signed per-event deltas ("event_derived" basis), because the
// served tier is append-only and none of these tables track a running balance. sorocredit's
// credit_positions is the exception (see CreditPositionsByOwner): it has no per-event amount, so
// its figure is the protocol's own latest published statement ("basis: stateful").
//
// Every fold orders by its venue key before the LIMIT so which venues survive positionsVenueLimit
// is not plan-dependent.
const positionsVenueLimit = 500

// queryFold runs one of this file's `WHERE <user_col> = $1 ... LIMIT $2` fold queries and scans
// each row via scan, sharing the query/scan/err shell. label is the calling method's name, used
// in the wrapped error text; each method keeps its own SQL and scanned struct.
func queryFold[T any](ctx context.Context, db *sql.DB, label, query string, args []any, scan func(*sql.Rows) (T, error)) ([]T, error) {
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("timescale: %s: %w", label, err)
	}
	defer func() { _ = rows.Close() }()

	var out []T
	for rows.Next() {
		f, err := scan(rows)
		if err != nil {
			return nil, fmt.Errorf("timescale: %s scan: %w", label, err)
		}
		out = append(out, f)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("timescale: %s rows: %w", label, err)
	}
	return out, nil
}

// BlendPositionFold is one (pool, asset) fold for a user from blend_positions; SupplyNet and BorrowNet are independent nets, each its own position row and independently net-zero-filtered.
type BlendPositionFold struct {
	Pool               string
	Asset              string
	HasSupplyLeg       bool
	SupplyNet          string
	SupplySuperseded   bool
	SupplyTokens       string
	SupplyLastActivity time.Time
	SupplyLastLedger   uint32
	HasBorrowLeg       bool
	BorrowNet          string
	BorrowSuperseded   bool
	BorrowTokens       string
	BorrowLastActivity time.Time
	BorrowLastLedger   uint32
}

// blendSupplyTokenExpr / blendBorrowTokenExpr sign blend_positions.b_or_d_amount
// (b-tokens minted/burnt, d-tokens minted/burnt) the way blendSupplyNetExpr /
// blendBorrowNetExpr sign token_amount.
const (
	blendSupplyTokenExpr = `CASE WHEN event_kind IN ('supply','supply_collateral') THEN b_or_d_amount
	                            WHEN event_kind IN ('withdraw','withdraw_collateral') THEN -b_or_d_amount END`
	blendBorrowTokenExpr = `CASE WHEN event_kind IN ('borrow','flash_loan') THEN b_or_d_amount
	                            WHEN event_kind = 'repay' THEN -b_or_d_amount END`
)

// blendAuctionMovesSQL yields one (pool, asset, addr, side, delta, ledger, ts) row per movement of
// an account's b-tokens (side 'supply') or d-tokens (side 'borrow') that blend_positions cannot
// hold; delta is the signed token amount:
//
//   - a UserLiquidation (0) fill moves the filled lot bTokens and bid dTokens from the auctioned
//     user to the filler (the event carries the filled portion; see
//     docs/operations/wasm-audits/blend.md);
//   - a BadDebt (1) fill moves the auctioned account's bid dTokens to the filler; its lot is
//     backstop tokens, not a pool position;
//   - a bad_debt emission moves the user's dTokens to the backstop.
//
// Interest (2) auctions trade underlying only and move no pool position.
// blendAuctionMovesForUserSQL is the same set pre-filtered to the account bound to $1 before the
// lot/bid expansion.
const (
	blendFillMovesSQL = `
		SELECT a.pool, x.elem->>'asset' AS asset, p.addr, x.side,
		       p.sign * (x.elem->>'amount')::numeric AS delta, a.ledger, a.ts
		  FROM blend_auctions a
		 CROSS JOIN LATERAL (VALUES (a.user_address, -1), (a.filler, 1)) AS p(addr, sign)
		 CROSS JOIN LATERAL (
		        SELECT l, 'supply'::text FROM jsonb_array_elements(
		               CASE WHEN a.auction_type = 0 AND jsonb_typeof(a.lot) = 'array' THEN a.lot END) l
		        UNION ALL
		        SELECT b, 'borrow'::text FROM jsonb_array_elements(
		               CASE WHEN jsonb_typeof(a.bid) = 'array' THEN a.bid END) b
		       ) AS x(elem, side)
		 WHERE a.event_kind = 'fill' AND a.auction_type IN (0, 1) AND p.addr IS NOT NULL`
	blendBadDebtMovesSQL = `
		SELECT pool, asset, user_address, 'borrow'::text, -amount, ledger, ledger_close_time
		  FROM blend_emissions
		 WHERE event_kind = 'bad_debt' AND asset IS NOT NULL AND user_address IS NOT NULL
		   AND amount IS NOT NULL`
	blendAuctionMovesSQL        = blendFillMovesSQL + ` UNION ALL ` + blendBadDebtMovesSQL
	blendAuctionMovesForUserSQL = blendFillMovesSQL + ` AND (a.user_address = $1 OR a.filler = $1)
		UNION ALL ` + blendBadDebtMovesSQL + ` AND user_address = $1`
)

// blendPositionsByUserSQL is BlendPositionsByUser's query: the account's
// blend_positions nets FULL JOINed to its auction moves, so a moved leg
// is flagged and a leg only a move created still appears.
const blendPositionsByUserSQL = `
		WITH ev AS (
		  SELECT pool, asset,
		         COUNT(*) FILTER (WHERE event_kind IN (` + lendingSupplySideKinds + `)) AS s_n,
		         COALESCE(SUM(` + blendSupplyNetExpr + `),0) AS s_net,
		         COALESCE(SUM(` + blendSupplyTokenExpr + `),0) AS s_tok,
		         MAX(ledger_close_time) FILTER (WHERE event_kind IN (` + lendingSupplySideKinds + `)) AS s_ts,
		         MAX(ledger) FILTER (WHERE event_kind IN (` + lendingSupplySideKinds + `)) AS s_ledger,
		         COUNT(*) FILTER (WHERE event_kind IN (` + lendingBorrowSideKinds + `)) AS b_n,
		         COALESCE(SUM(` + blendBorrowNetExpr + `),0) AS b_net,
		         COALESCE(SUM(` + blendBorrowTokenExpr + `),0) AS b_tok,
		         MAX(ledger_close_time) FILTER (WHERE event_kind IN (` + lendingBorrowSideKinds + `)) AS b_ts,
		         MAX(ledger) FILTER (WHERE event_kind IN (` + lendingBorrowSideKinds + `)) AS b_ledger
		    FROM blend_positions
		   WHERE user_address = $1
		   GROUP BY pool, asset
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
		    FROM (` + blendAuctionMovesForUserSQL + `) m
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

// BlendPositionsByUser folds blend_positions into one row per (pool, asset) the user touched, with independent supply and borrow nets (see BlendPositionFold); sargable on `user_address = $1`.
func (s *Store) BlendPositionsByUser(ctx context.Context, address string) ([]BlendPositionFold, error) {
	return queryFold(ctx, s.db, "BlendPositionsByUser", blendPositionsByUserSQL, []any{address, positionsVenueLimit}, func(rows *sql.Rows) (BlendPositionFold, error) {
		var (
			f                              BlendPositionFold
			supplyActivity, borrowActivity sql.NullTime
			supplyLedger, borrowLedger     sql.NullInt64
		)
		if err := rows.Scan(&f.Pool, &f.Asset, &f.HasSupplyLeg, &f.SupplyNet, &f.SupplySuperseded, &f.SupplyTokens,
			&supplyActivity, &supplyLedger, &f.HasBorrowLeg, &f.BorrowNet, &f.BorrowSuperseded, &f.BorrowTokens,
			&borrowActivity, &borrowLedger); err != nil {
			return f, err
		}
		if supplyActivity.Valid {
			f.SupplyLastActivity = supplyActivity.Time.UTC()
		}
		if supplyLedger.Valid {
			f.SupplyLastLedger = uint32(supplyLedger.Int64) //nolint:gosec // ledger seq fits uint32
		}
		if borrowActivity.Valid {
			f.BorrowLastActivity = borrowActivity.Time.UTC()
		}
		if borrowLedger.Valid {
			f.BorrowLastLedger = uint32(borrowLedger.Int64) //nolint:gosec // ledger seq fits uint32
		}
		return f, nil
	})
}

// BlendBackstopFold is one pool's backstop-share fold for a user from blend_backstop_events; SharesNet sums `amount2` signed +deposit/-withdraw, an exact current share count.
type BlendBackstopFold struct {
	Pool         string
	SharesNet    string
	LastActivity time.Time
	LastLedger   uint32
}

// BlendBackstopSharesByUser folds blend_backstop_events into one row per pool the user has deposited into / withdrawn from.
func (s *Store) BlendBackstopSharesByUser(ctx context.Context, address string) ([]BlendBackstopFold, error) {
	const q = `
		SELECT pool,
		       COALESCE(SUM(CASE WHEN event_kind = 'deposit' THEN amount2
		                         WHEN event_kind = 'withdraw' THEN -amount2 END),0)::text,
		       MAX(ledger_close_time), MAX(ledger)
		  FROM blend_backstop_events
		 WHERE user_address = $1
		   AND event_kind IN ('deposit','withdraw')
		   AND pool IS NOT NULL
		 GROUP BY pool
		 ORDER BY pool
		 LIMIT $2`
	return queryFold(ctx, s.db, "BlendBackstopSharesByUser", q, []any{address, positionsVenueLimit}, func(rows *sql.Rows) (BlendBackstopFold, error) {
		var (
			f       BlendBackstopFold
			ledger  int64
			closeAt time.Time
		)
		if err := rows.Scan(&f.Pool, &f.SharesNet, &closeAt, &ledger); err != nil {
			return f, err
		}
		f.LastActivity = closeAt.UTC()
		f.LastLedger = uint32(ledger) //nolint:gosec // ledger seq fits uint32
		return f, nil
	})
}

// PhoenixStakeFold is one stake contract's bond/unbond fold for a user; NetAmount sums the always-positive `amount` signed +bond/-unbond, reading only bond/unbond actions.
type PhoenixStakeFold struct {
	StakeContract string
	LPToken       string
	NetAmount     string
	LastActivity  time.Time
	LastLedger    uint32
}

// PhoenixStakeByUser folds phoenix_stake_events into one row per stake contract the user has bonded into.
func (s *Store) PhoenixStakeByUser(ctx context.Context, address string) ([]PhoenixStakeFold, error) {
	const q = `
		SELECT stake_contract, lp_token,
		       COALESCE(SUM(CASE WHEN action = 'bond' THEN amount
		                         WHEN action = 'unbond' THEN -amount END),0)::text,
		       MAX(ledger_close_time), MAX(ledger)
		  FROM phoenix_stake_events
		 WHERE user_addr = $1
		   AND action IN ('bond','unbond')
		 GROUP BY stake_contract, lp_token
		 ORDER BY stake_contract, lp_token
		 LIMIT $2`
	return queryFold(ctx, s.db, "PhoenixStakeByUser", q, []any{address, positionsVenueLimit}, func(rows *sql.Rows) (PhoenixStakeFold, error) {
		var (
			f       PhoenixStakeFold
			ledger  int64
			closeAt time.Time
		)
		if err := rows.Scan(&f.StakeContract, &f.LPToken, &f.NetAmount, &closeAt, &ledger); err != nil {
			return f, err
		}
		f.LastActivity = closeAt.UTC()
		f.LastLedger = uint32(ledger) //nolint:gosec // ledger seq fits uint32
		return f, nil
	})
}

// DefindexVaultFold is one vault's df-token-share fold for a user from defindex_flows, VAULT layer only; SharesNet sums `df_tokens` signed +deposit/-withdraw and STRATEGY-layer rows are excluded.
type DefindexVaultFold struct {
	ContractID   string
	SharesNet    string
	LastActivity time.Time
	LastLedger   uint32
}

// DefindexVaultSharesByUser folds defindex_flows (vault layer) into one row per vault the user has deposited into.
func (s *Store) DefindexVaultSharesByUser(ctx context.Context, address string) ([]DefindexVaultFold, error) {
	const q = `
		SELECT contract_id,
		       COALESCE(SUM(CASE WHEN direction = 'deposit' THEN df_tokens
		                         WHEN direction = 'withdraw' THEN -df_tokens END),0)::text,
		       MAX(ledger_close_time), MAX(ledger)
		  FROM defindex_flows
		 WHERE actor = $1
		   AND layer = 'vault'
		 GROUP BY contract_id
		 ORDER BY contract_id
		 LIMIT $2`
	return queryFold(ctx, s.db, "DefindexVaultSharesByUser", q, []any{address, positionsVenueLimit}, func(rows *sql.Rows) (DefindexVaultFold, error) {
		var (
			f       DefindexVaultFold
			ledger  int64
			closeAt time.Time
		)
		if err := rows.Scan(&f.ContractID, &f.SharesNet, &closeAt, &ledger); err != nil {
			return f, err
		}
		f.LastActivity = closeAt.UTC()
		f.LastLedger = uint32(ledger) //nolint:gosec // ledger seq fits uint32
		return f, nil
	})
}

// CreditPositionFold is one sorocredit position for an owner from credit_positions joined to its MOST RECENT credit_statements row; there is no per-event amount, so the latest statement is the closest size.
type CreditPositionFold struct {
	CollateralContract string
	PositionUUID       string
	OpenedAt           time.Time
	OpenedLedger       uint32
	LatestAmount       string
	LatestActivity     time.Time
	LatestLedger       uint32
	Withdrawn          bool
}

// CreditPositionsByOwner reads every credit_positions row this owner has opened, each joined to its latest statement.
func (s *Store) CreditPositionsByOwner(ctx context.Context, address string) ([]CreditPositionFold, error) {
	const q = `
		SELECT p.collateral_contract, p.position_uuid, p.ledger_close_time, p.ledger,
		       s.amount, s.ledger_close_time, s.ledger,
		       EXISTS (
		           SELECT 1 FROM credit_events e
		            WHERE e.event_type = 'withdrawal'
		              AND e.collateral_contract = p.collateral_contract
		       )
		  FROM credit_positions p
		  LEFT JOIN LATERAL (
		         SELECT amount, ledger_close_time, ledger
		           FROM credit_statements st
		          WHERE st.position_uuid = p.position_uuid
		          ORDER BY st.ledger_close_time DESC, st.ledger DESC
		          LIMIT 1
		       ) s ON true
		 WHERE p.owner = $1
		 ORDER BY p.collateral_contract, p.position_uuid, p.ledger
		 LIMIT $2`
	return queryFold(ctx, s.db, "CreditPositionsByOwner", q, []any{address, positionsVenueLimit}, func(rows *sql.Rows) (CreditPositionFold, error) {
		var (
			f            CreditPositionFold
			opened       time.Time
			openedLedger int64
			latestAmount sql.NullString
			latestClose  sql.NullTime
			latestLedger sql.NullInt64
		)
		if err := rows.Scan(&f.CollateralContract, &f.PositionUUID, &opened, &openedLedger,
			&latestAmount, &latestClose, &latestLedger, &f.Withdrawn); err != nil {
			return f, err
		}
		f.OpenedAt = opened.UTC()
		f.OpenedLedger = uint32(openedLedger) //nolint:gosec // ledger seq fits uint32
		if latestAmount.Valid {
			f.LatestAmount = latestAmount.String
		}
		if latestClose.Valid {
			f.LatestActivity = latestClose.Time.UTC()
		}
		if latestLedger.Valid {
			f.LatestLedger = uint32(latestLedger.Int64) //nolint:gosec // ledger seq fits uint32
		}
		return f, nil
	})
}

// AquariusGaugeFold is one pool's gauge-position fold for a user from `position_update` events; NetDelta sums the SIGNED decimal `attributes->>'delta'` (not the non-negative Amount field).
type AquariusGaugeFold struct {
	ContractID   string
	NetDelta     string
	LastActivity time.Time
	LastLedger   uint32
}

// AquariusGaugeByUser folds aquarius_rewards_events position_update rows into one row per pool.
func (s *Store) AquariusGaugeByUser(ctx context.Context, address string) ([]AquariusGaugeFold, error) {
	const q = `
		SELECT contract_id,
		       COALESCE(SUM((attributes->>'delta')::numeric),0)::text,
		       MAX(ledger_close_time), MAX(ledger)
		  FROM aquarius_rewards_events
		 WHERE user_address = $1
		   AND event_kind = 'position_update'
		 GROUP BY contract_id
		 ORDER BY contract_id
		 LIMIT $2`
	return queryFold(ctx, s.db, "AquariusGaugeByUser", q, []any{address, positionsVenueLimit}, func(rows *sql.Rows) (AquariusGaugeFold, error) {
		var (
			f       AquariusGaugeFold
			ledger  int64
			closeAt time.Time
		)
		if err := rows.Scan(&f.ContractID, &f.NetDelta, &closeAt, &ledger); err != nil {
			return f, err
		}
		f.LastActivity = closeAt.UTC()
		f.LastLedger = uint32(ledger) //nolint:gosec // ledger seq fits uint32
		return f, nil
	})
}
