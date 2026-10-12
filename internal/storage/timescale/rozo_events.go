package timescale

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// RozoEventType discriminates the two Rozo v1 Payment event variants.
type RozoEventType string

const (
	RozoPayment RozoEventType = "payment"
	RozoFlush   RozoEventType = "flush"
)

// IsValid reports whether t is one of the two known Rozo v1 events.
func (t RozoEventType) IsValid() bool {
	switch t {
	case RozoPayment, RozoFlush:
		return true
	}
	return false
}

// RozoEvent is one rozo_events row; Amount and Destination are on both event types, From/Memo payment-only and Token flush-only, and nil *string writes SQL NULL ("not applicable") as distinct from "".
type RozoEvent struct {
	ContractID string
	Ledger     uint32
	TxHash     string
	OpIndex    uint32
	// EventIndex is the position of this event within its operation's
	// contract-event list — the migration-0112 PK discriminator that keeps
	// two same-type events emitted by one op from collapsing.
	EventIndex  uint32
	ObservedAt  time.Time
	EventType   RozoEventType
	Amount      string // decimal i128
	Destination string
	From        *string // payment-only
	// Memo is payment-only ('' is a valid tag). The on-chain memo is payer-chosen BYTES, handed
	// over in scval.ToText form (literal if valid NUL-free UTF-8, else `\x` + hex). InsertRozoEvent
	// binds it unchanged: ToText is not idempotent, so re-encoding would corrupt it.
	Memo  *string
	Token *string // flush-only
}

// InsertRozoEvent appends one Rozo event row, idempotent on the PK with a derive_generation-guarded upsert that corrects on an equal-or-higher replay and no-ops a stale one.
func (s *Store) InsertRozoEvent(ctx context.Context, e RozoEvent) error {
	if e.ContractID == "" {
		return errors.New("timescale: InsertRozoEvent: ContractID is empty")
	}
	if e.TxHash == "" {
		return errors.New("timescale: InsertRozoEvent: TxHash is empty")
	}
	if e.Destination == "" {
		return errors.New("timescale: InsertRozoEvent: Destination is empty")
	}
	if !e.EventType.IsValid() {
		return fmt.Errorf("timescale: InsertRozoEvent: invalid EventType %q", e.EventType)
	}
	if e.Amount == "" {
		return fmt.Errorf("timescale: InsertRozoEvent: Amount is empty (contract=%s tx=%s)", e.ContractID, e.TxHash)
	}

	// Generation-guarded corrective upsert: a corrected re-derive lands in place when its
	// generation is >= the stored one, so a live gen-0 replay never reverts it. event_index ($13)
	// MUST be in the ON CONFLICT target: two same-type events from one op differ only by it, and
	// otherwise the second row collapses onto the first.
	const q = `
        INSERT INTO rozo_events (
            contract_id, ledger, tx_hash, op_index, ts,
            event_type, amount, destination, from_addr, memo, token,
            derive_generation, event_index
        ) VALUES (
            $1, $2, $3, $4, $5,
            $6, $7, $8, $9, $10, $11,
            $12, $13
        )
        ON CONFLICT (contract_id, ledger, tx_hash, op_index, event_type, event_index, ts) DO UPDATE SET
            amount            = EXCLUDED.amount,
            destination       = EXCLUDED.destination,
            from_addr         = EXCLUDED.from_addr,
            memo              = EXCLUDED.memo,
            token             = EXCLUDED.token,
            derive_generation = EXCLUDED.derive_generation
          WHERE rozo_events.derive_generation <= EXCLUDED.derive_generation
    `
	_, err := s.db.ExecContext(ctx, q,
		e.ContractID, int(e.Ledger), e.TxHash, int(e.OpIndex), e.ObservedAt.UTC(),
		string(e.EventType), e.Amount, e.Destination,
		ptrToNullString(e.From), ptrToNullString(e.Memo), ptrToNullString(e.Token),
		s.deriveGeneration, int(e.EventIndex),
	)
	if err != nil {
		return fmt.Errorf("timescale: InsertRozoEvent %s@%d: %w", e.ContractID, e.Ledger, err)
	}
	return nil
}

// ptrToNullString maps a nil *string to SQL NULL and a non-nil one to
// its value (including the empty string, which is a meaningful value
// — see RozoEvent.Memo).
func ptrToNullString(v *string) sql.NullString {
	if v == nil {
		return sql.NullString{}
	}
	return sql.NullString{String: *v, Valid: true}
}
