package timescale

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// PhoenixInitializeEvent is one observed Phoenix pool-deploy
// `initialize` token announcement (migration 0131) — one row per pool
// token (slot 'a' / 'b'), each carrying the token contract address.
type PhoenixInitializeEvent struct {
	Pool            string
	Ledger          uint32
	LedgerCloseTime time.Time
	TxHash          string
	OpIndex         uint32
	EventIndex      uint32
	TokenSlot       string // "a" | "b"
	Token           string
}

// InsertPhoenixInitialize lands one initialize event, idempotent on the
// (ledger_close_time, pool, ledger, tx_hash, op_index, event_index) PK
// with the INV-3 generation-guarded corrective upsert (migration 0110).
//
// Defensive: rejects an empty Pool / TxHash / Token, a zero
// LedgerCloseTime, and a TokenSlot outside {'a','b'}.
func (s *Store) InsertPhoenixInitialize(ctx context.Context, e PhoenixInitializeEvent) error {
	if e.Pool == "" {
		return errors.New("timescale: InsertPhoenixInitialize: Pool is empty")
	}
	if e.TxHash == "" {
		return errors.New("timescale: InsertPhoenixInitialize: TxHash is empty")
	}
	if e.Token == "" {
		return errors.New("timescale: InsertPhoenixInitialize: Token is empty")
	}
	if e.LedgerCloseTime.IsZero() {
		return fmt.Errorf("timescale: InsertPhoenixInitialize: zero LedgerCloseTime (pool=%s ledger=%d)", e.Pool, e.Ledger)
	}
	if e.TokenSlot != "a" && e.TokenSlot != "b" {
		return fmt.Errorf("timescale: InsertPhoenixInitialize: TokenSlot %q not in {a,b}", e.TokenSlot)
	}

	const q = `
        INSERT INTO phoenix_initialize (
            pool, ledger, ledger_close_time, tx_hash,
            op_index, event_index, token_slot, token, derive_generation
        ) VALUES (
            $1, $2, $3, $4, $5, $6, $7, $8, $9
        )
        ON CONFLICT (ledger_close_time, pool, ledger, tx_hash, op_index, event_index) DO UPDATE SET
            token_slot        = EXCLUDED.token_slot,
            token             = EXCLUDED.token,
            derive_generation = EXCLUDED.derive_generation
          WHERE phoenix_initialize.derive_generation <= EXCLUDED.derive_generation
    `
	if _, err := s.db.ExecContext(ctx, q,
		e.Pool, int(e.Ledger), e.LedgerCloseTime.UTC(), e.TxHash,
		int(e.OpIndex), int(e.EventIndex), e.TokenSlot, e.Token,
		s.deriveGeneration,
	); err != nil {
		return fmt.Errorf("timescale: InsertPhoenixInitialize %s@%d: %w", e.Pool, e.Ledger, err)
	}
	return nil
}
