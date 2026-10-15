package explorer

import (
	"context"
	"fmt"
	"net/http"
	"time"
)

// AssetTrustlineDayEntry is one UTC day of GET /v1/assets/{asset_id}/trustline-history; Live is the open trustline
// count at the day's end.
type AssetTrustlineDayEntry struct {
	Date    string `json:"date"`
	Created uint64 `json:"created"`
	Removed uint64 `json:"removed"`
	Live    int64  `json:"live"`
}

// AssetTrustlineHistoryView is GET /v1/assets/{asset_id}/trustline-history.
type AssetTrustlineHistoryView struct {
	Asset         string                   `json:"asset"`
	Days          []AssetTrustlineDayEntry `json:"days"`
	ThroughLedger uint32                   `json:"through_ledger"`
	CoverageNote  string                   `json:"coverage_note"`
}

// AssetTrustlineHistory serves GET /v1/assets/{asset_id}/trustline-history: per UTC day, the asset's trustlines
// created and removed and the open count at day's end, summed from the entry index since genesis.
func (h *Handler) AssetTrustlineHistory(w http.ResponseWriter, r *http.Request) {
	if h.Reader == nil {
		h.unavailable(w, r)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), explorerReadTimeout)
	defer cancel()
	asset, lookupFailed, ok := h.assetMovementsID(ctx, w, r)
	if !ok {
		return
	}
	wm, err := h.Reader.EntryIndexWatermark(ctx)
	if err != nil {
		h.assetTrustlineHistoryError(w, r, ctx, err, asset)
		return
	}
	if wm == 0 {
		h.WriteProblem(w, r, "https://api.stellarindex.io/errors/entry-index-not-built", "Trustline history not yet derived",
			http.StatusServiceUnavailable, "the entry index has not been built on this deployment yet; retry later")
		return
	}
	days, err := h.Reader.AssetTrustlineDaily(ctx, asset, wm)
	if err != nil {
		h.assetTrustlineHistoryError(w, r, ctx, err, asset)
		return
	}
	out := AssetTrustlineHistoryView{Asset: asset, Days: make([]AssetTrustlineDayEntry, 0, len(days)), ThroughLedger: wm}
	var live int64
	for _, d := range days {
		live += int64(d.Created) - int64(d.Removed)
		out.Days = append(out.Days, AssetTrustlineDayEntry{
			Date: d.Day.UTC().Format(time.DateOnly), Created: d.Created, Removed: d.Removed, Live: live,
		})
	}
	out.CoverageNote = fmt.Sprintf("trustlines of this asset created and removed per UTC day, from genesis through ledger %d; "+
		"live is the open count at the day's end; days with no change are omitted; native XLM and contract-only tokens "+
		"have no trustlines", wm)
	if lookupFailed {
		out.CoverageNote += "; the contract could not be resolved to a classic asset, so this history may be empty"
	}
	h.writeJSONAt(w, out, h.movementsStale(ctx, wm), lookupFailed, time.Time{})
}

func (h *Handler) assetTrustlineHistoryError(w http.ResponseWriter, r *http.Request, ctx context.Context, err error, asset string) {
	if h.ClientAborted(r, err) {
		return
	}
	if retryableColdMiss(ctx, err) {
		h.Logger.Warn("explorer AssetTrustlineHistory deadline exceeded", "asset", asset)
		h.writeRetryable(w, r, err, "https://api.stellarindex.io/errors/asset-trustline-history-timeout",
			"Asset trustline history timed out")
		return
	}
	h.Logger.Error("explorer AssetTrustlineHistory failed", "err", err, "asset", asset)
	h.WriteProblem(w, r, "https://api.stellarindex.io/errors/internal", "Internal error", http.StatusInternalServerError, "")
}
