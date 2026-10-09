package explorer

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// AssetEntryChangeEntry is one change in GET /v1/assets/{asset_id}/entry-changes.
// Amount is the entry's post-change balance for this asset as a decimal string
// of stroops (ADR-0003); it is absent for roles that hold no balance of it.
type AssetEntryChangeEntry struct {
	Ledger          uint32          `json:"ledger"`
	LedgerCloseTime string          `json:"ledger_close_time"`
	TxHash          string          `json:"tx_hash"`
	OpIndex         int32           `json:"op_index"`
	ChangeIndex     uint32          `json:"change_index"`
	Role            string          `json:"role"`
	EntryType       string          `json:"entry_type"`
	ChangeType      string          `json:"change_type"`
	Changed         []string        `json:"changed"`
	Account         string          `json:"account,omitempty"`
	Amount          string          `json:"amount,omitempty"`
	Entry           json.RawMessage `json:"entry"`
}

// AssetEntryChangesView is the wire response for GET /v1/assets/{asset_id}/entry-changes.
type AssetEntryChangesView struct {
	Asset         string                  `json:"asset"`
	Changes       []AssetEntryChangeEntry `json:"changes"`
	NextCursor    string                  `json:"next_cursor,omitempty"`
	ThroughLedger uint32                  `json:"through_ledger"`
	// LowerBound is true until the derive is verified from the lake's first
	// ledger: older pages may end before the asset's first entry change.
	LowerBound   bool   `json:"lower_bound"`
	CoverageNote string `json:"coverage_note"`
}

// AssetEntryChanges serves GET /v1/assets/{asset_id}/entry-changes: every
// trustline, offer and claimable-balance change that touches one asset,
// newest first, from stellar.asset_entry_changes, ceilinged at the
// ch-entry-history watermark so a page never shows a half-derived ledger.
func (h *Handler) AssetEntryChanges(w http.ResponseWriter, r *http.Request) {
	if h.Reader == nil {
		h.unavailable(w, r)
		return
	}
	limit, ok := h.ParseLimit(w, r, accountMovementsDefaultLimit, accountMovementsMaxLimit)
	if !ok {
		return
	}
	cur, ok := h.parseEntryChangeCursor(w, r)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), explorerReadTimeout)
	defer cancel()
	asset, lookupFailed, ok := h.assetMovementsID(ctx, w, r)
	if !ok {
		return
	}
	// The watermark is the page's ceiling; without it no page is provably whole.
	wm, backfilledThru, err := h.Reader.EntryHistoryCoverage(ctx)
	if err != nil {
		h.assetEntryChangesError(w, r, ctx, err, asset)
		return
	}
	out := AssetEntryChangesView{Asset: asset, Changes: []AssetEntryChangeEntry{}, ThroughLedger: wm}
	if wm > 0 {
		rows, err := h.Reader.AssetEntryChanges(ctx, asset, limit, cur, wm)
		if err != nil {
			h.assetEntryChangesError(w, r, ctx, err, asset)
			return
		}
		for _, row := range rows {
			e, err := assetEntryChangeEntry(row)
			if err != nil {
				h.Logger.Error("explorer AssetEntryChanges bad fields JSON", "err", err, "asset", asset, "ledger", row.Ledger)
				h.WriteProblem(w, r, "https://api.stellarindex.io/errors/internal", "Internal error", http.StatusInternalServerError, "")
				return
			}
			out.Changes = append(out.Changes, e)
		}
		if len(rows) == limit {
			last := rows[len(rows)-1]
			out.NextCursor = fmt.Sprintf("%d.%s.%d.%d.%s", last.Ledger, last.TxHash, last.OpIndex, last.ChangeIndex, last.Role)
		}
	}
	// An unresolved SAC id keys no rows, so the empty page is not an answer.
	out.LowerBound = lookupFailed || backfilledThru == 0
	out.CoverageNote = assetEntryChangesNote(wm, out.LowerBound, lookupFailed)
	h.writeJSONAt(w, out, h.movementsStale(ctx, wm), lookupFailed, time.Time{})
}

func (h *Handler) assetEntryChangesError(w http.ResponseWriter, r *http.Request, ctx context.Context, err error, asset string) {
	if h.ClientAborted(r, err) {
		return
	}
	if retryableColdMiss(ctx, err) {
		h.Logger.Warn("explorer AssetEntryChanges deadline exceeded", "asset", asset)
		h.writeRetryable(w, r, err, "https://api.stellarindex.io/errors/asset-entry-changes-timeout",
			"Asset entry changes timed out")
		return
	}
	h.Logger.Error("explorer AssetEntryChanges failed", "err", err, "asset", asset)
	h.WriteProblem(w, r, "https://api.stellarindex.io/errors/internal", "Internal error", http.StatusInternalServerError, "")
}

func assetEntryChangeEntry(row clickhouse.AssetEntryChange) (AssetEntryChangeEntry, error) {
	entry := json.RawMessage(row.Fields)
	if !json.Valid(entry) {
		return AssetEntryChangeEntry{}, fmt.Errorf("fields is not JSON")
	}
	e := AssetEntryChangeEntry{
		Ledger:          row.Ledger,
		LedgerCloseTime: row.CloseTime.UTC().Format(time.RFC3339),
		TxHash:          row.TxHash,
		OpIndex:         row.OpIndex,
		ChangeIndex:     row.ChangeIndex,
		Role:            row.Role,
		EntryType:       row.EntryType,
		ChangeType:      row.ChangeType,
		Changed:         row.Changed,
		Account:         row.Account,
		Entry:           entry,
	}
	if e.Changed == nil {
		e.Changed = []string{}
	}
	// An offer's buying side and a pool's own row carry no balance of the asset.
	if row.Role != "buying" && row.Role != "pool" && row.Balance != nil {
		e.Amount = row.Balance.String()
	}
	return e, nil
}

func assetEntryChangesNote(wm uint32, lowerBound, lookupFailed bool) string {
	switch {
	case lookupFailed:
		return "the asset's contract could not be resolved to a classic asset; this page may be incomplete"
	case wm == 0:
		return "entry history has not been derived on this deployment; no changes can be shown"
	case lowerBound:
		return fmt.Sprintf("entry changes through ledger %d; the derive is not yet verified from the lake's first ledger, so older history may be missing", wm)
	default:
		return fmt.Sprintf("every trustline, offer and claimable-balance change for this asset through ledger %d", wm)
	}
}

// parseEntryChangeCursor reads "ledger.tx_hash.op_index.change_index.role".
// op_index is -1 for tx-level changes and tx_hash is empty for upgrade-level ones.
func (h *Handler) parseEntryChangeCursor(w http.ResponseWriter, r *http.Request) (clickhouse.AssetEntryChangeCursor, bool) {
	raw := r.URL.Query().Get("cursor")
	if raw == "" {
		return clickhouse.AssetEntryChangeCursor{}, true
	}
	bad := func() (clickhouse.AssetEntryChangeCursor, bool) {
		h.WriteProblem(w, r, "https://api.stellarindex.io/errors/invalid-cursor",
			"Invalid cursor", http.StatusBadRequest,
			"cursor must be an opaque value returned in a prior next_cursor")
		return clickhouse.AssetEntryChangeCursor{}, false
	}
	parts := strings.Split(raw, ".")
	if len(parts) != 5 || parts[4] == "" {
		return bad()
	}
	ledger, err := strconv.ParseUint(parts[0], 10, 32)
	if err != nil || ledger == 0 {
		return bad()
	}
	opIdx, err := strconv.ParseInt(parts[2], 10, 32)
	if err != nil || opIdx < -1 {
		return bad()
	}
	changeIdx, err := strconv.ParseUint(parts[3], 10, 32)
	if err != nil {
		return bad()
	}
	return clickhouse.AssetEntryChangeCursor{
		Ledger: uint32(ledger), TxHash: parts[1], OpIndex: int32(opIdx), ChangeIndex: uint32(changeIdx), Role: parts[4],
	}, true
}
