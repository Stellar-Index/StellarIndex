package timescale

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// SushiswapV3PositionEvent is one sushiswap_v3_position_events row — a
// single observed pool `mint`, `burn` or `collect` (migration 0203).
//
// Amounts are decimal-string u128 values per ADR-0003, stored verbatim into
// the NUMERIC columns. Liquidity is "" for a collect (→ SQL NULL); Sender
// and Recipient are "" when the event does not carry them.
type SushiswapV3PositionEvent struct {
	Pool            string
	Ledger          uint32
	LedgerCloseTime time.Time
	TxHash          []byte // 32-byte raw hash; hex strings auto-decoded via DecodeSoroswapTxHash
	OpIndex         int16
	EventIndex      int16
	Action          string // "mint" | "burn" | "collect"
	Owner           string
	Sender          string // mint only; "" → NULL
	Recipient       string // collect only; "" → NULL
	Token0          string
	Token1          string
	TickLower       int32
	TickUpper       int32
	Liquidity       string // decimal u128; "" → NULL (collect)
	Amount0         string // decimal u128
	Amount1         string // decimal u128
}

// InsertSushiswapV3PositionEvent appends one position event, idempotent on
// the (ledger_close_time, pool, ledger, tx_hash, op_index, event_index,
// action) PK, with the generation-guarded corrective upsert (migration 0110
// pattern): a corrected re-derive lands in place when its generation is >=
// the stored one; a live gen-0 replay can never revert it.
func (s *Store) InsertSushiswapV3PositionEvent(ctx context.Context, e SushiswapV3PositionEvent) error {
	if err := validateSushiswapV3PositionEvent(e); err != nil {
		return err
	}

	const q = `
        INSERT INTO sushiswap_v3_position_events (
            ledger_close_time, pool, ledger, tx_hash, op_index, event_index,
            action, owner, sender, recipient, token_0, token_1,
            tick_lower, tick_upper, liquidity, amount_0, amount_1,
            derive_generation
        ) VALUES (
            $1, $2, $3, $4, $5, $6,
            $7, $8, $9, $10, $11, $12,
            $13, $14, $15, $16, $17,
            $18
        )
        ON CONFLICT (ledger_close_time, pool, ledger, tx_hash, op_index, event_index, action) DO UPDATE SET
            owner             = EXCLUDED.owner,
            sender            = EXCLUDED.sender,
            recipient         = EXCLUDED.recipient,
            token_0           = EXCLUDED.token_0,
            token_1           = EXCLUDED.token_1,
            tick_lower        = EXCLUDED.tick_lower,
            tick_upper        = EXCLUDED.tick_upper,
            liquidity         = EXCLUDED.liquidity,
            amount_0          = EXCLUDED.amount_0,
            amount_1          = EXCLUDED.amount_1,
            derive_generation = EXCLUDED.derive_generation
          WHERE sushiswap_v3_position_events.derive_generation <= EXCLUDED.derive_generation
    `

	nullStr := func(v string) sql.NullString {
		return sql.NullString{String: v, Valid: v != ""}
	}

	_, err := s.db.ExecContext(ctx, q,
		e.LedgerCloseTime.UTC(), e.Pool, int(e.Ledger), e.TxHash, e.OpIndex, e.EventIndex,
		e.Action, e.Owner, nullStr(e.Sender), nullStr(e.Recipient), e.Token0, e.Token1,
		e.TickLower, e.TickUpper, nullStr(e.Liquidity), e.Amount0, e.Amount1,
		s.deriveGeneration,
	)
	if err != nil {
		return fmt.Errorf("timescale: InsertSushiswapV3PositionEvent %s@%d: %w", e.Pool, e.Ledger, err)
	}
	return nil
}

func validateSushiswapV3PositionEvent(e SushiswapV3PositionEvent) error {
	const fn = "timescale: InsertSushiswapV3PositionEvent"
	switch {
	case e.Pool == "":
		return errors.New(fn + ": Pool is empty")
	case len(e.TxHash) == 0:
		return errors.New(fn + ": TxHash is empty")
	case e.Owner == "":
		return errors.New(fn + ": Owner is empty")
	case e.Token0 == "" || e.Token1 == "":
		return fmt.Errorf("%s: token identity missing (pool=%s ledger=%d)", fn, e.Pool, e.Ledger)
	case e.LedgerCloseTime.IsZero():
		return fmt.Errorf("%s: zero LedgerCloseTime (pool=%s ledger=%d)", fn, e.Pool, e.Ledger)
	case e.Amount0 == "" || e.Amount1 == "":
		return fmt.Errorf("%s: amount is empty (pool=%s ledger=%d)", fn, e.Pool, e.Ledger)
	}
	switch e.Action {
	case "mint", "burn":
		if e.Liquidity == "" {
			return fmt.Errorf("%s: Liquidity is empty for %s (pool=%s ledger=%d)", fn, e.Action, e.Pool, e.Ledger)
		}
	case "collect":
	default:
		return fmt.Errorf("%s: Action %q not in (mint, burn, collect)", fn, e.Action)
	}
	return nil
}
