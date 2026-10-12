package timescale

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math/big"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// VWAPUSDFXResolver implements USDVolumeFXResolver against prices_1m: an on-chain quote asset's latest VWAP against any operator-declared USD peg, treating the peg as exactly $1, cached per (asset, 1-minute bucket).
type VWAPUSDFXResolver struct {
	store *Store

	// usdPegs is the operator-declared classic USD-peg list; the resolver queries prices_1m for
	// `<asset>/<peg>` for each until one returns a row.
	usdPegs []string

	// pegForms is [usdPegs] made alias-complete: each classic peg plus its SAC wrapper, bound to
	// `quote_asset = ANY(...)` by [VWAPUSDFXResolver.queryDirectLeg]. A pool quoting in the USDC SAC
	// is the same declared dollar, so the exact and FX tiers must agree on the set.
	//
	// Built by [usdPegForms] from `[supply].sac_wrappers`, not [canonical.AssetAliases]: that
	// registry is installed only by the api and aggregator binaries, so the indexer and
	// `stellarindex-ops` (which write `usd_volume`) would silently collapse to the classic form.
	pegForms []string

	// pegAssets and xlmForms are the quote spelling sets the direct and the
	// bridged market are measured over by [VWAPUSDFXResolver.substanceAllows].
	pegAssets []canonical.Asset
	xlmForms  []canonical.Asset

	// substanceGate holds a rate for a classic or Soroban asset to the
	// published-price substance floor of the market it came from.
	substanceGate bool

	// freshness is the maximum allowable (now - VWAP timestamp).
	// Entries older than this return ok=false rather than letting
	// a stale rate land in a fresh trade's usd_volume.
	freshness time.Duration

	// bridgeFreshness is the staleness bound for the tier-3b XLM bridge leg, deliberately much wider
	// than `freshness`: it prices thin tokens trading a few dollars a day, and the alternative to a
	// stale rate is NULL, which drops the trade from every usd_volume aggregate. The error admitted
	// is bounded because these are sub-cent valuations.
	bridgeFreshness time.Duration

	// cacheTTL caps how long a cached rate is valid before re-querying. Default 5 min.
	cacheTTL time.Duration

	clock func() time.Time

	mu    sync.RWMutex
	cache map[fxCacheKey]fxCacheEntry
	// sweepAt is the cache size that triggers the next sweep; it doubles past the survivors so a
	// resident set of fresh entries is not rescanned on every insert.
	sweepAt int
}

// fxCacheSweepThreshold bounds the in-memory cache. The key space is (asset, 1-minute bucket)
// including negative results, so a long backfill would otherwise grow the map without bound on the
// trade-insert hot path. Past this size storeCache sweeps entries beyond their TTL, which
// lookupCache ignores anyway, so correctness is unchanged. Steady-state live ingest never reaches it.
const fxCacheSweepThreshold = 8192

// defaultBridgeFreshness is how stale a tier-3b XLM bridge leg may be.
// See [VWAPUSDFXResolver.bridgeFreshness] for why this is a day rather
// than the direct market's hour.
const defaultBridgeFreshness = 24 * time.Hour

// fxCacheKey is (asset.String(), 1-minute floor of `at`). Two
// trades within the same minute against the same asset share a
// resolved rate — same as the CAGG's natural granularity.
type fxCacheKey struct {
	asset    string
	bucketMs int64
}

type fxCacheEntry struct {
	rate     string // empty when no rate available
	cachedAt time.Time
}

// VWAPUSDFXResolverOptions tunes the resolver.
type VWAPUSDFXResolverOptions struct {
	// USDPegs is the operator's classic USD-peg list in "CODE-ISSUER" form, the same value
	// [USDVolumeQuoteSpec] consumes. Pegs are queried in order, first match wins; empty makes
	// the resolver a no-op (USDPriceAt returns ok=false).
	USDPegs []string

	// PegSACWrappers is the operator's `[supply].sac_wrappers` map (SAC C-strkey -> classic
	// "CODE:ISSUER"), the same value [NewUSDVolumeQuoteSpec] consumes. A wrapper pointing at a
	// declared peg makes that SAC a peg form too (see [VWAPUSDFXResolver.pegForms]). Optional.
	PegSACWrappers map[string]string

	// Freshness is the max staleness of a returned rate: 0 inherits the default 1h, positive
	// overrides it, negative disables the check (tests, per-minute-cadence sources).
	Freshness time.Duration

	// BridgeFreshness is the max staleness for the tier-3b bridge leg, with the same sentinels:
	// 0 inherits [defaultBridgeFreshness], negative disables the bound.
	BridgeFreshness time.Duration

	// CacheTTL bounds the in-memory cache. Default 5 min.
	CacheTTL time.Duration

	// DisableSubstanceGate skips [VWAPUSDFXResolver.substanceAllows], for
	// tests that isolate the rate lookup itself. Production wiring never sets it.
	DisableSubstanceGate bool

	// Clock is the time source. Override in tests.
	Clock func() time.Time
}

// Compile-time conformance check.
var _ USDVolumeFXResolver = (*VWAPUSDFXResolver)(nil)

// NewVWAPUSDFXResolver constructs the resolver.
func NewVWAPUSDFXResolver(store *Store, opts VWAPUSDFXResolverOptions) (*VWAPUSDFXResolver, error) {
	if store == nil {
		return nil, errors.New("timescale: VWAPUSDFXResolver: store is required")
	}
	// 0 → default 1h; negative → disabled (sentinel 0
	// inside the resolver so the runtime check below can stay
	// `freshness > 0`); positive → use as-is.
	switch {
	case opts.Freshness < 0:
		opts.Freshness = 0
	case opts.Freshness == 0:
		opts.Freshness = time.Hour
	}
	switch {
	case opts.BridgeFreshness < 0:
		opts.BridgeFreshness = 0
	case opts.BridgeFreshness == 0:
		opts.BridgeFreshness = defaultBridgeFreshness
	}
	if opts.CacheTTL == 0 {
		opts.CacheTTL = 5 * time.Minute
	}
	if opts.Clock == nil {
		opts.Clock = time.Now
	}
	// Defensive copy — caller may mutate their slice after.
	pegs := make([]string, len(opts.USDPegs))
	copy(pegs, opts.USDPegs)
	pegForms, err := usdPegForms(pegs, opts.PegSACWrappers)
	if err != nil {
		return nil, err
	}
	// An unparseable peg form names no canonical asset, so it cannot hold
	// a market to measure; with none left the direct market fails the gate.
	pegAssets := make([]canonical.Asset, 0, len(pegForms))
	for _, form := range pegForms {
		if a, perr := canonical.ParseAsset(form); perr == nil {
			pegAssets = append(pegAssets, a)
		}
	}
	xlmSAC, err := canonical.NewSorobanAsset(canonical.NativeSACContractID())
	if err != nil {
		return nil, fmt.Errorf("timescale: VWAPUSDFXResolver: XLM SAC: %w", err)
	}
	return &VWAPUSDFXResolver{
		store:           store,
		usdPegs:         pegs,
		pegForms:        pegForms,
		pegAssets:       pegAssets,
		xlmForms:        []canonical.Asset{canonical.NativeAsset(), xlmSAC},
		substanceGate:   !opts.DisableSubstanceGate,
		freshness:       opts.Freshness,
		bridgeFreshness: opts.BridgeFreshness,
		cacheTTL:        opts.CacheTTL,
		clock:           opts.Clock,
		cache:           make(map[fxCacheKey]fxCacheEntry),
	}, nil
}

// usdPegForms expands the classic USD-peg list into the asset FORMS a prices_1m `quote_asset` may
// carry for that peg: each declared peg plus its declared SAC wrapper. The membership rule matches
// [NewUSDVolumeQuoteSpec]; a peg the exact tier honours but this tier cannot see is a silent
// coverage hole. A malformed WRAPPER is an error because dropping it would under-count volume.
// Order is deterministic so the bound array and query plan are stable.
func usdPegForms(classicPegs []string, sacWrappers map[string]string) ([]string, error) {
	forms := make([]string, 0, len(classicPegs))
	seen := make(map[string]struct{}, len(classicPegs))
	add := func(form string) {
		if _, dup := seen[form]; dup {
			return
		}
		seen[form] = struct{}{}
		forms = append(forms, form)
	}
	pegged := make(map[string]struct{}, len(classicPegs))
	for _, raw := range classicPegs {
		form := raw
		if a, err := canonical.ParseAsset(raw); err == nil && a.Type == canonical.AssetClassic {
			form = classicKey(a)
		}
		pegged[form] = struct{}{}
		add(form)
	}
	sacIDs := make([]string, 0, len(sacWrappers))
	for sacID := range sacWrappers {
		sacIDs = append(sacIDs, sacID)
	}
	sort.Strings(sacIDs)
	for _, sacID := range sacIDs {
		asset, err := canonical.ParseAsset(sacWrappers[sacID])
		if err != nil {
			return nil, fmt.Errorf("usd-volume fx resolver: sac_wrapper for %s (%q): %w", sacID, sacWrappers[sacID], err)
		}
		if _, ok := pegged[classicKey(asset)]; ok {
			add(sacID)
		}
	}
	return forms, nil
}

// USDPriceAt implements [USDVolumeFXResolver]: the USD price of `asset` at-or-before `at`, each configured peg treated as exactly $1.
func (r *VWAPUSDFXResolver) USDPriceAt(ctx context.Context, asset canonical.Asset, at time.Time) (string, bool, error) {
	// Fiat resolves from fx_quotes (see [VWAPUSDFXResolver.usdPriceForFiat]). This MUST precede
	// the peg check: pricing EUR needs no Stellar USD-peg market.
	if asset.Type == canonical.AssetFiat {
		return r.usdPriceForFiat(ctx, asset, at)
	}
	if len(r.usdPegs) == 0 {
		return "", false, nil
	}
	// Floor `at` to the minute for cache-key stability — matches
	// the prices_1m CAGG's natural resolution.
	bucket := at.UTC().Truncate(time.Minute)
	key := fxCacheKey{asset: asset.String(), bucketMs: bucket.UnixMilli()}

	if rate, ok := r.lookupCache(key); ok {
		if rate == "" {
			return "", false, nil
		}
		return rate, true, nil
	}

	rate, err := r.resolveRate(ctx, asset, at)
	if err != nil {
		return "", false, err
	}
	if rate == "" {
		r.storeCache(key, fxCacheEntry{rate: "", cachedAt: r.clock()})
		return "", false, nil
	}
	// Postgres NUMERIC::text keeps the column's full scale (`1.085000000000000000000`); trim the
	// padding so consumers see the canonical decimal form. The value is mathematically unchanged.
	rate = trimNumericText(rate)
	r.storeCache(key, fxCacheEntry{rate: rate, cachedAt: r.clock()})
	return rate, true, nil
}

// resolveRate runs tiers 3a and 3b for a non-fiat asset and returns the
// first rate whose market clears [VWAPUSDFXResolver.substanceAllows], or
// "" when neither does.
func (r *VWAPUSDFXResolver) resolveRate(ctx context.Context, asset canonical.Asset, at time.Time) (string, error) {
	rate, observedAt, err := r.queryDB(ctx, asset, at)
	if err != nil {
		return "", err
	}
	if rate != "" && !r.fresh(observedAt, at, r.freshness) {
		// Direct market exists but its most recent VWAP is too old to
		// price this trade. Discard it and let the bridge try — a
		// current XLM-routed rate beats a stale direct one, and before
		// tier 3b existed this simply returned NULL.
		rate = ""
	}
	if rate != "" {
		ok, err := r.substanceAllows(ctx, asset, r.pegAssets, "direct", at)
		if err != nil {
			return "", err
		}
		if ok {
			return rate, nil
		}
	}
	// Tier 3b — no usable direct <asset>/<peg> rate. Most Stellar
	// tokens route their liquidity through XLM rather than a
	// stablecoin, so try <asset>/XLM x XLM/USD before giving up.
	// The bridge enforces its own (wider) freshness internally.
	rate, err = r.bridgeViaXLM(ctx, asset, at)
	if err != nil || rate == "" {
		return "", err
	}
	ok, err := r.substanceAllows(ctx, asset, r.xlmForms, "xlm", at)
	if err != nil || !ok {
		return "", err
	}
	return rate, nil
}

// ThinMarketRefused reports whether asset had a candidate rate at `at` that the substance gate refused and none it allowed, from verdicts cached by the last USDPriceAt call; usd_volume stays NULL either way.
func (r *VWAPUSDFXResolver) ThinMarketRefused(asset canonical.Asset, at time.Time) bool {
	if !r.substanceGate || isXLMAsset(asset) {
		return false
	}
	asOf := at.UTC().Truncate(time.Hour).UnixMilli()
	refused := false
	for _, market := range []string{"direct", "xlm"} {
		verdict, ok := r.lookupCache(fxCacheKey{asset: "substance:" + market + ":" + asset.String(), bucketMs: asOf})
		if !ok {
			continue
		}
		if verdict != "" {
			return false
		}
		refused = true
	}
	return refused
}

// ─── the substance gate ──────────────────────────────────────────────

// The valuation substance floor: pricingguard's default serve floor,
// mirrored here because pricingguard imports this package. Lockstep with
// pricingguard's own verdict is pinned by
// TestValuationSubstanceOK_MatchesThePublishedPriceGate.
const (
	valuationSubstanceMinVolumeUSD = 1000           // pricingguard.DefaultSubstanceMinVolumeUSD
	valuationSubstanceMinBuckets   = 20             // pricingguard.DefaultSubstanceMinBuckets
	valuationSubstanceMinSpan      = 6 * time.Hour  // pricingguard.DefaultSubstanceMinSpan
	valuationSubstanceWindow       = 24 * time.Hour // pricingguard.DefaultSubstanceWindow
	// valuationSubstanceMinHourBuckets is pricingguard's hourGrainPolicy
	// of the minute floor: ceil(20/60) raised to 2 by the 6h span.
	valuationSubstanceMinHourBuckets = 2
)

// valuationSubstanceGrain is pricingguard's policyAt: minute grain while
// prices_1m is guaranteed to hold the window, hour grain beyond.
func valuationSubstanceGrain(age time.Duration) HistoryGranularity {
	if age <= PriceAtMinuteRungMaxAge {
		return Granularity1m
	}
	return Granularity1h
}

// valuationSubstanceOK reports whether a market measured at grain g
// clears the valuation substance floor. Volume compares exactly (ADR-0003).
func valuationSubstanceOK(sub MarketSubstance, g HistoryGranularity) bool {
	minBuckets := int64(valuationSubstanceMinBuckets)
	if g == Granularity1h {
		minBuckets = valuationSubstanceMinHourBuckets
	}
	if sub.Buckets < minBuckets || time.Duration(sub.SpanSeconds)*time.Second < valuationSubstanceMinSpan {
		return false
	}
	vol, ok := new(big.Rat).SetString(sub.VolumeUSD)
	return ok && vol.Cmp(big.NewRat(valuationSubstanceMinVolumeUSD, 1)) >= 0
}

// substanceAllows reports whether the market a classic or Soroban asset's rate came from (every
// spelling of `asset` against `quotes`) cleared the published-price substance floor over the window
// ending at the trade's hour. Without it, two wallets swapping cents would set a rate that values
// every trade quoted in the asset, however large. XLM and off-chain assets are anchors, not gated.
//
// The window ends at the hour so one cached verdict per (market, asset, hour) serves every trade
// in it, using only buckets that closed before the trade. Measurement errors propagate uncached.
func (r *VWAPUSDFXResolver) substanceAllows(
	ctx context.Context, asset canonical.Asset, quotes []canonical.Asset, market string, at time.Time,
) (bool, error) {
	if !r.substanceGate || isXLMAsset(asset) ||
		(asset.Type != canonical.AssetClassic && asset.Type != canonical.AssetSoroban) {
		return true, nil
	}
	if len(quotes) == 0 {
		return false, nil
	}
	asOf := at.UTC().Truncate(time.Hour)
	key := fxCacheKey{asset: "substance:" + market + ":" + asset.String(), bucketMs: asOf.UnixMilli()}
	if verdict, ok := r.lookupCache(key); ok {
		return verdict != "", nil
	}
	grain := valuationSubstanceGrain(r.clock().Sub(asOf))
	sub, err := r.store.PairMarketSubstanceAt(
		ctx, canonical.AssetAliases(asset), quotes, asOf, valuationSubstanceWindow, grain)
	if err != nil {
		return false, err
	}
	ok := valuationSubstanceOK(sub, grain)
	verdict := ""
	if ok {
		verdict = "1"
	}
	r.storeCache(key, fxCacheEntry{rate: verdict, cachedAt: r.clock()})
	return ok, nil
}

// fiatUSDRateScale is the decimal scale for rendering a fiat->USD rate from its exact *big.Rat.
// Rates span ~1.34 (GBP) to ~3.8e-5 (VND), so 18 places keeps 13+ significant digits for the
// weakest currency, well beyond the 8 decimals of `usd_volume`. [trimNumericText] drops the padding.
const fiatUSDRateScale = 18

// usdPriceForFiat resolves the USD price of one fiat unit from `fx_quotes`; prices_1m holds crypto
// only, so without this every CEX pair quoted in a non-USD currency would get `usd_volume` NULL.
// The rate is an exact *big.Rat from [fxSnapFromRows]; the float-derived `inverse_usd` column is
// NOT used (ADR-0003). Two deviations from the prices_1m path:
//
//   - Freshness is NOT applied: fx_quotes buckets are daily and weekday-only, so a 1h bound would
//     reject every fiat rate; [fxQuotesSnapLookback] (7 days) bounds it instead.
//   - The cache key floors to the UTC day, not the minute: buckets sit at UTC midnight, and a
//     minute key would multiply misses by 1440.
func (r *VWAPUSDFXResolver) usdPriceForFiat(ctx context.Context, asset canonical.Asset, at time.Time) (string, bool, error) {
	// USD is the anchor rate_usd is expressed against — exactly 1 by
	// definition, and fx_quotes holds no USD row to look up.
	if asset.Code == usdFiatCode {
		return "1", true, nil
	}
	key := fxCacheKey{
		asset:    asset.String(),
		bucketMs: at.UTC().Truncate(24 * time.Hour).UnixMilli(),
	}
	if rate, ok := r.lookupCache(key); ok {
		if rate == "" {
			return "", false, nil
		}
		return rate, true, nil
	}

	pair := canonical.Pair{Base: asset, Quote: canonical.Asset{Type: canonical.AssetFiat, Code: usdFiatCode}}
	price, _, _, err := r.store.fxQuotesSnapAtOrBefore(ctx, pair, at)
	if err != nil {
		// No quote in the lookback window is a miss, not a failure:
		// negative-cache it and let the trade insert with NULL.
		if errors.Is(err, ErrNoFXQuote) {
			r.storeCache(key, fxCacheEntry{rate: "", cachedAt: r.clock()})
			return "", false, nil
		}
		return "", false, err
	}
	if price == nil || price.Sign() <= 0 {
		r.storeCache(key, fxCacheEntry{rate: "", cachedAt: r.clock()})
		return "", false, nil
	}
	rate := trimNumericText(price.FloatString(fiatUSDRateScale))
	r.storeCache(key, fxCacheEntry{rate: rate, cachedAt: r.clock()})
	return rate, true, nil
}

// fresh reports whether a rate observed at `observedAt` may price a trade at `at` within `window`;
// a non-positive window disables the check. Staleness is measured against the TRADE timestamp, not
// wall-clock, so replay and backfill keep a contemporaneous FX anchor that wall-clock would reject.
func (r *VWAPUSDFXResolver) fresh(observedAt, at time.Time, window time.Duration) bool {
	if window <= 0 {
		return true
	}
	return at.Sub(observedAt) <= window
}

// lookupCache returns (rate, true) when the cache has a fresh
// entry, otherwise ("", false). Empty rate means "already
// resolved as no-rate-available" — caller still treats that as
// ok=false at the boundary.
func (r *VWAPUSDFXResolver) lookupCache(key fxCacheKey) (string, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	entry, ok := r.cache[key]
	if !ok {
		return "", false
	}
	if r.clock().Sub(entry.cachedAt) > r.cacheTTL {
		return "", false
	}
	return entry.rate, true
}

func (r *VWAPUSDFXResolver) storeCache(key fxCacheKey, entry fxCacheEntry) {
	r.mu.Lock()
	defer r.mu.Unlock()
	// Drop entries past their TTL (lookupCache already treats them as
	// misses, so this is correctness-neutral) once the map is large.
	if len(r.cache) >= max(fxCacheSweepThreshold, r.sweepAt) {
		now := r.clock()
		for k, e := range r.cache {
			if now.Sub(e.cachedAt) > r.cacheTTL {
				delete(r.cache, k)
			}
		}
		r.sweepAt = 2 * len(r.cache)
	}
	r.cache[key] = entry
}

// trimNumericText strips trailing zeros from a Postgres NUMERIC text (`1.085000` -> `1.085`,
// `1.000` -> `1`, `0.000` -> `0`) so consumers need not be scale-aware.
func trimNumericText(s string) string {
	if !strings.ContainsRune(s, '.') {
		return s
	}
	// Strip trailing zeros, then strip a lone trailing dot.
	s = strings.TrimRight(s, "0")
	s = strings.TrimRight(s, ".")
	if s == "" || s == "-" || s == "-0" {
		return "0"
	}
	return s
}

// ─── tier 3b: the XLM bridge ─────────────────────────────────────────

// bridgeLegMinUSDVolume is the dust floor a bridge bucket must clear before its VWAP may value
// another trade. A few-stroop fill has ~100% quantisation error, and a bucket holding ONLY dust
// would set the rate for every trade bridging through the pair. $0.01 matches the OHLC-extremes
// floor so the dust defences agree (docs/operations/finding-dust-trades-set-chart-extremes.md).
const bridgeLegMinUSDVolume = "0.01"

// bridgeRateSigDigits is how many SIGNIFICANT digits a bridged rate is rendered with. prices_1m
// stores raw ratios, so an 18-decimal token priced against XLM's 7 sits near 1e-12 and a fixed
// 18-place render would leave barely six significant figures. 25 digits exceeds what usd_volume's
// 8-decimal render can express, and [trimNumericText] removes the padding.
const bridgeRateSigDigits = 25

// bridgeRateMaxScale caps the decimal places rateScaleFor will ask for,
// so a pathological or hostile token declaration cannot make us render
// an unbounded string on the trade-insert hot path.
const bridgeRateMaxScale = 80

// bridgeViaXLM prices `asset` in USD through XLM when no direct <asset>/<peg> market exists:
// USD per asset = (XLM per asset) x (USD per XLM). It returns "" when there is no XLM market
// either, the end of the waterfall.
//
// The XLM/USD leg goes back through USDPriceAt to share its cache (one query per minute, not per
// bridged asset) and freshness check; recursion stops at depth 2 via the isXLMAsset guard. A
// bridged rate is fresh iff BOTH legs are: the XLM leg by USDPriceAt, the asset leg by the SQL
// bound in queryXLMLeg.
func (r *VWAPUSDFXResolver) bridgeViaXLM(ctx context.Context, asset canonical.Asset, at time.Time) (string, error) {
	if isXLMAsset(asset) {
		// Base case. XLM resolves directly against a peg or not at
		// all; bridging it through itself would be circular.
		return "", nil
	}
	// Freshness for the asset leg is applied as the SQL lower bound in
	// queryXLMLeg, so a row returned here is fresh by construction.
	xlmPerAsset, err := r.queryXLMLeg(ctx, asset, at)
	if err != nil || xlmPerAsset == nil {
		return "", err
	}
	usdPerXLMStr, ok, err := r.USDPriceAt(ctx, canonical.NativeAsset(), at)
	if err != nil || !ok || usdPerXLMStr == "" {
		return "", err
	}
	rate, ok := bridgeRate(xlmPerAsset, usdPerXLMStr)
	if !ok {
		return "", nil
	}
	return rate, nil
}

// bridgeRate multiplies the two bridge legs into a USD-per-asset rate string. It is split out of
// [VWAPUSDFXResolver.bridgeViaXLM] so the money arithmetic is testable without a database. It
// returns ok=false on an unparseable or non-positive leg rather than put a bad rate in usd_volume.
func bridgeRate(xlmPerAsset *big.Rat, usdPerXLMText string) (string, bool) {
	if xlmPerAsset == nil || xlmPerAsset.Sign() <= 0 {
		return "", false
	}
	usdPerXLM, ok := new(big.Rat).SetString(usdPerXLMText)
	if !ok || usdPerXLM.Sign() <= 0 {
		return "", false
	}
	rate := new(big.Rat).Mul(xlmPerAsset, usdPerXLM)
	return rate.FloatString(rateScaleFor(rate, bridgeRateSigDigits)), true
}

// rateScaleFor returns the decimal places needed to render r with `sig` significant digits.
// big.Rat renders only at a fixed scale, but bridged rates span ~1e0 to below 1e-12, so a fixed
// scale truncates small rates or bloats large ones. Counting leading zeros keeps relative
// precision constant and stays float-free (ADR-0003), unlike log10.
func rateScaleFor(r *big.Rat, sig int) int {
	if r == nil || r.Sign() == 0 {
		return sig
	}
	x := new(big.Rat).Abs(r)
	one := big.NewRat(1, 1)
	ten := big.NewRat(10, 1)
	leadingZeros := 0
	for x.Cmp(one) < 0 && leadingZeros < bridgeRateMaxScale {
		x.Mul(x, ten)
		leadingZeros++
	}
	scale := leadingZeros + sig
	if scale > bridgeRateMaxScale {
		return bridgeRateMaxScale
	}
	return scale
}

// xlmLegRate turns a prices_1m VWAP row into XLM-per-asset, inverting when the pair is stored
// XLM-first. It is split out for testing: a missed inversion yields a rate wrong by vwap^2.
func xlmLegRate(vwapText string, inverted bool) (*big.Rat, bool) {
	vwap, ok := new(big.Rat).SetString(vwapText)
	if !ok || vwap.Sign() <= 0 {
		return nil, false
	}
	if inverted {
		// Stored as XLM/<asset>, i.e. asset-per-XLM. We need
		// XLM-per-asset.
		return new(big.Rat).Inv(vwap), true
	}
	return vwap, true
}

// queryXLMLeg returns XLM-per-unit-of-asset at-or-before `at`, from whichever orientation the pair
// is stored in. Trades keep the venue's base/quote ordering (see [canonical.Trade]), so one token
// can have both TOKEN/XLM and XLM/TOKEN buckets; the latter is asset-per-XLM and must be inverted.
//
// Buckets below [bridgeLegMinUSDVolume] are excluded so a dust-only bucket cannot set the rate. The
// lower bucket bound stops a miss walking prices_1m back to genesis, as in queryDirectLeg.
func (r *VWAPUSDFXResolver) queryXLMLeg(ctx context.Context, asset canonical.Asset, at time.Time) (*big.Rat, error) {
	// Both on-chain wire forms of XLM — the classic `native` type and
	// the SAC wrapper a Soroban pool holds. Mirrors [isXLMAsset].
	xlmForms := []string{canonical.NativeAsset().String(), canonical.NativeSACContractID()}
	args := []any{
		asset.String(),
		xlmForms,
		at.UTC(),
		bridgeLegMinUSDVolume,
	}
	// The lower bound must live INSIDE each UNION branch: chunk exclusion is decided per scan,
	// so a bound on the outer query still lets both branches walk back to genesis on a miss.
	// The vwap > 0 guard is inlined for the same reason.
	lowerBound := ""
	if r.bridgeFreshness > 0 {
		lowerBound = `
		            AND bucket     >= $5`
		args = append(args, at.UTC().Add(-r.bridgeFreshness))
	}
	row := r.store.db.QueryRowContext(ctx, xlmLegQuery(lowerBound), args...)
	var (
		bucket   time.Time
		vwapText string
		inverted bool
	)
	if err := row.Scan(&bucket, &vwapText, &inverted); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("timescale: VWAPUSDFXResolver XLM leg: %w", err)
	}
	vwap, ok := xlmLegRate(vwapText, inverted)
	if !ok {
		return nil, nil
	}
	return vwap, nil
}

// xlmLegQuery is queryXLMLeg's SQL. Each UNION arm is parenthesised because Postgres rejects a
// bare ORDER BY/LIMIT inside one; the per-arm LIMIT 1 is an index-ordered walk that stops at the
// first qualifying bucket. Ties resolve to the form listed first in $2, then to the
// stored-as-(asset, XLM) arm, never to scan order.
func xlmLegQuery(lowerBound string) string {
	return fmt.Sprintf(`
		SELECT bucket, vwap::text, inverted
		  FROM (
		        (SELECT bucket, vwap, false AS inverted
		           FROM prices_1m
		          WHERE base_asset  = $1
		            AND quote_asset = ANY($2)
		            AND bucket     <= $3
		            AND volume_usd >= $4::numeric
		            AND vwap        > 0%[1]s
		          ORDER BY bucket DESC, array_position($2::text[], quote_asset)
		          LIMIT 1)
		        UNION ALL
		        (SELECT bucket, vwap, true AS inverted
		           FROM prices_1m
		          WHERE base_asset  = ANY($2)
		            AND quote_asset = $1
		            AND bucket     <= $3
		            AND volume_usd >= $4::numeric
		            AND vwap        > 0%[1]s
		          ORDER BY bucket DESC, array_position($2::text[], base_asset)
		          LIMIT 1)
		       ) legs
		 ORDER BY bucket DESC, inverted
		 LIMIT 1
	`, lowerBound)
}

// ─── tier 3a: the direct <asset>/<peg> market ────────────────────────

// directLegMinQuoteVolume is the dust floor a direct `<asset>/<peg>` bucket must clear before its
// VWAP may value another trade, in QUOTE units (the peg, treated as exactly $1). It is the same
// $0.01 as [bridgeLegMinUSDVolume] so the dust defences agree.
//
// It cannot use `volume_usd` as the bridge leg does: that sums `usd_volume`, which this resolver
// itself writes, so a never-priced pair could never clear it (prices_1m coalesces NULL to 0).
// `vwap * volume_priced` is the sum of quote_amount over trades with both legs > 0. Not
// `volume_quote`, which counts zero-base trades and would let a quote-only row carry a dust fill
// over the floor. Matched quotes are operator pegs, so it is dollars.
const directLegMinQuoteVolume = "0.01"

// pegQuoteScaleDenominator converts a prices_1m `vwap * volume_priced` product from raw stroops to
// whole quote units. Amounts are stored unscaled (see [canonical.Amount]) and classic pegs are fixed
// at [stellarClassicDecimals] places. A unit test pins it to scaleDenominator(stellarClassicDecimals).
const pegQuoteScaleDenominator = 10_000_000

// queryDB resolves `<asset>/<peg>` out of prices_1m at-or-before `at`, trying each of the asset's
// canonical FORMS in turn and returning the VWAP and bucket of the first that hits.
//
// XLM's USD markets are split by venue across its three identities (`native`, `crypto:XLM`, its
// SAC), so both asset and peg sides loop their forms ([VWAPUSDFXResolver.pegForms]). The CEX
// `crypto:XLM/fiat:USD` series is out of scope: `fiat:USD` is not a declared peg.
//
// Order is load-bearing: [canonical.AssetAliases] returns the SAC LAST so a thin Soroban pool
// cannot outrank a deep SDEX/CEX book; a set-shaped `base_asset = ANY(forms)` would lose that.
// Freshness is a SQL lower bound (see [VWAPUSDFXResolver.queryDirectLeg]), so the first row is usable.
func (r *VWAPUSDFXResolver) queryDB(ctx context.Context, asset canonical.Asset, at time.Time) (string, time.Time, error) {
	for _, form := range canonical.AssetAliases(asset) {
		vwap, bucket, err := r.queryDirectLeg(ctx, form, at)
		if err != nil {
			return "", time.Time{}, err
		}
		if vwap != "" {
			return vwap, bucket, nil
		}
	}
	return "", time.Time{}, nil
}

// queryDirectLeg does one prices_1m read for `<form>/<peg>` over any peg form in
// [VWAPUSDFXResolver.pegForms], at-or-before `at`. Set semantics (`quote_asset = ANY(...)`) are
// right on this side: every peg form is the same declared dollar, so there is no priority.
//
// Dust floor: it takes the FRESHEST qualifying bucket, so without [directLegMinQuoteVolume] a
// 2-stroop remainder in the newest minute would price every trade in this asset until it aged out.
//
// Lower bound: when freshness is enforced, `bucket >= at - freshness` lets TimescaleDB prune to the
// window's chunks instead of walking prices_1m back to genesis on a miss. USDPriceAt would reject
// older rows anyway. With freshness disabled (0) the scan is unbounded.
func (r *VWAPUSDFXResolver) queryDirectLeg(ctx context.Context, asset canonical.Asset, at time.Time) (string, time.Time, error) {
	args := []any{
		asset.String(),
		r.pegForms,
		at.UTC(),
		directLegMinQuoteVolume,
	}
	if r.freshness > 0 {
		args = append(args, at.UTC().Add(-r.freshness))
	}
	row := r.store.db.QueryRowContext(ctx, directLegQuery(r.freshness > 0), args...)
	var (
		bucket time.Time
		vwap   string
	)
	if err := row.Scan(&bucket, &vwap); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", time.Time{}, nil
		}
		return "", time.Time{}, fmt.Errorf("timescale: VWAPUSDFXResolver query: %w", err)
	}
	return vwap, bucket, nil
}

// directLegQuery is queryDirectLeg's SQL; bounded adds the `$5` freshness
// floor. Two peg forms printing in one bucket resolve to the form listed
// first in $2 (classic before SAC), never to scan order.
func directLegQuery(bounded bool) string {
	q := fmt.Sprintf(`
		SELECT bucket, vwap::text
		  FROM prices_1m
		 WHERE base_asset  = $1
		   AND quote_asset = ANY($2)
		   AND bucket     <= $3
		   AND vwap        > 0
		   AND vwap * volume_priced / %d::numeric >= $4::numeric`, pegQuoteScaleDenominator)
	if bounded {
		q += `
		   AND bucket     >= $5`
	}
	return q + `
		 ORDER BY bucket DESC, array_position($2::text[], quote_asset)
		 LIMIT 1
	`
}

// InstallUSDVolumeResolution wires BOTH usd_volume tiers (USDVolumeQuoteSpec and VWAPUSDFXResolver) onto a store in one call; every trade-writing process must install both.
func InstallUSDVolumeResolution(store *Store, classicUSDPegs []string, sacWrappers map[string]string) error {
	if store == nil {
		return errors.New("timescale: InstallUSDVolumeResolution: store is required")
	}
	// Set even on the no-pegs path below: reDeriveNullVolumeGuard checks "was Install called", so
	// a no-pegs re-derive passes while one that forgot Install entirely is refused.
	store.usdVolumeResolutionInstalled = true
	if len(classicUSDPegs) == 0 {
		return nil
	}
	spec, err := NewUSDVolumeQuoteSpec(classicUSDPegs, sacWrappers)
	if err != nil {
		return fmt.Errorf("usd-volume quote spec: %w", err)
	}
	store.SetUSDVolumeQuoteSpec(spec)

	fxResolver, err := NewVWAPUSDFXResolver(store, VWAPUSDFXResolverOptions{
		USDPegs: classicUSDPegs,
		// Same map the quote spec above consumed, so tiers 1/2 and tiers
		// 3/4 recognise the same peg forms. See [usdPegForms].
		PegSACWrappers: sacWrappers,
	})
	if err != nil {
		return fmt.Errorf("usd-volume fx resolver: %w", err)
	}
	store.SetUSDVolumeFXResolver(fxResolver)
	return nil
}
