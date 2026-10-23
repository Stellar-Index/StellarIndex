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

// DexSourceNames returns every Exchange/DEX source, sorted; exported so the prewarm computes the SAME slice as the handler and keys match.
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

// CexSourceNames returns every CEX source, sorted; exported so the prewarm can warm `/v1/markets?source=<name>`.
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

// MarketsReader is the storage-side interface for /v1/markets and /v1/pairs; returned rows belong to the caller, so a retaining implementation (a cache) must hand out copies.
type MarketsReader interface {
	// DistinctPairsExt returns one page of (base, quote) pairs present in the trades store.
	// Cursor is opaque; empty starts at page 1.
	DistinctPairsExt(ctx context.Context, cursor string, limit int, order timescale.MarketsOrder) ([]Market, string, error)

	// SourceMarkets is DistinctPairsExt narrowed to one source (/v1/markets?source=<name>).
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

	// GetPairsVolumeHistory24hBatch returns per-pair hourly USD-volume buckets for the trailing
	// 24h (?include=sparkline).
	GetPairsVolumeHistory24hBatch(ctx context.Context, pairs [][2]string) (map[string][]timescale.PairVolumePoint, error)

	// FirstTradeBatch returns each pair's first daily bucket (?include=inception).
	FirstTradeBatch(ctx context.Context, pairs [][2]string) (map[string]time.Time, error)
}

// marketsStaleReader is the optional CachedMarketsReader capability reporting observedAt (zero = as_of=now) and stale (past TTL),
// so handleMarkets never stamps as_of=now over old rows while refresh keeps failing.
type marketsStaleReader interface {
	DistinctPairsExtAt(ctx context.Context, cursor string, limit int, order timescale.MarketsOrder) ([]Market, string, time.Time, bool, error)
	SourceMarketsAt(ctx context.Context, source, cursor string, limit int, order timescale.MarketsOrder) ([]Market, string, time.Time, bool, error)
	AssetMarketsAt(ctx context.Context, asset, cursor string, limit int, order timescale.MarketsOrder) ([]Market, string, time.Time, bool, error)
}

// sparklineStaleReader is the optional cached sparkline batch read; stale means the batch came from the stale-while-revalidate
// branch, and the page then carries flags.stale.
type sparklineStaleReader interface {
	GetPairsVolumeHistory24hBatchAt(ctx context.Context, pairs [][2]string) (map[string][]timescale.PairVolumePoint, bool, error)
}

// poolsStaleReader is marketsStaleReader's /v1/pools counterpart; stale means the rows came from
// the cache's stale-while-revalidate branch.
type poolsStaleReader interface {
	AllPoolsStale(ctx context.Context, filter timescale.PoolsFilter, cursor string, limit int, order timescale.MarketsOrder) ([]Pool, string, time.Time, bool, error)
}

// allPools reads through poolsStaleReader when available; an uncached read is live (zero
// observedAt, stale false).
func allPools(ctx context.Context, reader MarketsReader, filter timescale.PoolsFilter, cursor string, limit int, order timescale.MarketsOrder) ([]Pool, string, time.Time, bool, error) {
	if sr, ok := reader.(poolsStaleReader); ok {
		return sr.AllPoolsStale(ctx, filter, cursor, limit, order)
	}
	rows, next, err := reader.AllPools(ctx, filter, cursor, limit, order)
	return rows, next, time.Time{}, false, err
}

// Pool is the /v1/pools entry shape: Market plus a `source`, so one pair on two DEXes is two rows.
type Pool struct {
	Source        string   `json:"source"`
	Base          string   `json:"base"`
	Quote         string   `json:"quote"`
	LastTradeAt   WireTime `json:"last_trade_at"`
	TradeCount24h int64    `json:"trade_count_24h"`
	Volume24hUSD  *string  `json:"volume_24h_usd,omitempty"`
	// VolumeLowerBound: Volume24hUSD excludes trades with no trade-time usd_volume.
	VolumeLowerBound bool `json:"volume_lower_bound,omitempty"`
	// LastPrice is the latest quote-per-base price for THIS pool, so two venues of one pair
	// surface independent prices.
	LastPrice *string `json:"last_price,omitempty"`
}

// handlePools serves GET /v1/pools: DEX/AMM pools only (CEX pairs go through /v1/markets); asset is exclusive with base/quote.
// Asset filters are re-spelled via canonical.ParseAsset (canonicaliseAssetFilter): a bare "XLM" would match nothing and cache an empty
// 200 for 60s.
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

	// DEX-only, so no CEX rows ever appear under "pools".
	dexSources := DexSourceNames()
	if reqSource := r.URL.Query().Get("source"); reqSource != "" {
		// Non-DEX names give an empty list, not a 400, so callers can pass user input through.
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
		// The source= filter matched no DEX; skip the trades-hypertable scan.
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

// Market is the /v1/markets entry; Volume24hUSD is a pointer so null means "unknown" not "0", and LastTradeAt (not BucketCloseAt) is the freshness field.
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
	// LastPrice is the latest quote-per-base price in the trailing 24h, across every source or
	// that source's own with `?source=`. Null when none was observed.
	LastPrice *string `json:"last_price,omitempty"`
	// VolumeHistory24h holds 24 hourly USD-volume buckets, oldest to newest, zero-filled so the
	// length is stable. Populated only with `?include=sparkline` without `?source=`.
	VolumeHistory24h []MarketVolumeBucket `json:"volume_history_24h,omitempty"`
	// FirstTradeAt is the pair's first recorded daily bucket (day precision). Populated only with
	// `?include=inception` without `?source=`.
	FirstTradeAt *WireTime `json:"first_trade_at,omitempty"`
}

// MarketVolumeBucket is one hourly USD-volume sparkline point; volume_usd is a string for precision.
type MarketVolumeBucket struct {
	Hour      WireTime `json:"hour"`
	VolumeUSD string   `json:"volume_usd"`
}

// handleMarkets serves GET /v1/markets; order_by defaults to volume_24h_usd_desc so clients skip ~5K dust pairs.
// asset is exclusive with source; data vendors are refused by sourceFilterOK.
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
			// Not a canonical id: try the catalogue slug; no match is a 400.
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
	// OpenAPI types data as an array, so null would violate the schema.
	if rows == nil {
		rows = []Market{}
	}
	// Same dex-nonstandard-decimals normalization as handlePools.
	for i := range rows {
		rows[i].LastPrice = s.adjustListingPriceStrings(mCtx, rows[i].Base, rows[i].Quote, rows[i].LastPrice, "markets")
	}

	// Opt-in enrichment, default off to avoid ~50KB of sparkline per page.
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
		var hist map[string][]timescale.PairVolumePoint
		var hErr error
		if at, ok := reader.(sparklineStaleReader); ok {
			var sparkStale bool
			hist, sparkStale, hErr = at.GetPairsVolumeHistory24hBatchAt(mCtx, pairs)
			stale = stale || sparkStale
		} else {
			hist, hErr = reader.GetPairsVolumeHistory24hBatch(mCtx, pairs)
		}
		if hErr != nil {
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

// expandSlugToAssetIDs maps a catalogue slug to its Stellar networks[].asset_id values plus crypto:/fiat: tickers; nil if unwired or
// unknown.
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

// fanOutAssetMarkets runs AssetMarkets per expanded asset_id, dedupes on (base, quote), sorts and caps at limit; no pagination (cursors
// don't compose).
// Freshness is conservative: observedAt is the oldest leg, stale the OR of legs; zero/false when the reader cannot report them.
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
	// No fail-fast: an asset_id with no recent trades just contributes zero rows.
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
			// A failed leg leaves the composite incomplete, which is what `stale` signals.
			stale = true
			continue
		}
		// Oldest observed-at across legs; any stale leg makes the composite stale.
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

// sortMergedMarkets mirrors the single-asset SQL order so order_by matches for asset_id and slug: NULL volume is zero, ties by (base,
// quote).
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

// compareVolumeUSD compares two volume_24h_usd strings as decimals (never float64, ADR-0003).
// A nil pointer compares as zero, matching the SQL path's `COALESCE(vol_24h_num, 0)`.
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

// adjustListingPrice is the sole path a listing last_price takes to the wire: scam-issuer withholding (nil; the row still lists,
// as /v1/price 404s) then nonstandard-decimals normalization. Same pointer returned when decimals match: reformatting would
// change bytes for every 7dp pair.
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

// adjustListingPriceStrings is [Server.adjustListingPrice] for wire-shape base/quote strings; a parse failure fails open (lastPrice
// unchanged).
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
