// Substance gate — the serving-side thin-market floor.
//
// The trailing-baseline guard in guard.go protects against a single
// manipulated bucket in an otherwise-healthy market. It is structurally
// blind to the other attack: a market whose ENTIRE history is
// attacker-authored. On a permissionless DEX anyone can mint a token,
// seed a "market" with a handful of dust trades, and the raw prices_1m
// serving paths will then publish the attacker's rate as our price —
// with a consistent baseline, so guard.go accepts it (2026-08-04
// valuation incident: a 21-minute-stale 1:1 seed rate valued a 5-XLM
// trade at $8.56M; the served /v1/price for the pair was
// attacker-authored in both directions).
//
// The substance gate closes that class by refusing to serve an
// AGGREGATED price claim for an on-chain pair whose trailing market
// activity is below an operator-set floor: minimum USD volume, minimum
// distinct 1-minute buckets, and minimum wall-clock span. Honest low
// volume remains fully visible through the raw surfaces (/v1/ohlc,
// /v1/observations, /v1/history) — the gate withholds only the "the
// price of X is P" claim, per the ADR-0018 surface model: a consumer
// who wants a price for a thin market makes a deliberate URL choice to
// the raw data and computes it themselves.
//
// Fail postures, deliberately asymmetric:
//   - measurement says "below floor"  → withhold (fail-closed);
//   - the substance query ERRORS      → serve (fail-open) — a DB blip
//     must not 404 the whole price surface (same posture as guard.go).
//
// Pairs with NO on-chain leg (fiat/fiat crosses, CEX crypto:/fiat:
// tickers) are exempt: their trades come from vendor APIs of listed
// venues, not from permissionless on-chain markets, so the
// mint-and-dust attack does not apply and the floors would only
// blackout legitimate synthetic pairs.
package pricingguard

import (
	"context"
	"log/slog"
	"math"
	"math/big"
	"strconv"
	"sync"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// MarketSubstanceReader is the storage seam the substance gate needs.
// *timescale.Store satisfies it; an interface keeps the gate
// unit-testable without a database. It takes each leg's full set of
// spellings so the union is measured in one read: distinct buckets do
// not add across spellings that trade in the same minute.
type MarketSubstanceReader interface {
	PairMarketSubstance(ctx context.Context, bases, quotes []canonical.Asset, window time.Duration) (timescale.MarketSubstance, error)
}

// MarketSubstanceAtReader is the storage seam for the POINT-IN-TIME
// question ([SubstanceGate.AllowedAt]): the same three legs, measured
// over the window ending at `asOf` rather than at now, at the named
// grain.
type MarketSubstanceAtReader interface {
	PairMarketSubstanceAt(ctx context.Context, bases, quotes []canonical.Asset, asOf time.Time, window time.Duration, g timescale.HistoryGranularity) (timescale.MarketSubstance, error)
}

// SubstanceStore is the store [NewSubstanceGate] requires: both
// questions, so a store or wrapper that answers only the live one fails
// to compile instead of silently judging history by today's market.
type SubstanceStore interface {
	MarketSubstanceReader
	MarketSubstanceAtReader
}

var _ SubstanceStore = (*timescale.Store)(nil)

// SubstancePolicy is the serve floor. Zero-valued fields are replaced
// by the defaults below at gate construction; the binaries map
// config.PricingGuardConfig onto this at the boundary (this package
// must stay config-free — same layering rule as the freeze thresholds,
// see the note above config.AnomalyConfig).
type SubstancePolicy struct {
	// MinVolumeUSD is the minimum trailing-window USD volume
	// (exact-rational compare, ADR-0003). Default $1,000: two orders of
	// magnitude above the measured $8.57 seed that priced the 2026-08-04
	// incident pair, while low enough that genuinely-traded small classic
	// assets clear it.
	MinVolumeUSD *big.Rat
	// MinBuckets is the minimum number of distinct closed 1-minute
	// buckets with at least one trade in the window. Default 20: a
	// single-burst wash session produces few buckets no matter its size.
	MinBuckets int64
	// MinSpan is the minimum wall-clock spread (max bucket - min bucket).
	// Default 6h: the cross-timeframe persistence property — a market
	// must have existed at more than one point in time for its price to
	// be publishable. Forces an attacker to sustain a consistent fake
	// market for hours under the trailing-baseline guard, not minutes.
	MinSpan time.Duration
	// Window is the trailing measurement window. Default 24h.
	Window time.Duration
}

// Default substance floors. See the field comments on [SubstancePolicy]
// for the rationale behind each number.
const (
	DefaultSubstanceMinVolumeUSD = 1000
	DefaultSubstanceMinBuckets   = 20
	DefaultSubstanceMinSpan      = 6 * time.Hour
	DefaultSubstanceWindow       = 24 * time.Hour
)

// withDefaults fills zero-valued policy fields.
func (p SubstancePolicy) withDefaults() SubstancePolicy {
	if p.MinVolumeUSD == nil {
		p.MinVolumeUSD = new(big.Rat).SetInt64(DefaultSubstanceMinVolumeUSD)
	}
	if p.MinBuckets == 0 {
		p.MinBuckets = DefaultSubstanceMinBuckets
	}
	if p.MinSpan == 0 {
		p.MinSpan = DefaultSubstanceMinSpan
	}
	if p.Window == 0 {
		p.Window = DefaultSubstanceWindow
	}
	return p
}

// SubstancePolicyFromValues maps the raw [pricing_guard] config values
// onto a policy. Shared by every binary that wires the gate so the
// config→policy conversion can't drift between them (this package
// stays config-free; the binaries pass the section's fields). Zero
// values keep the package defaults; a NaN/Inf volume (SetFloat64
// returns nil) and a span/window too large for time.Duration also fall
// back to the default floor — never zero, never a wrapped negative.
func SubstancePolicyFromValues(minVolumeUSD float64, minBuckets, minSpanMinutes, windowHours int) SubstancePolicy {
	pol := SubstancePolicy{
		MinBuckets: int64(minBuckets),
		MinSpan:    durationOrZero(minSpanMinutes, time.Minute),
		Window:     durationOrZero(windowHours, time.Hour),
	}
	if minVolumeUSD > 0 {
		pol.MinVolumeUSD = new(big.Rat).SetFloat64(minVolumeUSD)
	}
	return pol
}

// durationOrZero is n*unit, or 0 (the "use the default" value) when the
// product overflows time.Duration — a wrapped window would fail the gate open.
func durationOrZero(n int, unit time.Duration) time.Duration {
	if int64(n) > math.MaxInt64/int64(unit) || int64(n) < math.MinInt64/int64(unit) {
		return 0
	}
	return time.Duration(n) * unit
}

// substanceCacheTTL bounds how stale a cached verdict may be. 60s keeps
// the gate at ~1 substance query per pair per minute regardless of
// request rate, while a pair crossing the floor flips within a minute.
const substanceCacheTTL = 60 * time.Second

// substanceCacheMax bounds the verdict cache. Stellar has tens of
// thousands of assets; 8192 pairs is far above any realistic hot set.
// On overflow the whole map is dropped (crude, but bounded and simple —
// the cost of a reset is one substance query per hot pair).
const substanceCacheMax = 8192

type substanceVerdict struct {
	allowed bool
	expires time.Time
}

// SubstanceGate is the serving-side thin-market gate. Construct with
// [NewSubstanceGate]; a nil *SubstanceGate is a valid no-op gate that
// allows everything (so callers don't need their own nil checks).
type SubstanceGate struct {
	store  SubstanceStore
	policy SubstancePolicy
	logger *slog.Logger
	now    func() time.Time // nil → time.Now

	mu    sync.Mutex
	cache map[string]substanceVerdict
	// atCache holds point-in-time verdicts, keyed by pair + grain +
	// truncated instant. Separate from `cache` on purpose: the instant
	// is caller-chosen, so a client walking timestamps can fill this
	// map at will, and the overflow reset must not be able to evict
	// the LIVE verdicts every other price surface runs on.
	atCache map[string]substanceVerdict
}

// SubstanceGateOptions configures [NewSubstanceGate]. Zero-valued
// policy fields fall back to the package defaults; a nil Logger
// disables warn logging (decisions are unaffected).
type SubstanceGateOptions struct {
	Policy SubstancePolicy
	Logger *slog.Logger
}

// NewSubstanceGate builds a gate over the store.
func NewSubstanceGate(store SubstanceStore, opts SubstanceGateOptions) *SubstanceGate {
	return &SubstanceGate{
		store:   store,
		policy:  opts.Policy.withDefaults(),
		logger:  opts.Logger,
		cache:   make(map[string]substanceVerdict),
		atCache: make(map[string]substanceVerdict),
	}
}

// SubstanceGated reports whether the pair is in scope for the gate at
// all: at least one leg is an on-chain asset class (native / classic /
// soroban) — i.e. a leg anyone can author trades for on a
// permissionless market. Off-chain synthetic pairs (fiat:EUR/fiat:USD,
// crypto:BTC/fiat:USD) are out of scope. Exported for the pure decision
// tests.
func SubstanceGated(base, quote canonical.Asset) bool {
	onChain := func(a canonical.Asset) bool {
		switch a.Type {
		case canonical.AssetNative, canonical.AssetClassic, canonical.AssetSoroban:
			return true
		default:
			return false
		}
	}
	return onChain(base) || onChain(quote)
}

// SubstanceVerdicter is the per-pair verdict seam [AssetSubstanceVerdict]
// folds over; *SubstanceGate satisfies it.
type SubstanceVerdicter interface {
	Verdict(ctx context.Context, base, quote canonical.Asset, surface string) (allowed, measured bool)
}

// fiatUSD is the USD quote every asset-level verdict tries.
var fiatUSD = func() canonical.Asset {
	a, err := canonical.NewFiatAsset("USD")
	if err != nil {
		panic(err)
	}
	return a
}()

// AssetSubstanceVerdict is the substance gate's answer for a single
// asset's USD price rather than one pair. The backing quotes are XLM,
// fiat:USD (the alias union covers the CEX series) and each
// operator-declared USD peg in usdPegs. Allowed when the asset is out of
// scope (no on-chain identity), is native (definitionally liquid; its
// identity pairs degenerate under the alias union), or when ANY backing
// quote clears the floor. measured is false when no quote cleared and at
// least one could not be measured. The /v1/assets listing and the
// priceless-popular tripwire both ask this, so "withheld" means one
// thing on the surface and on the alert. A nil gate allows.
func AssetSubstanceVerdict(
	ctx context.Context, gate SubstanceVerdicter, asset canonical.Asset,
	usdPegs []canonical.Asset, surface string,
) (allowed, measured bool) {
	if gate == nil {
		return true, true
	}
	switch asset.Type {
	case canonical.AssetNative:
		return true, true
	case canonical.AssetClassic, canonical.AssetSoroban:
	default:
		return true, true
	}
	quotes := append([]canonical.Asset{canonical.NativeAsset(), fiatUSD}, usdPegs...)
	measured = true
	for _, quote := range quotes {
		ok, m := gate.Verdict(ctx, asset, quote, surface)
		if ok && m {
			return true, true
		}
		if !m {
			measured = false
		}
	}
	return false, measured
}

// SubstanceOK is the pure decision: does the measured substance clear
// the policy floor? Exact-rational volume compare (ADR-0003). An
// unparseable volume string counts as zero — fail-closed, consistent
// with "if the volume cannot be verified, the floor cannot be
// verified".
func SubstanceOK(volumeUSD *big.Rat, buckets, spanSeconds int64, policy SubstancePolicy) bool {
	if volumeUSD == nil {
		volumeUSD = new(big.Rat)
	}
	if volumeUSD.Cmp(policy.MinVolumeUSD) < 0 {
		return false
	}
	if buckets < policy.MinBuckets {
		return false
	}
	if time.Duration(spanSeconds)*time.Second < policy.MinSpan {
		return false
	}
	return true
}

// Allowed reports whether an aggregated price claim for (base, quote)
// may be served. `surface` labels the withheld metric
// (obs.PriceServeSubstanceWithheldTotal) so operators can see WHICH
// serving path is withholding — it must be a low-cardinality constant
// ("price_read", "tip", "oracle", "asset_headline", "price_alert"),
// never a pair string.
//
// The measurement is the ALIAS UNION of the pair: XLM's three canonical
// spellings (native / crypto:XLM / the SAC) hold disjoint venue
// populations (CS: the aggregator writes CEX volume under crypto:XLM
// while SDEX writes under native), and the pair's real market is their
// union — volumes add, a minute active under two spellings is one
// bucket. Without the union, /v1/price?asset=native&quote=fiat:USD
// would measure only the literal native/fiat:USD pair — which has zero
// rows by construction — and withhold XLM itself.
//
// Verdicts are cached for [substanceCacheTTL] per direction-insensitive
// pair key. Nil-receiver safe: a nil gate allows everything.
//
// Allowed FAILS OPEN when the pair could not be measured — right for a
// single price lookup, where a store blip must not 404 the surface. A
// surface that must not publish an unverified claim (listings, market
// caps — ADR-0018) asks [SubstanceGate.Verdict] instead.
func (g *SubstanceGate) Allowed(ctx context.Context, base, quote canonical.Asset, surface string) bool {
	allowed, measured := g.Verdict(ctx, base, quote, surface)
	return allowed || !measured
}

// Verdict is [SubstanceGate.Allowed] without the fail-open: measured is
// false when a store error or the request deadline prevented a verdict,
// and then allowed is false too — the caller decides what an unmeasured
// pair means on its surface. Every unmeasured verdict is counted in
// obs.PriceServeSubstanceUnmeasuredTotal. A nil gate and an out-of-scope
// pair are measured-and-allowed: no verdict is owed for them.
func (g *SubstanceGate) Verdict(ctx context.Context, base, quote canonical.Asset, surface string) (allowed, measured bool) {
	if g == nil {
		return true, true
	}
	if !SubstanceGated(base, quote) {
		return true, true
	}
	key := pairCacheKey(base, quote)
	now := g.clock()
	g.mu.Lock()
	prior, hadPrior := g.cache[key]
	if hadPrior && now.Before(prior.expires) {
		g.mu.Unlock()
		if !prior.allowed {
			obs.PriceServeSubstanceWithheldTotal.WithLabelValues(surface).Inc()
		}
		return prior.allowed, true
	}
	g.mu.Unlock()

	allowed, measured = g.measure(ctx, base, quote)
	if !measured {
		// Not cached, so the next request re-measures.
		obs.PriceServeSubstanceUnmeasuredTotal.WithLabelValues(surface).Inc()
		return false, false
	}
	g.mu.Lock()
	if len(g.cache) >= substanceCacheMax {
		g.cache = make(map[string]substanceVerdict)
	}
	g.cache[key] = substanceVerdict{allowed: allowed, expires: now.Add(substanceCacheTTL)}
	g.mu.Unlock()
	if !allowed {
		obs.PriceServeSubstanceWithheldTotal.WithLabelValues(surface).Inc()
		// Log on verdict TRANSITIONS only — first observation of a pair,
		// or a flip from allowed. The steady state (hundreds of thin
		// long-tail pairs re-measured every TTL expiry) produced 6,000
		// WARNs/hour on r1 (2026-08-05), churning the journald ring
		// buffer past anything an operator could triage; the metric is
		// the volume signal, the log is the change signal.
		if g.logger != nil && (!hadPrior || prior.allowed) {
			g.logger.Warn("substance gate: aggregated price withheld — trailing market below serve floor",
				"base", base.String(), "quote", quote.String(), "surface", surface)
		}
	} else if g.logger != nil && hadPrior && !prior.allowed {
		g.logger.Info("substance gate: pair recovered above the serve floor — price serving resumed",
			"base", base.String(), "quote", quote.String(), "surface", surface)
	}
	return allowed, true
}

// measure runs the alias-union substance measurement. measured=false
// means an infrastructure error prevented a verdict.
func (g *SubstanceGate) measure(ctx context.Context, base, quote canonical.Asset) (allowed, measured bool) {
	return g.measureUnion(ctx, base, quote, g.policy,
		func(ctx context.Context, bases, quotes []canonical.Asset) (timescale.MarketSubstance, error) {
			return g.store.PairMarketSubstance(ctx, bases, quotes, g.policy.Window)
		})
}

// measureUnion is the alias-union measurement shared by the live and the
// point-in-time gate: `read` measures every spelling of base against
// every spelling of quote as ONE market, held to `policy`. One read, not
// a per-spelling fold: XLM's SDEX and CEX legs trade in the same
// minutes, and summing each spelling's distinct-bucket count would count
// a shared minute once per spelling.
func (g *SubstanceGate) measureUnion(
	ctx context.Context, base, quote canonical.Asset, policy SubstancePolicy,
	read func(ctx context.Context, bases, quotes []canonical.Asset) (timescale.MarketSubstance, error),
) (allowed, measured bool) {
	sub, err := read(ctx, canonical.AssetAliases(base), canonical.AssetAliases(quote))
	if err != nil {
		if g.logger != nil && ctx.Err() == nil {
			g.logger.Warn("substance gate: measurement failed — no verdict for the pair",
				"base", base.String(), "quote", quote.String(), "err", err)
		}
		return true, false
	}
	vol, ok := new(big.Rat).SetString(sub.VolumeUSD)
	if !ok {
		// An unparsable volume verifies nothing, so it fails the volume leg.
		vol = new(big.Rat)
	}
	return SubstanceOK(vol, sub.Buckets, sub.SpanSeconds, policy), true
}

// hourGrainPolicy is the floor a market is held to when its legs are
// counted at HOUR grain (a historical instant — see
// [SubstanceGate.AllowedAt]): the strictest floor that never refuses a
// market the minute-grain policy p admits, so history and live agree
// under every legal policy, not only the default.
//
// Buckets: n distinct minutes fill at least ceil(n/60) hours, and a
// minute span of an hour or more puts the first and last minute in two
// different hours; both bounds are reachable (pack whole hours, place
// the last far from the first). Span: two minutes S apart sit at least
// floor(S/1h) whole hours apart, and exactly that when the first is on
// the hour. At the defaults (20 buckets, 6h) this is two hour buckets
// over six hours. Volume is grain-independent and carries over.
func hourGrainPolicy(p SubstancePolicy) SubstancePolicy {
	const minutesPerHour = int64(time.Hour / time.Minute)
	h := p
	h.MinBuckets = (p.MinBuckets + minutesPerHour - 1) / minutesPerHour
	h.MinSpan = p.MinSpan.Truncate(time.Hour)
	if h.MinSpan >= time.Hour && h.MinBuckets < 2 {
		h.MinBuckets = 2
	}
	return h
}

// policyAt returns the grain a point-in-time measurement is counted at
// and the floor that grain is held to, for an instant `age` behind now.
//
// The boundary is the point-in-time READER's own
// ([timescale.PriceAtMinuteRungMaxAge]): inside it the served number
// can be a raw prices_1m bucket — the attacker-authorable minute this
// gate exists for — so the measurement is the live gate's, grain and
// floor, with only the window moved. prices_1m's reach through history
// is a deployment setting (a retention policy on it ships disarmed,
// migration 0156), but 48h + one window is inside every retention the
// schema has ever carried. Past the boundary the reader serves hour and
// day bars, and the legs are counted on prices_1h — indefinite by
// design (migration 0002), and re-materialised by the backfill tool in
// the same pass as the prices_1d bars the reader serves from.
func (g *SubstanceGate) policyAt(age time.Duration) (timescale.HistoryGranularity, SubstancePolicy) {
	if age <= timescale.PriceAtMinuteRungMaxAge {
		return timescale.Granularity1m, g.policy
	}
	return timescale.Granularity1h, hourGrainPolicy(g.policy)
}

// AllowedAt reports whether an aggregated price claim for (base, quote)
// AS OF the instant `at` may be served — the point-in-time form of
// [SubstanceGate.Allowed], for the reads that answer "what was the
// price at ts" (/v1/price/at, and each /v1/price/changes horizon).
//
// Those reads used to ask [SubstanceGate.Allowed], and a trailing
// window ending NOW decides nothing about a bucket that closed at ts
// (finding T038). It was wrong in both directions. A market that was
// deep and honest at ts but is dormant today had every historical
// price withheld — a cost-basis read 404'd for data we hold and trust.
// And a market that is thick today but was attacker-seeded dust at ts
// PASSED, so the historical read served exactly the manipulated price
// the gate exists to refuse. The safety property does not transfer
// across time, so the window has to: substance is measured over
// [policy.Window] ending at `at`, closed buckets only, alias union as
// for the live gate. See [SubstanceGate.policyAt] for the grain.
//
// An `at` in the future is measured as of now. The fail postures are
// the live gate's: below floor → withhold; measurement error → serve,
// uncached. Verdicts are cached per (pair, grain, truncated instant)
// for [substanceCacheTTL] in a map of their own. Withheld verdicts
// count on the same metric under `surface`; they are NOT logged — the
// transition log is a statement about a pair's present, and a
// caller-chosen instant has no transitions to report.
//
// Nil-receiver safe: a nil gate allows everything.
func (g *SubstanceGate) AllowedAt(ctx context.Context, base, quote canonical.Asset, at time.Time, surface string) bool {
	if g == nil {
		return true
	}
	if !SubstanceGated(base, quote) {
		return true
	}
	now := g.clock()
	asOf := at.UTC()
	if asOf.After(now) {
		asOf = now.UTC()
	}
	grain, policy := g.policyAt(now.Sub(asOf))
	// Truncating to the grain changes no verdict's upper edge (a bucket
	// closed at the truncated instant iff it closed at the raw one) and
	// makes the window a whole number of buckets and the key shareable.
	asOf = asOf.Truncate(grain.BucketDuration())
	key := pairCacheKey(base, quote) + "\x00" + string(grain) + "\x00" + strconv.FormatInt(asOf.Unix(), 10)

	g.mu.Lock()
	prior, hadPrior := g.atCache[key]
	g.mu.Unlock()
	if hadPrior && now.Before(prior.expires) {
		if !prior.allowed {
			obs.PriceServeSubstanceWithheldTotal.WithLabelValues(surface).Inc()
		}
		return prior.allowed
	}

	allowed, measured := g.measureUnion(ctx, base, quote, policy,
		func(ctx context.Context, bases, quotes []canonical.Asset) (timescale.MarketSubstance, error) {
			return g.store.PairMarketSubstanceAt(ctx, bases, quotes, asOf, policy.Window, grain)
		})
	if !measured {
		obs.PriceServeSubstanceUnmeasuredTotal.WithLabelValues(surface).Inc()
		return true
	}
	g.mu.Lock()
	if len(g.atCache) >= substanceCacheMax {
		g.atCache = make(map[string]substanceVerdict)
	}
	g.atCache[key] = substanceVerdict{allowed: allowed, expires: now.Add(substanceCacheTTL)}
	g.mu.Unlock()
	if !allowed {
		obs.PriceServeSubstanceWithheldTotal.WithLabelValues(surface).Inc()
	}
	return allowed
}

func (g *SubstanceGate) clock() time.Time {
	if g.now != nil {
		return g.now()
	}
	return time.Now()
}

// pairCacheKey is direction-insensitive: (A,B) and (B,A) share a
// verdict, matching the both-directions measurement.
func pairCacheKey(base, quote canonical.Asset) string {
	b, q := base.String(), quote.String()
	if b > q {
		b, q = q, b
	}
	return b + "\x00" + q
}
