package timescale

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// SoroswapLiquidityEvent is one soroswap_liquidity row (a pair deposit or withdraw); amounts are decimal-string i128 stored verbatim into NUMERIC (ADR-0003).
type SoroswapLiquidityEvent struct {
	Pair            string
	Ledger          uint32
	LedgerCloseTime time.Time
	TxHash          []byte // 32-byte raw hash; hex strings auto-decoded via DecodeSoroswapTxHash
	OpIndex         int16
	EventIndex      int16
	Action          string // "deposit" | "withdraw"
	Provider        string // the event's `to`
	Token0          string // "" → NULL
	Token1          string // "" → NULL
	Amount0         string // decimal i128
	Amount1         string // decimal i128
	Liquidity       string // decimal i128
	NewReserve0     string // decimal i128
	NewReserve1     string // decimal i128
}

// InsertSoroswapLiquidity appends one Soroswap liquidity row, idempotent on the PK via a generation-guarded upsert: a corrected re-derive lands at >= the stored generation and a live gen-0 replay never reverts it.
func (s *Store) InsertSoroswapLiquidity(ctx context.Context, e SoroswapLiquidityEvent) error {
	if e.Pair == "" {
		return errors.New("timescale: InsertSoroswapLiquidity: Pair is empty")
	}
	if len(e.TxHash) == 0 {
		return errors.New("timescale: InsertSoroswapLiquidity: TxHash is empty")
	}
	if e.Provider == "" {
		return errors.New("timescale: InsertSoroswapLiquidity: Provider is empty")
	}
	if e.Action != "deposit" && e.Action != "withdraw" {
		return fmt.Errorf("timescale: InsertSoroswapLiquidity: Action %q not in (deposit, withdraw)", e.Action)
	}
	if e.LedgerCloseTime.IsZero() {
		return fmt.Errorf("timescale: InsertSoroswapLiquidity: zero LedgerCloseTime (pair=%s ledger=%d)", e.Pair, e.Ledger)
	}
	for name, v := range map[string]string{
		"Amount0": e.Amount0, "Amount1": e.Amount1, "Liquidity": e.Liquidity,
		"NewReserve0": e.NewReserve0, "NewReserve1": e.NewReserve1,
	} {
		if v == "" {
			return fmt.Errorf("timescale: InsertSoroswapLiquidity: %s is empty (pair=%s ledger=%d)", name, e.Pair, e.Ledger)
		}
	}

	// direction is a stored convenience derived from action (deposit →
	// add, withdraw → remove) so dashboards can SUM by polarity without
	// re-encoding the mapping in SQL (comet_liquidity 0042).
	direction := "add"
	if e.Action == "withdraw" {
		direction = "remove"
	}

	const q = `
        INSERT INTO soroswap_liquidity (
            ledger_close_time, pair, ledger, tx_hash, op_index, event_index,
            action, direction, provider, token_0, token_1,
            amount_0, amount_1, liquidity, new_reserve_0, new_reserve_1,
            derive_generation
        ) VALUES (
            $1, $2, $3, $4, $5, $6,
            $7, $8, $9, $10, $11,
            $12, $13, $14, $15, $16,
            $17
        )
        ON CONFLICT (ledger_close_time, pair, ledger, tx_hash, op_index, event_index, action) DO UPDATE SET
            direction         = EXCLUDED.direction,
            provider          = EXCLUDED.provider,
            token_0           = EXCLUDED.token_0,
            token_1           = EXCLUDED.token_1,
            amount_0          = EXCLUDED.amount_0,
            amount_1          = EXCLUDED.amount_1,
            liquidity         = EXCLUDED.liquidity,
            new_reserve_0     = EXCLUDED.new_reserve_0,
            new_reserve_1     = EXCLUDED.new_reserve_1,
            derive_generation = EXCLUDED.derive_generation
          WHERE soroswap_liquidity.derive_generation <= EXCLUDED.derive_generation
    `

	nullStr := func(v string) sql.NullString {
		if v == "" {
			return sql.NullString{}
		}
		return sql.NullString{String: v, Valid: true}
	}

	_, err := s.db.ExecContext(ctx, q,
		e.LedgerCloseTime.UTC(), e.Pair, int(e.Ledger), e.TxHash, e.OpIndex, e.EventIndex,
		e.Action, direction, e.Provider, nullStr(e.Token0), nullStr(e.Token1),
		e.Amount0, e.Amount1, e.Liquidity, e.NewReserve0, e.NewReserve1,
		s.deriveGeneration,
	)
	if err != nil {
		return fmt.Errorf("timescale: InsertSoroswapLiquidity %s@%d: %w", e.Pair, e.Ledger, err)
	}
	return nil
}
