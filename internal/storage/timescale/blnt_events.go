package timescale

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"
)

// BLNTEvent is one blnt_events row. "" writes SQL NULL for Pool, UserAddress, Asset, Tier and the
// amounts; Amount and Amount2 are decimal i128 strings (never int64); Attributes is the rest of the
// decoded payload.
type BLNTEvent struct {
	ContractID      string
	Ledger          uint32
	LedgerCloseTime time.Time
	TxHash          string
	OpIndex         uint32
	EventIndex      uint32
	TxIndex         *uint32 // 0-based apply position in the ledger; nil writes NULL and never clears a stored value
	ContractRole    string
	EventKind       string
	Pool            string
	UserAddress     string
	Asset           string
	Tier            string
	Amount          string
	Amount2         string
	Attributes      map[string]any
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
	if e.LedgerCloseTime.IsZero() {
		return errors.New("timescale: InsertBLNTEvent: LedgerCloseTime is zero")
	}
	attrs := []byte("{}")
	if len(e.Attributes) > 0 {
		marshaled, err := json.Marshal(e.Attributes)
		if err != nil {
			return fmt.Errorf("timescale: InsertBLNTEvent: marshal attributes: %w", err)
		}
		attrs = marshaled
	}

	var txIndex any
	if e.TxIndex != nil {
		txIndex = int64(*e.TxIndex)
	}

	const q = `
        INSERT INTO blnt_events (
            contract_id, ledger, ledger_close_time, tx_hash, op_index, event_index,
            contract_role, event_kind, pool, user_address, asset, tier,
            amount, amount2, attributes, derive_generation, tx_index
        ) VALUES (
            $1, $2, $3, $4, $5, $6,
            $7, $8, $9, $10, $11, $12,
            $13, $14, $15, $16, $17
        )
        ON CONFLICT (ledger_close_time, contract_id, ledger, tx_hash,
                     op_index, event_index) DO UPDATE SET
            contract_role     = EXCLUDED.contract_role,
            event_kind        = EXCLUDED.event_kind,
            pool              = EXCLUDED.pool,
            user_address      = EXCLUDED.user_address,
            asset             = EXCLUDED.asset,
            tier              = EXCLUDED.tier,
            amount            = EXCLUDED.amount,
            amount2           = EXCLUDED.amount2,
            attributes        = EXCLUDED.attributes,
            derive_generation = EXCLUDED.derive_generation,
            tx_index          = COALESCE(EXCLUDED.tx_index, blnt_events.tx_index)
          WHERE blnt_events.derive_generation <= EXCLUDED.derive_generation
    `
	_, err := s.db.ExecContext(ctx, q,
		e.ContractID, int(e.Ledger), e.LedgerCloseTime.UTC(), e.TxHash,
		int(e.OpIndex), int(e.EventIndex), e.ContractRole, e.EventKind,
		nullString(e.Pool), nullString(e.UserAddress), nullString(e.Asset), nullString(e.Tier),
		nullNumeric(e.Amount), nullNumeric(e.Amount2), attrs, s.deriveGeneration, txIndex,
	)
	if err != nil {
		return fmt.Errorf("timescale: InsertBLNTEvent %s@%d: %w", e.ContractID, e.Ledger, err)
	}
	return nil
}
