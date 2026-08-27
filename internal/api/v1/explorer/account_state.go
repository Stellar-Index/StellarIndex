package explorer

import (
	"context"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// AccountsListView is the wire response for GET /v1/accounts — accounts ranked
// by wealth. On the priced networks that is total USD value of holdings; on the
// lean networks that have no aggregator (testnet/futurenet), where no USD price
// map exists, it is native XLM balance. `ranked_by` says which, so clients label
// the numbers correctly.
type AccountsListView struct {
	PricedAssets int `json:"priced_assets"`
	// RankedBy is the wealth basis: "usd" (holdings valued in USD) or
	// "native_xlm" (ranked by native XLM balance, used where no USD pricing
	// is available). Each row's Value is expressed in this unit.
	RankedBy string             `json:"ranked_by"`
	Accounts []AccountWealthRow `json:"accounts"`
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
	// Value is the account's ranked wealth in the response's RankedBy unit —
	// USD dollars ("usd") or whole XLM ("native_xlm"). This is the canonical
	// field; read RankedBy to format it.
	Value string `json:"value"`
	// USDValue is a backward-compatible alias, populated only on the "usd"
	// basis (equal to Value there). Empty on the "native_xlm" basis — read
	// Value instead. Retained so existing USD-network clients keep working.
	USDValue string `json:"usd_value,omitempty"`
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
	limit, ok := h.ParseLimit(w, r, 100, 500)
	if !ok {
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), explorerReadTimeout)
	defer cancel()

	// Ranking inputs. With a USD price map we rank by total USD holdings; where
	// there is none — the lean networks run no aggregator, so usdPriceMap comes
	// back empty — fall back to ranking by native XLM balance rather than 503ing
	// or serving an empty list. The native fallback is exactly the single key
	// "native" priced at 1.0, so sum(balance × price) is the XLM quantity; the
	// cache records the basis (wealthBasis) and the handler reads it back so the
	// served numbers are labelled correctly.
	assets, prices := h.wealthRankingInputs(ctx)
	// Served from a background-refreshed cache; the underlying FINAL scan
	// over 43.6M current-state rows needs 11-20s and can never fit this
	// handler's deadline. Before this, the scan ran inline and timed out on
	// EVERY request — 8.1s of waiting followed by a 500, 100% of the time
	// (site-audit S3/S30).
	ranked, basis, rankedAt, warm := h.Reader.AccountsByWealthCached(ctx, assets, prices, limit)
	if !warm {
		// Honest degraded state instead of a 500. The previous copy blamed
		// "the current-state projection is still backfilling, or pricing is
		// offline" — neither was ever true; the query simply timed out.
		// Reached ONLY when no ranking has EVER been computed this process
		// (cold start before the first refresh lands) — an expired snapshot
		// is served with flags.stale below, never 503'd (route-sweep
		// 2026-07-29).
		h.WriteProblem(w, r, "https://api.stellarindex.io/errors/warming-up",
			"Ranking not ready", http.StatusServiceUnavailable,
			"the account wealth ranking is being computed; retry shortly")
		return
	}
	// Degraded when the snapshot has outlived its refresh contract (the
	// background refresher is failing/behind) — the data is real, the flag +
	// the envelope's as_of (stamped from rankedAt) say how old.
	snapshotStale := time.Since(rankedAt) > clickhouse.AccountsWealthCacheTTL
	// Locked-burn detection (Pass-B ACC-1): the SDF burn address ranked
	// as the richest account — $11.3B of provably unspendable XLM
	// presented as wealth. Badge, don't hide: the balance is real, the
	// spendability isn't.
	// The locked-burn flag is resolved by the background refresh and cached
	// on each row (a.Locked), NOT re-queried here — AccountsUnspendable is a
	// FINAL scan that was the residual 6-8s of /v1/accounts latency once the
	// ranking itself was cached (site-audit S3).
	wmLedger, stale, _ := h.LakeWatermark(ctx)
	out := AccountsListView{RankedBy: basis, Accounts: make([]AccountWealthRow, len(ranked)), AsOfLedger: wmLedger}
	// priced_assets is a USD-basis notion; on the native-XLM basis nothing is
	// USD-priced, so report 0 rather than the "native" fallback key count.
	if basis != clickhouse.WealthBasisNative {
		out.PricedAssets = len(assets)
	}
	for i, a := range ranked {
		row := AccountWealthRow{AccountID: a.AccountID, Locked: a.Locked}
		if basis == clickhouse.WealthBasisNative {
			// a.USD carries the native XLM quantity (balance × 1.0); full
			// precision so the client formats it.
			row.Value = strconv.FormatFloat(a.USD, 'f', -1, 64)
		} else {
			row.Value = strconv.FormatFloat(a.USD, 'f', 2, 64)
			row.USDValue = row.Value // back-compat alias, USD basis only
		}
		out.Accounts[i] = row
	}
	h.writeJSONAt(w, out, stale || snapshotStale, rankedAt)
}

// usdPriceMap builds parallel (asset, price) arrays for wealth ranking: native
// XLM plus every browseable verified-currency Stellar asset that currently has
// a USD price. "native" is the key for the account entry's XLM balance. Prices
// resolve through LookupUSDPrice, so stablecoins pick up the fiat-proxy
// fallback (USDC/USDT/… → ~$1) rather than dropping out for want of a direct
// fiat:USD market.
// usdPriceMapTTL caches the priced-asset set used to rank account wealth.
//
// usdPriceMap walks every verified asset and resolves each price with a
// SEQUENTIAL LookupUSDPrice call. On production that loop measured ~8s on
// the request path — it, not the wealth scan, was what kept /v1/accounts at
// 8.1s even once the ranking itself was cached (site-audit S3 follow-up:
// the first fix removed the 500s but the latency survived, because there
// were two slow things stacked, not one).
//
// TTL is set ABOVE the prewarm cadence (5 min) on purpose: PrewarmAccounts-
// Wealth calls usdPriceMap every 5 minutes, so a 10-minute TTL means the
// entry is always refreshed before it expires and real requests never pay
// the walk. A 60s TTL (the first cut) expired 4 minutes out of every 5, so
// most requests still ate the ~8s cold walk even though the wealth ranking
// itself was warmly cached — the residual attempt-1 latency in the S3
// production verification. 10 minutes stale is fine: this feeds a ranking
// cached for 15 minutes, so the price set cannot change what the page shows.
const usdPriceMapTTL = 10 * time.Minute

type usdPriceMapEntry struct {
	assets   []string
	prices   []float64
	cachedAt time.Time
}

var (
	usdPriceMapMu    sync.Mutex
	usdPriceMapCache usdPriceMapEntry
)

// usdPriceMap builds parallel (asset, price) arrays for wealth ranking,
// memoised for [usdPriceMapTTL]. See the const's doc for why.
func (h *Handler) usdPriceMap(ctx context.Context) (assets []string, prices []float64) {
	usdPriceMapMu.Lock()
	if c := usdPriceMapCache; c.assets != nil && time.Since(c.cachedAt) < usdPriceMapTTL {
		usdPriceMapMu.Unlock()
		return c.assets, c.prices
	}
	usdPriceMapMu.Unlock()

	assets, prices = h.usdPriceMapUncached(ctx)
	if len(assets) > 0 {
		usdPriceMapMu.Lock()
		usdPriceMapCache = usdPriceMapEntry{assets: assets, prices: prices, cachedAt: time.Now()}
		usdPriceMapMu.Unlock()
	}
	return assets, prices
}

// wealthRankingInputs returns the (asset, price) arrays to rank account wealth
// by. It is the USD price map where one exists; where it doesn't — pricing
// disabled, or the catalogue is empty because no aggregator runs (the lean
// test nets) — it returns the native-XLM fallback (key "native" priced at 1.0)
// so the ranking is by native XLM balance instead of coming back empty. Both
// the request path and the prewarmer call this so the cache is populated with
// one consistent basis (wealthBasis derives it from these inputs).
func (h *Handler) wealthRankingInputs(ctx context.Context) (assets []string, prices []float64) {
	if h.PricingEnabled {
		if a, p := h.usdPriceMap(ctx); len(a) > 0 {
			return a, p
		}
	}
	return []string{"native"}, []float64{1.0}
}

func (h *Handler) usdPriceMapUncached(ctx context.Context) (assets []string, prices []float64) {
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
	// Directory is the curated third-party label for this address
	// (directory.go) — display attribution, not verification. Omitted
	// when the address isn't listed or no directory reader is wired.
	Directory *DirectoryInfoV `json:"directory,omitempty"`
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
	// Checksum-valid strkey (canonical.IsAccountID via parseAccountStrkey),
	// matching every sibling account endpoint (AccountTransactions,
	// AccountOperations, AccountMovements, AccountPositions). The former
	// looksLikeStellarAccount check only validated shape (length + base32
	// alphabet), so a well-formed-but-corrupted-checksum G-strkey sailed
	// through to the lake read, which then 500'd + error-logged on every
	// such request instead of 400ing up front (API-01 / API-03).
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
			// Either the read blew explorerReadTimeout or the shared
			// detached-refresh gate was saturated (clickhouse.ErrRefreshSaturated,
			// recon-R3) — both transient/retryable, so 503, not the 500 a real
			// bug gets. Mirrors the sibling SWR handlers (AssetHolders,
			// ContractDetail, ContractsList) which map the same class here.
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

	wmLedger, stale, _ := h.LakeWatermark(ctx)
	out := AccountStateView{AccountID: g, Exists: st.Exists, AsOfLedger: wmLedger}
	// Directory labels apply regardless of Exists — a listed address
	// whose AccountEntry predates the captured window (or was merged
	// away) is exactly where a label helps most.
	out.Directory = h.directoryFor(ctx, g)
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
	// snapStale: the served state came from an expired cache entry while a
	// detached refresh runs (whale-account stale-serve, route-sweep
	// 2026-07-30) — surfaced on the same flags.stale the watermark uses.
	h.WriteJSON(w, out, stale || snapStale)
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

// isNativeHoldersAsset reports whether a requested asset form serves the
// native-XLM holders board. Native has no trustlines — every account holds
// XLM in its AccountEntry balance — so the board is computed from the
// account-balance ranking under the single key "native", and every non-SAC
// member of XLM's alias family (`native`, `crypto:XLM`, plus ParseAsset's
// "XLM" shorthand — the CLAUDE.md native ↔ crypto:XLM dual-form rule) folds
// down to it: one cache entry, one detached flight, one reader arm.
//
// The family's THIRD member — the SAC wrapper C-address (CAS3J7GY…) — is
// deliberately NOT folded in: a caller naming the C-address is asking about
// the wrapper's own contract-balance domain (Soroban `Balance(addr)`
// entries), which is a genuinely different holder set from classic
// AccountEntry balances. It keeps the (currently empty) trustline read
// rather than being mislabelled as the account board.
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

// AssetHolders serves GET /v1/assets/{asset_id}/holders — the top holders
// of an asset by current balance, plus the total holder count. asset_id is
// the canonical form ("CODE-ISSUER" / "native"). Lake-backed
// (ledger_entry_changes): trustline balances for issued assets; for native
// XLM (and its crypto:XLM alias) the AccountEntry balance ranking, with
// holder_count = the exact number of funded accounts — native has no
// trustlines, so the trustline read served {"holder_count":0} for it by
// construction (fixed 2026-07-31). The response's `asset` echoes the
// canonical board key ("native" for any XLM alias form).
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
	parsed, err := canonical.ParseAsset(asset)
	if err != nil {
		h.WriteProblem(w, r, "https://api.stellarindex.io/errors/invalid-asset-id",
			"Invalid asset", http.StatusBadRequest,
			"asset_id must be a canonical asset_id (e.g. 'native', 'USDC-G…'); got "+asset+" ("+err.Error()+")")
		return
	}
	// Normalize to the canonical spelling BEFORE the cache and the lake
	// query: ParseAsset also admits the Horizon "CODE:ISSUER" spelling
	// (and bare "XLM"), but the lake stores canonical "CODE-ISSUER" —
	// keying/querying the raw request string would scan for a spelling
	// that never matches and cache the resulting authoritative-looking
	// EMPTY board under a duplicate key.
	asset = parsed.String()
	// Fold XLM's alias forms to the one canonical board key BEFORE the
	// cache: "native" and "crypto:XLM" are the same asset (assetAliases
	// dual-form rule), and keying them separately would run two identical
	// account-range scans and split the SWR cache.
	if isNativeHoldersAsset(parsed) {
		asset = canonical.NativeAsset().String()
	}
	limit, ok := h.ParseLimit(w, r, 100, 500)
	if !ok {
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), explorerReadTimeout)
	defer cancel()

	// Cached + single-flighted (C3-002, audit-2026-07-23): this is an
	// unauthenticated route over two ledger_entries_current FINAL scans on
	// the shared 8-connection explorer pool, so a single client looping it
	// could hold every connection and stall every other lake-backed
	// endpoint. Snapshot-served since route-sweep 2026-07-29: a stale
	// board is 200 + flags.stale + its real as_of while the detached
	// rescan runs; only a never-computed asset can time out here (and its
	// detached scan keeps running, so a retry lands warm). See hot_reads.go.
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

	wmLedger, stale, _ := h.LakeWatermark(ctx)
	out := AssetHoldersView{Asset: asset, HolderCount: total, Holders: make([]AssetHolderV, len(holders)), AsOfLedger: wmLedger}
	for i, hh := range holders {
		out.Holders[i] = AssetHolderV{AccountID: hh.AccountID, Balance: strconv.FormatInt(hh.Balance, 10)}
	}
	h.writeJSONAt(w, out, stale || degraded, asOf)
}

// PrewarmAccountsWealth primes the wealth-ranking cache so no user ever
// meets the cold state.
//
// The ranking is a FINAL scan over 43.6M current-state rows (~23 s) and
// cannot run on a request deadline — before site-audit S3 it was attempted
// inline and 500'd on 100% of requests. Calling the cached reader here is
// enough: on a miss it kicks off the detached background refresh and
// returns immediately, so this is cheap to call on a timer. The cache
// stores ONE ranking (the top accountsWealthMaxLimit) that every request
// size slices from, so a single warm entry covers all traffic — the limit
// argument is not a cache key.
func (h *Handler) PrewarmAccountsWealth(ctx context.Context) {
	if h.Reader == nil {
		return
	}
	// Same inputs the request path uses — USD price map where available, native
	// XLM fallback otherwise — so the cache is warmed with one consistent basis
	// on every network, including the lean nets that would otherwise never
	// prewarm (usdPriceMap is always empty there) and 503 the first request.
	assets, prices := h.wealthRankingInputs(ctx)
	h.Reader.AccountsByWealthCached(ctx, assets, prices, 0)
}
