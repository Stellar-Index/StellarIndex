package timescale

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/Stellar-Index/StellarIndex/internal/domain"
)

// toAssetAmountRows converts a V1 new_liquidation_auction's decoded
// bid/lot into blendAssetAmountRow (blend_auctions.go, same package —
// the JSONB {asset, amount} element shape) for the blend_admin.attributes
// column. amount is stringified for full i128 precision (ADR-0003).
func toAssetAmountRows(in []domain.BlendAssetAmount) []blendAssetAmountRow {
	if len(in) == 0 {
		return nil
	}
	out := make([]blendAssetAmountRow, len(in))
	for i, a := range in {
		out[i] = blendAssetAmountRow{Asset: a.Asset, Amount: bigIntOrEmpty(a.Amount)}
	}
	return out
}

// InsertBlendAdminEvent appends one Blend admin / pool-config /
// pool-factory lifecycle event (set_admin / update_pool /
// queue_set_reserve / cancel_set_reserve / set_reserve / set_status
// / deploy) to the blend_admin hypertable. Idempotent on the PK
// (contract_id, ledger, tx_hash, op_index, event_kind,
// ledger_close_time).
//
// Promoted typed columns: admin / asset / target — populated when
// the event kind carries them; NULL otherwise (see per-kind doc
// in migration 0042 + blend.AdminEvent godoc). Event-type-specific
// remainder (update_pool body, queue_set_reserve ReserveConfig,
// set_reserve index, set_status status+by_admin) lands in the
// attributes jsonb column.
//
// i128 amounts (update_pool.min_collateral, ReserveConfig.supply_cap)
// are decimal strings inside jsonb per ADR-0003 — NUMERIC inside
// jsonb is lossy, but a decimal string round-trips at full
// precision.
func (s *Store) InsertBlendAdminEvent(ctx context.Context, e domain.BlendAdminEvent) error {
	if e.ContractID == "" {
		return errors.New("timescale: InsertBlendAdminEvent: ContractID is empty")
	}
	if e.TxHash == "" {
		return errors.New("timescale: InsertBlendAdminEvent: TxHash is empty")
	}
	if !isBlendAdminKind(e.Kind) {
		return fmt.Errorf("timescale: InsertBlendAdminEvent: invalid Kind %q", e.Kind)
	}

	attrs := buildAdminAttributes(e)
	attrsJSON, err := json.Marshal(attrs)
	if err != nil {
		return fmt.Errorf("timescale: InsertBlendAdminEvent: marshal attributes: %w", err)
	}

	// INV-3 generation-guarded corrective upsert (migration 0110): a
	// corrected re-derive of the decoded columns (admin / asset / target and
	// the attributes jsonb, which carries i128 amounts like
	// min_collateral / supply_cap) lands in place when its generation is >=
	// the stored one; a live gen-0 replay can never revert it. Replaces DO NOTHING.
	const q = `
        INSERT INTO blend_admin (
            contract_id, ledger, tx_hash, op_index, event_index, ledger_close_time,
            event_kind, admin, asset, target,
            attributes, derive_generation
        ) VALUES (
            $1, $2, $3, $4, $5, $6,
            $7, $8, $9, $10,
            $11, $12
        )
        ON CONFLICT (contract_id, ledger, tx_hash, op_index, event_kind, event_index, ledger_close_time) DO UPDATE SET
            admin             = EXCLUDED.admin,
            asset             = EXCLUDED.asset,
            target            = EXCLUDED.target,
            attributes        = EXCLUDED.attributes,
            derive_generation = EXCLUDED.derive_generation
          WHERE blend_admin.derive_generation <= EXCLUDED.derive_generation
    `
	_, err = s.db.ExecContext(ctx, q,
		e.ContractID, int(e.Ledger), e.TxHash, int(e.OpIndex), int(e.EventIndex), e.Timestamp.UTC(),
		e.Kind,
		nullString(e.Admin), nullString(e.Asset), nullString(e.Target),
		attrsJSON, s.deriveGeneration,
	)
	if err != nil {
		return fmt.Errorf("timescale: InsertBlendAdminEvent %s@%d: %w", e.ContractID, e.Ledger, err)
	}
	return nil
}

// buildAdminAttributes builds the jsonb payload per event kind.
func buildAdminAttributes(e domain.BlendAdminEvent) map[string]any {
	attrs := map[string]any{}
	switch e.Kind {
	case domain.BlendEventUpdatePool:
		attrs["backstop_take_rate"] = e.BackstopTakeRate
		attrs["max_positions"] = e.MaxPositions
		attrs["min_collateral"] = bigIntOrEmpty(e.MinCollateral)
	case domain.BlendEventQueueSetReserve:
		if e.ReserveConfig != nil {
			attrs["metadata"] = e.ReserveConfig
		}
	case domain.BlendEventSetReserve:
		attrs["index"] = e.ReserveIndex
	case domain.BlendEventSetStatus:
		attrs["status"] = e.NewStatus
		attrs["by_admin"] = e.ByAdmin
	case domain.BlendEventNewLiquidationAuction:
		if len(e.AuctionBid) > 0 {
			attrs["bid"] = toAssetAmountRows(e.AuctionBid)
		}
		if len(e.AuctionLot) > 0 {
			attrs["lot"] = toAssetAmountRows(e.AuctionLot)
		}
		attrs["block"] = e.AuctionBlock
	}
	return attrs
}

// isBlendAdminKind reports whether kind is one of the admin event
// kinds (including the pool-factory `deploy` and the V1-pool-factory
// liquidation-auction pair). Mirrors the CHECK constraint in
// migration 0042, widened by migration 0097.
func isBlendAdminKind(kind string) bool {
	switch kind {
	case domain.BlendEventSetAdmin,
		domain.BlendEventUpdatePool,
		domain.BlendEventQueueSetReserve,
		domain.BlendEventCancelSetReserve,
		domain.BlendEventSetReserve,
		domain.BlendEventSetStatus,
		domain.BlendEventDeploy,
		domain.BlendEventNewLiquidationAuction,
		domain.BlendEventDeleteLiquidationAuction:
		return true
	}
	return false
}
