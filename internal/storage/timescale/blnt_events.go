package timescale

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"
)

// BLNTEvent is one lossless row in blnt_events. TopicsXDR and ValueXDR are
// base64-encoded SCVals; validation and semantic classification happen in the
// source decoder before this writer is called.
type BLNTEvent struct {
	ContractID      string
	Ledger          uint32
	LedgerCloseTime time.Time
	TxHash          string
	OpIndex         uint32
	EventIndex      uint32
	ContractRole    string
	EventKind       string
	TopicsXDR       []string
	ValueXDR        string
}

var blntContractRoles = []string{
	"backfill", "backstop", "emitter", "factory", "lockup_backfill", "pool",
}

// InsertBLNTEvent appends one BLNT V3 event, idempotent on its ledger identity.
// Equal-or-higher derive generations correct a prior projection; a lower
// generation cannot overwrite it.
func (s *Store) InsertBLNTEvent(ctx context.Context, e BLNTEvent) error {
	if e.ContractID == "" {
		return errors.New("timescale: InsertBLNTEvent: ContractID is empty")
	}
	if e.TxHash == "" {
		return errors.New("timescale: InsertBLNTEvent: TxHash is empty")
	}
	if !slices.Contains(blntContractRoles, e.ContractRole) {
		return fmt.Errorf("timescale: InsertBLNTEvent: invalid ContractRole %q", e.ContractRole)
	}
	if e.EventKind == "" {
		return errors.New("timescale: InsertBLNTEvent: EventKind is empty")
	}
	if len(e.TopicsXDR) == 0 {
		return errors.New("timescale: InsertBLNTEvent: TopicsXDR is empty")
	}
	if e.ValueXDR == "" {
		return errors.New("timescale: InsertBLNTEvent: ValueXDR is empty")
	}
	if e.LedgerCloseTime.IsZero() {
		return errors.New("timescale: InsertBLNTEvent: LedgerCloseTime is zero")
	}

	const q = `
        INSERT INTO blnt_events (
            contract_id, ledger, ledger_close_time, tx_hash, op_index, event_index,
            contract_role, event_kind, topics_xdr, value_xdr, derive_generation
        ) VALUES (
            $1, $2, $3, $4, $5, $6,
            $7, $8, $9, $10, $11
        )
        ON CONFLICT (ledger_close_time, contract_id, ledger, tx_hash,
                     op_index, event_index) DO UPDATE SET
            contract_role     = EXCLUDED.contract_role,
            event_kind        = EXCLUDED.event_kind,
            topics_xdr        = EXCLUDED.topics_xdr,
            value_xdr         = EXCLUDED.value_xdr,
            derive_generation = EXCLUDED.derive_generation
          WHERE blnt_events.derive_generation <= EXCLUDED.derive_generation
    `
	_, err := s.db.ExecContext(ctx, q,
		e.ContractID, int(e.Ledger), e.LedgerCloseTime.UTC(), e.TxHash,
		int(e.OpIndex), int(e.EventIndex), e.ContractRole, e.EventKind,
		e.TopicsXDR, e.ValueXDR, s.deriveGeneration,
	)
	if err != nil {
		return fmt.Errorf("timescale: InsertBLNTEvent %s@%d: %w", e.ContractID, e.Ledger, err)
	}
	return nil
}
