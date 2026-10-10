// Substance gate: the serving-side thin-market floor.
//
// The trailing-baseline guard in guard.go is blind to a market whose ENTIRE
// history is attacker-authored: anyone can mint a token and seed a "market" with
// dust trades, and the baseline is then consistent with the lie. This gate
// refuses to serve an AGGREGATED price claim for an on-chain pair whose trailing
// activity is below an operator-set floor (USD volume, distinct 1-minute
// buckets, wall-clock span). Raw surfaces (/v1/ohlc, /v1/observations,
// /v1/history) stay visible, per the ADR-0018 surface model.
//
// Fail postures are deliberately asymmetric: below floor -> withhold
// (fail-closed); the substance query ERRORS -> serve (fail-open), so a DB blip
// cannot 404 the whole price surface (same as guard.go).
//
// Pairs with no on-chain leg (fiat/fiat, CEX crypto:/fiat: tickers) are exempt:
// their trades come from vendor APIs, so mint-and-dust does not apply and the
// floors would only blackout legitimate synthetic pairs.
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
	// magnitude above the measured $8.57 seed volume behind the $8.56M
	// valuation described at the top of this file, while low enough that
	// genuinely-traded small classic assets clear it.
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
	allowed  bool
	floor    SubstanceFloor
	evidence SubstanceEvidence
	expires  time.Time
}

// verdict returns the cached entry as a [SubstanceVerdict]; the evidence
// is a copy, so a caller cannot write through it into the cache.
func (e substanceVerdict) verdict() SubstanceVerdict {
	ev := e.evidence
	return SubstanceVerdict{Allowed: e.allowed, Measured: true, Floor: e.floor, Evidence: &ev}
}

// SubstanceEvidence is the measurement a substance verdict was reached
// on: the market measured (the alias union of Base and Quote), its
// figures, and the policy they were held to. It lives in the same cache
// entry as the verdict, so the two cannot disagree.
type SubstanceEvidence struct {
	Base, Quote canonical.Asset
	// Window is the trailing measurement window (Policy.Window).
	Window     time.Duration
	MeasuredAt time.Time
	// WindowEnd is the measurement instant for a live verdict and the
	// grain-truncated instant for a point-in-time one.
	WindowEnd time.Time
	// VolumeUSD is the store's decimal string, verbatim (ADR-0003).
	VolumeUSD     string
	Buckets       int64
	ValuedBuckets int64
	SpanSeconds   int64
	// Policy is the policy actually applied: the hour-grain one for an old
	// point-in-time instant, not the gate's live policy.
	Policy SubstancePolicy
	Floor  SubstanceFloor
}

// SubstanceVerdict is one pair's substance answer with its evidence.
// Measured=false means no verdict was reached (store error or deadline):
// Allowed is then false and Evidence nil. A nil gate and an ungated pair
// are Allowed and Measured with nil Evidence, because nothing was measured.
type SubstanceVerdict struct {
	Allowed  bool
	Measured bool
	Floor    SubstanceFloor
	Evidence *SubstanceEvidence
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

// Policy returns the resolved (defaults-applied) policy this gate
// serves against, for a caller that needs to derive a RELATED policy
// from the same operator-configured floor rather than duplicating the
// [DefaultSubstance*] constants.
func (g *SubstanceGate) Policy() SubstancePolicy {
	if g == nil {
		return SubstancePolicy{}.withDefaults()
	}
	return g.policy
}

// NewSubstanceGate builds a gate over the store and logs the floors it
// enforces, defaults resolved: an unset key runs at a value no config
// file shows.
func NewSubstanceGate(store SubstanceStore, opts SubstanceGateOptions) *SubstanceGate {
	g := &SubstanceGate{
		store:   store,
		policy:  opts.Policy.withDefaults(),
		logger:  opts.Logger,
		cache:   make(map[string]substanceVerdict),
		atCache: make(map[string]substanceVerdict),
	}
	if g.logger != nil {
		g.logger.Info("substance gate armed",
			"min_volume_usd", g.policy.MinVolumeUSD.RatString(), "min_buckets", g.policy.MinBuckets,
			"min_span", g.policy.MinSpan.String(), "window", g.policy.Window.String())
	}
	return g
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
// folds over; *SubstanceGate satisfies it. Probe is uncounted, so the
// asset-level decision can count once however many quotes it tries.
type SubstanceVerdicter interface {
	Probe(ctx context.Context, base, quote canonical.Asset) (allowed, measured bool, floor SubstanceFloor)
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
// operator-declared USD peg in usdPegs; the degenerate identity pair is
// skipped. Allowed when the asset is out of scope (no on-chain
// identity), is native (definitionally liquid; its identity pairs
// degenerate under the alias union), or when ANY backing quote clears
// the floor. measured is false when no quote cleared and at least one
// could not be measured. The /v1/assets listing and the priceless-popular
// tripwire both ask this, so "withheld" means one thing on the surface
// and on the alert. A nil gate allows.
//
// The quotes are probes of ONE decision, so the withheld/unmeasured
// metrics count the asset once under `surface`, not once per quote.
func AssetSubstanceVerdict(
	ctx context.Context, gate SubstanceVerdicter, asset canonical.Asset,
	usdPegs []canonical.Asset, surface string,
) (allowed, measured bool) {
	allowed, measured, floor := assetSubstance(ctx, gate, asset, usdPegs)
	if !allowed {
		countVerdict(surface, false, measured, floor)
	}
	return allowed, measured
}

// AssetSubstanceVerdictThinServed is [AssetSubstanceVerdict] for a caller
// that serves a measured-thin asset flagged under the include_thin
// opt-in. An unmeasured verdict counts as usual; a thin one counts
// nothing here, because whether the row is finally served thin is known
// only after later overlays, and the caller counts it then.
func AssetSubstanceVerdictThinServed(
	ctx context.Context, gate SubstanceVerdicter, asset canonical.Asset,
	usdPegs []canonical.Asset, surface string,
) (allowed, measured bool, floor SubstanceFloor) {
	allowed, measured, floor = assetSubstance(ctx, gate, asset, usdPegs)
	if !allowed && !measured {
		countVerdict(surface, false, false, floor)
	}
	return allowed, measured, floor
}

// assetSubstance is the uncounted fold shared by both asset verdicts.
func assetSubstance(
	ctx context.Context, gate SubstanceVerdicter, asset canonical.Asset, usdPegs []canonical.Asset,
) (allowed, measured bool, floor SubstanceFloor) {
	if gate == nil {
		return true, true, FloorNone
	}
	switch asset.Type {
	case canonical.AssetNative:
		return true, true, FloorNone
	case canonical.AssetClassic, canonical.AssetSoroban:
	default:
		return true, true, FloorNone
	}
	quotes := append([]canonical.Asset{canonical.NativeAsset(), fiatUSD}, usdPegs...)
	measured = true
	for _, quote := range quotes {
		if asset.Equal(quote) {
			continue
		}
		ok, m, f := gate.Probe(ctx, asset, quote)
		if ok && m {
			return true, true, FloorNone
		}
		if !m {
			measured = false
		}
		floor = furthestFloor(floor, f)
	}
	return false, measured, floor
}

// SubstanceFloor names the floor a withheld market failed; it is the
// `floor` label on obs.PriceServeSubstanceWithheldTotal. The floors are
// checked in the order below, so a market that fails on volume cleared
// both persistence floors.
type SubstanceFloor string

const (
	// FloorNone: every floor cleared.
	FloorNone SubstanceFloor = ""
	// FloorBuckets: too few distinct active buckets in the window.
	FloorBuckets SubstanceFloor = "buckets"
	// FloorSpan: the active buckets span too little wall-clock.
	FloorSpan SubstanceFloor = "span"
	// FloorVolume: USD volume below the floor, measured over a market
	// most of whose active buckets carried a USD valuation.
	FloorVolume SubstanceFloor = "volume"
	// FloorVolumeUnvalued: USD volume below the floor AND fewer than half
	// the active buckets carried a USD valuation — the insert-time
	// waterfall could not value this market (a SEP-41/SEP-41 pair, or an
	// XLM anchor outage), so the dollar floor failed for want of a dollar
	// figure rather than on evidence of thinness. Still withheld: an
	// unvaluable market's volume cannot be verified, and waiving the floor
	// for it would admit exactly the self-minted pair the gate exists for.
	FloorVolumeUnvalued SubstanceFloor = "volume_unvalued"
)

// floorRank orders floors by how far a market got through the checks.
var floorRank = map[SubstanceFloor]int{
	FloorNone: 0, FloorBuckets: 1, FloorSpan: 2, FloorVolume: 3, FloorVolumeUnvalued: 4,
}

// furthestFloor is the floor of whichever of two refused quotes got
// further — the asset's best market names why the asset was withheld.
func furthestFloor(a, b SubstanceFloor) SubstanceFloor {
	if floorRank[b] > floorRank[a] {
		return b
	}
	return a
}

// countVerdict records one gate decision on the withheld/unmeasured
// metrics; an allowed decision is not counted.
func countVerdict(surface string, allowed, measured bool, floor SubstanceFloor) {
	switch {
	case !measured:
		obs.PriceServeSubstanceUnmeasuredTotal.WithLabelValues(surface).Inc()
	case !allowed:
		obs.PriceServeSubstanceWithheldTotal.WithLabelValues(surface, string(floor)).Inc()
	}
}

// SubstanceOK is the pure decision: does the measured substance clear
// the policy floor? Exact-rational volume compare (ADR-0003). An
// unparseable volume string counts as zero — fail-closed, consistent
// with "if the volume cannot be verified, the floor cannot be
// verified".
func SubstanceOK(volumeUSD *big.Rat, buckets, spanSeconds int64, policy SubstancePolicy) bool {
	return FailedFloor(volumeUSD, buckets, buckets, spanSeconds, policy) == FloorNone
}

// FailedFloor is [SubstanceOK] naming the first floor the measurement
// fails, or [FloorNone]. valuedBuckets (active buckets carrying a USD
// valuation) only labels a volume failure; it never changes the verdict.
func FailedFloor(volumeUSD *big.Rat, buckets, valuedBuckets, spanSeconds int64, policy SubstancePolicy) SubstanceFloor {
	if volumeUSD == nil {
		volumeUSD = new(big.Rat)
	}
	switch {
	case buckets < policy.MinBuckets:
		return FloorBuckets
	case time.Duration(spanSeconds)*time.Second < policy.MinSpan:
		return FloorSpan
	case volumeUSD.Cmp(policy.MinVolumeUSD) < 0:
		if 2*valuedBuckets < buckets {
			return FloorVolumeUnvalued
		}
		return FloorVolume
	}
	return FloorNone
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
	allowed, measured, floor := g.Probe(ctx, base, quote)
	countVerdict(surface, allowed, measured, floor)
	return allowed, measured
}

// Probe is [SubstanceGate.Verdict] without the metrics, also naming the
// floor a withheld pair failed. It is for a caller that folds several
// pairs into one decision and counts that decision itself.
func (g *SubstanceGate) Probe(ctx context.Context, base, quote canonical.Asset) (allowed, measured bool, floor SubstanceFloor) {
	v := g.Measure(ctx, base, quote)
	return v.Allowed, v.Measured, v.Floor
}

// Measure is the live verdict with the evidence behind it, uncounted:
// whoever decides from it counts the decision. Allowed, Verdict and Probe
// are projections of it, so every live read shares one cache entry.
func (g *SubstanceGate) Measure(ctx context.Context, base, quote canonical.Asset) SubstanceVerdict {
	if g == nil || !SubstanceGated(base, quote) {
		return SubstanceVerdict{Allowed: true, Measured: true}
	}
	key := pairCacheKey(base, quote)
	now := g.clock()
	g.mu.Lock()
	prior, hadPrior := g.cache[key]
	if hadPrior && now.Before(prior.expires) {
		g.mu.Unlock()
		return prior.verdict()
	}
	g.mu.Unlock()

	entry, measured := g.measureUnion(ctx, base, quote, g.policy, now, now,
		func(ctx context.Context, bases, quotes []canonical.Asset) (timescale.MarketSubstance, error) {
			return g.store.PairMarketSubstance(ctx, bases, quotes, g.policy.Window)
		})
	if !measured {
		// Not cached, so the next request re-measures.
		return SubstanceVerdict{}
	}
	entry.expires = now.Add(substanceCacheTTL)
	g.mu.Lock()
	if len(g.cache) >= substanceCacheMax {
		g.cache = make(map[string]substanceVerdict)
	}
	g.cache[key] = entry
	g.mu.Unlock()
	g.logTransition(base, quote, entry.allowed, entry.floor, hadPrior, prior.allowed)
	return entry.verdict()
}

// logTransition logs a pair's verdict on TRANSITIONS only — first
// observation, or a flip. Logging the steady state (hundreds of thin
// long-tail pairs re-measured every TTL expiry) produced 6,000 WARNs/hour on
// r1; the metric is the volume signal, the log is the change signal.
func (g *SubstanceGate) logTransition(base, quote canonical.Asset, allowed bool, floor SubstanceFloor, hadPrior, priorAllowed bool) {
	if g.logger == nil {
		return
	}
	if !allowed && (!hadPrior || priorAllowed) {
		g.logger.Warn("substance gate: aggregated price withheld — trailing market below serve floor",
			"base", base.String(), "quote", quote.String(), "floor", string(floor))
	} else if allowed && hadPrior && !priorAllowed {
		g.logger.Info("substance gate: pair recovered above the serve floor — price serving resumed",
			"base", base.String(), "quote", quote.String())
	}
}

// measureUnion is the alias-union measurement shared by the live and the
// point-in-time gate: `read` measures every spelling of base against
// every spelling of quote as ONE market, held to `policy`. One read, not
// a per-spelling fold: XLM's SDEX and CEX legs trade in the same
// minutes, and summing each spelling's distinct-bucket count would count
// a shared minute once per spelling. measured=false means an
// infrastructure error prevented a verdict; the entry's expiry is unset.
func (g *SubstanceGate) measureUnion(
	ctx context.Context, base, quote canonical.Asset, policy SubstancePolicy, measuredAt, windowEnd time.Time,
	read func(ctx context.Context, bases, quotes []canonical.Asset) (timescale.MarketSubstance, error),
) (entry substanceVerdict, measured bool) {
	sub, err := read(ctx, canonical.AssetAliases(base), canonical.AssetAliases(quote))
	if err != nil {
		if g.logger != nil && ctx.Err() == nil {
			g.logger.Warn("substance gate: measurement failed — no verdict for the pair",
				"base", base.String(), "quote", quote.String(), "err", err)
		}
		return substanceVerdict{}, false
	}
	vol, ok := new(big.Rat).SetString(sub.VolumeUSD)
	if !ok {
		// An unparsable volume verifies nothing, so it fails the volume leg.
		vol = new(big.Rat)
	}
	floor := FailedFloor(vol, sub.Buckets, sub.ValuedBuckets, sub.SpanSeconds, policy)
	return substanceVerdict{
		allowed: floor == FloorNone,
		floor:   floor,
		evidence: SubstanceEvidence{
			Base: base, Quote: quote, Window: policy.Window,
			MeasuredAt: measuredAt.UTC(), WindowEnd: windowEnd.UTC(),
			VolumeUSD: sub.VolumeUSD, Buckets: sub.Buckets, ValuedBuckets: sub.ValuedBuckets,
			SpanSeconds: sub.SpanSeconds, Policy: policy, Floor: floor,
		},
	}, true
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

// AllowedAt reports whether an aggregated price claim for (base, quote) AS OF
// `at` may be served: the point-in-time form of [SubstanceGate.Allowed], for
// /v1/price/at and each /v1/price/changes horizon.
//
// Allowed would be wrong in both directions: a trailing window ending NOW would
// withhold history of a market that was honest at `at` but is dormant today, and
// would pass a market that is thick today but was attacker-seeded dust at `at`.
// So substance is measured over [policy.Window] ending at `at`, closed buckets
// only, alias union as for the live gate (see [SubstanceGate.policyAt]).
//
// A future `at` is measured as of now. Fail postures match the live gate (below
// floor withholds; error serves, uncached). Verdicts are cached per (pair, grain,
// truncated instant) for [substanceCacheTTL]. Withheld verdicts count on the
// metric under `surface` but are not logged: the transition log describes a
// pair's present. Nil-receiver safe: a nil gate allows everything.
func (g *SubstanceGate) AllowedAt(ctx context.Context, base, quote canonical.Asset, at time.Time, surface string) bool {
	v := g.MeasureAt(ctx, base, quote, at)
	countVerdict(surface, v.Allowed, v.Measured, v.Floor)
	return v.Allowed || !v.Measured
}

// MeasureAt is [SubstanceGate.AllowedAt]'s verdict with its evidence,
// uncounted.
func (g *SubstanceGate) MeasureAt(ctx context.Context, base, quote canonical.Asset, at time.Time) SubstanceVerdict {
	if g == nil || !SubstanceGated(base, quote) {
		return SubstanceVerdict{Allowed: true, Measured: true}
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
		return prior.verdict()
	}

	entry, measured := g.measureUnion(ctx, base, quote, policy, now, asOf,
		func(ctx context.Context, bases, quotes []canonical.Asset) (timescale.MarketSubstance, error) {
			return g.store.PairMarketSubstanceAt(ctx, bases, quotes, asOf, policy.Window, grain)
		})
	if !measured {
		return SubstanceVerdict{}
	}
	entry.expires = now.Add(substanceCacheTTL)
	g.mu.Lock()
	if len(g.atCache) >= substanceCacheMax {
		g.atCache = make(map[string]substanceVerdict)
	}
	g.atCache[key] = entry
	g.mu.Unlock()
	return entry.verdict()
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
