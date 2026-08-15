package timescale

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math/big"

	"github.com/Stellar-Index/StellarIndex/internal/domain"
)

// InsertBlendPositionEvent appends one money-market position-change
// event (supply / withdraw / supply_collateral / withdraw_collateral
// / borrow / repay / flash_loan) to the blend_positions hypertable.
// Idempotent on the PK (pool, ledger, tx_hash, op_index, event_kind,
// ledger_close_time) — re-running over the same range is a no-op
// rather than producing duplicates.
//
// i128 amounts are written as decimal strings to the NUMERIC
// column (ADR-0003 — full precision preserved through Go's
// *big.Int → decimal-text → NUMERIC chain).
//
// Defensive: rejects empty Pool / TxHash, an invalid Kind, and a
// nil or negative money amount before touching the DB. Mirrors the
// SQL-boundary magnitude guards the sibling money-market writers
// already carry (comet Amount.Sign() > 0, aquarius reserve >= 0):
// the sole producer (decode_money_market.go) errors rather than
// emitting a nil amount today, so this closes the one spot-checked
// writer whose committed value was not guarded at the insert
// boundary (BLEND-1) — a defaulted/fuzzed struct can no longer land
// a bad row via the nil-to-"0" coercion.
func (s *Store) InsertBlendPositionEvent(ctx context.Context, e domain.BlendPositionEvent) error {
	if e.Pool == "" {
		return errors.New("timescale: InsertBlendPositionEvent: Pool is empty")
	}
	if e.TxHash == "" {
		return errors.New("timescale: InsertBlendPositionEvent: TxHash is empty")
	}
	if !isBlendPositionKind(e.Kind) {
		return fmt.Errorf("timescale: InsertBlendPositionEvent: invalid Kind %q", e.Kind)
	}
	if e.TokenAmount == nil {
		return errors.New("timescale: InsertBlendPositionEvent: TokenAmount is nil")
	}
	if e.TokenAmount.Sign() < 0 {
		return fmt.Errorf("timescale: InsertBlendPositionEvent: TokenAmount must be >= 0 (got %s)", e.TokenAmount)
	}
	if e.BOrDAmount == nil {
		return errors.New("timescale: InsertBlendPositionEvent: BOrDAmount is nil")
	}
	if e.BOrDAmount.Sign() < 0 {
		return fmt.Errorf("timescale: InsertBlendPositionEvent: BOrDAmount must be >= 0 (got %s)", e.BOrDAmount)
	}

	// INV-3 generation-guarded corrective upsert (migration 0110): a
	// corrected re-derive of the i128 amounts (token_amount / b_or_d_amount)
	// lands in place when its generation is >= the stored one; a live gen-0
	// replay can never revert it. Replaces the old DO NOTHING no-op.
	const q = `
        INSERT INTO blend_positions (
            pool, ledger, tx_hash, op_index, event_index, ledger_close_time,
            event_kind, asset, user_address,
            token_amount, b_or_d_amount,
            counterparty, derive_generation
        ) VALUES (
            $1, $2, $3, $4, $5, $6,
            $7, $8, $9,
            $10::numeric, $11::numeric,
            $12, $13
        )
        ON CONFLICT (pool, ledger, tx_hash, op_index, event_kind, event_index, ledger_close_time) DO UPDATE SET
            asset             = EXCLUDED.asset,
            user_address      = EXCLUDED.user_address,
            token_amount      = EXCLUDED.token_amount,
            b_or_d_amount     = EXCLUDED.b_or_d_amount,
            counterparty      = EXCLUDED.counterparty,
            derive_generation = EXCLUDED.derive_generation
          WHERE blend_positions.derive_generation <= EXCLUDED.derive_generation
    `
	var counterparty sql.NullString
	if e.Counterparty != "" {
		counterparty = sql.NullString{String: e.Counterparty, Valid: true}
	}
	_, err := s.db.ExecContext(ctx, q,
		e.Pool, int(e.Ledger), e.TxHash, int(e.OpIndex), int(e.EventIndex), e.Timestamp.UTC(),
		e.Kind, e.Asset, e.User,
		bigIntToNumericString(e.TokenAmount), bigIntToNumericString(e.BOrDAmount),
		counterparty, s.deriveGeneration,
	)
	if err != nil {
		return fmt.Errorf("timescale: InsertBlendPositionEvent %s@%d: %w", e.Pool, e.Ledger, err)
	}
	return nil
}

// isBlendPositionKind reports whether kind is one of the seven
// money-market position event kinds. Mirrors the CHECK constraint
// in migration 0045 (0042 is comet_liquidity).
func isBlendPositionKind(kind string) bool {
	switch kind {
	case domain.BlendEventSupply,
		domain.BlendEventWithdraw,
		domain.BlendEventSupplyCollateral,
		domain.BlendEventWithdrawCollateral,
		domain.BlendEventBorrow,
		domain.BlendEventRepay,
		domain.BlendEventFlashLoan:
		return true
	}
	return false
}

// bigIntToNumericString converts a *big.Int amount to the decimal
// string the postgres driver hands to a NUMERIC column verbatim.
// Nil becomes "0" — defensive default to keep the insert successful
// rather than producing a NOT NULL violation on a malformed event
// the decoder shouldn't have produced.
func bigIntToNumericString(n *big.Int) string {
	if n == nil {
		return "0"
	}
	return n.String()
}
