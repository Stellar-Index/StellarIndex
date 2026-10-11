package v1

import (
	"context"
	"math/big"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/aggregate"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/currency"
	"github.com/Stellar-Index/StellarIndex/internal/sources/external"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// DexSourceNames returns every Class=Exchange + Subclass=DEX source, sorted for stable order.
//
// Exported so the prewarm goroutine computes the SAME slice the handler does: otherwise the
// prewarmed unfiltered /v1/pools key (Sources nil) never matches the handler's key.
func DexSourceNames() []string {
	out := make([]string, 0, len(external.Registry))
	for name, md := range external.Registry {
		if md.Class == external.ClassExchange && md.Subclass == external.SubclassDEX {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// CexSourceNames returns every source registered with
// Class=Exchange + Subclass=CEX, sorted for stable order. Exported so the
// prewarm in cmd/stellarindex-api can warm `/v1/markets?source=<name>`.
func CexSourceNames() []string {
	out := make([]string, 0, len(external.Registry))
	for name := range external.Registry {
		if isCEXVenue(name) {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// MarketsReader is the storage-side interface for /v1/markets and /v1/pairs lookups.
//
// The row slices the list methods return belong to the caller, who normalises and enriches them in
// place, so an implementation that retains rows (a cache) must hand out a copy, as
// [CachedMarketsReader] does. Callers may replace a row's fields but not write through its
// pointer/slice fields.
type MarketsReader interface {
	// DistinctPairsExt returns one page of (base, quote) pairs
	// present in the trades store under the requested ordering.
	// Cursor opaque; empty starts at page 1.
	DistinctPairsExt(ctx context.Context, cursor string, limit int, order timescale.MarketsOrder) ([]Market, string, error)

	// SourceMarkets is DistinctPairsExt narrowed to a single
	// source — the per-DEX pool list backing
	// /v1/markets?source=<name>. Same shape as DistinctPairsExt
	// for paginated drill-down.
	SourceMarkets(ctx context.Context, source, cursor string, limit int, order timescale.MarketsOrder) ([]Market, string, error)

	// AssetMarkets is DistinctPairsExt narrowed to pairs where the asset_id is base or quote, so
	// the Markets tab avoids a global scan plus client-side filter.
	AssetMarkets(ctx context.Context, asset, cursor string, limit int, order timescale.MarketsOrder) ([]Market, string, error)

	// AllPools returns every (source, base, quote) tuple, so the same pair on two venues is two
	// rows. A non-empty filter sources list restricts rows to those venues (the DEX subset).
	AllPools(ctx context.Context, filter timescale.PoolsFilter, cursor string, limit int, order timescale.MarketsOrder) ([]Pool, string, error)

	// PairMarket returns the activity summary for one pair. The bool is false when it has no
	// trades; /v1/pairs answers that with an empty 200 array, not a 404, per PairsEnvelope.
	PairMarket(ctx context.Context, base, quote canonical.Asset) (Market, bool, error)

	// GetPairsVolumeHistory24hBatch returns per-pair hourly USD-volume
	// buckets for the trailing 24h. Backs the /v1/markets sparkline
	// column when the caller passes ?include=sparkline.
	GetPairsVolumeHistory24hBatch(ctx context.Context, pairs [][2]string) (map[string][]timescale.PairVolumePoint, error)

	// FirstTradeBatch returns each pair's first daily bucket — the
	// ?include=inception enrichment on /v1/markets.
	FirstTradeBatch(ctx context.Context, pairs [][2]string) (map[string]time.Time, error)
}

// marketsStaleReader is the optional capability of a MarketsReader that reports each served
// row-set's observed-at time and staleness; CachedMarketsReader implements it. It lets handleMarkets
// stamp an honest as_of and flags.stale instead of the stale-while-revalidate path claiming
// as_of=now over old rows while refresh keeps failing. The *At methods add observedAt (zero means
// as_of=now, not stale) and stale (the served bytes are past the cache TTL).
type marketsStaleReader interface {
	DistinctPairsExtAt(ctx context.Context, cursor string, limit int, order timescale.MarketsOrder) ([]Market, string, time.Time, bool, error)
	SourceMarketsAt(ctx context.Context, source, cursor string, limit int, order timescale.MarketsOrder) ([]Market, string, time.Time, bool, error)
	AssetMarketsAt(ctx context.Context, asset, cursor string, limit int, order timescale.MarketsOrder) ([]Market, string, time.Time, bool, error)
}

// poolsStaleReader is marketsStaleReader's /v1/pools counterpart: observedAt
// is the served rows' fetch time and stale reports rows served from the
// cache's stale-while-revalidate branch.
type poolsStaleReader interface {
	AllPoolsStale(ctx context.Context, filter timescale.PoolsFilter, cursor string, limit int, order timescale.MarketsOrder) ([]Pool, string, time.Time, bool, error)
}

// allPools reads through poolsStaleReader when the wired reader has it; an
// uncached read is live: zero observedAt, stale false.
func allPools(ctx context.Context, reader MarketsReader, filter timescale.PoolsFilter, cursor string, limit int, order timescale.MarketsOrder) ([]Pool, string, time.Time, bool, error) {
	if sr, ok := reader.(poolsStaleReader); ok {
		return sr.AllPoolsStale(ctx, filter, cursor, limit, order)
	}
	rows, next, err := reader.AllPools(ctx, filter, cursor, limit, order)
	return rows, next, time.Time{}, false, err
}

// Pool is the wire shape for /v1/pools entries. Same fields as
// Market but with a `source` dimension so the same physical pair
// traded on two DEXes shows as two rows.
type Pool struct {
	Source        string   `json:"source"`
	Base          string   `json:"base"`
	Quote         string   `json:"quote"`
	LastTradeAt   WireTime `json:"last_trade_at"`
	TradeCount24h int64    `json:"trade_count_24h"`
	Volume24hUSD  *string  `json:"volume_24h_usd,omitempty"`
	// VolumeLowerBound: Volume24hUSD excludes trades with no trade-time usd_volume.
	VolumeLowerBound bool `json:"volume_lower_bound,omitempty"`
	// LastPrice is the most recent quote-per-base price observed
	// for THIS pool — same wire shape as Market.LastPrice but
	// per-source, so two venues trading the same pair surface
	// independent prices.
	LastPrice *string `json:"last_price,omitempty"`
}

// handlePools serves GET /v1/pools: DEX/AMM pools only, one row per (source, base, quote) where
// the source is a DEX. CEX pairs go through /v1/markets.
//
// Query params (all optional): cursor, limit (1-500, default 100), order_by ("volume_24h_usd_desc"
// default, or "pair"), source (one DEX name; unknown or non-DEX names give an empty list),
// base / quote (AND-combined), asset (either side; exclusive with base/quote since AND vs OR has no
// well-defined combination).
//
// Every ?asset=/?base=/?quote= filter is re-spelled through canonical.ParseAsset (see
// canonicaliseAssetFilter): a bare alias like "XLM" differs from the stored form, and the SQL `=`
// would return an authoritative empty 200, cached for 60s. `native` and `crypto:XLM` are stored
// as distinct rows, so canonicalising does not merge them.
func (s *Server) handlePools(w http.ResponseWriter, r *http.Request) { //nolint:gocognit,gocyclo,funlen // option parsing + DEX-source filter + asset/base+quote validation + 8s-timeout guard are linear; splitting fragments the request lifecycle
	cursor := r.URL.Query().Get("cursor")
	limit := 100
	if raw := r.URL.Query().Get("limit"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 || parsed > 500 {
			writeProblem(w, r,
				"https://api.stellarindex.io/errors/invalid-limit",
				"Invalid limit", http.StatusBadRequest,
				"limit must be an integer in [1, 500]")
			return
		}
		limit = parsed
	}
	var order timescale.MarketsOrder
	switch r.URL.Query().Get("order_by") {
	case "", "volume_24h_usd_desc":
		order = timescale.MarketsOrderVolume24hDesc
	case "pair":
		order = timescale.MarketsOrderPair
	default:
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/invalid-order",
			"Invalid order_by", http.StatusBadRequest,
			"order_by must be 'pair' or 'volume_24h_usd_desc'")
		return
	}

	if err := timescale.ValidateMarketsCursor(cursor, order); err != nil {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/invalid-cursor",
			"Invalid cursor", http.StatusBadRequest,
			"cursor: "+err.Error()+". Pass back the pagination.next value from a prior /v1/pools response, or omit the parameter to start at page 1.")
		return
	}

	// Resolve DEX source list from the registry. Hard-coded to
	// Subclass=DEX so the endpoint is unambiguously "pools" — no
	// CEX rows ever.
	dexSources := DexSourceNames()
	if reqSource := r.URL.Query().Get("source"); reqSource != "" {
		// Filter to the requested DEX. Non-DEX names get rejected
		// here (empty intersection → empty result list, not a 400)
		// so callers can pass through user input without separately
		// validating against the registry.
		filtered := make([]string, 0, 1)
		for _, s := range dexSources {
			if s == reqSource {
				filtered = append(filtered, s)
				break
			}
		}
		dexSources = filtered
	}

	reader := s.Markets
	if reader == nil {
		writeJSON(w, []Pool{}, Flags{})
		return
	}
	if len(dexSources) == 0 {
		// Either the registry has no DEX sources (impossible) or
		// the source= filter didn't match a DEX. Return an empty
		// list rather than scan the trades hypertable for nothing.
		writeJSON(w, []Pool{}, Flags{})
		return
	}
	baseFilter := r.URL.Query().Get("base")
	quoteFilter := r.URL.Query().Get("quote")
	assetFilter := r.URL.Query().Get("asset")
	if assetFilter != "" {
		parsed, err := canonical.ParseAsset(assetFilter)
		if err != nil {
			writeProblem(w, r,
				"https://api.stellarindex.io/errors/invalid-asset-id",
				"Invalid asset", http.StatusBadRequest,
				"asset must be a canonical asset_id (e.g. 'native', 'USDC-G…', 'fiat:USD'); got "+assetFilter+" ("+err.Error()+")")
			return
		}
		// Keep the PARSED spelling — see canonicaliseAssetFilter.
		assetFilter = parsed.String()
		if baseFilter != "" || quoteFilter != "" {
			writeProblem(w, r,
				"https://api.stellarindex.io/errors/conflicting-filters",
				"Conflicting filters", http.StatusBadRequest,
				"asset (base OR quote) cannot be combined with base or quote (AND-shape) on /v1/pools; pick one filter shape.")
			return
		}
	}
	// Validate base/quote up front, as `asset` is, so malformed input is rejected rather than
	// reaching the trades-hypertable scan as a silent empty page or DoS lever.
	if baseFilter != "" {
		parsed, err := canonical.ParseAsset(baseFilter)
		if err != nil {
			writeProblem(w, r,
				"https://api.stellarindex.io/errors/invalid-asset-id",
				"Invalid base", http.StatusBadRequest,
				"base must be a canonical asset_id (e.g. 'native', 'USDC-G…'); got "+baseFilter+" ("+err.Error()+")")
			return
		}
		baseFilter = parsed.String()
	}
	if quoteFilter != "" {
		parsed, err := canonical.ParseAsset(quoteFilter)
		if err != nil {
			writeProblem(w, r,
				"https://api.stellarindex.io/errors/invalid-asset-id",
				"Invalid quote", http.StatusBadRequest,
				"quote must be a canonical asset_id (e.g. 'native', 'USDC-G…'); got "+quoteFilter+" ("+err.Error()+")")
			return
		}
		quoteFilter = parsed.String()
	}
	filter := timescale.PoolsFilter{
		Sources: dexSources,
		Base:    baseFilter,
		Quote:   quoteFilter,
		Asset:   assetFilter,
	}
	// Hard 8s ceiling: AllPools scans the trades hypertable's 24h window and can take 10s+ cold
	// (prewarm covers only limit=25 with no filter). Better a fast 503 to retry against a warm
	// cache than a hung request that times out at the ingress.
	pCtx, pCancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer pCancel()
	rows, next, observedAt, stale, err := allPools(pCtx, reader, filter, cursor, limit, order)
	if err != nil {
		if clientAborted(r, err) {
			return
		}
		if handlerTimedOut(pCtx, err) {
			s.logger.Warn("AllPools deadline exceeded", "limit", limit, "filter", filter)
			writeProblem(w, r,
				"https://api.stellarindex.io/errors/pools-timeout",
				"Pools query timed out", http.StatusServiceUnavailable,
				"the underlying trades-hypertable scan didn't return in 8s; cache may still be warming. Retry in a few seconds.")
			return
		}
		s.logger.Error("AllPools failed", "err", err)
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/internal",
			"Internal error", http.StatusInternalServerError, "")
		return
	}
	if rows == nil {
		rows = []Pool{}
	}
	// last_price needs dex-nonstandard-decimals normalization (docs/operations/runbooks/dex.md):
	// pools_per_source_1h's bucket_last_price is the same raw ratio /v1/price serves. See
	// adjustListingPriceStrings for the byte-identical-on-7dp contract.
	for i := range rows {
		rows[i].LastPrice = s.adjustListingPriceStrings(pCtx, rows[i].Base, rows[i].Quote, rows[i].LastPrice, "pools")
	}
	env := Envelope{Data: rows, Flags: Flags{Stale: stale, Degraded: stale}}
	if !observedAt.IsZero() {
		env.AsOf = WireTime(observedAt.UTC())
	}
	if next != "" {
		env.Pagination = &Pagination{Next: next}
	}
	writeEnvelope(w, env)
}

// Market is the wire shape for /v1/markets entries.
//
// TradeCount24h may be zero even when LastTradeAt is recent: they measure
// different windows (activity vs most-recent event).
//
// Volume24hUSD sums prices_1m's per-bucket volume_usd over 24h. It is a pointer so a pair with
// no USD-equivalent trades emits null ("unknown") rather than "0" ("definitely zero").
//
// LastTradeAt is the latest prices_1m bucket-start with a trade, falling back to the daily
// bucket-start for pairs idle >24h but active in the 14d window; use it for freshness.
// BucketCloseAt is the latest prices_1d bucket-start (UTC midnight) and would show spuriously
// large staleness, so do not use it for that.
type Market struct {
	Base          string   `json:"base"`
	Quote         string   `json:"quote"`
	LastTradeAt   WireTime `json:"last_trade_at"`
	BucketCloseAt WireTime `json:"bucket_close_at"`
	TradeCount24h int64    `json:"trade_count_24h"`
	Volume24hUSD  *string  `json:"volume_24h_usd,omitempty"`
	// VolumeLowerBound: Volume24hUSD excludes trades with no trade-time
	// usd_volume. Set only under `?source=`.
	VolumeLowerBound bool `json:"volume_lower_bound,omitempty"`
	// LastPrice is the most recent quote-per-base price observed
	// for this pair within the trailing 24h: across every source, or
	// that source's own with `?source=`. Null when none was observed.
	LastPrice *string `json:"last_price,omitempty"`
	// VolumeHistory24h — per-hour USD-volume buckets for the
	// trailing 24h. Populated only when the request sets
	// `?include=sparkline` without `?source=`. 24 entries oldest →
	// newest, zero-filled server-side so the wire array length is stable.
	VolumeHistory24h []MarketVolumeBucket `json:"volume_history_24h,omitempty"`
	// FirstTradeAt is the pair's first recorded daily bucket — the
	// RFP's "since inception = first recorded trade", queryable per
	// market. Populated only with `?include=inception`
	// without `?source=`; day precision.
	FirstTradeAt *WireTime `json:"first_trade_at,omitempty"`
}

// MarketVolumeBucket — one hourly USD-volume datapoint for the
// /v1/markets sparkline. Hour is RFC 3339; volume_usd is
// numeric-stringified for precision parity.
type MarketVolumeBucket struct {
	Hour      WireTime `json:"hour"`
	VolumeUSD string   `json:"volume_usd"`
}

// handleMarkets serves GET /v1/markets.
//
// Query params (all optional): cursor, limit (1-500, default 100), order_by ("pair" or
// "volume_24h_usd_desc", the default, so clients skip ~5K dust pairs), source (one on-chain source
// or CEX venue seen in the recency window; data vendors are refused by sourceFilterOK), and asset
// (canonical asset_id on either side; mutually exclusive with `source`).
func (s *Server) handleMarkets(w http.ResponseWriter, r *http.Request) { //nolint:gocognit,gocyclo,funlen // option parsing + source/asset/no-filter dispatch + 8s-timeout guard + sparkline backfill are linear; splitting fragments the request lifecycle
	cursor := r.URL.Query().Get("cursor")
	limit := 100
	if raw := r.URL.Query().Get("limit"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 || parsed > 500 {
			writeProblem(w, r,
				"https://api.stellarindex.io/errors/invalid-limit",
				"Invalid limit", http.StatusBadRequest,
				"limit must be an integer in [1, 500]")
			return
		}
		limit = parsed
	}
	var order timescale.MarketsOrder
	switch r.URL.Query().Get("order_by") {
	case "":
		// Default is volume-desc, not MarketsOrderPair: alphabetical order puts spam tokens at the
		// top. Callers wanting it pass `?order_by=pair`.
		order = timescale.MarketsOrderVolume24hDesc
	case "pair":
		order = timescale.MarketsOrderPair
	case "volume_24h_usd_desc":
		order = timescale.MarketsOrderVolume24hDesc
	default:
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/invalid-order",
			"Invalid order_by", http.StatusBadRequest,
			"order_by must be 'pair' or 'volume_24h_usd_desc'")
		return
	}

	if err := timescale.ValidateMarketsCursor(cursor, order); err != nil {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/invalid-cursor",
			"Invalid cursor", http.StatusBadRequest,
			"cursor: "+err.Error()+". Pass back the pagination.next value from a prior /v1/markets response, or omit the parameter to start at page 1.")
		return
	}

	source := r.URL.Query().Get("source")
	if !sourceFilterOK(w, r, source) {
		return
	}

	asset := r.URL.Query().Get("asset")
	// Slug expansion: an `asset` that is not a canonical asset_id but matches a verified-currency
	// catalogue slug fans out to every asset_id form the catalogue knows (networks[].asset_id plus
	// crypto:<TICKER> for CEX trades), so one query surfaces both SDEX and CEX markets.
	var expandedAssets []string
	if asset != "" {
		if parsed, err := canonical.ParseAsset(asset); err == nil {
			// Keep the PARSED spelling — see canonicaliseAssetFilter.
			asset = parsed.String()
		} else {
			// Try the catalogue. Successful lookup expands to a slice
			// of canonical asset_ids; failure → 400 (existing
			// behaviour for unparseable input).
			expanded := s.expandSlugToAssetIDs(asset)
			if len(expanded) == 0 {
				writeProblem(w, r,
					"https://api.stellarindex.io/errors/invalid-asset-id",
					"Invalid asset", http.StatusBadRequest,
					"asset must be a canonical asset_id (e.g. 'native', 'USDC-G…', 'fiat:USD') or a catalogue slug (e.g. 'usdc', 'btc'); got "+asset+" ("+err.Error()+")")
				return
			}
			expandedAssets = expanded
		}
	}
	if source != "" && asset != "" {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/conflicting-filters",
			"Conflicting filters", http.StatusBadRequest,
			"source and asset cannot be combined on /v1/markets; pick one. To find a single source's markets involving a specific asset, fetch /v1/markets?source=<src> and filter client-side, or use /v1/pools?source=<src>&base=<asset>.")
		return
	}

	reader := s.Markets
	if reader == nil {
		// Feature not wired — empty list is consistent with the
		// contract and doesn't force a 503. Mirrors the /v1/assets
		// degradation pattern.
		writeJSON(w, []Market{}, Flags{})
		return
	}
	// A reader that reports observed-at and staleness (the TTL cache) gets an honest as_of and
	// stale; otherwise the read is live-fresh and as_of=now / stale=false is truthful.
	staleReader, staleAware := reader.(marketsStaleReader)

	var (
		rows       []Market
		next       string
		observedAt time.Time
		stale      bool
		err        error
	)
	// Hard 8s ceiling, as on /v1/pools: DistinctPairsExt and SourceMarkets scan the trades
	// hypertable's 24h window and can take 10s+ cold, and prewarm covers only limit=25.
	mCtx, mCancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer mCancel()
	switch {
	case source != "":
		if staleAware {
			rows, next, observedAt, stale, err = staleReader.SourceMarketsAt(mCtx, source, cursor, limit, order)
		} else {
			rows, next, err = reader.SourceMarkets(mCtx, source, cursor, limit, order)
		}
	case len(expandedAssets) > 0:
		// Slug expansion fans out one AssetMarkets query per asset_id and merges in memory. There
		// is no pagination: each cursor is per-asset_id and no combined keyset exists, so callers
		// wanting the full stream pass the canonical asset_id.
		rows, observedAt, stale, err = s.fanOutAssetMarkets(mCtx, reader, expandedAssets, limit, order)
		next = ""
	case asset != "":
		if staleAware {
			rows, next, observedAt, stale, err = staleReader.AssetMarketsAt(mCtx, asset, cursor, limit, order)
		} else {
			rows, next, err = reader.AssetMarkets(mCtx, asset, cursor, limit, order)
		}
	default:
		if staleAware {
			rows, next, observedAt, stale, err = staleReader.DistinctPairsExtAt(mCtx, cursor, limit, order)
		} else {
			rows, next, err = reader.DistinctPairsExt(mCtx, cursor, limit, order)
		}
	}
	if err != nil {
		if clientAborted(r, err) {
			return
		}
		if handlerTimedOut(mCtx, err) {
			s.logger.Warn("DistinctPairs deadline exceeded",
				"limit", limit, "source", source)
			writeProblem(w, r,
				"https://api.stellarindex.io/errors/markets-timeout",
				"Markets query timed out", http.StatusServiceUnavailable,
				"the underlying trades-hypertable scan didn't return in 8s; cache may still be warming. Retry in a few seconds.")
			return
		}
		s.logger.Error("DistinctPairs failed", "err", err)
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/internal",
			"Internal error", http.StatusInternalServerError, "")
		return
	}
	// Defensive nil-to-empty: OpenAPI's MarketsEnvelope.data is
	// `type: array`, which means "data": null violates the schema.
	// Mirrors the handleAssetList guard.
	if rows == nil {
		rows = []Market{}
	}
	// dex-nonstandard-decimals forward normalization — see handlePools's
	// equivalent comment. /v1/markets's last_price sources from the same
	// prices_1d / pools_per_source_1h raw ratio.
	for i := range rows {
		rows[i].LastPrice = s.adjustListingPriceStrings(mCtx, rows[i].Base, rows[i].Quote, rows[i].LastPrice, "markets")
	}

	// Optional opt-in: attach 24h hourly volume history per row
	// for sparkline columns. Default off (avoids ~50KB per page
	// of bloat for SDK consumers that don't render charts).
	includeSparkline := false
	includeInception := false
	for _, f := range strings.Split(r.URL.Query().Get("include"), ",") {
		switch strings.TrimSpace(f) {
		case "sparkline":
			includeSparkline = true
		case "inception":
			includeInception = true
		}
	}
	// Both enrichment readers are pair-wide (every venue). Beside a
	// ?source= row's single-venue headline they would contradict it, so omit them.
	pairWide := source == ""
	// A dropped enrichment is a partial page the next request fills.
	enrichFailed := false
	if includeInception && pairWide && len(rows) > 0 {
		pairs := make([][2]string, len(rows))
		for i, m := range rows {
			pairs[i] = [2]string{m.Base, m.Quote}
		}
		// Best-effort like the sparkline path: a failure ships the
		// page without inception rather than 5xx-ing a listing.
		if firsts, fErr := reader.FirstTradeBatch(mCtx, pairs); fErr != nil {
			s.logger.Warn("markets inception batch failed", "err", fErr)
			enrichFailed = true
		} else {
			for i, m := range rows {
				if t, ok := firsts[m.Base+"|"+m.Quote]; ok {
					tt := t
					rows[i].FirstTradeAt = wireTimePtr(&tt)
				}
			}
		}
	}
	if includeSparkline && pairWide && len(rows) > 0 {
		pairs := make([][2]string, len(rows))
		for i, m := range rows {
			pairs[i] = [2]string{m.Base, m.Quote}
		}
		// Share mCtx so the sparkline batch cannot push the request past the 8s cap. It is
		// best-effort: on failure it logs at WARN and the response ships without sparklines.
		if hist, hErr := reader.GetPairsVolumeHistory24hBatch(mCtx, pairs); hErr != nil {
			s.logger.Warn("markets sparkline batch failed", "err", hErr)
			enrichFailed = true
		} else {
			for i, m := range rows {
				key := m.Base + "|" + m.Quote
				series := hist[key]
				if len(series) == 0 {
					continue
				}
				out := make([]MarketVolumeBucket, len(series))
				for j, p := range series {
					out[j] = MarketVolumeBucket{Hour: WireTime(p.Hour), VolumeUSD: p.VolumeUSD}
				}
				rows[i].VolumeHistory24h = out
			}
		}
	}

	// A stale-while-revalidate serve stamps flags.stale and the data's actual observation time,
	// never now() over rows a failing refresh let age past the TTL. An uncached read leaves
	// observedAt zero and writeEnvelope defaults as_of to now.
	env := Envelope{
		Data:  rows,
		Flags: Flags{Stale: stale, Degraded: stale || enrichFailed},
	}
	if !observedAt.IsZero() {
		env.AsOf = WireTime(observedAt.UTC())
	}
	if next != "" {
		env.Pagination = &Pagination{Next: next}
	}
	writeEnvelope(w, env)
}

// expandSlugToAssetIDs resolves a verified-currency catalogue slug (e.g. "usdc", "xlm") to the
// canonical asset_ids it represents, for /v1/markets?asset=<slug> fan-out.
//
// The expansion is every networks[].asset_id (non-Stellar networks contribute nothing, as we index
// no trades there), plus `crypto:<TICKER>` for crypto and stablecoin classes so CEX trades match,
// plus `fiat:<TICKER>` for fiat. Nil when no catalogue is wired or the slug is unknown.
func (s *Server) expandSlugToAssetIDs(slug string) []string {
	if s.VerifiedCurrencies == nil {
		return nil
	}
	vc, ok := s.VerifiedCurrencies.LookupBySlug(slug)
	if !ok {
		vc, ok = s.VerifiedCurrencies.LookupByTicker(slug)
	}
	if !ok || vc == nil {
		return nil
	}
	seen := make(map[string]struct{})
	out := make([]string, 0, 4)
	add := func(s string) {
		if s == "" {
			return
		}
		if _, dup := seen[s]; dup {
			return
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	for _, n := range vc.Issuance {
		if n.AssetID != "" {
			add(n.AssetID)
		}
	}
	switch vc.Class {
	case currency.ClassCrypto, currency.ClassStablecoin:
		add("crypto:" + vc.Ticker)
	case currency.ClassFiat:
		add("fiat:" + vc.Ticker)
	}
	return out
}

// assetMarketsLeg fetches one expanded asset_id's markets plus observedAt/stale when the reader is
// staleness-aware (zero/false otherwise), keeping fanOutAssetMarkets free of the capability branch.
func assetMarketsLeg(ctx context.Context, reader MarketsReader, asset string, limit int, order timescale.MarketsOrder) ([]Market, time.Time, bool, error) {
	if sr, ok := reader.(marketsStaleReader); ok {
		rows, _, observedAt, stale, err := sr.AssetMarketsAt(ctx, asset, "", limit, order)
		return rows, observedAt, stale, err
	}
	rows, _, err := reader.AssetMarkets(ctx, asset, "", limit, order)
	return rows, time.Time{}, false, err
}

// fanOutAssetMarkets calls AssetMarkets in parallel per expanded asset_id, dedupes on (base, quote)
// so a pair matched by two queries is one row, sorts per `order` and caps at `limit`. There is no
// pagination because cursors across heterogeneous streams are not well-defined.
//
// Freshness is conservative: observedAt is the oldest contributing read and stale is the OR of the
// legs; both are zero/false when the reader cannot report them, so the handler uses as_of=now.
func (s *Server) fanOutAssetMarkets(ctx context.Context, reader MarketsReader, assets []string, limit int, order timescale.MarketsOrder) ([]Market, time.Time, bool, error) {
	type result struct {
		rows       []Market
		observedAt time.Time
		stale      bool
		err        error
	}
	results := make([]result, len(assets))
	forEachBounded(s.logger, len(assets), readFanoutConcurrency, func(i int) {
		rows, observedAt, stale, err := assetMarketsLeg(ctx, reader, assets[i], limit, order)
		results[i] = result{rows: rows, observedAt: observedAt, stale: stale, err: err} // distinct indices — no mutex needed
	})
	// Surface the first error if any. We don't fail-fast on partial
	// success because a single asset_id with no recent trades is
	// not an error condition — it just contributes zero rows.
	var firstErr error
	var observedAt time.Time
	var stale bool
	type pairKey struct{ base, quote string }
	seen := make(map[pairKey]struct{})
	merged := make([]Market, 0, limit)
	for _, r := range results {
		if r.err != nil {
			if firstErr == nil {
				firstErr = r.err
			}
			// A dropped leg means the composite is missing whatever
			// that asset_id would have contributed — that's exactly
			// what `stale` exists to signal, so a failed leg taints
			// the composite the same as a stale one.
			stale = true
			continue
		}
		// Oldest observed-at across the contributing legs; any
		// stale leg makes the composite stale.
		if !r.observedAt.IsZero() && (observedAt.IsZero() || r.observedAt.Before(observedAt)) {
			observedAt = r.observedAt
		}
		stale = stale || r.stale
		for _, m := range r.rows {
			k := pairKey{base: m.Base, quote: m.Quote}
			if _, dup := seen[k]; dup {
				continue
			}
			seen[k] = struct{}{}
			merged = append(merged, m)
		}
	}
	if firstErr != nil && len(merged) == 0 {
		return nil, time.Time{}, false, firstErr
	}
	sortMergedMarkets(merged, order)
	if len(merged) > limit {
		merged = merged[:limit]
	}
	return merged, observedAt, stale, nil
}

// sortMergedMarkets orders a fan-out's merged rows as the single-asset SQL path does
// (buildSourceMarketsQuery / distinctPairsCommon), so `order_by` behaves the same for an asset_id
// and a slug. Volume order treats NULL as zero and ties break ascending by (base, quote), matching
// `COALESCE(vol_24h_num, 0) DESC, base||quote ASC`.
func sortMergedMarkets(merged []Market, order timescale.MarketsOrder) {
	switch order {
	case timescale.MarketsOrderPair:
		sort.SliceStable(merged, func(i, j int) bool {
			if merged[i].Base != merged[j].Base {
				return merged[i].Base < merged[j].Base
			}
			return merged[i].Quote < merged[j].Quote
		})
	default: // MarketsOrderVolume24hDesc
		sort.SliceStable(merged, func(i, j int) bool {
			cmp := compareVolumeUSD(merged[i].Volume24hUSD, merged[j].Volume24hUSD)
			if cmp != 0 {
				return cmp > 0
			}
			if merged[i].Base != merged[j].Base {
				return merged[i].Base < merged[j].Base
			}
			return merged[i].Quote < merged[j].Quote
		})
	}
}

// compareVolumeUSD compares two volume_24h_usd wire strings as
// decimals (never float64 — ADR-0003) and returns -1/0/1. A nil
// pointer (no recent volume) compares as zero, matching the SQL
// path's `COALESCE(vol_24h_num, 0)`.
func compareVolumeUSD(a, b *string) int {
	toRat := func(s *string) *big.Rat {
		if s == nil {
			return new(big.Rat)
		}
		r, ok := new(big.Rat).SetString(*s)
		if !ok {
			return new(big.Rat)
		}
		return r
	}
	return toRat(a).Cmp(toRat(b))
}

// adjustListingPrice is the one path a listing row's last_price takes to the wire: scam-issuer
// withholding, then dex-nonstandard-decimals normalization (docs/operations/runbooks/dex.md).
//
// Withholding returns nil: the row still lists (trades and volume are activity, not a price) but
// has no last_price, matching the 404 from /v1/price, /v1/vwap and /v1/twap (see scamWithheld).
// `surface` labels the metric. A nil or empty lastPrice passes through, and lastPrice is returned
// unchanged (same pointer) when decimals match: the CAGG's NUMERIC::text differs from
// [ratToDecimal]'s fixed 10-digit rendering, so reformatting would change bytes for every 7dp pair.
func (s *Server) adjustListingPrice(ctx context.Context, base, quote canonical.Asset, lastPrice *string, surface string) *string {
	if lastPrice == nil || *lastPrice == "" {
		return lastPrice
	}
	if scamWithheld(ctx, s.Scam, base, quote, surface) {
		return nil
	}
	baseDec := aggregate.ResolveDecimals(s.NonstandardDecimals, base)
	quoteDec := aggregate.ResolveDecimals(s.NonstandardDecimals, quote)
	if baseDec == quoteDec {
		return lastPrice
	}
	raw := ratFromDecimal(*lastPrice)
	if raw == nil {
		return lastPrice
	}
	adjusted := ratToDecimal(aggregate.AdjustPrice(raw, baseDec, quoteDec), ohlcPriceDigits)
	return &adjusted
}

// adjustListingPriceStrings is [Server.adjustListingPrice] for callers holding only the wire-shape
// base/quote strings. They come from canonical.Asset.String(), so a parse failure is defensive-only
// and fails open, returning lastPrice unchanged rather than dropping the row or 500ing the listing.
func (s *Server) adjustListingPriceStrings(ctx context.Context, baseRaw, quoteRaw string, lastPrice *string, surface string) *string {
	if lastPrice == nil || *lastPrice == "" {
		return lastPrice
	}
	base, err := canonical.ParseAsset(baseRaw)
	if err != nil {
		return lastPrice
	}
	quote, err := canonical.ParseAsset(quoteRaw)
	if err != nil {
		return lastPrice
	}
	return s.adjustListingPrice(ctx, base, quote, lastPrice, surface)
}
