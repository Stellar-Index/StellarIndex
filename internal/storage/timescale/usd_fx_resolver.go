package timescale

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"sync"
	"time"

	"github.com/lib/pq"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// VWAPUSDFXResolver implements [USDVolumeFXResolver] against the
// `prices_1m` continuous-aggregate. For a given on-chain quote
// asset + timestamp, it returns the asset's most-recent VWAP
// against any of the operator-declared USD-pegged classics
// (typically Circle USDC, Stellar USDT, AnchorUSD) — treating the
// peg as exactly $1.
//
// L2.2 Phase 2 / F-1268 (audit-2026-05-12). Pre-Phase-2:
// on-chain trades whose quote asset wasn't already in the
// operator's USD-pegged list contributed 0 to volume_24h_usd.
// This resolver closes the gap by looking up `<quote>/<USD-peg>`
// at the trade's timestamp; if a recent VWAP exists, the trade
// inherits the USD value through that chain.
//
// Cache: per (asset, 1-minute bucket) → resolved rate string,
// with a TTL (default 5 minutes). The trade-insert hot path can
// stamp hundreds of trades per second; without a cache we'd
// hammer prices_1m with one query per insert. The minute-bucket
// key matches the CAGG's resolution — finer-grained caching adds
// no precision but multiplies misses.
//
// Three resolution routes, tried in this order (2026-07-22 — see
// docs/operations/usd-volume-coverage-plan.md for the measurements
// that motivated the last two):
//
//  1. FIAT assets → `fx_quotes`, not prices_1m. prices_1m holds
//     crypto markets only, so a fiat:EUR quote could never resolve
//     here at all. [VWAPUSDFXResolver.usdPriceForFiat].
//  2. Direct `<asset>/<peg>` VWAP in prices_1m — the original path.
//     [VWAPUSDFXResolver.queryDB].
//  3. The XLM bridge — `<asset>/XLM x XLM/USD`. Most Stellar tokens
//     have no stablecoin market but do have an XLM one, so this is
//     what carries on-chain coverage past the USD-pegged pairs.
//     [VWAPUSDFXResolver.bridgeViaXLM].
type VWAPUSDFXResolver struct {
	store *Store

	// usdPegs is the operator-declared classic USD-peg list (e.g.
	// USDC-GA5Z…, USDT-GCQT…). The resolver queries prices_1m for
	// `<asset>/<peg>` for each peg until one returns a row.
	usdPegs []string

	// freshness is the maximum allowable (now - VWAP timestamp).
	// Entries older than this return ok=false rather than letting
	// a stale rate land in a fresh trade's usd_volume.
	freshness time.Duration

	// bridgeFreshness is the staleness bound for the tier-3b XLM
	// bridge leg. Deliberately much wider than `freshness`: that one
	// is calibrated for liquid direct markets, while the bridge exists
	// precisely to price tokens whose markets are thin. Measured
	// 2026-07-22, the tokens behind the largest unpriced classes trade
	// $8-$220 across a whole day, so requiring a bridge rate from the
	// last hour rejects most of them — and the alternative to a
	// slightly-stale rate is not a better rate, it is NULL, which
	// silently drops the trade from every aggregate built on
	// usd_volume. The error this admits is also bounded by the size of
	// what it prices: these are sub-cent valuations, so even a badly
	// stale rate moves aggregate volume by a rounding error, whereas
	// each NULL removes a row outright.
	bridgeFreshness time.Duration

	// cacheTTL caps how long a cached rate is valid before
	// re-querying. Default 5 min.
	cacheTTL time.Duration

	clock func() time.Time

	mu    sync.RWMutex
	cache map[fxCacheKey]fxCacheEntry
}

// fxCacheSweepThreshold bounds the resolver's in-memory cache. The
// key space is (asset, 1-minute bucket) including negative results,
// and nothing evicted entries before audit-2026-06-11 G11-05, so a
// long-running backfill (every historical minute × every traded
// asset) grew the map without bound on the trade-insert hot path.
// When the map exceeds this many entries, storeCache opportunistically
// sweeps everything past its TTL before inserting. The TTL (default
// 5 min) already makes stale entries dead weight, so the sweep only
// drops rows lookupCache would have ignored anyway — correctness is
// unchanged, only resident size is bounded. The threshold is high
// enough that steady-state live ingest (a handful of assets × a few
// minutes) never triggers a sweep.
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
	// USDPegs is the operator's classic USD-peg list (the
	// same value the [USDVolumeQuoteSpec] consumes; pass through
	// the canonical "CODE-ISSUER" wire form). Resolver queries
	// pegs in order, first match wins. Empty list = resolver is
	// a no-op (every USDPriceAt returns ok=false).
	USDPegs []string

	// Freshness — max staleness for a returned rate. Set to a
	// negative value (e.g. -1) to DISABLE the freshness check
	// entirely (used by tests + by deployments where the
	// source's per-minute cadence guarantees near-zero lag). Set
	// to 0 (the zero value) to inherit the default 1h. Set to a
	// positive duration to override the default.
	//
	// F-1251 (codex audit-2026-05-12): pre-fix the docstring
	// said "Set to 0 to disable" but the constructor's
	// `if opts.Freshness == 0 { opts.Freshness = time.Hour }`
	// silently turned a 0 into the 1h default, so callers who
	// thought they'd disabled freshness were still enforcing it.
	// The negative-disable convention removes the ambiguity.
	Freshness time.Duration

	// BridgeFreshness — max staleness for the tier-3b XLM bridge leg.
	// Same sentinel convention as Freshness: 0 inherits the default
	// ([defaultBridgeFreshness]), negative disables the bound entirely.
	BridgeFreshness time.Duration

	// CacheTTL bounds the in-memory cache. Default 5 min.
	CacheTTL time.Duration

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
	// F-1251: 0 → default 1h; negative → disabled (sentinel 0
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
	return &VWAPUSDFXResolver{
		store:           store,
		usdPegs:         pegs,
		freshness:       opts.Freshness,
		bridgeFreshness: opts.BridgeFreshness,
		cacheTTL:        opts.CacheTTL,
		clock:           opts.Clock,
		cache:           make(map[fxCacheKey]fxCacheEntry),
	}, nil
}

// USDPriceAt implements [USDVolumeFXResolver]. Returns the
// resolved USD price for `asset` at-or-before `at`, treating each
// configured peg as exactly $1.
//
// Returns ("", false, nil) when:
//   - no peg query returned a row (asset isn't traded against any
//     covered peg in the lookup window)
//   - the most-recent matching row is older than Freshness
//   - the resolver has no pegs configured
//
// Real DB errors propagate so the caller can surface them via
// metrics; the calling trade still inserts, just with usd_volume
// NULL.
func (r *VWAPUSDFXResolver) USDPriceAt(ctx context.Context, asset canonical.Asset, at time.Time) (string, bool, error) {
	// Fiat quotes resolve from fx_quotes, not prices_1m — see
	// [VWAPUSDFXResolver.usdPriceForFiat]. This branch MUST precede the
	// peg check below: pricing EUR needs no Stellar USD-peg market.
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

	rate, observedAt, err := r.queryDB(ctx, asset, at)
	if err != nil {
		return "", false, err
	}
	if rate != "" && !r.fresh(observedAt, at, r.freshness) {
		// Direct market exists but its most recent VWAP is too old to
		// price this trade. Discard it and let the bridge try — a
		// current XLM-routed rate beats a stale direct one, and before
		// tier 3b existed this simply returned NULL.
		rate = ""
	}
	if rate == "" {
		// Tier 3b — no usable direct <asset>/<peg> rate. Most Stellar
		// tokens route their liquidity through XLM rather than a
		// stablecoin, so try <asset>/XLM x XLM/USD before giving up.
		// The bridge enforces its own (wider) freshness internally.
		rate, err = r.bridgeViaXLM(ctx, asset, at)
		if err != nil {
			return "", false, err
		}
	}
	if rate == "" {
		r.storeCache(key, fxCacheEntry{rate: "", cachedAt: r.clock()})
		return "", false, nil
	}
	// F-1251 (codex audit-2026-05-12): Postgres NUMERIC::text
	// preserves the column's full scale, so a VWAP that's
	// arithmetically `1.085` arrives here as
	// `1.085000000000000000000`. Trim the trailing zeros (and
	// the lone trailing decimal point) so consumers (the
	// indexer, integration tests, the API JSON envelope) see
	// the canonical decimal form. Mathematically equivalent;
	// just easier to compare + display.
	rate = trimNumericText(rate)
	r.storeCache(key, fxCacheEntry{rate: rate, cachedAt: r.clock()})
	return rate, true, nil
}

// fiatUSDRateScale is the decimal scale used to render a fiat→USD
// rate from its exact *big.Rat form. The rate is USD per 1 unit of
// the ticker, which spans ~1.34 (GBP) down to ~3.8e-5 (VND), so 18
// places keeps at least 13 significant digits even for the weakest
// currency in the table — far beyond the 8 decimals `usd_volume`
// itself is rendered at. [trimNumericText] then drops the padding.
const fiatUSDRateScale = 18

// usdPriceForFiat resolves the USD price of one unit of a fiat asset
// from `fx_quotes`, and is the reason non-USD-quoted external-exchange
// trades can be priced at all.
//
// A fiat asset can NEVER resolve through [VWAPUSDFXResolver.queryDB]:
// that path looks for `<asset>/<peg>` in prices_1m, and prices_1m holds
// crypto markets only — there is no `fiat:EUR/fiat:USD` row and there
// never will be one. Before this branch existed, every CEX pair quoted
// in a currency other than USD (binance BTC/EUR, kraken ETH/GBP, …)
// fell through all four tiers of [tradeUSDVolume] and inserted with
// `usd_volume` NULL, silently deflating every aggregate built on that
// column — measured at ~$939M of unpriced volume on 2026-07-17 alone,
// ~23% of that day's total. See docs/operations/usd-volume-coverage-plan.md.
//
// The rate is computed by [fxSnapFromRows] as an exact *big.Rat
// (rate_usd(USD)/rate_usd(TICKER), with the USD leg an exact 1).
// The float-derived `inverse_usd` column is deliberately NOT used —
// ADR-0003 keeps money math out of float space.
//
// Two deviations from the prices_1m path, both deliberate:
//
//   - **Freshness is NOT applied.** fx_quotes buckets are daily and
//     weekday-only, so the resolver's default 1h freshness would reject
//     100% of fiat rates. [fxQuotesSnapAtOrBefore]'s own
//     [fxQuotesSnapLookback] (7 days — the longest routine
//     weekend/holiday gap) is the freshness bound instead.
//   - **The cache key floors to the UTC day, not the minute.** Buckets
//     are stored at exactly UTC midnight, one per (date, ticker), so a
//     day floor is the source's true resolution — a minute key would
//     multiply misses by 1440 and, during a historical backfill, grow
//     the cache by one entry per traded minute per currency.
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

// fresh reports whether a rate observed at `observedAt` is recent
// enough to price a trade at `at`, under the given window. A
// non-positive window disables the check (the F-1251 negative-disable
// sentinel).
//
// F-1251 (codex audit-2026-05-12): staleness is measured against the
// TRADE timestamp `at`, not wall-clock. Pre-fix the comparison used
// `r.clock().Sub(observedAt)`, which rejected every historical /
// backfill trade older than the window even when a contemporaneous FX
// anchor existed (the trade ran at T, the anchor was at T-30m, both an
// hour ago — fine in trade-time but the old check saw it as "anchor is
// 1h30m stale by my wall-clock"). Now: at-time freshness, so
// historical replay and backfill correctly inherit a peer-aligned USD
// rate.
func (r *VWAPUSDFXResolver) fresh(observedAt, at time.Time, window time.Duration) bool {
	if window <= 0 {
		return true
	}
	return at.Sub(observedAt) <= window
}

// lookupCache returns (rate, true) when the cache has a fresh
// entry, otherwise ("", false). Empty rate means "previously
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
	// Bounded eviction (audit-2026-06-11 G11-05): before the map can
	// grow unbounded, opportunistically drop every entry past its TTL.
	// These are entries lookupCache already treats as misses, so the
	// sweep is correctness-neutral. Only runs when the map is large,
	// so the O(n) scan is amortised away under steady-state ingest.
	if len(r.cache) >= fxCacheSweepThreshold {
		now := r.clock()
		for k, e := range r.cache {
			if now.Sub(e.cachedAt) > r.cacheTTL {
				delete(r.cache, k)
			}
		}
	}
	r.cache[key] = entry
}

// trimNumericText strips trailing zeros from a Postgres NUMERIC
// text representation. `1.085000` → `1.085`; `1.000000` → `1`;
// `42` (no decimal) → `42`; `0.000` → `0`. Caller-friendly
// canonical form so downstream consumers don't need to be
// scale-aware. F-1251 (codex audit-2026-05-12).
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

// bridgeLegMinUSDVolume is the dust floor a bridge bucket must clear
// before its VWAP is allowed to value another trade.
//
// Rationale is the dust finding (docs/operations/
// finding-dust-trades-set-chart-extremes.md): amounts are integers
// (stroops), so a fill of a few stroops carries a quantisation error
// near 100% — its "price" is an artifact of dividing two tiny integers,
// not an observation. VWAP is volume-weighted and therefore already
// near-immune to a crumb sitting inside a real bucket; what it cannot
// survive is a bucket containing ONLY dust, where the crumb sets the
// rate outright. This floor removes exactly that case. Without it a
// 2-stroop remainder could set the valuation rate for every trade that
// bridges through the pair.
//
// $0.01 matches the floor chosen for the OHLC extremes, so the two
// dust defences agree rather than each picking their own threshold.
const bridgeLegMinUSDVolume = "0.01"

// bridgeRateSigDigits is how many SIGNIFICANT digits a bridged rate is
// rendered with. Significance, not a fixed decimal scale, is what this
// needs: prices_1m stores raw ratios, so a token declaring 18 decimals
// prices against XLM's 7 at around 1e-12, and a fixed 18-place render
// would leave such a rate barely six significant figures — silently
// degrading the valuation of exactly the tokens that need the bridge
// most. (Three tokens confirmed on R1 declare decimals()=18.)
//
// 25 digits comfortably exceeds what usd_volume's own 8-decimal render
// can express for any realistic trade size, so the rate is never the
// limiting factor. Trailing zeros are trimmed by trimNumericText, so
// generosity here costs nothing in the common case.
const bridgeRateSigDigits = 25

// bridgeRateMaxScale caps the decimal places rateScaleFor will ask for,
// so a pathological or hostile token declaration cannot make us render
// an unbounded string on the trade-insert hot path.
const bridgeRateMaxScale = 80

// bridgeViaXLM prices `asset` in USD through XLM when no direct
// <asset>/<peg> market exists:
//
//	USD per asset = (XLM per asset) x (USD per XLM)
//
// This is what takes on-chain coverage past the USD-pegged markets.
// Most Stellar tokens have no stablecoin pair at all but do have an XLM
// one — measured 2026-07-22, the tokens behind the largest unpriced
// classes (6T, F8, YxT, aTTaiN, GYEN, uniT) ALL have XLM markets, and
// 237,305 on-chain trades on 2026-07-17 were unpriced purely because
// neither leg was XLM or a stablecoin while both legs had XLM markets.
//
// Returns ("", zero, nil) on a miss — a token with no XLM market
// either, which is the documented end of the waterfall.
//
// The XLM/USD leg goes back through USDPriceAt rather than straight to
// the DB, which both shares its cache — XLM/USD is identical for every
// asset bridging in the same minute, so this collapses one query per
// bridged asset into one per minute — and applies the freshness check
// to that leg independently.
//
// That recursion is bounded at depth 2 and provably terminates: the
// only call is with native XLM, and the isXLMAsset guard above returns
// before any further bridging. A bridged rate is therefore fresh iff
// BOTH legs are fresh — the XLM leg by USDPriceAt's own check, the
// asset leg by the caller applying freshness to the bucket returned
// here.
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

// bridgeRate multiplies the two bridge legs into a USD-per-asset rate
// string. Split out of [VWAPUSDFXResolver.bridgeViaXLM] so the money
// arithmetic is unit-testable without a database — the DB parts of the
// bridge are just row fetches; this is the part that can be wrong.
//
// Returns ok=false on an unparseable or non-positive XLM/USD leg rather
// than propagating a bad rate into usd_volume.
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

// rateScaleFor returns the number of decimal places needed to render r
// with `sig` significant digits.
//
// big.Rat only renders at a FIXED decimal scale, which is the wrong
// shape for bridged rates: they span roughly 1e0 down to 1e-12 and
// below depending purely on the token's declared decimals, so any
// single fixed scale either truncates the small ones or bloats the
// large ones. This counts the leading zeros and adds the significant
// digits on top, keeping precision constant in RELATIVE terms — which
// is what a rate needs, since it is always multiplied by an amount.
//
// Deliberately float-free (ADR-0003): a log10 would be the obvious way
// to find the magnitude, and the obvious way to reintroduce float error
// into money math.
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

// xlmLegRate turns a prices_1m VWAP row into XLM-per-asset, inverting
// when the pair is stored XLM-first. Split out for testability: the
// inversion is the one place an orientation bug would silently produce
// a rate that is wrong by a factor of vwap^2.
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

// queryXLMLeg returns XLM-per-unit-of-asset at-or-before `at`, from
// whichever orientation the pair is stored in.
//
// Both orientations genuinely occur for the same token — trades keep
// the venue's observed base/quote ordering rather than being
// re-oriented (see [canonical.Trade]), so a token can have both
// TOKEN/XLM and XLM/TOKEN buckets on the same day. `native` is the
// classic wire form; the SAC wrapper is the form a Soroban pool holds.
// A stored `<asset>/XLM` VWAP is already XLM-per-asset; an `XLM/<asset>`
// VWAP is asset-per-XLM and must be inverted.
//
// Buckets below [bridgeLegMinUSDVolume] are excluded so a dust-only
// bucket cannot set the rate. The lower bucket bound mirrors the
// G11-06 rationale on queryDB: without it a miss walks prices_1m back
// to genesis before returning a row the caller would discard anyway.
func (r *VWAPUSDFXResolver) queryXLMLeg(ctx context.Context, asset canonical.Asset, at time.Time) (*big.Rat, error) {
	// Both on-chain wire forms of XLM — the classic `native` type and
	// the SAC wrapper a Soroban pool holds. Mirrors [isXLMAsset].
	xlmForms := []string{canonical.NativeAsset().String(), nativeXLMSAC}
	args := []any{
		asset.String(),
		pq.Array(xlmForms),
		at.UTC(),
		bridgeLegMinUSDVolume,
	}
	// The lower bucket bound must live INSIDE each UNION branch, not on
	// the outer query: chunk exclusion is decided per-scan, so a bound
	// applied after the union still lets both branches walk prices_1m
	// back to genesis on a miss — the very thing G11-06 fixed on
	// queryDB. Same reason the vwap > 0 guard is inlined too.
	lowerBound := ""
	if r.bridgeFreshness > 0 {
		lowerBound = `
		            AND bucket     >= $5`
		args = append(args, at.UTC().Add(-r.bridgeFreshness))
	}
	// Each UNION branch is parenthesised: Postgres rejects a bare
	// ORDER BY/LIMIT inside an unparenthesised union arm. The per-arm
	// LIMIT 1 is what keeps this cheap — each side is an index-ordered
	// walk that stops at its first qualifying bucket.
	q := fmt.Sprintf(`
		SELECT bucket, vwap::text, inverted
		  FROM (
		        (SELECT bucket, vwap, false AS inverted
		           FROM prices_1m
		          WHERE base_asset  = $1
		            AND quote_asset = ANY($2)
		            AND bucket     <= $3
		            AND volume_usd >= $4::numeric
		            AND vwap        > 0%[1]s
		          ORDER BY bucket DESC
		          LIMIT 1)
		        UNION ALL
		        (SELECT bucket, vwap, true AS inverted
		           FROM prices_1m
		          WHERE base_asset  = ANY($2)
		            AND quote_asset = $1
		            AND bucket     <= $3
		            AND volume_usd >= $4::numeric
		            AND vwap        > 0%[1]s
		          ORDER BY bucket DESC
		          LIMIT 1)
		       ) legs
		 ORDER BY bucket DESC
		 LIMIT 1
	`, lowerBound)
	row := r.store.db.QueryRowContext(ctx, q, args...)
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

// queryDB does one prices_1m read for `<asset>/<peg>` for any peg
// in the configured list, at-or-before `at`. Returns the VWAP
// string + the row's bucket timestamp on hit, or ("", zero, nil)
// on miss.
//
// Implementation: single round-trip with `quote_asset = ANY(...)`
// so the DB picks the highest-bucket row across all pegs in one
// pass.
//
// Lower bucket bound (audit-2026-06-11 G11-06): when freshness is
// enforced (>0), USDPriceAt rejects any row whose bucket is older
// than `at - freshness`, so a miss within the window is the only
// useful result. Without a lower bound the index scan walks
// prices_1m chunks back to genesis on a miss before returning a row
// the caller would discard. The `bucket >= at - freshness` floor is
// behaviour-preserving — anything below it is rejected anyway — and
// lets TimescaleDB prune to the freshness window's chunks. When
// freshness is disabled (0) we keep the unbounded scan.
func (r *VWAPUSDFXResolver) queryDB(ctx context.Context, asset canonical.Asset, at time.Time) (string, time.Time, error) {
	q := `
		SELECT bucket, vwap::text
		  FROM prices_1m
		 WHERE base_asset  = $1
		   AND quote_asset = ANY($2)
		   AND bucket     <= $3`
	args := []any{
		asset.String(),
		pq.Array(r.usdPegs),
		at.UTC(),
	}
	if r.freshness > 0 {
		q += `
		   AND bucket     >= $4`
		args = append(args, at.UTC().Add(-r.freshness))
	}
	q += `
		 ORDER BY bucket DESC
		 LIMIT 1
	`
	row := r.store.db.QueryRowContext(ctx, q, args...)
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

// InstallUSDVolumeResolution wires BOTH `usd_volume` resolution tiers
// onto a store in one call: the operator's [USDVolumeQuoteSpec]
// (tier 2 — declared USD-pegged classics + their SAC wrappers) and the
// [VWAPUSDFXResolver] (tiers 3/4 — FX-anchored multiplication, and
// since 2026-07-22 fiat quotes via fx_quotes).
//
// It exists because wiring these separately drifted. Every process that
// writes trades must install BOTH, but the two calls lived in three
// hand-maintained copies and two of them were incomplete:
//
//   - `internal/ops/chops/ch_rebuild.go` installed NEITHER, while
//     setting a high `derive_generation`.
//   - `internal/ops/ingest/backfill_external.go` installed only the
//     quote spec ("mirror the indexer's wiring (L2.2 phase 1)" — it
//     mirrored phase 1 and stopped).
//
// That combination is actively destructive, not merely incomplete:
// InsertTrade/BatchInsertTrades resolve `usd_volume` from whatever is
// installed and then upsert it with an UNCONDITIONAL
// `usd_volume = EXCLUDED.usd_volume` (trades.go), gated only by
// `trades.derive_generation <= EXCLUDED.derive_generation`. A re-derive
// that runs without the resolvers therefore computes NULL and
// OVERWRITES correct stored values — a high generation guarantees it
// wins. Wiring both from one place makes that class of drift structural
// rather than a thing each new call site has to remember.
//
// Empty `classicUSDPegs` is a no-op (both tiers stay nil), preserving
// the documented no-config behaviour.
func InstallUSDVolumeResolution(store *Store, classicUSDPegs []string, sacWrappers map[string]string) error {
	if store == nil {
		return errors.New("timescale: InstallUSDVolumeResolution: store is required")
	}
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
	})
	if err != nil {
		return fmt.Errorf("usd-volume fx resolver: %w", err)
	}
	store.SetUSDVolumeFXResolver(fxResolver)
	return nil
}
