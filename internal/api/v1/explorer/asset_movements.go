package explorer

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// AssetMovementEntry is one movement in GET /v1/assets/{asset_id}/movements.
// Amount is a decimal string (ADR-0003); an absent from/to is a side the
// decoder does not attribute.
type AssetMovementEntry struct {
	Ledger          uint32         `json:"ledger"`
	LedgerCloseTime string         `json:"ledger_close_time"`
	TxHash          string         `json:"tx_hash"`
	OpIndex         uint32         `json:"op_index"`
	LegIndex        uint32         `json:"leg_index"`
	MovementKind    string         `json:"movement_kind"`
	From            string         `json:"from,omitempty"`
	To              string         `json:"to,omitempty"`
	Amount          string         `json:"amount"`
	Decimals        *int           `json:"decimals,omitempty"`
	Provenance      string         `json:"provenance"`
	Attributes      map[string]any `json:"attributes,omitempty"`
}

// AssetMovementsView is the wire response for GET /v1/assets/{asset_id}/movements.
type AssetMovementsView struct {
	Asset      string               `json:"asset"`
	Movements  []AssetMovementEntry `json:"movements"`
	NextCursor string               `json:"next_cursor,omitempty"`
	// ThroughLedger is the newest ledger this feed may serve (the movement
	// archive's derive watermark, or the pre-P23 boundary without one).
	ThroughLedger uint32 `json:"through_ledger"`
	// LowerBound is true while the asset-keyed copy's history backfill is
	// unverified: older pages may end before the asset's first movement.
	LowerBound   bool   `json:"lower_bound"`
	CoverageNote string `json:"coverage_note"`
}

// AssetMovements serves GET /v1/assets/{asset_id}/movements: every movement
// of one asset, newest first, from stellar.movements_by_asset (account_movements
// re-keyed asset-first). One ClickHouse arm, ceilinged at the cap67 watermark
// like the account feed's archive arm, so the ledgers it covers are the ones
// the archive has finished deriving.
func (h *Handler) AssetMovements(w http.ResponseWriter, r *http.Request) {
	if h.Reader == nil {
		h.unavailable(w, r)
		return
	}
	limit, ok := h.ParseLimit(w, r, accountMovementsDefaultLimit, accountMovementsMaxLimit)
	if !ok {
		return
	}
	cur, ok := h.parseMovementCursor(w, r)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), explorerReadTimeout)
	defer cancel()
	asset, lookupFailed, ok := h.assetMovementsID(ctx, w, r)
	if !ok {
		return
	}
	wm, wmFailed, ok := h.movementsWatermark(ctx, w, r, cur)
	if !ok {
		return
	}
	ceiling, _ := movementsSplit(wm)
	chCur := clickhouse.AccountMovementCursor{Ledger: cur.Ledger, TxHash: cur.TxHash, OpIndex: cur.OpIndex, LegIndex: cur.LegIndex}
	rows, err := h.Reader.AssetMovements(ctx, asset, limit, chCur, ceiling)
	if err != nil {
		if h.ClientAborted(r, err) {
			return
		}
		if retryableColdMiss(ctx, err) {
			h.Logger.Warn("explorer AssetMovements deadline exceeded", "asset", asset)
			h.writeRetryable(w, r, err, "https://api.stellarindex.io/errors/asset-movements-timeout",
				"Asset movements timed out")
			return
		}
		h.Logger.Error("explorer AssetMovements failed", "err", err, "asset", asset)
		h.WriteProblem(w, r, "https://api.stellarindex.io/errors/internal", "Internal error", http.StatusInternalServerError, "")
		return
	}
	lowerBound, markerFailed := h.assetMovementsLowerBound(ctx)
	// An unresolved SAC id keys no rows, so the empty page is not an answer.
	lowerBound = lowerBound || lookupFailed
	out := AssetMovementsView{
		Asset:         asset,
		Movements:     h.assetMovementEntries(ctx, asset, rows),
		ThroughLedger: ceiling,
		LowerBound:    lowerBound,
		CoverageNote:  assetMovementsNote(wm, ceiling, lowerBound, wmFailed, h.movementsSupplyRange(ctx, wm)),
	}
	if len(rows) == limit {
		last := rows[len(rows)-1]
		out.NextCursor = fmt.Sprintf("%d.%s.%d.%d", last.Ledger, last.TxHash, last.OpIndex, last.LegIndex)
	}
	h.writeJSONAt(w, out, h.movementsStale(ctx, wm), wmFailed || markerFailed || lookupFailed, time.Time{})
}

// assetMovementsID folds the path asset to the one id the movement tables
// store for it: XLM's alias forms to "native", and a SAC address to the
// classic asset it wraps (cross-checked, so a token claiming a trusted name
// stays its own contract id). Off-chain ids have no movements: 400.
func (h *Handler) assetMovementsID(ctx context.Context, w http.ResponseWriter, r *http.Request) (asset string, lookupFailed, ok bool) {
	raw := r.PathValue("asset_id")
	parsed, err := canonical.ParseAsset(raw)
	if err == nil && isNativeHoldersAsset(parsed) {
		return canonical.NativeAsset().String(), false, true
	}
	if err == nil {
		switch parsed.Type {
		case canonical.AssetClassic:
			return parsed.String(), false, true
		case canonical.AssetSoroban:
			asset, failed := h.resolveSEP41MovementAssetChecked(ctx, parsed.ContractID)
			return asset, failed, true
		case canonical.AssetNative, canonical.AssetCrypto, canonical.AssetFiat, canonical.AssetRWA, canonical.AssetOracleRaw:
			// Native was folded above; the rest are off-chain and never move on the ledger.
		}
	}
	h.WriteProblem(w, r, "https://api.stellarindex.io/errors/invalid-asset-id", "Invalid asset", http.StatusBadRequest,
		"asset_id must be an on-chain asset: 'native', 'CODE-ISSUER' or a C… contract id; got "+raw)
	return "", false, false
}

// assetMovementsLowerBound reports whether the history copy is unverified.
// An unreadable marker counts as unverified and as a degraded read.
func (h *Handler) assetMovementsLowerBound(ctx context.Context) (lowerBound, failed bool) {
	thru, err := h.Reader.AssetMovementsBackfilledThru(ctx)
	if err != nil {
		h.Logger.Warn("movements_by_asset backfill marker read failed — serving as a lower bound", "err", err)
		return true, true
	}
	return thru == 0, false
}

// assetMovementsNote states what the feed covers: the archive span, the
// backfill caveat while it is a lower bound, and the kinds it serves.
func assetMovementsNote(wm, ceiling uint32, lowerBound, wmFailed bool, supply supplyRange) string {
	note := fmt.Sprintf("movements through ledger %d from the movement archive derived from the CAP-67 event lake "+
		"(derive progress, not a verified completeness verdict); newer movements appear as the archive follows the tip", ceiling)
	if wm == 0 && wmFailed {
		note = fmt.Sprintf("the movement archive coverage read failed, so movements are served only through ledger %d "+
			"(2025-09-03, P23)", ceiling)
	} else if wm == 0 {
		note = fmt.Sprintf("this deployment has no post-P23 movement archive, so movements end at ledger %d "+
			"(2025-09-03, P23)", ceiling)
	}
	if lowerBound {
		note += "; the asset-keyed copy of the archive is still being backfilled, so movements from before it " +
			"began capturing may be missing and older pages can end early (a lower bound); " +
			"/accounts/{g}/movements serves each account's full archive"
	}
	return note + "; " + movementsKindGapNote(supply)
}

// assetMovementEntries renders the page; one asset means one decimals lookup.
func (h *Handler) assetMovementEntries(ctx context.Context, asset string, rows []clickhouse.AssetMovementRow) []AssetMovementEntry {
	out := make([]AssetMovementEntry, len(rows))
	if len(rows) == 0 {
		return out
	}
	dctx, cancel := context.WithTimeout(ctx, movementDecimalsBudget)
	defer cancel()
	decimals := h.movementAssetDecimals(dctx, asset)
	for i, m := range rows {
		amt := "0"
		if m.Amount != nil {
			amt = m.Amount.String()
		}
		out[i] = AssetMovementEntry{
			Ledger:          m.Ledger,
			LedgerCloseTime: m.LedgerCloseTime.UTC().Format(time.RFC3339),
			TxHash:          m.TxHash,
			OpIndex:         m.OpIndex,
			LegIndex:        m.LegIndex,
			MovementKind:    m.MovementKind,
			From:            m.From,
			To:              m.To,
			Amount:          amt,
			Decimals:        decimals,
			Provenance:      m.Provenance,
			Attributes:      m.Attributes,
		}
	}
	return out
}
