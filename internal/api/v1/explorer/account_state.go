package explorer

import (
	"context"
	"math/big"
	"net/http"
	"strconv"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/worker"
)

// AccountsListView is GET /v1/accounts; `ranked_by` says whether ranking is total USD value or native XLM balance (lean networks with no price map).
type AccountsListView struct {
	PricedAssets int `json:"priced_assets"`
	// RankedBy is the wealth basis: "usd" (holdings valued in USD) or
	// "native_xlm" (ranked by native XLM balance, used where no USD pricing
	// is available). Each row's Value is expressed in this unit.
	RankedBy string             `json:"ranked_by"`
	Accounts []AccountWealthRow `json:"accounts"`
	// AsOfLedger is the snapshot's vintage (ADR-0041 Decision 4): the lake watermark read just
	// before the scan, so it never names a ledger later than the data ranked. Omitted when unreadable.
	AsOfLedger uint32 `json:"as_of_ledger,omitempty"`
	// LowerBound is always true: each Value counts only the holding domains
	// CoverageNote names, so it can under-state an account, never over-state it.
	LowerBound   bool   `json:"lower_bound"`
	CoverageNote string `json:"coverage_note"`
}

// accountsWealthCoverageNote names what the wealth ranking excludes.
const accountsWealthCoverageNote = "Ranked on classic holdings only: the account entry's native XLM, " +
	"plus, on the usd basis, trustline balances of USD-priced assets. Claimable balances, " +
	"liquidity-pool shares and Soroban contract balances (including SAC balances) are not counted."

type AccountWealthRow struct {
	AccountID string `json:"account_id"`
	// Value is the ranked wealth in the response's RankedBy unit: USD dollars ("usd", 2dp) or
	// whole XLM ("native_xlm", exact to the stroop from the integer balance, no float).
	Value string `json:"value"`
	// USDValue is a backward-compatible alias, populated only on the "usd"
	// basis (equal to Value there). Empty on the "native_xlm" basis — read
	// Value instead. Retained so existing USD-network clients keep working.
	USDValue string `json:"usd_value,omitempty"`
	// Locked marks a provably unspendable burn address (master weight
	// 0, all thresholds 0, no signers) — the balance is real but no
	// key can ever move it (the SDF burn account).
	Locked bool `json:"locked,omitempty"`
}

// AccountsList serves GET /v1/accounts, ranked by total USD wealth over native XLM plus trustline assets with a verified USD price.
func (h *Handler) AccountsList(w http.ResponseWriter, r *http.Request) {
	if h.Reader == nil {
		h.unavailable(w, r)
		return
	}
	limit, ok := h.ParseLimit(w, r, 100, 500)
	if !ok {
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), explorerReadTimeout)
	defer cancel()

	// With no USD price map (lean networks run no aggregator) rank by native XLM balance rather
	// than 503ing or serving an empty list. That fallback is the single key "native" priced at 1;
	// the cache records the basis (wealthBasis) so the served numbers are labelled correctly.
	assets, prices := h.wealthRankingInputs(ctx)
	// Served from a background-refreshed cache: the FINAL scan over tens of millions of
	// current-state rows takes 11-20s and can never fit this handler's deadline.
	snap, warm := h.Reader.AccountsByWealthCached(ctx, assets, prices, limit)
	if !warm {
		// Degraded state instead of a 500, reached ONLY when no ranking has ever been computed
		// this process; an expired snapshot is served with flags.stale below, never 503'd.
		h.WriteProblem(w, r, "https://api.stellarindex.io/errors/warming-up",
			"Ranking not ready", http.StatusServiceUnavailable,
			"the account wealth ranking is being computed; retry shortly")
		return
	}
	// Degraded when the snapshot has outlived its refresh contract (the
	// background refresher is failing/behind) — the data is real, the flag +
	// the envelope's as_of (stamped from the snapshot) say how old.
	snapshotStale := time.Since(snap.AsOf) > clickhouse.AccountsWealthCacheTTL
	// The SDF burn address would rank as the richest account with provably unspendable XLM, so
	// it is badged rather than hidden. The flag is resolved by the background refresh and cached
	// on each row (a.Locked); AccountsUnspendable is a FINAL scan too slow to run per request.
	// as_of_ledger is the snapshot's own vintage; the serve-time watermark read contributes only
	// the lake-wedged `stale` signal.
	_, stale, _ := h.LakeWatermark(ctx)
	out := AccountsListView{
		RankedBy: snap.Basis, Accounts: make([]AccountWealthRow, len(snap.Rows)), AsOfLedger: snap.AsOfLedger,
		LowerBound: true, CoverageNote: accountsWealthCoverageNote,
	}
	// priced_assets is a USD-basis notion; on the native-XLM basis nothing is
	// USD-priced, so report 0 rather than the "native" fallback key count.
	if snap.Basis != clickhouse.WealthBasisNative {
		out.PricedAssets = len(assets)
	}
	for i, a := range snap.Rows {
		row := AccountWealthRow{AccountID: a.AccountID, Locked: a.Locked}
		if snap.Basis == clickhouse.WealthBasisNative {
			row.Value = stroops7(a.NativeStroops.BigInt())
		} else {
			row.Value = formatUSD(a.Value)
			row.USDValue = row.Value // back-compat alias, USD basis only
		}
		out.Accounts[i] = row
	}
	h.writeJSONAt(w, out, stale || snapshotStale, snapshotStale, snap.AsOf)
}

// usdPriceMap builds parallel (asset, price) arrays for wealth ranking: native XLM (key "native")
// plus every browseable verified-currency asset with a USD price. Prices resolve through
// LookupUSDPrice, so stablecoins pick up the fiat-proxy fallback (~$1) rather than dropping out
// for want of a direct fiat:USD market.
// usdPriceMapTTL caches that set. usdPriceMap makes SEQUENTIAL LookupUSDPrice calls (~8s), so the
// TTL is deliberately above the 5-minute PrewarmAccountsWealth cadence: the entry is refreshed
// before it expires and requests never pay the walk. 10 minutes stale is fine for a ranking
// cached for 15.
const usdPriceMapTTL = 10 * time.Minute

// usdPriceMapEmptyTTL is shorter so a pricing outage clears quickly, yet
// still stops every request from re-running the walk while it lasts.
const usdPriceMapEmptyTTL = 30 * time.Second

// usdPriceMapFillTimeout bounds the shared walk, which no caller can cancel.
const usdPriceMapFillTimeout = time.Minute

type usdPriceMapEntry struct {
	assets   []string
	prices   []string
	cachedAt time.Time
}

func (e usdPriceMapEntry) fresh(now time.Time) bool {
	ttl := usdPriceMapTTL
	if len(e.assets) == 0 {
		ttl = usdPriceMapEmptyTTL
	}
	return !e.cachedAt.IsZero() && now.Sub(e.cachedAt) < ttl
}

var (
	usdPriceMapMu     sync.Mutex
	usdPriceMapCache  usdPriceMapEntry
	usdPriceMapFlight singleflight.Group
)

func loadUSDPriceMapCache() usdPriceMapEntry {
	usdPriceMapMu.Lock()
	defer usdPriceMapMu.Unlock()
	return usdPriceMapCache
}

// usdPriceMap builds parallel (asset, price) arrays for wealth ranking,
// memoised for [usdPriceMapTTL] with one walk in flight at a time.
func (h *Handler) usdPriceMap(ctx context.Context) (assets, prices []string) {
	last := loadUSDPriceMapCache()
	if last.fresh(time.Now()) {
		return last.assets, last.prices
	}
	//nolint:contextcheck // the walk is shared by every waiter, so no single caller's cancellation may truncate it
	ch := usdPriceMapFlight.DoChan("", func() (val any, err error) {
		// singleflight re-raises a fill panic on a goroutine nothing can
		// recover, so it would kill the process; fail the flight instead.
		defer func() {
			if rec := recover(); rec != nil {
				worker.Report(h.Logger, "explorer-usd-price-map-fill", rec)
				val, err = nil, errRefreshPanicked
			}
		}()
		if c := loadUSDPriceMapCache(); c.fresh(time.Now()) {
			return c, nil
		}
		fillCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), usdPriceMapFillTimeout)
		defer cancel()
		e := usdPriceMapEntry{cachedAt: time.Now()}
		e.assets, e.prices = h.usdPriceMapUncached(fillCtx)
		// A walk cut short by the deadline is partial; serve it but don't cache it.
		if fillCtx.Err() == nil {
			usdPriceMapMu.Lock()
			usdPriceMapCache = e
			usdPriceMapMu.Unlock()
		}
		return e, nil
	})
	select {
	case res := <-ch:
		e, ok := res.Val.(usdPriceMapEntry)
		if res.Err != nil || !ok {
			return last.assets, last.prices
		}
		return e.assets, e.prices
	case <-ctx.Done():
		// The caller's deadline bounds its wait, not the walk: serve the last
		// entry (even expired, or none) and let the walk fill the cache.
		return last.assets, last.prices
	}
}

// wealthRankingInputs returns the (asset, price) arrays to rank wealth by: the USD price map, or
// the native-XLM fallback (key "native" priced at 1) when pricing is disabled or no aggregator
// runs. The request path and the prewarmer both call it so the cache has one consistent basis
// (wealthBasis derives it from these inputs).
func (h *Handler) wealthRankingInputs(ctx context.Context) (assets, prices []string) {
	if h.PricingEnabled {
		if a, p := h.usdPriceMap(ctx); len(a) > 0 {
			return a, p
		}
	}
	return []string{"native"}, []string{"1"}
}

func (h *Handler) usdPriceMapUncached(ctx context.Context) (assets, prices []string) {
	for _, key := range h.priceableAssetIDs() {
		asset, err := canonical.ParseAsset(key)
		if err != nil {
			continue
		}
		raw, ok := h.LookupUSDPrice(ctx, asset)
		if !ok {
			continue
		}
		if p, ok := new(big.Rat).SetString(raw); !ok || p.Sign() <= 0 {
			continue
		}
		assets = append(assets, key)
		prices = append(prices, raw)
	}
	return assets, prices
}

// priceableAssetIDs is the de-duplicated set of canonical asset ids priced for wealth ranking:
// native XLM plus every browseable verified currency's asset (the catalogue can list one asset
// under several currency entries, so dedup by id).
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

// AccountStateView is GET /v1/accounts/{g_strkey}; balances are strings (ADR-0003) since stroop amounts past 2^53 lose precision as JSON numbers.
type AccountStateView struct {
	AccountID string `json:"account_id"`
	Exists    bool   `json:"exists"`
	Balance   string `json:"balance,omitempty"`
	SeqNum    string `json:"seq_num,omitempty"`
	// Pointers so a live account's zero counts and flags are served while an
	// exists:false body stays empty.
	NumSubentries *uint32 `json:"num_subentries,omitempty"`
	Flags         *uint32 `json:"flags,omitempty"`
	NumSponsoring *uint32 `json:"num_sponsoring,omitempty"`
	NumSponsored  *uint32 `json:"num_sponsored,omitempty"`
	// Native XLM liabilities in stroops; selling_liabilities is locked by offers.
	BuyingLiabilities  string             `json:"buying_liabilities,omitempty"`
	SellingLiabilities string             `json:"selling_liabilities,omitempty"`
	HomeDomain         string             `json:"home_domain,omitempty"`
	Thresholds         *AccountThresholds `json:"thresholds,omitempty"`
	Signers            []AccountSignerV   `json:"signers,omitempty"`
	Trustlines         []TrustlineV       `json:"trustlines,omitempty"`
	Offers             []OfferV           `json:"offers,omitempty"`
	LastLedger         uint32             `json:"last_modified_ledger,omitempty"`
	// AsOfLedger is the lake watermark when the state was SCANNED (its cache vintage), not a
	// serve-time read: the state can be up to AccountStateCacheTTL old, so a serve-time watermark
	// could name a ledger the cached rows never saw. Omitted when unreadable at fill time.
	AsOfLedger uint32 `json:"as_of_ledger,omitempty"`
	// Directory is the curated third-party label for this address
	// (directory.go) — display attribution, not verification. Omitted
	// when the address isn't listed or no directory reader is wired.
	Directory *DirectoryInfoV `json:"directory,omitempty"`
	// CoverageNote declares the holding domains this view serves, so a
	// missing claimable or contract balance is not read as a zero one.
	CoverageNote string `json:"coverage_note,omitempty"`
	// DirectoryUnavailable is true when the directory read failed, so an
	// absent `directory` means "not checked", not "not listed".
	DirectoryUnavailable bool `json:"directory_unavailable,omitempty"`
}

// accountStateCoverageNote is the holding scope of a live account's view.
const accountStateCoverageNote = "Classic holdings only: native XLM, trustlines (liquidity-pool " +
	"shares carry kind \"pool_share\") and offers. Claimable balances and Soroban contract " +
	"balances (including SAC balances) are not included."

// Trustline kinds: a single-asset balance, or a liquidity-pool share.
const (
	trustlineKindAsset     = "asset"
	trustlineKindPoolShare = "pool_share"
)

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
	Asset string `json:"asset"`
	// Kind is "asset" or "pool_share" (asset "pool:<hex>", a claim on a
	// pool's two reserves rather than a balance of one asset).
	Kind               string `json:"kind"`
	Balance            string `json:"balance"`
	Limit              string `json:"limit"`
	Flags              uint32 `json:"flags"`
	BuyingLiabilities  string `json:"buying_liabilities"`
	SellingLiabilities string `json:"selling_liabilities"`
}

type OfferV struct {
	OfferID int64  `json:"offer_id"`
	Selling string `json:"selling"`
	Buying  string `json:"buying"`
	Amount  string `json:"amount"`
	PriceN  int32  `json:"price_n"`
	PriceD  int32  `json:"price_d"`
}

// AccountState serves GET /v1/accounts/{g_strkey} from the lake; an account with no live AccountEntry gets `exists:false` (200, not 404), distinguishing "not captured" from a malformed request.
func (h *Handler) AccountState(w http.ResponseWriter, r *http.Request) {
	if h.Reader == nil {
		h.unavailable(w, r)
		return
	}
	// Checksum-valid strkey, as on every sibling account endpoint. A shape-only check would send a
	// corrupted-checksum G-strkey to the lake read, which 500s instead of 400ing up front.
	g, ok := h.parseAccountStrkey(w, r)
	if !ok {
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), explorerReadTimeout)
	defer cancel()

	st, snapStale, err := h.Reader.AccountStateCached(ctx, g)
	if err != nil {
		if h.ClientAborted(r, err) {
			return
		}
		if retryableColdMiss(ctx, err) {
			// The read blew explorerReadTimeout or the shared detached-refresh gate was saturated
			// (clickhouse.ErrRefreshSaturated). Both are retryable, so 503 rather than a 500.
			h.Logger.Warn("explorer AccountState deadline/saturation", "account", g, "err", err)
			h.writeRetryable(w, r, err, "https://api.stellarindex.io/errors/account-state-timeout",
				"Account state timed out")
			return
		}
		h.Logger.Error("explorer AccountState failed", "err", err, "account", g)
		h.WriteProblem(w, r, "https://api.stellarindex.io/errors/internal",
			"Internal error", http.StatusInternalServerError, "")
		return
	}

	// as_of_ledger is the cached state's own vintage (stamped at fill time, before its scan), not a
	// serve-time read, which could name a ledger the cached rows never saw. The serve-time
	// watermark still feeds flags.stale, which is about lake health.
	_, stale, _ := h.LakeWatermark(ctx)
	out := AccountStateView{AccountID: g, Exists: st.Exists, AsOfLedger: st.AsOfLedger}
	// Directory labels apply regardless of Exists — a listed address
	// whose AccountEntry predates the captured window (or was merged
	// away) is exactly where a label helps most.
	var dirOK bool
	out.Directory, dirOK = h.directoryFor(ctx, g)
	out.DirectoryUnavailable = !dirOK
	if st.Exists {
		fillAccountStateView(&out, st)
	}
	// snapStale: the served state came from an expired cache entry while a
	// detached refresh runs (whale-account stale-serve) —
	// surfaced on the same flags.stale the watermark uses.
	h.writeJSONAt(w, out, stale || snapStale, out.DirectoryUnavailable, time.Time{})
}

// fillAccountStateView renders a live account's state onto the wire view.
func fillAccountStateView(out *AccountStateView, st clickhouse.AccountState) {
	out.Balance = strconv.FormatInt(st.Balance, 10)
	out.SeqNum = strconv.FormatInt(st.SeqNum, 10)
	out.NumSubentries = &st.NumSubEntries
	out.Flags = &st.Flags
	out.NumSponsoring = &st.NumSponsoring
	out.NumSponsored = &st.NumSponsored
	out.BuyingLiabilities = strconv.FormatInt(st.BuyingLiabilities, 10)
	out.SellingLiabilities = strconv.FormatInt(st.SellingLiabilities, 10)
	out.HomeDomain = st.HomeDomain
	out.Thresholds = &AccountThresholds{Master: st.MasterWeight, Low: st.ThreshLow, Med: st.ThreshMed, High: st.ThreshHigh}
	out.LastLedger = st.LastModifiedLedger
	out.CoverageNote = accountStateCoverageNote
	for _, sg := range st.Signers {
		out.Signers = append(out.Signers, AccountSignerV{Key: sg.Key, Weight: sg.Weight})
	}
	for _, t := range st.Trustlines {
		kind := trustlineKindAsset
		if t.PoolShare {
			kind = trustlineKindPoolShare
		}
		out.Trustlines = append(out.Trustlines, TrustlineV{
			Asset: t.Asset, Kind: kind, Balance: strconv.FormatInt(t.Balance, 10),
			Limit: strconv.FormatInt(t.Limit, 10), Flags: t.Flags,
			BuyingLiabilities:  strconv.FormatInt(t.BuyingLiabilities, 10),
			SellingLiabilities: strconv.FormatInt(t.SellingLiabilities, 10),
		})
	}
	for _, o := range st.Offers {
		out.Offers = append(out.Offers, OfferV{
			OfferID: o.OfferID, Selling: o.Selling, Buying: o.Buying,
			Amount: strconv.FormatInt(o.Amount, 10), PriceN: o.PriceN, PriceD: o.PriceD,
		})
	}
}

// AssetHoldersView is the wire response for GET /v1/assets/{asset_id}/holders.
type AssetHoldersView struct {
	Asset       string         `json:"asset"`
	HolderCount int64          `json:"holder_count"`
	Holders     []AssetHolderV `json:"holders"`
	// AsOfLedger is the board snapshot's vintage (ADR-0041 Decision 4): the
	// lake watermark read immediately before its scan, paired with the
	// envelope's as_of. Omitted when no watermark was available.
	AsOfLedger uint32 `json:"as_of_ledger,omitempty"`
}

type AssetHolderV struct {
	AccountID string `json:"account_id"`
	Balance   string `json:"balance"`
}

// isNativeHoldersAsset reports whether a requested asset form serves the native-XLM holders
// board. Native has no trustlines (every account holds XLM in its AccountEntry), so the board
// comes from the account-balance ranking under the single key "native", and the non-SAC members
// of XLM's alias family (`native`, `crypto:XLM`, "XLM") fold into it: one cache entry, one
// detached flight, one reader arm.
//
// The third member, the SAC wrapper C-address, is deliberately NOT folded in: it names the
// wrapper's own Soroban `Balance(addr)` domain, a different holder set from classic AccountEntry
// balances, and must not be mislabelled as the account board.
func isNativeHoldersAsset(a canonical.Asset) bool {
	if a.Type == canonical.AssetSoroban {
		return false
	}
	for _, alias := range canonical.AssetAliases(a) {
		if alias.Type == canonical.AssetNative {
			return true
		}
	}
	return false
}

// AssetHolders serves GET /v1/assets/{asset_id}/holders: top holders by balance plus total count; native XLM reads the AccountEntry ranking, as a trustline read would give holder_count 0.
func (h *Handler) AssetHolders(w http.ResponseWriter, r *http.Request) {
	if h.Reader == nil {
		h.unavailable(w, r)
		return
	}
	asCSV := NegotiateCSV(w, r)
	asset := r.PathValue("asset_id")
	if asset == "" {
		h.WriteProblem(w, r, "https://api.stellarindex.io/errors/invalid-asset-id",
			"Invalid asset", http.StatusBadRequest, "asset_id path segment is required")
		return
	}
	// Validate up front: a malformed asset_id would otherwise drive TWO ledger_entries_current FINAL
	// scans before any 400.
	parsed, err := canonical.ParseAsset(asset)
	if err != nil {
		h.WriteProblem(w, r, "https://api.stellarindex.io/errors/invalid-asset-id",
			"Invalid asset", http.StatusBadRequest,
			"asset_id must be a canonical asset_id (e.g. 'native', 'USDC-G…'); got "+asset+" ("+err.Error()+")")
		return
	}
	// Normalize to the canonical spelling BEFORE the cache and lake query: ParseAsset also admits
	// "CODE:ISSUER" and bare "XLM", but the lake stores "CODE-ISSUER", so the raw string would
	// cache an authoritative-looking empty board under a duplicate key.
	asset = parsed.String()
	// Fold XLM's alias forms to one board key BEFORE the cache: "native" and "crypto:XLM" are the
	// same asset, and separate keys would run two identical account scans and split the SWR cache.
	if isNativeHoldersAsset(parsed) {
		asset = canonical.NativeAsset().String()
	}
	limit, ok := h.ParseLimit(w, r, 100, 500)
	if !ok {
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), explorerReadTimeout)
	defer cancel()

	// Cached + single-flighted: this unauthenticated route runs two ledger_entries_current FINAL
	// scans on the shared explorer pool, so one looping client could stall every lake-backed
	// endpoint. A stale board is served as 200 + flags.stale + its real as_of while the detached
	// rescan runs; only a never-computed asset can time out (its scan keeps running, so a retry
	// lands warm). See hot_reads.go.
	holders, total, asOf, degraded, err := h.assetHoldersCached(ctx, asset, limit)
	if err != nil {
		if h.ClientAborted(r, err) {
			return
		}
		if retryableColdMiss(ctx, err) {
			h.Logger.Warn("explorer AssetHolders deadline/saturation on cold asset", "asset", asset, "err", err)
			h.writeRetryable(w, r, err, "https://api.stellarindex.io/errors/asset-holders-timeout",
				"Asset holders timed out")
			return
		}
		h.Logger.Error("explorer AssetHolders failed", "err", err, "asset", asset)
		h.WriteProblem(w, r, "https://api.stellarindex.io/errors/internal",
			"Internal error", http.StatusInternalServerError, "")
		return
	}

	_, stale, _ := h.LakeWatermark(ctx)
	out := AssetHoldersView{Asset: asset, HolderCount: total, Holders: make([]AssetHolderV, len(holders)), AsOfLedger: asOf.ledger}
	for i, hh := range holders {
		out.Holders[i] = AssetHolderV{AccountID: hh.AccountID, Balance: strconv.FormatInt(hh.Balance, 10)}
	}
	if asCSV {
		h.writeAssetHoldersCSV(w, r, out, stale || degraded, degraded)
		return
	}
	h.writeJSONAt(w, out, stale || degraded, degraded, asOf.at)
}

var assetHoldersCSVColumns = []string{"asset", "account_id", "balance"}

// writeAssetHoldersCSV writes the board with the JSON's exact cell text;
// balance stays the integer string in the asset's smallest unit.
func (h *Handler) writeAssetHoldersCSV(w http.ResponseWriter, r *http.Request, v AssetHoldersView, stale, degraded bool) {
	p := CSVPage{
		Columns: assetHoldersCSVColumns,
		Rows:    make([][]string, len(v.Holders)),
		Headers: map[string]string{"X-StellarIndex-Holder-Count": strconv.FormatInt(v.HolderCount, 10)},
	}
	if v.AsOfLedger != 0 {
		p.Headers["X-StellarIndex-As-Of-Ledger"] = strconv.FormatUint(uint64(v.AsOfLedger), 10)
	}
	if stale {
		p.Flags = append(p.Flags, "stale")
	}
	if degraded {
		p.Flags = append(p.Flags, "degraded")
	}
	for i, hh := range v.Holders {
		p.Rows[i] = []string{v.Asset, hh.AccountID, hh.Balance}
	}
	if err := WriteCSVPage(w, r, p); err != nil && !h.ClientAborted(r, err) {
		h.Logger.Warn("explorer AssetHolders CSV write failed", "err", err, "asset", v.Asset)
	}
}

// PrewarmAccountsWealth primes the wealth-ranking cache (one ranking of top accountsWealthMaxLimit sliced per request, so limit is not a cache key) to avoid a ~23 s cold FINAL scan.
func (h *Handler) PrewarmAccountsWealth(ctx context.Context) {
	if h.Reader == nil {
		return
	}
	// Same inputs as the request path (USD map, else native-XLM fallback) so the cache warms on
	// one basis on every network, including lean nets where usdPriceMap is empty.
	assets, prices := h.wealthRankingInputs(ctx)
	h.Reader.AccountsByWealthCached(ctx, assets, prices, 0)
}
