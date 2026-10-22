package explorer

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/stellar/go-stellar-sdk/strkey"

	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// assetEntryTypes are the entry types the asset feed asks the entry index for; offers are left out (an order book
// is not asset history). The index lists trustline changes only, so the feed is a lower bound until the
// claimable-balance and liquidity-pool legs are indexed.
var assetEntryTypes = []string{"trustline", "claimable_balance", "liquidity_pool"}

// assetEntryOfferRoles are the projector roles an offer yields; the feed drops them.
var assetEntryOfferRoles = []string{"selling", "buying"}

// AssetEntryChangeEntry is one change in GET /v1/assets/{asset_id}/entry-changes; Amount is a decimal string of
// stroops (ADR-0003), the entry's balance of the asset after the change, absent on a pool's own row.
type AssetEntryChangeEntry struct {
	Ledger          uint32          `json:"ledger"`
	LedgerCloseTime string          `json:"ledger_close_time"`
	IntraLedgerSeq  uint32          `json:"intra_ledger_seq"`
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

// AssetEntryChangesView is one page of GET /v1/assets/{asset_id}/entry-changes.
type AssetEntryChangesView struct {
	Asset         string                  `json:"asset"`
	Changes       []AssetEntryChangeEntry `json:"changes"`
	NextCursor    string                  `json:"next_cursor,omitempty"`
	ThroughLedger uint32                  `json:"through_ledger"`
	LowerBound    bool                    `json:"lower_bound"`
	CoverageNote  string                  `json:"coverage_note"`
}

// AssetEntryChanges serves GET /v1/assets/{asset_id}/entry-changes: the asset's trustline, claimable-balance and
// liquidity-pool changes newest first, located through the entry index and decoded from the lake on read.
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
	var (
		page     entryPage[clickhouse.AssetEntryChange]
		wm       uint32
		degraded bool
		err      error
	)
	if cur.IsSet() {
		wm, err = h.Reader.EntryIndexWatermark(ctx)
		if err == nil && wm > 0 {
			page, err = h.assetEntryPage(ctx, asset, limit, cur, wm)
		}
	} else {
		page, wm, degraded, err = h.assetEntryFirstPageCached(ctx, asset, limit)
	}
	if err != nil {
		h.assetEntryChangesError(w, r, ctx, err, asset)
		return
	}
	if wm == 0 {
		h.WriteProblem(w, r, "https://api.stellarindex.io/errors/entry-index-not-built", "Entry changes not yet derived",
			http.StatusServiceUnavailable, "the entry index has not been built on this deployment yet; retry later")
		return
	}
	changes, err := assetEntryChangeEntries(page.rows)
	if err != nil {
		h.assetEntryChangesError(w, r, ctx, err, asset)
		return
	}
	partial := page.lowerBound || (cur.IsSet() && cur.Ledger > wm)
	out := AssetEntryChangesView{
		Asset: asset, Changes: changes, NextCursor: page.next, ThroughLedger: wm, LowerBound: true,
		CoverageNote: assetEntryChangesNote(wm, partial, lookupFailed),
	}
	h.writeJSONAt(w, out, degraded || h.movementsStale(ctx, wm), lookupFailed, time.Time{})
}

// assetEntryFirstWalk is a cached first page: the walk at the max limit and the watermark it was walked through.
type assetEntryFirstWalk struct {
	walk entryWalk[clickhouse.AssetEntryChange]
	wm   uint32
}

// assetEntryFirstPageCached serves the first page through the contract-detail SWR cache, walked once at the max
// limit and cut to the request's: a liquid asset's walk outlasts a page view, and the cursor pages stay inline.
func (h *Handler) assetEntryFirstPageCached(ctx context.Context, asset string, limit int) (entryPage[clickhouse.AssetEntryChange], uint32, bool, error) {
	v, _, degraded, err := h.contractDetailCached(ctx, "aec:"+asset, func(rctx context.Context) (any, error) {
		wm, werr := h.Reader.EntryIndexWatermark(rctx)
		if werr != nil || wm == 0 {
			return assetEntryFirstWalk{}, werr
		}
		walk, werr := walkEntryRows(rctx, h.assetEntrySource(asset), accountMovementsMaxLimit, clickhouse.AssetEntryChangeCursor{}, wm)
		if werr != nil {
			return nil, werr
		}
		return assetEntryFirstWalk{walk: walk, wm: wm}, nil
	})
	if err != nil {
		return entryPage[clickhouse.AssetEntryChange]{}, 0, false, err
	}
	first, _ := v.(assetEntryFirstWalk)
	return first.walk.page(assetEntryKey, limit), first.wm, degraded, nil
}

// assetEntryPage walks the asset's indexed ledgers and keeps the projector's asset rows other than an offer's.
func (h *Handler) assetEntryPage(ctx context.Context, asset string, limit int, cur clickhouse.AssetEntryChangeCursor, wm uint32) (entryPage[clickhouse.AssetEntryChange], error) {
	return walkEntryPage(ctx, h.assetEntrySource(asset), limit, cur, wm)
}

func (h *Handler) assetEntrySource(asset string) entryPageSource[clickhouse.AssetEntryChange] {
	return entryPageSource[clickhouse.AssetEntryChange]{
		ledgers: func(ctx context.Context, ceiling uint32) ([]uint32, error) {
			return h.Reader.AssetEntryLedgers(ctx, asset, assetEntryTypes, ceiling, entryLedgerBatch)
		},
		stream: assetEntryStream(h.Reader, asset),
		keep: func(out []clickhouse.AssetEntryChange, _ []clickhouse.AccountEntryChange, rows []clickhouse.AssetEntryChange) []clickhouse.AssetEntryChange {
			for _, a := range rows {
				if a.Asset == asset && !slices.Contains(assetEntryOfferRoles, a.Role) {
					out = append(out, a)
				}
			}
			return out
		},
		key: assetEntryKey,
	}
}

// assetEntryStream narrows the stream to the groups carrying the asset's raw issuer key or pool id: every entry an
// asset row comes from holds one of them in its key or body, so the kept rows are unchanged.
func assetEntryStream(r ExplorerReader, asset string) func(context.Context, []uint32, func(clickhouse.EntryHistorySourceRow) error) error {
	raw, ok := assetRawKey(asset)
	if !ok {
		return r.StreamEntryChangesForLedgers
	}
	return func(ctx context.Context, ledgers []uint32, fn func(clickhouse.EntryHistorySourceRow) error) error {
		return r.StreamAccountEntryChangesForLedgers(ctx, raw, ledgers, fn)
	}
}

// assetRawKey is the 32 bytes every entry of the asset carries: a credit asset's issuer key or a pool share's pool id.
func assetRawKey(asset string) (string, bool) {
	if id, ok := strings.CutPrefix(asset, "pool:"); ok {
		raw, err := hex.DecodeString(id)
		return string(raw), err == nil && len(raw) == 32
	}
	_, issuer, ok := strings.Cut(asset, "-")
	if !ok {
		return "", false
	}
	raw, err := strkey.Decode(strkey.VersionByteAccountID, issuer)
	return string(raw), err == nil
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

func assetEntryChangeEntries(rows []clickhouse.AssetEntryChange) ([]AssetEntryChangeEntry, error) {
	out := make([]AssetEntryChangeEntry, 0, len(rows))
	for _, a := range rows {
		entry := json.RawMessage(a.Fields)
		if !json.Valid(entry) {
			return nil, fmt.Errorf("ledger %d change %s/%d/%d: fields is not JSON", a.Ledger, a.TxHash, a.OpIndex, a.ChangeIndex)
		}
		e := AssetEntryChangeEntry{
			Ledger: a.Ledger, LedgerCloseTime: a.CloseTime.UTC().Format(time.RFC3339), IntraLedgerSeq: a.IntraLedgerSeq,
			TxHash: a.TxHash, OpIndex: a.OpIndex, ChangeIndex: a.ChangeIndex, Role: a.Role, EntryType: a.EntryType,
			ChangeType: a.ChangeType, Changed: a.Changed, Account: a.Account, Entry: entry,
		}
		if e.Changed == nil {
			e.Changed = []string{}
		}
		if a.Balance != nil {
			e.Amount = a.Balance.String()
		}
		out = append(out, e)
	}
	return out, nil
}

func assetEntryChangesNote(wm uint32, partial, lookupFailed bool) string {
	note := fmt.Sprintf("trustline, claimable-balance and liquidity-pool changes of this asset, decoded from the ledger-entry-change "+
		"lake through ledger %d; offers are not served here; the entry index lists trustline changes only, so a "+
		"claimable-balance or liquidity-pool change appears only in a ledger where a trustline of the asset also changed; "+
		"ledgers %d to %d carry the earlier intra_ledger_seq numbering", wm, entryChangesOldNumberingFrom, entryChangesOldNumberingThru)
	if lookupFailed {
		note += "; the contract could not be resolved to a classic asset, so this page may be incomplete"
	}
	if partial {
		note += "; this page is partial: follow next_cursor"
	}
	return note
}
