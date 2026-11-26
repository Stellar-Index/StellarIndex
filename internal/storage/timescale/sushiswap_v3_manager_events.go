package timescale

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// SushiswapV3ManagerEvent is one position-manager row; amounts are decimal-string u128 stored verbatim into NUMERIC, and an
// empty string is SQL NULL (a field the event kind does not carry).
type SushiswapV3ManagerEvent struct {
	Manager         string
	Ledger          uint32
	LedgerCloseTime time.Time
	TxHash          []byte // 32-byte raw hash
	OpIndex         int16
	EventIndex      int16
	Kind            string // mint | burn | increase_liquidity | decrease_liquidity | collect
	TokenID         uint32
	Owner           string // mint / burn
	Recipient       string // collect
	Liquidity       string // increase / decrease
	Amount0         string // increase / decrease / collect
	Amount1         string
}

// sushiswapV3ManagerShape is the column set each kind must carry; every other nullable column must be empty.
var sushiswapV3ManagerShape = map[string]struct{ owner, recipient, liquidity, amounts bool }{
	"mint":               {owner: true},
	"burn":               {owner: true},
	"increase_liquidity": {liquidity: true, amounts: true},
	"decrease_liquidity": {liquidity: true, amounts: true},
	"collect":            {recipient: true, amounts: true},
}

// InsertSushiswapV3ManagerEvent appends one manager event, idempotent on the PK via a generation-guarded upsert so a corrected re-derive lands and a live gen-0 replay never reverts it.
func (s *Store) InsertSushiswapV3ManagerEvent(ctx context.Context, e SushiswapV3ManagerEvent) error {
	if err := validateSushiswapV3ManagerEvent(e); err != nil {
		return err
	}

	const q = `
        INSERT INTO sushiswap_v3_manager_events (
            ledger_close_time, manager, ledger, tx_hash, op_index, event_index,
            kind, token_id, owner, recipient, liquidity, amount_0, amount_1,
            derive_generation
        ) VALUES (
            $1, $2, $3, $4, $5, $6,
            $7, $8, $9, $10, $11, $12, $13,
            $14
        )
        ON CONFLICT (ledger_close_time, manager, ledger, tx_hash, op_index, event_index, kind) DO UPDATE SET
            token_id          = EXCLUDED.token_id,
            owner             = EXCLUDED.owner,
            recipient         = EXCLUDED.recipient,
            liquidity         = EXCLUDED.liquidity,
            amount_0          = EXCLUDED.amount_0,
            amount_1          = EXCLUDED.amount_1,
            derive_generation = EXCLUDED.derive_generation
          WHERE sushiswap_v3_manager_events.derive_generation <= EXCLUDED.derive_generation
    `

	nullStr := func(v string) sql.NullString {
		return sql.NullString{String: v, Valid: v != ""}
	}

	_, err := s.db.ExecContext(ctx, q,
		e.LedgerCloseTime.UTC(), e.Manager, int(e.Ledger), e.TxHash, e.OpIndex, e.EventIndex,
		e.Kind, int64(e.TokenID), nullStr(e.Owner), nullStr(e.Recipient),
		nullStr(e.Liquidity), nullStr(e.Amount0), nullStr(e.Amount1),
		s.deriveGeneration,
	)
	if err != nil {
		return fmt.Errorf("timescale: InsertSushiswapV3ManagerEvent %s@%d: %w", e.Manager, e.Ledger, err)
	}
	return nil
}

func validateSushiswapV3ManagerEvent(e SushiswapV3ManagerEvent) error {
	const fn = "timescale: InsertSushiswapV3ManagerEvent"
	switch {
	case e.Manager == "":
		return errors.New(fn + ": Manager is empty")
	case len(e.TxHash) == 0:
		return errors.New(fn + ": TxHash is empty")
	case e.LedgerCloseTime.IsZero():
		return fmt.Errorf("%s: zero LedgerCloseTime (manager=%s ledger=%d)", fn, e.Manager, e.Ledger)
	}
	shape, ok := sushiswapV3ManagerShape[e.Kind]
	if !ok {
		return fmt.Errorf("%s: Kind %q is not a position-manager event", fn, e.Kind)
	}
	for _, c := range []struct {
		name      string
		want, got bool
	}{
		{"owner", shape.owner, e.Owner != ""},
		{"recipient", shape.recipient, e.Recipient != ""},
		{"liquidity", shape.liquidity, e.Liquidity != ""},
		{"amount_0", shape.amounts, e.Amount0 != ""},
		{"amount_1", shape.amounts, e.Amount1 != ""},
	} {
		if c.want != c.got {
			return fmt.Errorf("%s: %s present=%t, want %t for %s (manager=%s ledger=%d)",
				fn, c.name, c.got, c.want, e.Kind, e.Manager, e.Ledger)
		}
	}
	return nil
}
