package timescale

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// AquariusLiquidityAction discriminates the two Aquarius
// liquidity-mutating events. String values match the
// aquarius_liquidity.action CHECK constraint (migration 0089) and the
// LiquidityAction constants in internal/sources/aquarius/consumer.go.
type AquariusLiquidityAction string

const (
	AquariusLiquidityDeposit  AquariusLiquidityAction = "deposit"
	AquariusLiquidityWithdraw AquariusLiquidityAction = "withdraw"
)

// IsValid reports whether a is one of the two known actions.
func (a AquariusLiquidityAction) IsValid() bool {
	switch a {
	case AquariusLiquidityDeposit, AquariusLiquidityWithdraw:
		return true
	}
	return false
}

// AquariusReservesEvent is one observed update_reserves event — the
// pool's POST-STATE reserve vector (one i128 per pool token). The
// writer fans it out to one aquarius_reserves row per token position.
type AquariusReservesEvent struct {
	ContractID      string
	Ledger          uint32
	LedgerCloseTime time.Time
	TxHash          string
	OpIndex         uint32
	EventIndex      uint32
	// Reserves is the post-state reserve per token, ordered by the
	// pool's canonical token index. i128 per ADR-0003.
	Reserves []canonical.Amount
}

// AquariusLiquidityEvent is one observed deposit_liquidity /
// withdraw_liquidity event. The writer fans it out to one
// aquarius_liquidity row per token position; Shares (the per-event LP
// share amount) lands on the token_index = 0 row only.
type AquariusLiquidityEvent struct {
	ContractID      string
	Ledger          uint32
	LedgerCloseTime time.Time
	TxHash          string
	OpIndex         uint32
	EventIndex      uint32
	Action          AquariusLiquidityAction
	// Tokens[i] is the token address at position i; Amounts[i] the
	// amount that moved for it. len(Tokens) == len(Amounts).
	Tokens  []string
	Amounts []canonical.Amount
	// Shares is the LP-share amount minted (deposit) / burned
	// (withdraw) — a single per-event value.
	Shares canonical.Amount
}

// InsertAquariusReserves appends one update_reserves observation,
// fanned to one row per token position. Idempotent on the
// (ledger_close_time, contract_id, ledger, tx_hash, op_index,
// event_index, token_index) PK — a projector-replay or ch-rebuild over
// the same range writes the same rows (ON CONFLICT DO NOTHING).
//
// Defensive: rejects an empty ContractID / TxHash, an empty reserve
// vector, and a negative reserve before touching the DB.
func (s *Store) InsertAquariusReserves(ctx context.Context, e AquariusReservesEvent) error {
	if e.ContractID == "" {
		return errors.New("timescale: InsertAquariusReserves: ContractID is empty")
	}
	if e.TxHash == "" {
		return errors.New("timescale: InsertAquariusReserves: TxHash is empty")
	}
	if len(e.Reserves) == 0 {
		return errors.New("timescale: InsertAquariusReserves: empty reserve vector")
	}

	// INV-3 generation-guarded corrective upsert (migration 0110): a
	// corrected re-derive of the post-state `reserve` lands in place when its
	// generation is >= the stored one; a live gen-0 replay can never revert
	// it. Each fanned row carries a distinct token_index (a conflict-key
	// component), so one statement never repeats a key — no intra-batch dedup
	// needed. Replaces the old DO NOTHING no-op.
	const q = `
        INSERT INTO aquarius_reserves (
            contract_id, ledger, ledger_close_time, tx_hash,
            op_index, event_index, token_index, reserve,
            derive_generation
        ) VALUES (
            $1, $2, $3, $4, $5, $6, $7, $8, $9
        )
        ON CONFLICT (ledger_close_time, contract_id, ledger, tx_hash,
                     op_index, event_index, token_index) DO UPDATE SET
            reserve           = EXCLUDED.reserve,
            derive_generation = EXCLUDED.derive_generation
          WHERE aquarius_reserves.derive_generation <= EXCLUDED.derive_generation
    `
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("timescale: InsertAquariusReserves begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	for i, r := range e.Reserves {
		if r.Sign() < 0 {
			return fmt.Errorf("timescale: InsertAquariusReserves: reserve[%d] must be >= 0 (got %s)", i, r)
		}
		if _, err := tx.ExecContext(ctx, q,
			e.ContractID, int(e.Ledger), e.LedgerCloseTime.UTC(), e.TxHash,
			int(e.OpIndex), int(e.EventIndex), i, r.String(),
			s.deriveGeneration,
		); err != nil {
			return fmt.Errorf("timescale: InsertAquariusReserves %s@%d[%d]: %w", e.ContractID, e.Ledger, i, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("timescale: InsertAquariusReserves commit: %w", err)
	}
	return nil
}

// InsertAquariusLiquidity appends one deposit_liquidity /
// withdraw_liquidity observation, fanned to one row per token
// position. Shares lands on the token_index = 0 row only (NULL on the
// others). Idempotent on the (ledger_close_time, contract_id, ledger,
// tx_hash, op_index, event_index, action, token_index) PK.
//
// Defensive: rejects empty identifiers, an invalid Action, a
// tokens/amounts length mismatch, an empty token address, a negative
// amount, and negative shares before touching the DB.
func (s *Store) InsertAquariusLiquidity(ctx context.Context, e AquariusLiquidityEvent) error {
	if e.ContractID == "" {
		return errors.New("timescale: InsertAquariusLiquidity: ContractID is empty")
	}
	if e.TxHash == "" {
		return errors.New("timescale: InsertAquariusLiquidity: TxHash is empty")
	}
	if !e.Action.IsValid() {
		return fmt.Errorf("timescale: InsertAquariusLiquidity: invalid Action %q", e.Action)
	}
	if len(e.Tokens) == 0 {
		return errors.New("timescale: InsertAquariusLiquidity: no tokens")
	}
	if len(e.Tokens) != len(e.Amounts) {
		return fmt.Errorf("timescale: InsertAquariusLiquidity: tokens/amounts length mismatch (%d vs %d)",
			len(e.Tokens), len(e.Amounts))
	}
	if e.Shares.Sign() < 0 {
		return fmt.Errorf("timescale: InsertAquariusLiquidity: shares must be >= 0 (got %s)", e.Shares)
	}

	// INV-3 generation-guarded corrective upsert (migration 0110): a
	// corrected re-derive of the per-leg `amount` / `shares` lands in place
	// when its generation is >= the stored one; a live gen-0 replay can never
	// revert it. Each fanned row carries a distinct token_index (a
	// conflict-key component), so one statement never repeats a key — no
	// intra-batch dedup needed. Replaces the old DO NOTHING no-op.
	const q = `
        INSERT INTO aquarius_liquidity (
            contract_id, ledger, ledger_close_time, tx_hash,
            op_index, event_index, action, token_index, token, amount, shares,
            derive_generation
        ) VALUES (
            $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12
        )
        ON CONFLICT (ledger_close_time, contract_id, ledger, tx_hash,
                     op_index, event_index, action, token_index) DO UPDATE SET
            token             = EXCLUDED.token,
            amount            = EXCLUDED.amount,
            shares            = EXCLUDED.shares,
            derive_generation = EXCLUDED.derive_generation
          WHERE aquarius_liquidity.derive_generation <= EXCLUDED.derive_generation
    `
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("timescale: InsertAquariusLiquidity begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	for i, tok := range e.Tokens {
		if tok == "" {
			return fmt.Errorf("timescale: InsertAquariusLiquidity: token[%d] is empty", i)
		}
		amt := e.Amounts[i]
		if amt.Sign() < 0 {
			return fmt.Errorf("timescale: InsertAquariusLiquidity: amount[%d] must be >= 0 (got %s)", i, amt)
		}
		// Shares is a per-event value — write it on the canonical
		// token_index = 0 row only so SUM(shares) never N-counts.
		var shares sql.NullString
		if i == 0 {
			shares = sql.NullString{String: e.Shares.String(), Valid: true}
		}
		if _, err := tx.ExecContext(ctx, q,
			e.ContractID, int(e.Ledger), e.LedgerCloseTime.UTC(), e.TxHash,
			int(e.OpIndex), int(e.EventIndex), string(e.Action), i, tok, amt.String(), shares,
			s.deriveGeneration,
		); err != nil {
			return fmt.Errorf("timescale: InsertAquariusLiquidity %s@%d[%d]: %w", e.ContractID, e.Ledger, i, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("timescale: InsertAquariusLiquidity commit: %w", err)
	}
	return nil
}

// AquariusReserveLeg is one token position within a pool's latest reserve
// snapshot.
type AquariusReserveLeg struct {
	// TokenIndex is the 0-based position in the pool's canonical token
	// order.
	TokenIndex int
	// Token is the token address resolved for this position, or "" when no
	// deposit_liquidity / withdraw_liquidity has been observed for the
	// pool in the window — update_reserves carries no token address, only
	// the position (migration 0089).
	Token string
	// Reserve is the POST-STATE reserve for the token at TokenIndex, in the
	// token's base units (i128 per ADR-0003 — never int64).
	Reserve canonical.Amount
}

// AquariusPoolReserve is one Aquarius pool's latest POST-STATE reserve
// snapshot: the complete reserve vector from the pool's single most recent
// update_reserves event.
type AquariusPoolReserve struct {
	ContractID string
	ObservedAt time.Time
	Ledger     uint32
	// Legs is one entry per token position, ordered by TokenIndex ascending.
	Legs []AquariusReserveLeg
}

// LatestAquariusReserves returns the newest reserve snapshot per pool
// observed within the trailing windowDays, most-recently-updated pool
// first. A pool's snapshot is the complete reserve vector from its single
// most recent update_reserves event (max ledger_close_time, then
// ledger/op_index/event_index), so a multi-token stableswap pool comes
// back with all its legs. Token addresses are resolved positionally from
// the pool's most recent deposit/withdraw in the window (update_reserves
// carries no token address — CLAUDE.md / migration 0089); a leg with no
// observed liquidity event keeps an empty Token.
//
// This is the READ side of the Aquarius TVL / liquidity-depth signal that
// InsertAquariusReserves captures. Reserves are per-asset base units, not
// USD — Aquarius pools have no independently published price, so a clean
// USD TVL is not derived here; callers surface depth in native units.
//
// Empty-safe: returns (nil, nil) when no reserves have been captured in the
// window. windowDays <= 0 is treated as 90.
func (s *Store) LatestAquariusReserves(ctx context.Context, windowDays int) ([]AquariusPoolReserve, error) {
	if windowDays <= 0 {
		windowDays = 90
	}
	since := fmt.Sprintf("%d days", windowDays)

	pools, err := s.aquariusLatestReserveLegs(ctx, since)
	if err != nil {
		return nil, err
	}
	if len(pools) == 0 {
		return nil, nil
	}
	tokenByPos, err := s.aquariusReserveTokenPositions(ctx, since)
	if err != nil {
		return nil, err
	}
	for i := range pools {
		positions := tokenByPos[pools[i].ContractID]
		if positions == nil {
			continue
		}
		for j := range pools[i].Legs {
			pools[i].Legs[j].Token = positions[pools[i].Legs[j].TokenIndex]
		}
	}
	return pools, nil
}

// aquariusLatestReserveLegs reads, per pool, the fanned reserve legs of its
// single most recent update_reserves event within the window. Ordered so a
// pool's legs are contiguous and pools come back recency-first. Its result
// set is fully consumed and closed before returning, so LatestAquariusReserves
// never holds two open cursors.
func (s *Store) aquariusLatestReserveLegs(ctx context.Context, since string) ([]AquariusPoolReserve, error) {
	const q = `
		WITH latest AS (
		    SELECT DISTINCT ON (contract_id)
		           contract_id, ledger_close_time, ledger, tx_hash, op_index, event_index
		      FROM aquarius_reserves
		     WHERE ledger_close_time > now() - $1::interval
		     ORDER BY contract_id,
		              ledger_close_time DESC, ledger DESC, op_index DESC, event_index DESC
		)
		SELECT r.contract_id, r.ledger_close_time, r.ledger, r.token_index, r.reserve::text
		  FROM aquarius_reserves r
		  JOIN latest l USING (contract_id, ledger_close_time, ledger, tx_hash, op_index, event_index)
		 ORDER BY r.ledger_close_time DESC, r.contract_id, r.token_index`
	rows, err := s.db.QueryContext(ctx, q, since)
	if err != nil {
		return nil, fmt.Errorf("timescale: LatestAquariusReserves legs: %w", err)
	}
	defer func() { _ = rows.Close() }()

	// Build without holding pointers into a growing slice (append can
	// reallocate): accumulate legs per contract, preserve first-seen order.
	type meta struct {
		observedAt time.Time
		ledger     uint32
	}
	var order []string
	legs := map[string][]AquariusReserveLeg{}
	metas := map[string]meta{}
	for rows.Next() {
		var (
			contractID string
			closeTime  time.Time
			ledger     int64
			tokenIndex int
			reserve    canonical.Amount
		)
		if err := rows.Scan(&contractID, &closeTime, &ledger, &tokenIndex, &reserve); err != nil {
			return nil, fmt.Errorf("timescale: LatestAquariusReserves scan: %w", err)
		}
		if _, seen := legs[contractID]; !seen {
			order = append(order, contractID)
			metas[contractID] = meta{observedAt: closeTime.UTC(), ledger: uint32(ledger)}
		}
		legs[contractID] = append(legs[contractID], AquariusReserveLeg{TokenIndex: tokenIndex, Reserve: reserve})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("timescale: LatestAquariusReserves legs rows: %w", err)
	}

	out := make([]AquariusPoolReserve, 0, len(order))
	for _, id := range order {
		m := metas[id]
		out = append(out, AquariusPoolReserve{
			ContractID: id,
			ObservedAt: m.observedAt,
			Ledger:     m.ledger,
			Legs:       legs[id],
		})
	}
	return out, nil
}

// aquariusReserveTokenPositions resolves the token address at each
// (pool, token_index) from the most recent deposit/withdraw in the window
// — best-effort address recovery for the positional update_reserves stream.
// Returns pool → token_index → token address.
func (s *Store) aquariusReserveTokenPositions(ctx context.Context, since string) (map[string]map[int]string, error) {
	const q = `
		SELECT DISTINCT ON (contract_id, token_index) contract_id, token_index, token
		  FROM aquarius_liquidity
		 WHERE ledger_close_time > now() - $1::interval
		 ORDER BY contract_id, token_index, ledger_close_time DESC`
	rows, err := s.db.QueryContext(ctx, q, since)
	if err != nil {
		return nil, fmt.Errorf("timescale: LatestAquariusReserves tokens: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := map[string]map[int]string{}
	for rows.Next() {
		var (
			contractID string
			tokenIndex int
			token      string
		)
		if err := rows.Scan(&contractID, &tokenIndex, &token); err != nil {
			return nil, fmt.Errorf("timescale: LatestAquariusReserves token scan: %w", err)
		}
		positions := out[contractID]
		if positions == nil {
			positions = map[int]string{}
			out[contractID] = positions
		}
		positions[tokenIndex] = token
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("timescale: LatestAquariusReserves token rows: %w", err)
	}
	return out, nil
}
