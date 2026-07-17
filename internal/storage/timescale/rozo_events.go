package timescale

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// RozoEventType discriminates the two Rozo v1 Payment event variants.
// String values match the rozo_events.event_type CHECK constraint
// (migration 0039) and internal/sources/rozo's event-name constants.
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

// RozoEvent is one rozo_events row — a single observed Rozo v1
// intent-bridge contract event on Stellar. Mirrors the
// migration-0039 columns.
//
// Amount and Destination are present on both event types. From and
// Memo are payment-only; Token is flush-only — each is a *string so
// a nil pointer writes SQL NULL and distinguishes "not applicable to
// this event type" from an empty-string value (an empty payment memo
// is a real value and round-trips as an empty string, not NULL).
type RozoEvent struct {
	ContractID string
	Ledger     uint32
	TxHash     string
	OpIndex    uint32
	// EventIndex is the position of this event within its operation's
	// contract-event list — the migration-0112 PK discriminator that keeps
	// two same-type events emitted by one op from collapsing (C2-13a).
	EventIndex  uint32
	ObservedAt  time.Time
	EventType   RozoEventType
	Amount      string // decimal i128
	Destination string
	From        *string // payment-only
	Memo        *string // payment-only ('' is a valid tag)
	Token       *string // flush-only
}

// InsertRozoEvent appends one Rozo event row, idempotent on the
// (contract_id, ledger, tx_hash, op_index, event_type, ts) PK.
// Re-running the indexer or a backfill over the same range writes
// the same rows; ON CONFLICT DO NOTHING makes the replay a no-op.
//
// Defensive: rejects empty ContractID / TxHash / Destination, an
// invalid EventType, and an empty Amount (the column is NOT NULL)
// before touching the DB.
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

	// INV-3 generation-guarded corrective upsert (migration 0110): a
	// corrected re-derive of the payment `amount` (or destination / from /
	// memo / token) lands in place when its generation is >= the stored one;
	// a live gen-0 replay can never revert it. Replaces the old DO NOTHING.
	// event_index ($13) is in the INSERT column list AND the ON CONFLICT
	// target (migration 0112, C2-13a): two same-type events emitted by one
	// op differ only in event_index, so it MUST be part of the conflict key
	// or the second row collapses onto the first.
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
