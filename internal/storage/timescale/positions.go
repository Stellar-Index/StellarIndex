package timescale

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// This file is the read side for GET /v1/accounts/{g_strkey}/positions
// (the "DeFi positions" view) — six per-protocol fold queries, one per
// venue table the ADR-0035-gated on-chain sources write into. Every
// query is a `WHERE <user_col> = $1 GROUP BY <venue_col...>` bounded by
// positionsVenueLimit (a user's realistic venue fan-out is nowhere near
// the cap — this is defence-in-depth, the same convention
// poolTokensRowLimit uses in pool_tokens.go), and every `WHERE` clause
// is served by a user-leading index (four pre-existing; two added by
// migration 0107 — see that file's header for which four already had
// one).
//
// Net-position amounts are computed HERE, in SQL, as a per-venue SUM of
// signed per-event deltas — "event_derived" basis. None of these tables
// track a running balance the projector maintains; the served-tier
// design is strictly append-only (ADR-0031/0032 "one writer, sole
// writer" — the projector INSERTs one row per observed event, never
// UPDATEs a running total). sorocredit's credit_positions is the one
// exception (see CreditPositionsByOwner) — it has no per-event amount
// at all, so its "current" figure is the protocol's own most-recently-
// published statement, "basis: stateful", not a delta sum.
//
// positionsVenueLimit bounds every fold's venue fan-out per the
// task's "a user won't have >500 venues" note.
const positionsVenueLimit = 500

// BlendPositionFold is one (pool, asset) money-market fold for a user,
// read from blend_positions (migration 0045/0053/0054). SupplyNet and
// BorrowNet are independent nets — a user can carry both a supply and a
// borrow position in the same (pool, asset) simultaneously (over-
// collateralized borrowing against the same reserve is not how Blend
// works, but supplying reserve A while borrowing reserve A in the SAME
// pool is a legitimate degenerate case the fold does not special-case
// away) — each becomes its own position row at the handler layer,
// independently net-zero-filtered.
//
// SupplyNet sums `token_amount` (the migration-0045 doc comment:
// "token_amount = tokens_in (supply/supply_collateral) = tokens_out
// (withdraw/withdraw_collateral)" — the UNDERLYING asset amount, NOT
// `b_or_d_amount`, the b-token amount) signed +supply/+supply_collateral,
// -withdraw/-withdraw_collateral. BorrowNet sums the same `token_amount`
// column signed +borrow, -repay. `flash_loan` is EXCLUDED from both:
// its `token_amount` is `tokens_out` (a same-tx, fully-repaid draw per
// Blend's flash-loan contract invariant — funds must be returned within
// the same transaction or it reverts), not a lasting position; including
// it would misrepresent a transient intra-tx draw as a standing
// borrow-side balance.
//
// Because these are summed UNDERLYING amounts observed at each
// historical event, NOT a live read of the pool's current b/d-token
// exchange rate, the fold does not (and cannot, from this table alone)
// reflect interest accrued since each event — amount_semantics
// "net_underlying_at_event_time" at the handler layer documents this
// explicitly per event.
type BlendPositionFold struct {
	Pool               string
	Asset              string
	HasSupplyLeg       bool
	SupplyNet          string
	SupplyLastActivity time.Time
	SupplyLastLedger   uint32
	HasBorrowLeg       bool
	BorrowNet          string
	BorrowLastActivity time.Time
	BorrowLastLedger   uint32
}

// BlendPositionsByUser folds blend_positions into one row per (pool,
// asset) the user has ever touched via supply/withdraw/
// supply_collateral/withdraw_collateral/borrow/repay, computing an
// independent net for the supply side and the borrow side (see
// BlendPositionFold's doc comment for exactly which column/sign
// convention each uses and why flash_loan is excluded).
//
// Sargable: `WHERE user_address = $1` is served by
// blend_positions_user_ts_idx (migration 0107); the GROUP BY then
// operates only on that user's own (typically tiny) row set.
func (s *Store) BlendPositionsByUser(ctx context.Context, address string) ([]BlendPositionFold, error) {
	const q = `
		SELECT pool, asset,
		       (COUNT(*) FILTER (WHERE event_kind IN ('supply','withdraw','supply_collateral','withdraw_collateral'))) > 0,
		       COALESCE(SUM(CASE WHEN event_kind IN ('supply','supply_collateral') THEN token_amount
		                         WHEN event_kind IN ('withdraw','withdraw_collateral') THEN -token_amount END),0)::text,
		       MAX(ledger_close_time) FILTER (WHERE event_kind IN ('supply','withdraw','supply_collateral','withdraw_collateral')),
		       MAX(ledger) FILTER (WHERE event_kind IN ('supply','withdraw','supply_collateral','withdraw_collateral')),
		       (COUNT(*) FILTER (WHERE event_kind IN ('borrow','repay'))) > 0,
		       COALESCE(SUM(CASE WHEN event_kind = 'borrow' THEN token_amount
		                         WHEN event_kind = 'repay' THEN -token_amount END),0)::text,
		       MAX(ledger_close_time) FILTER (WHERE event_kind IN ('borrow','repay')),
		       MAX(ledger) FILTER (WHERE event_kind IN ('borrow','repay'))
		  FROM blend_positions
		 WHERE user_address = $1
		   AND event_kind IN ('supply','withdraw','supply_collateral','withdraw_collateral','borrow','repay')
		 GROUP BY pool, asset
		 LIMIT $2`
	rows, err := s.db.QueryContext(ctx, q, address, positionsVenueLimit)
	if err != nil {
		return nil, fmt.Errorf("timescale: BlendPositionsByUser: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []BlendPositionFold
	for rows.Next() {
		var (
			f                              BlendPositionFold
			supplyActivity, borrowActivity sql.NullTime
			supplyLedger, borrowLedger     sql.NullInt64
		)
		if err := rows.Scan(&f.Pool, &f.Asset, &f.HasSupplyLeg, &f.SupplyNet, &supplyActivity, &supplyLedger,
			&f.HasBorrowLeg, &f.BorrowNet, &borrowActivity, &borrowLedger); err != nil {
			return nil, fmt.Errorf("timescale: BlendPositionsByUser scan: %w", err)
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
		out = append(out, f)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("timescale: BlendPositionsByUser rows: %w", err)
	}
	return out, nil
}

// BlendBackstopFold is one pool's backstop-share fold for a user, read
// from blend_backstop_events (migration 0063).
//
// SharesNet sums `amount2` — verified per decode.go's decodeDeposit /
// decodeWithdraw doc comments: deposit's body is `Vec[i128 amount, i128
// shares]` -> `Amount2: shares` (shares MINTED); withdraw's body is
// `Vec[i128 shares_burned, i128 tokens_out]` but the decoder normalizes
// `Amount` to `tokens_out` and `Amount2` to `sharesBurned` — so
// `amount2` is "shares" on BOTH event kinds, signed
// +deposit/-withdraw. This is a real, exact current share count (shares
// are only minted on deposit and burned on withdraw — no other backstop
// event touches share supply), so amount_semantics at the handler layer
// is "shares", not an approximation.
//
// queue_withdrawal / dequeue_withdrawal are DELIBERATELY EXCLUDED: they
// are intent-only lifecycle events (a queued unstake is not yet
// executed — the shares stay staked and earning until the matching
// `withdraw` event fires) that carry a shares-shaped amount in a
// DIFFERENT column (`Amount`, not `Amount2` — see decodeQueueWithdrawal),
// so folding them in would both double-count against the eventual real
// `withdraw` and read the wrong column.
type BlendBackstopFold struct {
	Pool         string
	SharesNet    string
	LastActivity time.Time
	LastLedger   uint32
}

// BlendBackstopSharesByUser folds blend_backstop_events into one row per
// pool the user has deposited into / withdrawn from. Sargable via
// blend_backstop_events_user_ts_idx (migration 0107).
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
		 LIMIT $2`
	rows, err := s.db.QueryContext(ctx, q, address, positionsVenueLimit)
	if err != nil {
		return nil, fmt.Errorf("timescale: BlendBackstopSharesByUser: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []BlendBackstopFold
	for rows.Next() {
		var (
			f       BlendBackstopFold
			ledger  int64
			closeAt time.Time
		)
		if err := rows.Scan(&f.Pool, &f.SharesNet, &closeAt, &ledger); err != nil {
			return nil, fmt.Errorf("timescale: BlendBackstopSharesByUser scan: %w", err)
		}
		f.LastActivity = closeAt.UTC()
		f.LastLedger = uint32(ledger) //nolint:gosec // ledger seq fits uint32
		out = append(out, f)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("timescale: BlendBackstopSharesByUser rows: %w", err)
	}
	return out, nil
}

// PhoenixStakeFold is one stake-contract's bond/unbond fold for a user,
// read from phoenix_stake_events (migration 0044/0060/0098).
//
// NetAmount sums `amount` — the table's own column comment: "the
// share-token amount, always positive — the action discriminator
// carries the direction". Signed +bond/-unbond. Only action IN
// ('bond','unbond') is read; `withdraw_rewards`/`distribute_rewards`
// (migration 0098) carry NULL amount and are a reward-claim surface,
// not a stake-balance change, so they are excluded by the WHERE clause
// itself.
type PhoenixStakeFold struct {
	StakeContract string
	LPToken       string
	NetAmount     string
	LastActivity  time.Time
	LastLedger    uint32
}

// PhoenixStakeByUser folds phoenix_stake_events into one row per stake
// contract the user has bonded into. Sargable via the pre-existing
// phoenix_stake_events_user_ts_idx (migration 0044).
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
		 LIMIT $2`
	rows, err := s.db.QueryContext(ctx, q, address, positionsVenueLimit)
	if err != nil {
		return nil, fmt.Errorf("timescale: PhoenixStakeByUser: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []PhoenixStakeFold
	for rows.Next() {
		var (
			f       PhoenixStakeFold
			ledger  int64
			closeAt time.Time
		)
		if err := rows.Scan(&f.StakeContract, &f.LPToken, &f.NetAmount, &closeAt, &ledger); err != nil {
			return nil, fmt.Errorf("timescale: PhoenixStakeByUser scan: %w", err)
		}
		f.LastActivity = closeAt.UTC()
		f.LastLedger = uint32(ledger) //nolint:gosec // ledger seq fits uint32
		out = append(out, f)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("timescale: PhoenixStakeByUser rows: %w", err)
	}
	return out, nil
}

// DefindexVaultFold is one vault's df-token-share fold for a user, read
// from defindex_flows (migration 0050/0055), VAULT layer only.
//
// SharesNet sums `df_tokens` — the migration-0050 doc comment: "Vault
// layer: ... share-token delta (`df_tokens_minted` on deposit,
// `df_tokens_minted` on withdraw)" (DfTokens column, always the
// magnitude — direction carries in `direction`), signed
// +deposit/-withdraw. STRATEGY-layer rows (Actor = the vault contract
// itself moving capital into/out of Blend, not an end user) are
// excluded by `layer = 'vault'` — see the migration's layer-discriminator
// doc comment for why the two layers can't be folded together (different
// actor identity space entirely).
type DefindexVaultFold struct {
	ContractID   string
	SharesNet    string
	LastActivity time.Time
	LastLedger   uint32
}

// DefindexVaultSharesByUser folds defindex_flows (vault layer) into one
// row per vault the user has deposited into. Sargable via the
// pre-existing defindex_flows_actor_ts_idx (migration 0050).
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
		 LIMIT $2`
	rows, err := s.db.QueryContext(ctx, q, address, positionsVenueLimit)
	if err != nil {
		return nil, fmt.Errorf("timescale: DefindexVaultSharesByUser: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []DefindexVaultFold
	for rows.Next() {
		var (
			f       DefindexVaultFold
			ledger  int64
			closeAt time.Time
		)
		if err := rows.Scan(&f.ContractID, &f.SharesNet, &closeAt, &ledger); err != nil {
			return nil, fmt.Errorf("timescale: DefindexVaultSharesByUser scan: %w", err)
		}
		f.LastActivity = closeAt.UTC()
		f.LastLedger = uint32(ledger) //nolint:gosec // ledger seq fits uint32
		out = append(out, f)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("timescale: DefindexVaultSharesByUser rows: %w", err)
	}
	return out, nil
}

// CreditPositionFold is one sorocredit position for an owner, read from
// credit_positions (migration 0090) LEFT-JOINed against that position's
// MOST RECENT credit_statements row.
//
// UNLIKE every other protocol in this file, sorocredit has no per-event
// amount to sum: credit_positions is purely an identity registry (one
// row per opened position, from `NewCollateralContract` — see
// migration 0090's header and internal/sources/sorocredit/README.md's
// event table). The closest thing to "how big is this position right
// now" is the protocol's own most-recently-published `StatementPublished`
// amount (credit_statements.amount) — a value the PROTOCOL computed and
// published, not one this fold derives by summing deltas. That is
// exactly "basis: stateful" at the handler layer (as opposed to every
// other protocol here, which is "basis: event_derived").
//
// LatestAmount is the empty string / LatestActivity is the zero time
// when the position has never had a statement published yet (a
// just-opened position) — the handler surfaces that as a position with
// no reportable amount rather than guessing 0.
//
// Withdrawn reports whether a `Withdrawal` event (credit_events,
// event_type='withdrawal') has ever fired against this position's
// collateral_contract — the same "still open" proxy
// CreditWindowAnalytics uses (sorocredit.go), except UNWINDOWED here
// (credit_events carries NO retention — migration 0090 — so an
// all-time EXISTS is the honest signal, not a window-scoped
// approximation).
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

// CreditPositionsByOwner reads every credit_positions row this owner has
// ever opened, each joined to its latest statement. Sargable: the outer
// WHERE uses credit_positions_owner_ts_idx (migration 0090); the LATERAL
// join uses credit_statements_position_ts_idx (migration 0090); the
// EXISTS probe uses credit_events_collateral_ts_idx (migration 0090) —
// three existing indexes, no new migration needed for this protocol.
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
		 LIMIT $2`
	rows, err := s.db.QueryContext(ctx, q, address, positionsVenueLimit)
	if err != nil {
		return nil, fmt.Errorf("timescale: CreditPositionsByOwner: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []CreditPositionFold
	for rows.Next() {
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
			return nil, fmt.Errorf("timescale: CreditPositionsByOwner scan: %w", err)
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
		out = append(out, f)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("timescale: CreditPositionsByOwner rows: %w", err)
	}
	return out, nil
}

// AquariusGaugeFold is one pool's gauge-position fold for a user, read
// from aquarius_rewards_events (migration 0099), `position_update` kind
// only.
//
// NetDelta sums `attributes->>'delta'` — decode_rewards.go's
// decodePositionUpdate: "delta is SIGNED and observed negative on a
// withdrawal ... Because delta can be negative it is NOT promoted to
// the universal (always >= 0) Amount field — it lands in Attributes as
// a signed decimal string." Summing that signed per-event delta is, by
// construction, the position's current size as of the latest observed
// checkpoint — but decodePositionUpdate's own doc comment marks the
// FIELD ITSELF best-effort ("this suggests field[1] is a range/tick-
// style checkpoint index and field[2] mirrors the correlated position's
// amount, but this is an observed correlation, not a confirmed
// contract-source semantic"). The handler layer reflects that
// uncertainty honestly rather than calling it "shares" or "underlying".
type AquariusGaugeFold struct {
	ContractID   string
	NetDelta     string
	LastActivity time.Time
	LastLedger   uint32
}

// AquariusGaugeByUser folds aquarius_rewards_events position_update rows
// into one row per pool. Sargable via the pre-existing partial
// aquarius_rewards_events_user_ts_idx (migration 0099).
func (s *Store) AquariusGaugeByUser(ctx context.Context, address string) ([]AquariusGaugeFold, error) {
	const q = `
		SELECT contract_id,
		       COALESCE(SUM((attributes->>'delta')::numeric),0)::text,
		       MAX(ledger_close_time), MAX(ledger)
		  FROM aquarius_rewards_events
		 WHERE user_address = $1
		   AND event_kind = 'position_update'
		 GROUP BY contract_id
		 LIMIT $2`
	rows, err := s.db.QueryContext(ctx, q, address, positionsVenueLimit)
	if err != nil {
		return nil, fmt.Errorf("timescale: AquariusGaugeByUser: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []AquariusGaugeFold
	for rows.Next() {
		var (
			f       AquariusGaugeFold
			ledger  int64
			closeAt time.Time
		)
		if err := rows.Scan(&f.ContractID, &f.NetDelta, &closeAt, &ledger); err != nil {
			return nil, fmt.Errorf("timescale: AquariusGaugeByUser scan: %w", err)
		}
		f.LastActivity = closeAt.UTC()
		f.LastLedger = uint32(ledger) //nolint:gosec // ledger seq fits uint32
		out = append(out, f)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("timescale: AquariusGaugeByUser rows: %w", err)
	}
	return out, nil
}
