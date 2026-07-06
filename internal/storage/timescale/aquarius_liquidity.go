package timescale

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/StellarIndex/stellar-index/internal/canonical"
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

	const q = `
        INSERT INTO aquarius_reserves (
            contract_id, ledger, ledger_close_time, tx_hash,
            op_index, event_index, token_index, reserve
        ) VALUES (
            $1, $2, $3, $4, $5, $6, $7, $8
        )
        ON CONFLICT (ledger_close_time, contract_id, ledger, tx_hash,
                     op_index, event_index, token_index) DO NOTHING
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

	const q = `
        INSERT INTO aquarius_liquidity (
            contract_id, ledger, ledger_close_time, tx_hash,
            op_index, event_index, action, token_index, token, amount, shares
        ) VALUES (
            $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11
        )
        ON CONFLICT (ledger_close_time, contract_id, ledger, tx_hash,
                     op_index, event_index, action, token_index) DO NOTHING
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
		); err != nil {
			return fmt.Errorf("timescale: InsertAquariusLiquidity %s@%d[%d]: %w", e.ContractID, e.Ledger, i, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("timescale: InsertAquariusLiquidity commit: %w", err)
	}
	return nil
}
