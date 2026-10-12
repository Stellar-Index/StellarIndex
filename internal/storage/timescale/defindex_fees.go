package timescale

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// DefindexFee is one defindex_fees row, a per-asset entry of a vault `dfees` distribution; FeeIndex is the position in distributed_fees and a PK component, and Token is the fee token C-strkey.
type DefindexFee struct {
	Ledger          uint32
	LedgerCloseTime time.Time
	TxHash          string
	OpIndex         uint32
	EventIndex      uint32 // distinguishes the dfees event from its same-op vault-flow siblings (PK component)
	FeeIndex        uint32 // position in the event's distributed_fees Vec (PK component)
	ContractID      string // the emitting DeFindex vault-wrapper contract
	Token           string // fee token contract C-strkey
	Amount          string // decimal i128 per ADR-0003
}

// InsertDefindexFee lands one dfees entry, idempotent on its PK with a generation-guarded corrective upsert (>= stored lands, lower is refused).
func (s *Store) InsertDefindexFee(ctx context.Context, e DefindexFee) error {
	if e.TxHash == "" {
		return errors.New("timescale: InsertDefindexFee: TxHash is empty")
	}
	if e.ContractID == "" {
		return errors.New("timescale: InsertDefindexFee: ContractID is empty")
	}
	if e.Token == "" {
		return errors.New("timescale: InsertDefindexFee: Token is empty")
	}
	if e.Amount == "" {
		return errors.New("timescale: InsertDefindexFee: Amount is empty")
	}
	if e.LedgerCloseTime.IsZero() {
		return fmt.Errorf("timescale: InsertDefindexFee: zero LedgerCloseTime (contract=%s ledger=%d)", e.ContractID, e.Ledger)
	}

	const q = `
        INSERT INTO defindex_fees (
            ledger, ledger_close_time, tx_hash, op_index, event_index, fee_index,
            contract_id, token, amount,
            derive_generation
        ) VALUES (
            $1, $2, $3, $4, $5, $6,
            $7, $8, $9,
            $10
        )
        ON CONFLICT (ledger_close_time, contract_id, ledger, tx_hash, op_index, event_index, fee_index) DO UPDATE SET
            token             = EXCLUDED.token,
            amount            = EXCLUDED.amount,
            derive_generation = EXCLUDED.derive_generation
          WHERE defindex_fees.derive_generation <= EXCLUDED.derive_generation
    `
	_, err := s.db.ExecContext(ctx, q,
		int(e.Ledger), e.LedgerCloseTime.UTC(), e.TxHash, int(e.OpIndex), int(e.EventIndex), int(e.FeeIndex),
		e.ContractID, e.Token, e.Amount,
		s.deriveGeneration,
	)
	if err != nil {
		return fmt.Errorf("timescale: InsertDefindexFee %s@%d: %w", e.TxHash, e.Ledger, err)
	}
	return nil
}
