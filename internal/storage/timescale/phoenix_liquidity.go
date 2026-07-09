package timescale

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/StellarIndex/stellar-index/internal/canonical"
)

// PhoenixLiquidityAction discriminates the two Phoenix pool
// liquidity-management actions. String values match the
// phoenix_liquidity.action CHECK constraint (migration 0044) and
// internal/sources/phoenix's EventActionProvideLiquidity /
// EventActionWithdrawLiquidity constants.
type PhoenixLiquidityAction string

const (
	PhoenixProvideLiquidity  PhoenixLiquidityAction = "provide_liquidity"
	PhoenixWithdrawLiquidity PhoenixLiquidityAction = "withdraw_liquidity"
)

// IsValid reports whether a is one of the two known actions.
func (a PhoenixLiquidityAction) IsValid() bool {
	switch a {
	case PhoenixProvideLiquidity, PhoenixWithdrawLiquidity:
		return true
	}
	return false
}

// PhoenixLiquidityChange is one phoenix_liquidity row — a single
// observed provide_liquidity / withdraw_liquidity event from a
// Phoenix pool (volatile or stableswap). Mirrors the migration-0044
// columns.
//
// Amount fields are decimal-string numerics (per ADR-0003 i128 ->
// *big.Int -> string). TokenA / TokenB / SharesAmount are empty on
// the side that doesn't carry them (withdraw has no token addresses;
// provide has no shares amount) — the writer maps "" → SQL NULL.
type PhoenixLiquidityChange struct {
	Pool       string
	Ledger     uint32
	ObservedAt time.Time
	TxHash     string
	OpIndex    uint32
	// EventIndex is the first field-event's in-op index — the per-event
	// discriminator added to the phoenix_liquidity PK by migration 0060
	// (F-1324) so two provides/withdraws in one op don't collide.
	EventIndex   uint32
	Action       PhoenixLiquidityAction
	Sender       string
	TokenA       string // provide-only; "" → NULL
	TokenB       string // provide-only; "" → NULL
	AmountA      string // decimal i128 — both actions
	AmountB      string // decimal i128 — both actions
	SharesAmount string // withdraw-only; "" → NULL
}

// InsertPhoenixLiquidityChange appends one phoenix_liquidity row,
// idempotent on the (ledger_close_time, pool, ledger, tx_hash,
// op_index, action, event_index) PK (event_index added by migration
// 0060 / F-1324 so two provides/withdraws in one op don't collide).
// Re-running the indexer over the same range or replaying a backfill
// writes the same rows; ON CONFLICT DO NOTHING makes the replay a
// no-op.
//
// Defensive: rejects empty Pool / TxHash / Sender, an invalid
// Action, and empty NUMERIC amounts (AmountA / AmountB are NOT NULL)
// before touching the DB.
func (s *Store) InsertPhoenixLiquidityChange(ctx context.Context, e PhoenixLiquidityChange) error {
	if e.Pool == "" {
		return errors.New("timescale: InsertPhoenixLiquidityChange: Pool is empty")
	}
	if e.TxHash == "" {
		return errors.New("timescale: InsertPhoenixLiquidityChange: TxHash is empty")
	}
	if e.Sender == "" {
		return errors.New("timescale: InsertPhoenixLiquidityChange: Sender is empty")
	}
	if !e.Action.IsValid() {
		return fmt.Errorf("timescale: InsertPhoenixLiquidityChange: invalid Action %q", e.Action)
	}
	if e.AmountA == "" || e.AmountB == "" {
		return fmt.Errorf("timescale: InsertPhoenixLiquidityChange: AmountA/AmountB required (pool=%s tx=%s)",
			e.Pool, e.TxHash)
	}

	// Provide events do not emit token addresses on the withdraw side
	// and do not emit shares on the provide side. The caller is
	// expected to honour this; we surface NULL accordingly.
	var (
		tokenA sql.NullString
		tokenB sql.NullString
		shares sql.NullString
	)
	if e.TokenA != "" {
		tokenA = sql.NullString{String: e.TokenA, Valid: true}
	}
	if e.TokenB != "" {
		tokenB = sql.NullString{String: e.TokenB, Valid: true}
	}
	if e.SharesAmount != "" {
		shares = sql.NullString{String: e.SharesAmount, Valid: true}
	}

	const q = `
        INSERT INTO phoenix_liquidity (
            pool, ledger, ledger_close_time, tx_hash, op_index,
            action, event_index, sender, token_a, token_b, amount_a, amount_b,
            shares_amount
        ) VALUES (
            $1, $2, $3, $4, $5,
            $6, $7, $8, $9, $10, $11, $12,
            $13
        )
        ON CONFLICT (ledger_close_time, pool, ledger, tx_hash, op_index, action, event_index) DO NOTHING
    `
	_, err := s.db.ExecContext(ctx, q,
		e.Pool, int(e.Ledger), e.ObservedAt.UTC(), e.TxHash, int(e.OpIndex),
		string(e.Action), int(e.EventIndex), e.Sender, tokenA, tokenB, e.AmountA, e.AmountB,
		shares,
	)
	if err != nil {
		return fmt.Errorf("timescale: InsertPhoenixLiquidityChange %s@%d: %w",
			e.Pool, e.Ledger, err)
	}
	return nil
}

// PhoenixPoolFlow is one Phoenix pool's net liquidity flow over a window —
// the two-token (leg A / leg B) net (provide − withdraw) from
// phoenix_liquidity per-event amounts (migration 0044). This is a WINDOW
// DELTA, NOT an absolute pool reserve: Phoenix pool events carry the moved
// amounts (actual_received / return_amount), not post-state reserves, so a
// clean pool-reserve / TVL is not derivable here.
//
// TokenA / TokenB are resolved from the pool's most recent provide_liquidity
// row (withdraw events omit the token addresses); they are "" when no
// provide was observed for the pool in the window. NetA / NetB are
// i128/NUMERIC (canonical.Amount, never int64 — ADR-0003) and can be
// negative when withdrawals exceed provides in the window.
type PhoenixPoolFlow struct {
	Pool      string
	TokenA    string
	TokenB    string
	NetA      canonical.Amount
	NetB      canonical.Amount
	Provides  int64
	Withdraws int64
	LatestAt  time.Time
}

// LatestPhoenixLiquidityFlows returns per-pool net liquidity flow over the
// trailing windowDays, most-active pool first. NetA / NetB are the leg-A /
// leg-B provide-minus-withdraw deltas; token addresses are resolved
// positionally from the pool's most recent provide (mirrors the
// aquarius_reserves address-recovery pattern — withdraw events carry no
// token address, migration 0044).
//
// This is the READ side of the Phoenix liquidity-depth signal
// InsertPhoenixLiquidityChange captures. Phoenix has no post-state reserve
// snapshot and no published price, so the figures are native-token
// base-unit WINDOW deltas, not absolute reserves or USD TVL. (Phoenix is
// contract-identity gated — the curated-set gate, 2026-07-02, earlier than
// Comet's 2026-07-08 gate — so unlike Comet these figures carry no
// historical-rows-may-predate-the-gate caveat.)
//
// Empty-safe: returns (nil, nil) when no liquidity events were captured in
// the window. windowDays <= 0 is treated as 90.
func (s *Store) LatestPhoenixLiquidityFlows(ctx context.Context, windowDays int) ([]PhoenixPoolFlow, error) {
	if windowDays <= 0 {
		windowDays = 90
	}
	since := fmt.Sprintf("%d days", windowDays)

	flows, err := s.phoenixPoolFlowRows(ctx, since)
	if err != nil {
		return nil, err
	}
	if len(flows) == 0 {
		return nil, nil
	}
	tokens, err := s.phoenixPoolTokenPositions(ctx, since)
	if err != nil {
		return nil, err
	}
	for i := range flows {
		if tp, ok := tokens[flows[i].Pool]; ok {
			flows[i].TokenA = tp[0]
			flows[i].TokenB = tp[1]
		}
	}
	return flows, nil
}

// phoenixPoolFlowRows reads the per-pool provide-minus-withdraw net flow
// (leg A / leg B) + action counts over the window. Its result set is fully
// consumed and closed before returning, so LatestPhoenixLiquidityFlows
// never holds two open cursors.
func (s *Store) phoenixPoolFlowRows(ctx context.Context, since string) ([]PhoenixPoolFlow, error) {
	const q = `
		SELECT pool,
		       COALESCE(sum(CASE WHEN action = 'provide_liquidity' THEN amount_a
		                         WHEN action = 'withdraw_liquidity' THEN -amount_a END),0)::text,
		       COALESCE(sum(CASE WHEN action = 'provide_liquidity' THEN amount_b
		                         WHEN action = 'withdraw_liquidity' THEN -amount_b END),0)::text,
		       count(*) FILTER (WHERE action = 'provide_liquidity'),
		       count(*) FILTER (WHERE action = 'withdraw_liquidity'),
		       max(ledger_close_time)
		  FROM phoenix_liquidity
		 WHERE ledger_close_time > now() - $1::interval
		 GROUP BY pool
		 ORDER BY count(*) DESC, pool`
	rows, err := s.db.QueryContext(ctx, q, since)
	if err != nil {
		return nil, fmt.Errorf("timescale: LatestPhoenixLiquidityFlows flows: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []PhoenixPoolFlow
	for rows.Next() {
		var (
			f      PhoenixPoolFlow
			latest time.Time
		)
		if err := rows.Scan(&f.Pool, &f.NetA, &f.NetB, &f.Provides, &f.Withdraws, &latest); err != nil {
			return nil, fmt.Errorf("timescale: LatestPhoenixLiquidityFlows scan: %w", err)
		}
		f.LatestAt = latest.UTC()
		out = append(out, f)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("timescale: LatestPhoenixLiquidityFlows rows: %w", err)
	}
	return out, nil
}

// phoenixPoolTokenPositions resolves (token_a, token_b) per pool from the
// most recent provide_liquidity in the window — best-effort address
// recovery for withdraw rows, which omit the token addresses. Returns
// pool → [tokenA, tokenB].
func (s *Store) phoenixPoolTokenPositions(ctx context.Context, since string) (map[string][2]string, error) {
	const q = `
		SELECT DISTINCT ON (pool) pool, token_a, token_b
		  FROM phoenix_liquidity
		 WHERE action = 'provide_liquidity' AND token_a IS NOT NULL
		   AND ledger_close_time > now() - $1::interval
		 ORDER BY pool, ledger_close_time DESC`
	rows, err := s.db.QueryContext(ctx, q, since)
	if err != nil {
		return nil, fmt.Errorf("timescale: LatestPhoenixLiquidityFlows tokens: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := map[string][2]string{}
	for rows.Next() {
		var (
			pool           string
			tokenA, tokenB sql.NullString
		)
		if err := rows.Scan(&pool, &tokenA, &tokenB); err != nil {
			return nil, fmt.Errorf("timescale: LatestPhoenixLiquidityFlows token scan: %w", err)
		}
		out[pool] = [2]string{tokenA.String, tokenB.String}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("timescale: LatestPhoenixLiquidityFlows token rows: %w", err)
	}
	return out, nil
}
