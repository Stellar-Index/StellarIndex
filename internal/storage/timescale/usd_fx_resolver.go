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

// fxCacheSweepThreshold bounds the cache (asset x minute keys, incl. negatives) on the insert hot path; past it storeCache sweeps
// entries beyond their TTL, which lookupCache ignores anyway.
const fxCacheSweepThreshold = 8192

// defaultBridgeFreshness is how stale a tier-3b XLM bridge leg may be; see [VWAPUSDFXResolver.bridgeFreshness] for why a day.
const defaultBridgeFreshness = 24 * time.Hour

// fxCacheKey is (asset.String(), 1-minute floor of `at`), the CAGG's granularity.
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

// usdPegForms expands classic pegs into the asset forms a prices_1m quote_asset may carry (peg plus declared SAC wrapper), matching
// [NewUSDVolumeQuoteSpec]. A malformed wrapper errors since dropping it would under-count volume; order is deterministic.
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

// resolveRate runs tiers 3a and 3b for a non-fiat asset, returning the first rate whose market clears
// [VWAPUSDFXResolver.substanceAllows], else "".
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

// substanceAllows reports whether the asset's market cleared the published-price substance floor over the window ending at the trade's
// hour, so two wallets swapping cents cannot set a rate. XLM and off-chain anchors are not gated; the verdict is cached per (market,
// asset, hour) using only closed buckets, and measurement errors propagate uncached.
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

// fiatUSDRateScale renders fiat->USD rates (~1.34 GBP to ~3.8e-5 VND) from exact *big.Rat; 18 places keeps 13+ significant digits for
// the weakest.
const fiatUSDRateScale = 18

// usdPriceForFiat resolves one fiat unit's USD price from fx_quotes (prices_1m holds crypto only) as an exact *big.Rat; inverse_usd is
// NOT used (ADR-0003). Freshness is [fxQuotesSnapLookback] (daily weekday buckets), and the cache key floors to the UTC day (midnight
// buckets).
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

// fresh reports whether a rate at `observedAt` may price a trade at `at` within `window` (non-positive disables), measured against the
// TRADE time so replay and backfill keep a contemporaneous anchor.
func (r *VWAPUSDFXResolver) fresh(observedAt, at time.Time, window time.Duration) bool {
	if window <= 0 {
		return true
	}
	return at.Sub(observedAt) <= window
}

// lookupCache returns (rate, true) on a fresh entry; an empty rate means "resolved as no rate", which callers treat as ok=false.
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

// bridgeLegMinUSDVolume is the dust floor a bridge bucket must clear before its VWAP values other trades; matches the OHLC-extremes
// floor (docs/operations/finding-dust-trades-set-chart-extremes.md).
const bridgeLegMinUSDVolume = "0.01"

// bridgeRateSigDigits is the SIGNIFICANT digits of a bridged rate: raw ratios reach ~1e-12 (18-decimal token vs XLM's 7), where a fixed
// 18-place render leaves ~6 figures. [trimNumericText] removes padding.
const bridgeRateSigDigits = 25

// bridgeRateMaxScale caps rateScaleFor's decimal places so a hostile token declaration cannot force an unbounded render on the hot path.
const bridgeRateMaxScale = 80

// bridgeViaXLM prices `asset` through XLM when no direct market exists: USD per asset = (XLM per asset) x (USD per XLM); "" if no XLM
// market. The XLM/USD leg goes back through USDPriceAt for its cache and freshness (recursion stops via isXLMAsset); fresh iff both legs
// are.
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

// bridgeRate multiplies the bridge legs into a USD-per-asset string, split out so the money arithmetic is DB-free; ok=false on a bad or
// non-positive leg.
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

// rateScaleFor returns the decimal places to render r with `sig` significant digits; counting leading zeros keeps relative precision
// float-free (ADR-0003).
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

// queryXLMLeg returns XLM-per-asset at-or-before `at` from either stored orientation (TOKEN/XLM, or XLM/TOKEN inverted). Buckets below
// [bridgeLegMinUSDVolume] are excluded; the lower bucket bound stops a miss walking prices_1m to genesis, as in queryDirectLeg.
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

// xlmLegQuery is queryXLMLeg's SQL. Arms are parenthesised (Postgres rejects bare ORDER BY/LIMIT in a UNION arm); per-arm LIMIT 1 is an
// index-ordered walk. Ties go to the form first in $2, then the (asset, XLM) arm, never scan order.
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

// directLegMinQuoteVolume is the dust floor (QUOTE units, pegs treated as $1) a direct bucket must clear; equals
// [bridgeLegMinUSDVolume]. It cannot use `volume_usd` (this resolver writes usd_volume, so a never-priced pair never clears); it is
// `vwap * volume_priced`, not `volume_quote`, which counts zero-base trades.
const directLegMinQuoteVolume = "0.01"

// pegQuoteScaleDenominator converts a prices_1m `vwap * volume_priced` product from stroops to whole units (classic pegs are
// [stellarClassicDecimals] places); a unit test pins it to scaleDenominator(stellarClassicDecimals).
const pegQuoteScaleDenominator = 10_000_000

// queryDB resolves `<asset>/<peg>` from prices_1m at-or-before `at`, trying each asset form in turn (XLM's three identities); the CEX
// crypto:XLM/fiat:USD series is out of scope. Order is load-bearing: [canonical.AssetAliases] puts the SAC LAST so a thin Soroban pool
// cannot outrank a deep SDEX/CEX book. Freshness is a SQL bound ([VWAPUSDFXResolver.queryDirectLeg]).
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

// queryDirectLeg does one prices_1m read for `<form>/<peg>` over any peg form ([VWAPUSDFXResolver.pegForms]), at-or-before `at`;
// ANY(...) is right for pegs (same declared dollar). Dust floor [directLegMinQuoteVolume] stops a 2-stroop newest-minute remainder
// pricing trades. With freshness enforced, `bucket >= at - freshness` prunes chunks on a miss; with 0 the scan is unbounded.
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
	xlm := isXLMAliasForm(asset)
	if xlm {
		args = append(args, canonical.NativeSACContractID())
	}
	row := r.store.db.QueryRowContext(ctx, directLegQuery(r.freshness > 0, xlm), args...)
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

// isXLMAliasForm reports whether asset is any of XLM's three identities; [isXLMAsset] omits `crypto:XLM`, which the alias loop
// also queries.
func isXLMAliasForm(asset canonical.Asset) bool {
	if isXLMAsset(asset) {
		return true
	}
	for _, form := range canonical.AssetAliases(canonical.NativeAsset()) {
		if form.String() == asset.String() {
			return true
		}
	}
	return false
}

// directLegQuery is queryDirectLeg's SQL; bounded adds the `$5` freshness floor. Ties go to the peg form first in $2, never scan order.
// xlm refuses a minute inside a recorded XLM/USD-proxy freeze ([xlmUSDFreezeOverlap]), the native SAC bound at the last placeholder:
// the XLM-base anchor and the bridge's XLM leg both price through this read, so a frozen rate never stamps a notional. The pick then
// falls back to the newest unfrozen minute within the freshness floor, else nothing.
func directLegQuery(bounded, xlm bool) string {
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
	if xlm {
		sac := "$5::text"
		if bounded {
			sac = "$6::text"
		}
		q += `
		   AND NOT ` + xlmUSDFreezeOverlap("prices_1m.bucket", "prices_1m.bucket", sac)
	}
	return q + `
		 ORDER BY bucket DESC, array_position($2::text[], quote_asset)
		 LIMIT 1
	`
}

// InstallUSDVolumeResolution wires BOTH usd_volume tiers (USDVolumeQuoteSpec and VWAPUSDFXResolver) onto a store in one call; every trade-writing process must install both. fiatPegs is [pricing_guard].fiat_pegged_classic_assets, read by the fiat-peg tier and refused without classicUSDPegs.
func InstallUSDVolumeResolution(store *Store, classicUSDPegs []string, sacWrappers map[string]string, fiatPegs map[string]string) error {
	if store == nil {
		return errors.New("timescale: InstallUSDVolumeResolution: store is required")
	}
	// Set even on the no-pegs path below: reDeriveNullVolumeGuard checks "was Install called", so
	// a no-pegs re-derive passes while one that forgot Install entirely is refused.
	store.usdVolumeResolutionInstalled = true
	if len(classicUSDPegs) == 0 {
		if len(fiatPegs) > 0 {
			// The fiat-peg tier gates on a resolver price, and the resolver prices nothing without a
			// USD peg; accepting the pair would leave the tier silently dead.
			return errors.New("usd-volume quote spec: fiat_pegged_classic_assets needs usd_pegged_classic_assets")
		}
		return nil
	}
	spec, err := NewUSDVolumeQuoteSpec(classicUSDPegs, sacWrappers)
	if err != nil {
		return fmt.Errorf("usd-volume quote spec: %w", err)
	}
	if err := spec.DeclareFiatPegs(fiatPegs); err != nil {
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
