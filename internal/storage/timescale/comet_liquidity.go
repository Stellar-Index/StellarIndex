package timescale

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// CometLiquidityKind discriminates the four Comet liquidity-mutating
// events. String values match the comet_liquidity.event_kind CHECK
// constraint (migration 0042) and the LiquidityKind constants in
// internal/sources/comet/events.go.
type CometLiquidityKind string

const (
	CometLiquidityJoinPool CometLiquidityKind = "join_pool"
	CometLiquidityExitPool CometLiquidityKind = "exit_pool"
	CometLiquidityDeposit  CometLiquidityKind = "deposit"
	CometLiquidityWithdraw CometLiquidityKind = "withdraw"
)

// IsValid reports whether k is one of the four known kinds.
func (k CometLiquidityKind) IsValid() bool {
	switch k {
	case CometLiquidityJoinPool, CometLiquidityExitPool,
		CometLiquidityDeposit, CometLiquidityWithdraw:
		return true
	}
	return false
}

// Direction returns the add/remove polarity for the kind, matching
// the comet_liquidity.direction CHECK constraint. Empty string for
// an invalid kind (caller should reject via IsValid first).
func (k CometLiquidityKind) Direction() string {
	switch k {
	case CometLiquidityJoinPool, CometLiquidityDeposit:
		return "add"
	case CometLiquidityExitPool, CometLiquidityWithdraw:
		return "remove"
	}
	return ""
}

// CometLiquidityEvent is one comet_liquidity row — a single observed
// (POOL, join_pool | exit_pool | deposit | withdraw) Comet event.
// Mirrors the migration-0042 columns.
//
// PoolAmountIn is populated only for withdraw events (the count of
// BPT-share tokens burned in exchange for the underlying); the
// writer translates a zero Amount to SQL NULL on the other three
// kinds.
type CometLiquidityEvent struct {
	ContractID      string
	Ledger          uint32
	LedgerCloseTime time.Time
	TxHash          string
	OpIndex         uint32
	// EventIndex is the contract event's index within its operation —
	// the per-event discriminator added to the comet_liquidity PK by
	// migration 0059 (F-1324) so two same-(kind,token) events from one
	// op don't collide.
	EventIndex   uint32
	Kind         CometLiquidityKind
	Caller       string
	Token        string
	Amount       canonical.Amount
	PoolAmountIn canonical.Amount // withdraw-only; zero on the other kinds
}

// InsertCometLiquidity appends one Comet liquidity event row,
// idempotent on the (ledger_close_time, contract_id, ledger,
// tx_hash, op_index, event_kind, token, event_index) PK (event_index
// added by migration 0059 / F-1324 so two same-(kind,token) events
// from one op don't collide). Re-running the indexer or a backfill
// over the same range writes the same rows; ON CONFLICT DO NOTHING
// makes the replay a no-op.
//
// Defensive: rejects empty ContractID / TxHash / Caller / Token, an
// invalid Kind, and a non-positive Amount before touching the DB —
// the decoder already enforces these but the writer double-checks
// so a malformed Event coming from an integration test or fuzz
// harness can't silently land bad rows.
func (s *Store) InsertCometLiquidity(ctx context.Context, e CometLiquidityEvent) error {
	if e.ContractID == "" {
		return errors.New("timescale: InsertCometLiquidity: ContractID is empty")
	}
	if e.TxHash == "" {
		return errors.New("timescale: InsertCometLiquidity: TxHash is empty")
	}
	if e.Caller == "" {
		return errors.New("timescale: InsertCometLiquidity: Caller is empty")
	}
	if e.Token == "" {
		return errors.New("timescale: InsertCometLiquidity: Token is empty")
	}
	if !e.Kind.IsValid() {
		return fmt.Errorf("timescale: InsertCometLiquidity: invalid Kind %q", e.Kind)
	}
	if e.Amount.Sign() <= 0 {
		return fmt.Errorf("timescale: InsertCometLiquidity: Amount must be > 0 (got %s)", e.Amount)
	}

	// pool_amount_in is withdraw-only. For withdraw it must be > 0
	// (the contract burns BPT shares — a zero burn would be a bug);
	// for the other kinds the column writes NULL.
	var poolAmountIn sql.NullString
	if e.Kind == CometLiquidityWithdraw {
		if e.PoolAmountIn.Sign() <= 0 {
			return fmt.Errorf("timescale: InsertCometLiquidity: withdraw PoolAmountIn must be > 0 (got %s)", e.PoolAmountIn)
		}
		poolAmountIn = sql.NullString{String: e.PoolAmountIn.String(), Valid: true}
	}

	const q = `
        INSERT INTO comet_liquidity (
            contract_id, ledger, ledger_close_time, tx_hash, op_index,
            event_kind, event_index, direction, caller, token, amount, pool_amount_in
        ) VALUES (
            $1, $2, $3, $4, $5,
            $6, $7, $8, $9, $10, $11, $12
        )
        ON CONFLICT (ledger_close_time, contract_id, ledger, tx_hash,
                     op_index, event_kind, token, event_index) DO NOTHING
    `
	_, err := s.db.ExecContext(ctx, q,
		e.ContractID, int(e.Ledger), e.LedgerCloseTime.UTC(),
		e.TxHash, int(e.OpIndex),
		string(e.Kind), int(e.EventIndex), e.Kind.Direction(),
		e.Caller, e.Token,
		e.Amount.String(), poolAmountIn,
	)
	if err != nil {
		return fmt.Errorf("timescale: InsertCometLiquidity %s@%d: %w", e.ContractID, e.Ledger, err)
	}
	return nil
}

// CometTokenFlow is one Comet pool's net liquidity flow for a single token
// over a window — the added / removed / net (added − removed) triple from
// comet_liquidity per-event amounts (migration 0042). This is a WINDOW
// DELTA, NOT an absolute pool reserve: comet_liquidity captures per-event
// join_pool / exit_pool / deposit / withdraw amounts, and the Comet port
// emits no post-state reserve snapshot — so a clean pool-reserve / TVL is
// not derivable here (unlike Aquarius's aquarius_reserves).
//
// All amounts are i128/NUMERIC (canonical.Amount, never int64 — ADR-0003);
// Net can be negative when removals exceed adds in the window.
type CometTokenFlow struct {
	ContractID string
	Token      string
	Added      canonical.Amount
	Removed    canonical.Amount
	Net        canonical.Amount
	Events     int64
	LatestAt   time.Time
}

// LatestCometLiquidityFlows returns per-(pool, token) net liquidity flow
// over the trailing windowDays, most-active token leg first. Added is the
// summed add-direction amount (join_pool / deposit), Removed the summed
// remove-direction amount (exit_pool / withdraw), Net = Added − Removed.
//
// This is the READ side of the Comet liquidity-depth signal InsertCometLiquidity
// captures. Comet has no post-state reserve snapshot and no published price,
// so the figures are native-token base-unit WINDOW deltas — not absolute
// reserves or USD TVL. Comet is contract-identity gated as of 2026-07-08
// (curated one-pool allowlist, CS-026 closed): the decoder's Matches() now
// requires the emitting contract to be in comet.MainnetGatedSet, so a
// look-alike Balancer-v1 deployment can no longer land NEW rows. Rows dated
// before the gate shipped were captured by the prior topic-only-match
// decoder and were not retroactively re-verified, so historical rows may
// predate the gate (see docs/protocols/comet.md); callers surface that
// caveat.
//
// Empty-safe: returns (nil, nil) when no liquidity events were captured in
// the window. windowDays <= 0 is treated as 90.
func (s *Store) LatestCometLiquidityFlows(ctx context.Context, windowDays int) ([]CometTokenFlow, error) {
	if windowDays <= 0 {
		windowDays = 90
	}
	since := fmt.Sprintf("%d days", windowDays)

	const q = `
		SELECT contract_id, token,
		       COALESCE(sum(amount) FILTER (WHERE direction = 'add'),0)::text,
		       COALESCE(sum(amount) FILTER (WHERE direction = 'remove'),0)::text,
		       COALESCE(sum(CASE WHEN direction = 'add' THEN amount
		                         WHEN direction = 'remove' THEN -amount END),0)::text,
		       count(*),
		       max(ledger_close_time)
		  FROM comet_liquidity
		 WHERE ledger_close_time > now() - $1::interval
		 GROUP BY contract_id, token
		 ORDER BY count(*) DESC, contract_id, token`
	rows, err := s.db.QueryContext(ctx, q, since)
	if err != nil {
		return nil, fmt.Errorf("timescale: LatestCometLiquidityFlows: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []CometTokenFlow
	for rows.Next() {
		var (
			f      CometTokenFlow
			latest time.Time
		)
		if err := rows.Scan(&f.ContractID, &f.Token, &f.Added, &f.Removed, &f.Net, &f.Events, &latest); err != nil {
			return nil, fmt.Errorf("timescale: LatestCometLiquidityFlows scan: %w", err)
		}
		f.LatestAt = latest.UTC()
		out = append(out, f)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("timescale: LatestCometLiquidityFlows rows: %w", err)
	}
	return out, nil
}
