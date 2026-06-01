package timescale

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/lib/pq"
)

// DefindexLayer discriminates which protocol layer emitted the
// flow. String values match the defindex_flows.layer CHECK
// constraint (migration 0050).
type DefindexLayer string

const (
	DefindexLayerStrategy DefindexLayer = "strategy"
	DefindexLayerVault    DefindexLayer = "vault"
)

// IsValid reports whether l is one of the two known layers.
func (l DefindexLayer) IsValid() bool {
	switch l {
	case DefindexLayerStrategy, DefindexLayerVault:
		return true
	}
	return false
}

// DefindexDirection discriminates deposit / withdraw. Matches the
// defindex_flows.direction CHECK constraint.
type DefindexDirection string

const (
	DefindexDeposit  DefindexDirection = "deposit"
	DefindexWithdraw DefindexDirection = "withdraw"
)

// IsValid reports whether d is one of the two known directions.
func (d DefindexDirection) IsValid() bool {
	switch d {
	case DefindexDeposit, DefindexWithdraw:
		return true
	}
	return false
}

// DefindexFlow is one defindex_flows row.
//
// Strategy-layer rows: Amount is set (single-asset), AmountsVec is
// nil/empty, DfTokens is empty. Actor is the vault contract
// C-strkey moving capital.
//
// Vault-layer rows: AmountsVec is set (one entry per vault basket
// asset), Amount is empty, DfTokens is set (share-token delta).
// Actor is the end-user G-strkey (or routing C-strkey).
//
// All amount fields are decimal-string numerics per ADR-0003.
type DefindexFlow struct {
	Ledger          uint32
	LedgerCloseTime time.Time
	TxHash          string
	OpIndex         uint32
	ContractID      string

	Layer     DefindexLayer
	Direction DefindexDirection
	Actor     string

	// Strategy-layer only.
	Amount string

	// Vault-layer only.
	AmountsVec []string
	DfTokens   string
}

// InsertDefindexFlow appends one defindex_flows row, idempotent on
// (ledger_close_time, contract_id, ledger, tx_hash, op_index, layer).
func (s *Store) InsertDefindexFlow(ctx context.Context, e DefindexFlow) error {
	if e.TxHash == "" {
		return errors.New("timescale: InsertDefindexFlow: TxHash is empty")
	}
	if e.ContractID == "" {
		return errors.New("timescale: InsertDefindexFlow: ContractID is empty")
	}
	if e.Actor == "" {
		return errors.New("timescale: InsertDefindexFlow: Actor is empty")
	}
	if !e.Layer.IsValid() {
		return fmt.Errorf("timescale: InsertDefindexFlow: invalid Layer %q", e.Layer)
	}
	if !e.Direction.IsValid() {
		return fmt.Errorf("timescale: InsertDefindexFlow: invalid Direction %q", e.Direction)
	}
	switch e.Layer {
	case DefindexLayerStrategy:
		if e.Amount == "" {
			return errors.New("timescale: InsertDefindexFlow: strategy layer requires Amount")
		}
	case DefindexLayerVault:
		if len(e.AmountsVec) == 0 {
			return errors.New("timescale: InsertDefindexFlow: vault layer requires AmountsVec")
		}
		if e.DfTokens == "" {
			return errors.New("timescale: InsertDefindexFlow: vault layer requires DfTokens")
		}
	}

	const q = `
        INSERT INTO defindex_flows (
            ledger, ledger_close_time, tx_hash, op_index,
            contract_id, layer, direction, actor,
            amount, amounts_vec, df_tokens
        ) VALUES (
            $1, $2, $3, $4,
            $5, $6, $7, $8,
            $9, $10, $11
        )
        ON CONFLICT (ledger_close_time, contract_id, ledger, tx_hash, op_index, layer) DO NOTHING
    `
	var amount, dfTokens interface{}
	if e.Amount != "" {
		amount = e.Amount
	}
	if e.DfTokens != "" {
		dfTokens = e.DfTokens
	}
	var amountsVec interface{}
	if len(e.AmountsVec) > 0 {
		amountsVec = pq.Array(e.AmountsVec)
	}
	_, err := s.db.ExecContext(ctx, q,
		int(e.Ledger), e.LedgerCloseTime.UTC(), e.TxHash, int(e.OpIndex),
		e.ContractID, string(e.Layer), string(e.Direction), e.Actor,
		amount, amountsVec, dfTokens,
	)
	if err != nil {
		return fmt.Errorf("timescale: InsertDefindexFlow %s@%d: %w", e.TxHash, e.Ledger, err)
	}
	return nil
}
