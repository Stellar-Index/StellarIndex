package timescale

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// AquariusProtocolFeeEvent is one observed Aquarius protocol-fee event
// (migration 0129), discriminated by Kind:
//   - "set_protocol_fee":   the four Fee* fields are the per-token
//     old→new protocol fee (u32); Recipient/Amount unused.
//   - "claim_protocol_fee": Recipient is the fee-sweep destination and
//     Amount the swept i128 decimal string; Fee* unused.
type AquariusProtocolFeeEvent struct {
	ContractID      string
	Ledger          uint32
	LedgerCloseTime time.Time
	TxHash          string
	OpIndex         uint32
	EventIndex      uint32
	Kind            string
	Fee0New         uint32
	Fee0Old         uint32
	Fee1New         uint32
	Fee1Old         uint32
	Recipient       string
	Amount          string // decimal i128; "" on set_protocol_fee
}

// InsertAquariusProtocolFee lands one protocol-fee event, idempotent on
// the (ledger_close_time, contract_id, ledger, tx_hash, op_index,
// event_index) PK with the INV-3 generation-guarded corrective upsert
// (migration 0110). The per-kind columns are NULL for the other kind.
//
// Defensive: rejects an empty ContractID / TxHash, a zero
// LedgerCloseTime, an unknown Kind, and — on claim — an empty Recipient
// or Amount, before touching the DB.
func (s *Store) InsertAquariusProtocolFee(ctx context.Context, e AquariusProtocolFeeEvent) error {
	if e.ContractID == "" {
		return errors.New("timescale: InsertAquariusProtocolFee: ContractID is empty")
	}
	if e.TxHash == "" {
		return errors.New("timescale: InsertAquariusProtocolFee: TxHash is empty")
	}
	if e.LedgerCloseTime.IsZero() {
		return fmt.Errorf("timescale: InsertAquariusProtocolFee: zero LedgerCloseTime (contract=%s ledger=%d)", e.ContractID, e.Ledger)
	}

	var (
		fee0New, fee0Old, fee1New, fee1Old sql.NullInt64
		recipient, amount                  sql.NullString
	)
	switch e.Kind {
	case "set_protocol_fee":
		fee0New = sql.NullInt64{Int64: int64(e.Fee0New), Valid: true}
		fee0Old = sql.NullInt64{Int64: int64(e.Fee0Old), Valid: true}
		fee1New = sql.NullInt64{Int64: int64(e.Fee1New), Valid: true}
		fee1Old = sql.NullInt64{Int64: int64(e.Fee1Old), Valid: true}
	case "claim_protocol_fee":
		if e.Recipient == "" {
			return fmt.Errorf("timescale: InsertAquariusProtocolFee: claim recipient empty (contract=%s ledger=%d)", e.ContractID, e.Ledger)
		}
		if e.Amount == "" {
			return fmt.Errorf("timescale: InsertAquariusProtocolFee: claim amount empty (contract=%s ledger=%d)", e.ContractID, e.Ledger)
		}
		recipient = sql.NullString{String: e.Recipient, Valid: true}
		amount = sql.NullString{String: e.Amount, Valid: true}
	default:
		return fmt.Errorf("timescale: InsertAquariusProtocolFee: unknown Kind %q", e.Kind)
	}

	const q = `
        INSERT INTO aquarius_protocol_fee (
            contract_id, ledger, ledger_close_time, tx_hash,
            op_index, event_index, kind,
            fee_protocol0_new, fee_protocol0_old, fee_protocol1_new, fee_protocol1_old,
            recipient, amount,
            derive_generation
        ) VALUES (
            $1, $2, $3, $4, $5, $6, $7,
            $8, $9, $10, $11,
            $12, $13,
            $14
        )
        ON CONFLICT (ledger_close_time, contract_id, ledger, tx_hash, op_index, event_index) DO UPDATE SET
            kind              = EXCLUDED.kind,
            fee_protocol0_new = EXCLUDED.fee_protocol0_new,
            fee_protocol0_old = EXCLUDED.fee_protocol0_old,
            fee_protocol1_new = EXCLUDED.fee_protocol1_new,
            fee_protocol1_old = EXCLUDED.fee_protocol1_old,
            recipient         = EXCLUDED.recipient,
            amount            = EXCLUDED.amount,
            derive_generation = EXCLUDED.derive_generation
          WHERE aquarius_protocol_fee.derive_generation <= EXCLUDED.derive_generation
    `
	if _, err := s.db.ExecContext(ctx, q,
		e.ContractID, int(e.Ledger), e.LedgerCloseTime.UTC(), e.TxHash,
		int(e.OpIndex), int(e.EventIndex), e.Kind,
		fee0New, fee0Old, fee1New, fee1Old,
		recipient, amount,
		s.deriveGeneration,
	); err != nil {
		return fmt.Errorf("timescale: InsertAquariusProtocolFee %s@%d: %w", e.ContractID, e.Ledger, err)
	}
	return nil
}
