package timescale

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// AquariusReservesSyncEvent is one observed Aquarius `reserves_sync`
// event (migration 0128). The writer fans it out to one
// aquarius_reserves_sync row per token position. reserves_sync is a
// distinct reserve-sync signal, separate from the update_reserves
// post-state (AquariusReservesEvent → aquarius_reserves).
type AquariusReservesSyncEvent struct {
	ContractID      string
	Ledger          uint32
	LedgerCloseTime time.Time
	TxHash          string
	OpIndex         uint32
	EventIndex      uint32
	// Reserves is the synced value per token, ordered by the pool's
	// canonical token index. i128 per ADR-0003.
	Reserves []canonical.Amount
}

// InsertAquariusReservesSync appends one reserves_sync observation,
// fanned to one row per token position. Idempotent on the
// (ledger_close_time, contract_id, ledger, tx_hash, op_index,
// event_index, token_index) PK, with the INV-3 generation-guarded
// corrective upsert (migration 0110): a re-derive at a HIGHER
// derive_generation lands its correction in place; a lower generation
// can never revert one. Mirrors InsertAquariusReserves.
//
// Defensive: rejects an empty ContractID / TxHash, an empty vector, and
// a negative value before touching the DB.
func (s *Store) InsertAquariusReservesSync(ctx context.Context, e AquariusReservesSyncEvent) error {
	if e.ContractID == "" {
		return errors.New("timescale: InsertAquariusReservesSync: ContractID is empty")
	}
	if e.TxHash == "" {
		return errors.New("timescale: InsertAquariusReservesSync: TxHash is empty")
	}
	if len(e.Reserves) == 0 {
		return errors.New("timescale: InsertAquariusReservesSync: empty reserve vector")
	}

	const q = `
        INSERT INTO aquarius_reserves_sync (
            contract_id, ledger, ledger_close_time, tx_hash,
            op_index, event_index, token_index, reserve_synced,
            derive_generation
        ) VALUES (
            $1, $2, $3, $4, $5, $6, $7, $8, $9
        )
        ON CONFLICT (ledger_close_time, contract_id, ledger, tx_hash,
                     op_index, event_index, token_index) DO UPDATE SET
            reserve_synced    = EXCLUDED.reserve_synced,
            derive_generation = EXCLUDED.derive_generation
          WHERE aquarius_reserves_sync.derive_generation <= EXCLUDED.derive_generation
    `
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("timescale: InsertAquariusReservesSync begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	for i, r := range e.Reserves {
		if r.Sign() < 0 {
			return fmt.Errorf("timescale: InsertAquariusReservesSync: reserve[%d] must be >= 0 (got %s)", i, r)
		}
		if _, err := tx.ExecContext(ctx, q,
			e.ContractID, int(e.Ledger), e.LedgerCloseTime.UTC(), e.TxHash,
			int(e.OpIndex), int(e.EventIndex), i, r.String(),
			s.deriveGeneration,
		); err != nil {
			return fmt.Errorf("timescale: InsertAquariusReservesSync %s@%d[%d]: %w", e.ContractID, e.Ledger, i, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("timescale: InsertAquariusReservesSync commit: %w", err)
	}
	return nil
}
