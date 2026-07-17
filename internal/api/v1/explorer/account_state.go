package explorer

import (
	"context"
	"net/http"
	"strconv"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// AccountsListView is the wire response for GET /v1/accounts — accounts ranked
// by total USD value of their holdings.
type AccountsListView struct {
	PricedAssets int                `json:"priced_assets"`
	Accounts     []AccountWealthRow `json:"accounts"`
	// AsOfLedger is the lake watermark this current-state ranking is
	// fresh to (ADR-0041 Decision 4): the highest ledger the ClickHouse
	// lake had captured at serve time. The ranking is a one-pass read
	// over the current-state (ledger_entry_changes) projection, so a
	// wedged sink makes it stale — pairs with `flags.stale`. Omitted
	// when no watermark reader is wired.
	AsOfLedger uint32 `json:"as_of_ledger,omitempty"`
}

type AccountWealthRow struct {
	AccountID string `json:"account_id"`
	USDValue  string `json:"usd_value"`
	// Locked marks a provably unspendable burn address (master weight
	// 0, all thresholds 0, no signers) — the balance is real but no
	// key can ever move it (ACC-1: the SDF burn account).
	Locked bool `json:"locked,omitempty"`
}

// AccountsList serves GET /v1/accounts — the accounts directory ranked
// by total USD wealth: native XLM plus every trustline asset we have a verified
// USD price for. Builds the price map from the verified-currency catalogue
// (the curated set of assets we price), then ranks over the current-state
// projection. Coverage tracks the entry-change capture + backfill.
func (h *Handler) AccountsList(w http.ResponseWriter, r *http.Request) {
	if h.Reader == nil {
		h.unavailable(w, r)
		return
	}
	if !h.PricingEnabled {
		h.WriteProblem(w, r, "https://api.stellarindex.io/errors/unavailable",
			"Pricing unavailable", http.StatusServiceUnavailable, "wealth ranking needs the pricing layer")
		return
	}
	limit, ok := h.ParseLimit(w, r, 100, 500)
	if !ok {
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), explorerReadTimeout)
	defer cancel()

	assets, prices := h.usdPriceMap(ctx)
	ranked, err := h.Reader.AccountsByWealth(ctx, assets, prices, limit)
	if err != nil {
		if h.ClientAborted(r, err) {
			return
		}
		h.Logger.Error("explorer AccountsByWealth failed", "err", err)
		h.WriteProblem(w, r, "https://api.stellarindex.io/errors/internal",
			"Internal error", http.StatusInternalServerError, "")
		return
	}
	// Locked-burn detection (Pass-B ACC-1): the SDF burn address ranked
	// as the richest account — $11.3B of provably unspendable XLM
	// presented as wealth. Badge, don't hide: the balance is real, the
	// spendability isn't.
	ids := make([]string, len(ranked))
	for i, a := range ranked {
		ids[i] = a.AccountID
	}
	locked, lockErr := h.Reader.AccountsUnspendable(ctx, ids)
	if lockErr != nil {
		h.Logger.Warn("accounts unspendable", "err", lockErr)
	}
	wmLedger, stale, _ := h.LakeWatermark(ctx)
	out := AccountsListView{PricedAssets: len(assets), Accounts: make([]AccountWealthRow, len(ranked)), AsOfLedger: wmLedger}
	for i, a := range ranked {
		out.Accounts[i] = AccountWealthRow{
			AccountID: a.AccountID,
			USDValue:  strconv.FormatFloat(a.USD, 'f', 2, 64),
			Locked:    locked[a.AccountID],
		}
	}
	h.WriteJSON(w, out, stale)
}

// usdPriceMap builds parallel (asset, price) arrays for wealth ranking: native
// XLM plus every browseable verified-currency Stellar asset that currently has
// a USD price. "native" is the key for the account entry's XLM balance. Prices
// resolve through LookupUSDPrice, so stablecoins pick up the fiat-proxy
// fallback (USDC/USDT/… → ~$1) rather than dropping out for want of a direct
// fiat:USD market.
func (h *Handler) usdPriceMap(ctx context.Context) (assets []string, prices []float64) {
	for _, key := range h.priceableAssetIDs() {
		asset, err := canonical.ParseAsset(key)
		if err != nil {
			continue
		}
		raw, ok := h.LookupUSDPrice(ctx, asset)
		if !ok {
			continue
		}
		p, perr := strconv.ParseFloat(raw, 64)
		if perr != nil || p <= 0 {
			continue
		}
		assets = append(assets, key)
		prices = append(prices, p)
	}
	return assets, prices
}

// priceableAssetIDs is the de-duplicated set of canonical asset ids we attempt
// to price for wealth ranking: native XLM plus every browseable verified
// currency's Stellar asset (the catalogue can list the same asset under more
// than one currency entry — dedup by id). "native" parses via canonical too.
func (h *Handler) priceableAssetIDs() []string {
	keys := []string{"native"}
	seen := map[string]struct{}{"native": {}}
	if h.VerifiedCurrencies == nil {
		return keys
	}
	for _, vc := range h.VerifiedCurrencies.All() {
		if vc.ReferenceOnly {
			continue
		}
		for _, n := range vc.Issuance {
			if n.AssetID == "" {
				continue
			}
			if _, dup := seen[n.AssetID]; dup {
				continue
			}
			seen[n.AssetID] = struct{}{}
			keys = append(keys, n.AssetID)
		}
	}
	return keys
}

// AccountStateView is the wire response for GET /v1/accounts/{g_strkey}.
// Balances are strings (ADR-0003 — stroop amounts past 2^53 lose precision as
// JSON numbers).
type AccountStateView struct {
	AccountID     string             `json:"account_id"`
	Exists        bool               `json:"exists"`
	Balance       string             `json:"balance,omitempty"`
	SeqNum        string             `json:"seq_num,omitempty"`
	NumSubentries uint32             `json:"num_subentries,omitempty"`
	Flags         uint32             `json:"flags,omitempty"`
	HomeDomain    string             `json:"home_domain,omitempty"`
	Thresholds    *AccountThresholds `json:"thresholds,omitempty"`
	Signers       []AccountSignerV   `json:"signers,omitempty"`
	Trustlines    []TrustlineV       `json:"trustlines,omitempty"`
	Offers        []OfferV           `json:"offers,omitempty"`
	LastLedger    uint32             `json:"last_modified_ledger,omitempty"`
	// AsOfLedger is the lake watermark this state read is fresh to
	// (ADR-0041 Decision 4) — the highest ledger the ClickHouse lake had
	// captured at serve time, NOT the account's last-modified ledger.
	// Omitted when no watermark reader is wired. Pairs with `flags.stale`.
	AsOfLedger uint32 `json:"as_of_ledger,omitempty"`
}

type AccountThresholds struct {
	Master byte `json:"master"`
	Low    byte `json:"low"`
	Med    byte `json:"med"`
	High   byte `json:"high"`
}

type AccountSignerV struct {
	Key    string `json:"key"`
	Weight uint32 `json:"weight"`
}

type TrustlineV struct {
	Asset   string `json:"asset"`
	Balance string `json:"balance"`
	Limit   string `json:"limit"`
	Flags   uint32 `json:"flags"`
}

type OfferV struct {
	OfferID int64  `json:"offer_id"`
	Selling string `json:"selling"`
	Buying  string `json:"buying"`
	Amount  string `json:"amount"`
	PriceN  int32  `json:"price_n"`
	PriceD  int32  `json:"price_d"`
}

// AccountState serves GET /v1/accounts/{g_strkey} — the account's current
// on-chain state reconstructed from the lake: native balance, sequence,
// thresholds, flags, signers, home domain, plus its live trustlines and offers.
// `exists:false` (200, not 404) for an account with no live AccountEntry in the
// captured window — clients distinguish "no such account / not yet captured"
// from a malformed request.
func (h *Handler) AccountState(w http.ResponseWriter, r *http.Request) {
	if h.Reader == nil {
		h.unavailable(w, r)
		return
	}
	g := r.PathValue("g_strkey")
	if !looksLikeStellarAccount(g) {
		h.WriteProblem(w, r, "https://api.stellarindex.io/errors/invalid-account",
			"Invalid account", http.StatusBadRequest, "g_strkey must be a 56-character G-strkey")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), explorerReadTimeout)
	defer cancel()

	st, err := h.Reader.AccountState(ctx, g)
	if err != nil {
		if h.ClientAborted(r, err) {
			return
		}
		h.Logger.Error("explorer AccountState failed", "err", err, "account", g)
		h.WriteProblem(w, r, "https://api.stellarindex.io/errors/internal",
			"Internal error", http.StatusInternalServerError, "")
		return
	}

	wmLedger, stale, _ := h.LakeWatermark(ctx)
	out := AccountStateView{AccountID: g, Exists: st.Exists, AsOfLedger: wmLedger}
	if st.Exists {
		out.Balance = strconv.FormatInt(st.Balance, 10)
		out.SeqNum = strconv.FormatInt(st.SeqNum, 10)
		out.NumSubentries = st.NumSubEntries
		out.Flags = st.Flags
		out.HomeDomain = st.HomeDomain
		out.Thresholds = &AccountThresholds{Master: st.MasterWeight, Low: st.ThreshLow, Med: st.ThreshMed, High: st.ThreshHigh}
		out.LastLedger = st.LastModifiedLedger
		for _, sg := range st.Signers {
			out.Signers = append(out.Signers, AccountSignerV{Key: sg.Key, Weight: sg.Weight})
		}
		for _, t := range st.Trustlines {
			out.Trustlines = append(out.Trustlines, TrustlineV{
				Asset: t.Asset, Balance: strconv.FormatInt(t.Balance, 10),
				Limit: strconv.FormatInt(t.Limit, 10), Flags: t.Flags,
			})
		}
		for _, o := range st.Offers {
			out.Offers = append(out.Offers, OfferV{
				OfferID: o.OfferID, Selling: o.Selling, Buying: o.Buying,
				Amount: strconv.FormatInt(o.Amount, 10), PriceN: o.PriceN, PriceD: o.PriceD,
			})
		}
	}
	h.WriteJSON(w, out, stale)
}

// AssetHoldersView is the wire response for GET /v1/assets/{asset_id}/holders.
type AssetHoldersView struct {
	Asset       string         `json:"asset"`
	HolderCount int64          `json:"holder_count"`
	Holders     []AssetHolderV `json:"holders"`
	// AsOfLedger is the lake watermark this read is fresh to (ADR-0041
	// Decision 4). Omitted when no watermark reader is wired. Pairs with
	// `flags.stale`.
	AsOfLedger uint32 `json:"as_of_ledger,omitempty"`
}

type AssetHolderV struct {
	AccountID string `json:"account_id"`
	Balance   string `json:"balance"`
}

// AssetHolders serves GET /v1/assets/{asset_id}/holders — the top holders
// of an asset by current trustline balance, plus the total holder count.
// asset_id is the canonical form ("CODE-ISSUER" / "native"). Lake-backed
// (ledger_entry_changes trustlines).
func (h *Handler) AssetHolders(w http.ResponseWriter, r *http.Request) {
	if h.Reader == nil {
		h.unavailable(w, r)
		return
	}
	asset := r.PathValue("asset_id")
	if asset == "" {
		h.WriteProblem(w, r, "https://api.stellarindex.io/errors/invalid-asset-id",
			"Invalid asset", http.StatusBadRequest, "asset_id path segment is required")
		return
	}
	// Validate up front (P2/C3-9, audit-2026-07-16): a malformed asset_id
	// otherwise reaches ClickHouse as-is and drives TWO
	// ledger_entries_current FINAL scans before any 400. ParseAsset is the
	// same validator the sibling /v1/pools?asset= (markets.go) enforces —
	// reject genuinely-malformed input here, cheaply.
	if _, err := canonical.ParseAsset(asset); err != nil {
		h.WriteProblem(w, r, "https://api.stellarindex.io/errors/invalid-asset-id",
			"Invalid asset", http.StatusBadRequest,
			"asset_id must be a canonical asset_id (e.g. 'native', 'USDC-G…'); got "+asset+" ("+err.Error()+")")
		return
	}
	limit, ok := h.ParseLimit(w, r, 100, 500)
	if !ok {
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), explorerReadTimeout)
	defer cancel()

	holders, total, err := h.Reader.AssetHolders(ctx, asset, limit)
	if err != nil {
		if h.ClientAborted(r, err) {
			return
		}
		h.Logger.Error("explorer AssetHolders failed", "err", err, "asset", asset)
		h.WriteProblem(w, r, "https://api.stellarindex.io/errors/internal",
			"Internal error", http.StatusInternalServerError, "")
		return
	}

	wmLedger, stale, _ := h.LakeWatermark(ctx)
	out := AssetHoldersView{Asset: asset, HolderCount: total, Holders: make([]AssetHolderV, len(holders)), AsOfLedger: wmLedger}
	for i, hh := range holders {
		out.Holders[i] = AssetHolderV{AccountID: hh.AccountID, Balance: strconv.FormatInt(hh.Balance, 10)}
	}
	h.WriteJSON(w, out, stale)
}

// looksLikeStellarAccount is a cheap shape check for a G-strkey (the real
// validation is the lake lookup). 56 chars, leading 'G'.
func looksLikeStellarAccount(s string) bool {
	if len(s) != 56 || s[0] != 'G' {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		upper := c >= 'A' && c <= 'Z'
		base32Digit := c >= '2' && c <= '7'
		if !upper && !base32Digit {
			return false
		}
	}
	return true
}
