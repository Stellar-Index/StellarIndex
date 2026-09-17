package timescale

import (
	"context"
	"fmt"
	"time"
)

// DeFiPositionHolder is one (owner, protocol, venue, asset) position as
// the per-protocol folds report it — the same six folds that back
// /v1/accounts/{g}/positions, walked for EVERY owner instead of one.
// Amount is the fold's own decimal string in the fold's own unit: blend
// token_amount, backstop shares, phoenix LP stake, defindex vault
// shares, a credit position's latest statement, an aquarius gauge's net
// delta. It is a magnitude to rank and display, never a settlement
// figure, and a zero net is still a row (a position that was opened and
// fully closed) — the consumer decides whether closed positions count.
type DeFiPositionHolder struct {
	Protocol     string
	PositionKind string
	Venue        string
	Asset        string // "" when the position is denominated in venue shares
	User         string
	Amount       string
	LastLedger   uint32
	LastActivity time.Time
}

// Position kinds, mirroring internal/api/v1/explorer's wire vocabulary
// so a cohort row and a per-account row say the same word.
const (
	holderKindLendingSupply  = "lending_supply"
	holderKindLendingBorrow  = "lending_borrow"
	holderKindBackstopShares = "backstop_shares"
	holderKindLPStake        = "lp_stake"
	holderKindVaultShares    = "vault_shares"
	holderKindCredit         = "credit_collateral"
	holderKindGauge          = "gauge_position"
)

// DeFiPositionHolders returns every open-or-closed position across the six
// folds. Sized for a daily rollup, not a request: on 2026-09-17 the six
// tables held ~190k distinct owners between them, so the whole set is a
// few hundred thousand rows — small beside the 25M-row cohort membership
// it is joined to on the ClickHouse side, which is why the join happens
// there and not here.
func (s *Store) DeFiPositionHolders(ctx context.Context) ([]DeFiPositionHolder, error) {
	var out []DeFiPositionHolder
	for _, part := range []struct {
		name string
		fn   func(context.Context) ([]DeFiPositionHolder, error)
	}{
		{"blend", s.blendPositionHolders},
		{"blend backstop", s.blendBackstopHolders},
		{"phoenix stake", s.phoenixStakeHolders},
		{"defindex", s.defindexVaultHolders},
		{"sorocredit", s.creditPositionHolders},
		{"aquarius gauge", s.aquariusGaugeHolders},
	} {
		rows, err := part.fn(ctx)
		if err != nil {
			return nil, fmt.Errorf("timescale: DeFiPositionHolders %s: %w", part.name, err)
		}
		out = append(out, rows...)
	}
	return out, nil
}

// scanHolders runs q and reads (venue, asset, user, amount, last_close,
// last_ledger) rows into holders tagged protocol/kind.
func (s *Store) scanHolders(ctx context.Context, protocol, kind, q string) ([]DeFiPositionHolder, error) {
	rows, err := s.db.QueryContext(ctx, q)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []DeFiPositionHolder
	for rows.Next() {
		var (
			h      DeFiPositionHolder
			ledger int64
		)
		if err := rows.Scan(&h.Venue, &h.Asset, &h.User, &h.Amount, &h.LastActivity, &ledger); err != nil {
			return nil, err
		}
		h.Protocol = protocol
		h.PositionKind = kind
		h.LastLedger = uint32(ledger) //nolint:gosec // ledger seq fits uint32
		h.LastActivity = h.LastActivity.UTC()
		out = append(out, h)
	}
	return out, rows.Err()
}

// blendPositionHolders folds blend_positions per (user, pool, asset) into a
// supply leg and a borrow leg — the same CASE arithmetic as
// BlendPositionsByUser, without the user predicate and with the user in
// the GROUP BY. Uses blend_positions_pool_user_asset_idx.
func (s *Store) blendPositionHolders(ctx context.Context) ([]DeFiPositionHolder, error) {
	const supply = `
		SELECT pool, asset, user_address,
		       COALESCE(SUM(CASE WHEN event_kind IN ('supply','supply_collateral') THEN token_amount
		                         WHEN event_kind IN ('withdraw','withdraw_collateral') THEN -token_amount END),0)::text,
		       MAX(ledger_close_time), MAX(ledger)
		  FROM blend_positions
		 WHERE event_kind IN ('supply','withdraw','supply_collateral','withdraw_collateral')
		 GROUP BY pool, asset, user_address`
	const borrow = `
		SELECT pool, asset, user_address,
		       COALESCE(SUM(CASE WHEN event_kind = 'borrow' THEN token_amount
		                         WHEN event_kind = 'repay' THEN -token_amount END),0)::text,
		       MAX(ledger_close_time), MAX(ledger)
		  FROM blend_positions
		 WHERE event_kind IN ('borrow','repay')
		 GROUP BY pool, asset, user_address`
	a, err := s.scanHolders(ctx, "blend", holderKindLendingSupply, supply)
	if err != nil {
		return nil, err
	}
	b, err := s.scanHolders(ctx, "blend", holderKindLendingBorrow, borrow)
	if err != nil {
		return nil, err
	}
	return append(a, b...), nil
}

func (s *Store) blendBackstopHolders(ctx context.Context) ([]DeFiPositionHolder, error) {
	const q = `
		SELECT pool, '' AS asset, user_address,
		       COALESCE(SUM(CASE WHEN event_kind = 'deposit' THEN amount2
		                         WHEN event_kind = 'withdraw' THEN -amount2 END),0)::text,
		       MAX(ledger_close_time), MAX(ledger)
		  FROM blend_backstop_events
		 WHERE event_kind IN ('deposit','withdraw')
		   AND pool IS NOT NULL
		 GROUP BY pool, user_address`
	return s.scanHolders(ctx, "blend", holderKindBackstopShares, q)
}

func (s *Store) phoenixStakeHolders(ctx context.Context) ([]DeFiPositionHolder, error) {
	const q = `
		SELECT stake_contract, lp_token, user_addr,
		       COALESCE(SUM(CASE WHEN action = 'bond' THEN amount
		                         WHEN action = 'unbond' THEN -amount END),0)::text,
		       MAX(ledger_close_time), MAX(ledger)
		  FROM phoenix_stake_events
		 WHERE action IN ('bond','unbond')
		 GROUP BY stake_contract, lp_token, user_addr`
	return s.scanHolders(ctx, "phoenix", holderKindLPStake, q)
}

func (s *Store) defindexVaultHolders(ctx context.Context) ([]DeFiPositionHolder, error) {
	const q = `
		SELECT contract_id, '' AS asset, actor,
		       COALESCE(SUM(CASE WHEN direction = 'deposit' THEN df_tokens
		                         WHEN direction = 'withdraw' THEN -df_tokens END),0)::text,
		       MAX(ledger_close_time), MAX(ledger)
		  FROM defindex_flows
		 WHERE layer = 'vault'
		 GROUP BY contract_id, actor`
	return s.scanHolders(ctx, "defindex", holderKindVaultShares, q)
}

// creditPositionHolders reports each position's LATEST statement amount,
// one row per position not yet withdrawn. "Withdrawn" is judged the way
// CreditPositionsByOwner judges it — a withdrawal event on the
// collateral contract — so a cohort row and the per-account page agree.
// Positions with no statement yet carry '0'.
func (s *Store) creditPositionHolders(ctx context.Context) ([]DeFiPositionHolder, error) {
	const q = `
		SELECT p.collateral_contract, '' AS asset, p.owner,
		       COALESCE(s.amount::text, '0'),
		       COALESCE(s.ledger_close_time, p.ledger_close_time),
		       COALESCE(s.ledger, p.ledger)
		  FROM credit_positions p
		  LEFT JOIN LATERAL (
		         SELECT amount, ledger_close_time, ledger
		           FROM credit_statements st
		          WHERE st.position_uuid = p.position_uuid
		          ORDER BY st.ledger_close_time DESC, st.ledger DESC
		          LIMIT 1
		       ) s ON true
		 WHERE NOT EXISTS (
		           SELECT 1 FROM credit_events e
		            WHERE e.event_type = 'withdrawal'
		              AND e.collateral_contract = p.collateral_contract
		       )`
	return s.scanHolders(ctx, "sorocredit", holderKindCredit, q)
}

func (s *Store) aquariusGaugeHolders(ctx context.Context) ([]DeFiPositionHolder, error) {
	const q = `
		SELECT contract_id, '' AS asset, user_address,
		       COALESCE(SUM((attributes->>'delta')::numeric),0)::text,
		       MAX(ledger_close_time), MAX(ledger)
		  FROM aquarius_rewards_events
		 WHERE event_kind = 'position_update'
		 GROUP BY contract_id, user_address`
	return s.scanHolders(ctx, "aquarius", holderKindGauge, q)
}
