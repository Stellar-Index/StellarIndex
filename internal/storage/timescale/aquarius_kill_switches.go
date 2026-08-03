package timescale

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// validAquariusKillActions is the closed set the migration 0130 CHECK
// enforces; the writer rejects anything else before touching the DB.
var validAquariusKillActions = map[string]bool{
	"kill_deposit": true, "unkill_deposit": true,
	"kill_swap": true, "unkill_swap": true,
	"kill_claim": true, "unkill_claim": true,
	"kill_gauges_claim": true, "unkill_gauges_claim": true,
}

// AquariusKillSwitchEvent is one observed Aquarius circuit-breaker
// toggle (migration 0130). The event carries no body — the row is the
// event identity plus the Action.
type AquariusKillSwitchEvent struct {
	ContractID      string
	Ledger          uint32
	LedgerCloseTime time.Time
	TxHash          string
	OpIndex         uint32
	EventIndex      uint32
	Action          string
}

// InsertAquariusKillSwitch lands one circuit-breaker toggle, idempotent
// on the (ledger_close_time, contract_id, ledger, tx_hash, op_index,
// event_index) PK with the INV-3 generation-guarded corrective upsert
// (migration 0110).
//
// Defensive: rejects an empty ContractID / TxHash, a zero
// LedgerCloseTime, and an Action outside the closed set.
func (s *Store) InsertAquariusKillSwitch(ctx context.Context, e AquariusKillSwitchEvent) error {
	if e.ContractID == "" {
		return errors.New("timescale: InsertAquariusKillSwitch: ContractID is empty")
	}
	if e.TxHash == "" {
		return errors.New("timescale: InsertAquariusKillSwitch: TxHash is empty")
	}
	if e.LedgerCloseTime.IsZero() {
		return fmt.Errorf("timescale: InsertAquariusKillSwitch: zero LedgerCloseTime (contract=%s ledger=%d)", e.ContractID, e.Ledger)
	}
	if !validAquariusKillActions[e.Action] {
		return fmt.Errorf("timescale: InsertAquariusKillSwitch: unknown Action %q", e.Action)
	}

	const q = `
        INSERT INTO aquarius_kill_switches (
            contract_id, ledger, ledger_close_time, tx_hash,
            op_index, event_index, action, derive_generation
        ) VALUES (
            $1, $2, $3, $4, $5, $6, $7, $8
        )
        ON CONFLICT (ledger_close_time, contract_id, ledger, tx_hash, op_index, event_index) DO UPDATE SET
            action            = EXCLUDED.action,
            derive_generation = EXCLUDED.derive_generation
          WHERE aquarius_kill_switches.derive_generation <= EXCLUDED.derive_generation
    `
	if _, err := s.db.ExecContext(ctx, q,
		e.ContractID, int(e.Ledger), e.LedgerCloseTime.UTC(), e.TxHash,
		int(e.OpIndex), int(e.EventIndex), e.Action,
		s.deriveGeneration,
	); err != nil {
		return fmt.Errorf("timescale: InsertAquariusKillSwitch %s@%d: %w", e.ContractID, e.Ledger, err)
	}
	return nil
}
