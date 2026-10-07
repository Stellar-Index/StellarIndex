package timescale

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// validDefindexAdminKinds is the closed set the migration 0192 CHECK
// enforces.
var validDefindexAdminKinds = map[string]bool{
	"rescue": true, "paused": true, "unpaused": true,
	"nreceiver": true, "nmanager": true, "nemanager": true, "rbmanager": true,
}

// DefindexAdminEvent is one defindex_admin_events row (migration 0192):
// a DeFindex vault role rotation, strategy pause toggle or rescue.
// Optional fields are "" when the kind's body does not carry them
// (stored NULL); Amount is a decimal i128 per ADR-0003.
type DefindexAdminEvent struct {
	Ledger          uint32
	LedgerCloseTime time.Time
	TxHash          string
	OpIndex         uint32
	EventIndex      uint32
	ContractID      string // the emitting vault
	EventKind       string
	Caller          string
	Strategy        string
	NewAddress      string
	Amount          string
}

// InsertDefindexAdminEvent lands one vault admin event, idempotent on
// the (ledger_close_time, contract_id, ledger, tx_hash, op_index,
// event_index) PK with the generation-guarded corrective upsert
// (migration 0110 convention).
func (s *Store) InsertDefindexAdminEvent(ctx context.Context, e DefindexAdminEvent) error {
	if e.TxHash == "" {
		return errors.New("timescale: InsertDefindexAdminEvent: TxHash is empty")
	}
	if e.ContractID == "" {
		return errors.New("timescale: InsertDefindexAdminEvent: ContractID is empty")
	}
	if e.LedgerCloseTime.IsZero() {
		return fmt.Errorf("timescale: InsertDefindexAdminEvent: zero LedgerCloseTime (contract=%s ledger=%d)", e.ContractID, e.Ledger)
	}
	if !validDefindexAdminKinds[e.EventKind] {
		return fmt.Errorf("timescale: InsertDefindexAdminEvent: unknown EventKind %q", e.EventKind)
	}

	const q = `
        INSERT INTO defindex_admin_events (
            ledger, ledger_close_time, tx_hash, op_index, event_index,
            contract_id, event_kind, caller, strategy, new_address, amount,
            derive_generation
        ) VALUES (
            $1, $2, $3, $4, $5,
            $6, $7, $8, $9, $10, $11::numeric,
            $12
        )
        ON CONFLICT (ledger_close_time, contract_id, ledger, tx_hash, op_index, event_index) DO UPDATE SET
            event_kind        = EXCLUDED.event_kind,
            caller            = EXCLUDED.caller,
            strategy          = EXCLUDED.strategy,
            new_address       = EXCLUDED.new_address,
            amount            = EXCLUDED.amount,
            derive_generation = EXCLUDED.derive_generation
          WHERE defindex_admin_events.derive_generation <= EXCLUDED.derive_generation
    `
	if _, err := s.db.ExecContext(ctx, q,
		int(e.Ledger), e.LedgerCloseTime.UTC(), e.TxHash, int(e.OpIndex), int(e.EventIndex),
		e.ContractID, e.EventKind, nullString(e.Caller), nullString(e.Strategy), nullString(e.NewAddress), nullString(e.Amount),
		s.deriveGeneration,
	); err != nil {
		return fmt.Errorf("timescale: InsertDefindexAdminEvent %s@%d: %w", e.TxHash, e.Ledger, err)
	}
	return nil
}
