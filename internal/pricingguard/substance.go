// Substance gate: the serving-side thin-market floor.
//
// The trailing-baseline guard in guard.go is blind to a market whose ENTIRE history is
// attacker-authored: anyone can mint a token and seed a "market" with dust trades. This gate
// refuses an AGGREGATED price claim for an on-chain pair whose trailing activity is below an
// operator-set floor (USD volume, distinct 1-minute buckets, wall-clock span). Raw surfaces
// (/v1/ohlc, /v1/observations, /v1/history) stay visible (ADR-0018).
//
// Fail postures are asymmetric: below floor withholds (fail-closed); a substance query ERROR
// serves (fail-open), so a DB blip cannot 404 the price surface.
//
// Pairs with no on-chain leg (fiat/fiat, CEX tickers) are exempt: their trades come from vendor
// APIs, so mint-and-dust does not apply and the floors would only blackout synthetic pairs.
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

// MarketSubstanceReader is the storage seam the substance gate needs; *timescale.Store
// satisfies it. It takes each leg's full set of spellings so the union is measured in one read:
// distinct buckets do not add across spellings that trade in the same minute.
type MarketSubstanceReader interface {
	PairMarketSubstance(ctx context.Context, bases, quotes []canonical.Asset, window time.Duration) (timescale.MarketSubstance, error)
}

// MarketSubstanceAtReader is the storage seam for the POINT-IN-TIME question
// ([SubstanceGate.AllowedAt]): the same three legs, measured over the window ending at `asOf`
// at the named grain.
type MarketSubstanceAtReader interface {
	PairMarketSubstanceAt(ctx context.Context, bases, quotes []canonical.Asset, asOf time.Time, window time.Duration, g timescale.HistoryGranularity) (timescale.MarketSubstance, error)
}

// SubstanceStore is the store [NewSubstanceGate] requires: both questions, so a store that
// answers only the live one fails to compile instead of silently judging history by today's
// market.
type SubstanceStore interface {
	MarketSubstanceReader
	MarketSubstanceAtReader
}

var _ SubstanceStore = (*timescale.Store)(nil)

// SubstancePolicy is the serve floor. Zero-valued fields take the defaults below; binaries map
// config.PricingGuardConfig onto this (this package stays config-free).
type SubstancePolicy struct {
	// MinVolumeUSD is the minimum trailing-window USD volume (exact-rational compare, ADR-0003).
	// Default $1,000: above dust-seeding volumes, low enough for genuinely-traded small assets.
	MinVolumeUSD *big.Rat
	// MinBuckets is the minimum count of distinct closed 1-minute buckets with a trade. Default
	// 20: a single-burst wash session produces few buckets however large it is.
	MinBuckets int64
	// MinSpan is the minimum wall-clock spread (max bucket - min bucket). Default 6h: it forces an
	// attacker to sustain a consistent fake market for hours, not minutes.
	MinSpan time.Duration
	// Window is the trailing measurement window. Default 24h.
	Window time.Duration
}

// Default substance floors; rationale is on the [SubstancePolicy] fields.
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

// SubstancePolicyFromValues maps raw [pricing_guard] config values onto a policy. Zero, a NaN/Inf
// volume and an overflowing span/window all fall back to the defaults, never zero or negative.
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

// durationOrZero is n*unit, or 0 (use the default) on overflow; a wrapped window would fail open.
func durationOrZero(n int, unit time.Duration) time.Duration {
	if int64(n) > math.MaxInt64/int64(unit) || int64(n) < math.MinInt64/int64(unit) {
		return 0
	}
	return time.Duration(n) * unit
}

// substanceCacheTTL caps verdict staleness: ~1 substance query per pair per minute regardless of
// request rate, and a pair crossing the floor flips within a minute.
const substanceCacheTTL = 60 * time.Second

// substanceCacheMax bounds the verdict cache, far above any realistic hot set. Overflow drops the
// whole map; a reset costs one substance query per hot pair.
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

// SubstanceEvidence is the measurement a verdict was reached on (the alias union of Base and
// Quote, its figures, the policy applied). It shares the verdict's cache entry, so they agree.
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

// SubstanceVerdict is one pair's substance answer with its evidence. Measured=false means no
// verdict (store error or deadline): Allowed is false and Evidence nil. A nil gate and an ungated
// pair are Allowed and Measured with nil Evidence.
type SubstanceVerdict struct {
	Allowed  bool
	Measured bool
	Floor    SubstanceFloor
	Evidence *SubstanceEvidence
}

// SubstanceGate is the serving-side thin-market gate. Build with [NewSubstanceGate]; a nil
// *SubstanceGate is a valid no-op gate that allows everything.
type SubstanceGate struct {
	store  SubstanceStore
	policy SubstancePolicy
	logger *slog.Logger
	now    func() time.Time // nil → time.Now

	mu    sync.Mutex
	cache map[string]substanceVerdict
	// atCache holds point-in-time verdicts, keyed by pair + grain + truncated instant. Separate
	// from `cache` because the instant is caller-chosen: a client walking timestamps can fill
	// this map at will, and its overflow reset must not evict the LIVE verdicts.
	atCache map[string]substanceVerdict
}

// SubstanceGateOptions configures [NewSubstanceGate]; a nil Logger disables logging only.
type SubstanceGateOptions struct {
	Policy SubstancePolicy
	Logger *slog.Logger
}

// Policy returns the resolved (defaults-applied) policy, for deriving a RELATED policy from the
// same operator-configured floor.
func (g *SubstanceGate) Policy() SubstancePolicy {
	if g == nil {
		return SubstancePolicy{}.withDefaults()
	}
	return g.policy
}

// NewSubstanceGate builds a gate and logs the floors it enforces, defaults resolved, since an
// unset key runs at a value no config file shows.
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

// SubstanceGated reports whether the pair is in scope: at least one leg is on-chain (native /
// classic / soroban), which anyone can author trades for. Off-chain synthetic pairs are out.
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

// SubstanceVerdicter is the per-pair seam [AssetSubstanceVerdict] folds over. Probe is uncounted,
// so the asset-level decision counts once however many quotes it tries.
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

// AssetSubstanceVerdict is the gate's answer for one asset's USD price rather than one pair. The
// backing quotes are XLM, fiat:USD and each USD peg in usdPegs; the identity pair is skipped.
// Allowed when the asset is out of scope, is native (definitionally liquid), or ANY quote clears
// the floor. measured is false when none cleared and at least one could not be measured. A nil
// gate allows.
//
// The quotes are probes of ONE decision, so the metrics count the asset once under `surface`.
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

// AssetSubstanceVerdictThinServed is [AssetSubstanceVerdict] for include_thin callers. A thin
// verdict is not counted here: whether the row is finally served thin is known only after later
// overlays, and the caller counts it then.
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

// SubstanceFloor names the floor a withheld market failed (the `floor` label on
// obs.PriceServeSubstanceWithheldTotal). Floors are checked in the order below, so a volume
// failure cleared both persistence floors.
type SubstanceFloor string

const (
	// FloorNone: every floor cleared.
	FloorNone SubstanceFloor = ""
	// FloorBuckets: too few distinct active buckets in the window.
	FloorBuckets SubstanceFloor = "buckets"
	// FloorSpan: the active buckets span too little wall-clock.
	FloorSpan SubstanceFloor = "span"
	// FloorVolume: USD volume below the floor over a mostly USD-valued market.
	FloorVolume SubstanceFloor = "volume"
	// FloorVolumeUnvalued: volume below the floor AND under half the active buckets carried a USD
	// valuation (a SEP-41/SEP-41 pair, or an XLM anchor outage). Still withheld: an unvaluable
	// market's volume cannot be verified, and waiving the floor would admit exactly the
	// self-minted pair the gate exists for.
	FloorVolumeUnvalued SubstanceFloor = "volume_unvalued"
)

// floorRank orders floors by how far a market got through the checks.
var floorRank = map[SubstanceFloor]int{
	FloorNone: 0, FloorBuckets: 1, FloorSpan: 2, FloorVolume: 3, FloorVolumeUnvalued: 4,
}

// furthestFloor picks the floor of the refused quote that got further; the best market names why.
func furthestFloor(a, b SubstanceFloor) SubstanceFloor {
	if floorRank[b] > floorRank[a] {
		return b
	}
	return a
}

// countVerdict records a withheld/unmeasured decision; an allowed one is not counted.
func countVerdict(surface string, allowed, measured bool, floor SubstanceFloor) {
	switch {
	case !measured:
		obs.PriceServeSubstanceUnmeasuredTotal.WithLabelValues(surface).Inc()
	case !allowed:
		obs.PriceServeSubstanceWithheldTotal.WithLabelValues(surface, string(floor)).Inc()
	}
}

// SubstanceOK is the pure decision: does the measured substance clear the policy floor?
// Exact-rational volume compare (ADR-0003). An unparseable volume string counts as zero
// (fail-closed): a volume that cannot be verified cannot clear the floor.
func SubstanceOK(volumeUSD *big.Rat, buckets, spanSeconds int64, policy SubstancePolicy) bool {
	return FailedFloor(volumeUSD, buckets, buckets, spanSeconds, policy) == FloorNone
}

// FailedFloor is [SubstanceOK] naming the first failed floor, or [FloorNone]. valuedBuckets only
// labels a volume failure; it never changes the verdict.
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

// Allowed reports whether an aggregated price claim for (base, quote) may be served. `surface`
// labels the withheld metric (obs.PriceServeSubstanceWithheldTotal) so operators can see WHICH
// serving path is withholding: a low-cardinality constant, never a pair string.
//
// The measurement is the ALIAS UNION of the pair: XLM's three spellings (native / crypto:XLM / the
// SAC) hold disjoint venue populations; volumes add, and a minute active under two spellings is
// one bucket. Without it, native/fiat:USD would measure zero rows and withhold XLM itself.
//
// Verdicts are cached for [substanceCacheTTL] per direction-insensitive pair key. A nil gate
// allows everything.
//
// Allowed FAILS OPEN when the pair could not be measured. A surface that must not publish an
// unverified claim (listings, market caps, ADR-0018) asks [SubstanceGate.Verdict].
func (g *SubstanceGate) Allowed(ctx context.Context, base, quote canonical.Asset, surface string) bool {
	allowed, measured := g.Verdict(ctx, base, quote, surface)
	return allowed || !measured
}

// Verdict is [SubstanceGate.Allowed] without the fail-open: measured is false (and allowed false)
// when a store error or deadline prevented a verdict, so the caller decides what that means on
// its surface. Unmeasured verdicts count in obs.PriceServeSubstanceUnmeasuredTotal.
func (g *SubstanceGate) Verdict(ctx context.Context, base, quote canonical.Asset, surface string) (allowed, measured bool) {
	allowed, measured, floor := g.Probe(ctx, base, quote)
	countVerdict(surface, allowed, measured, floor)
	return allowed, measured
}

// Probe is [SubstanceGate.Verdict] without the metrics, also naming the failed floor, for a
// caller that folds several pairs into one decision and counts it itself.
func (g *SubstanceGate) Probe(ctx context.Context, base, quote canonical.Asset) (allowed, measured bool, floor SubstanceFloor) {
	v := g.Measure(ctx, base, quote)
	return v.Allowed, v.Measured, v.Floor
}

// Measure is the live verdict with its evidence, uncounted. Allowed, Verdict and Probe project
// it, so every live read shares one cache entry.
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

// logTransition logs a verdict on TRANSITIONS only (first observation, or a flip): the steady
// state of hundreds of thin pairs re-measured every TTL is too noisy. The metric is the volume
// signal, the log the change signal.
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

// measureUnion is the alias-union measurement shared by the live and point-in-time gates: `read`
// measures all spellings of base against all of quote as ONE market. One read, not a per-spelling
// fold, since legs trading in the same minutes would be counted repeatedly. measured=false means
// an infrastructure error; the entry's expiry is unset.
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

// hourGrainPolicy is the floor for legs counted at HOUR grain (a historical instant, see
// [SubstanceGate.AllowedAt]): the strictest floor that never refuses a market the minute-grain
// policy p admits, so history and live agree under every legal policy.
//
// Buckets: n distinct minutes fill at least ceil(n/60) hours, and a span of an hour or more
// puts the first and last minute in different hours. Span: minutes S apart sit at least
// floor(S/1h) whole hours apart. Volume is grain-independent.
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

// policyAt returns the grain a point-in-time measurement is counted at and the floor that grain
// is held to, for an instant `age` behind now.
//
// The boundary is the point-in-time READER's own ([timescale.PriceAtMinuteRungMaxAge]): inside it
// the served number can be a raw prices_1m bucket, the attacker-authorable minute this gate is
// for, so the live grain and floor apply with only the window moved. 48h + one window is inside
// every prices_1m retention the schema has carried. Past it the reader serves hour and day bars,
// so legs are counted on prices_1h, which is indefinite and re-materialised with prices_1d.
func (g *SubstanceGate) policyAt(age time.Duration) (timescale.HistoryGranularity, SubstancePolicy) {
	if age <= timescale.PriceAtMinuteRungMaxAge {
		return timescale.Granularity1m, g.policy
	}
	return timescale.Granularity1h, hourGrainPolicy(g.policy)
}

// AllowedAt reports whether an aggregated price claim for (base, quote) AS OF `at` may be
// served: the point-in-time form of [SubstanceGate.Allowed], for /v1/price/at and each
// /v1/price/changes horizon.
//
// Allowed would be wrong both ways: a window ending NOW would withhold history of a market honest
// at `at` but dormant today, and pass one thick today but attacker-seeded dust at `at`. So
// substance is measured over [policy.Window] ending at `at`, closed buckets only, alias union as
// live (see [SubstanceGate.policyAt]).
//
// A future `at` is measured as of now. Fail postures match the live gate. Verdicts are cached per
// (pair, grain, truncated instant) for [substanceCacheTTL]; withheld ones are counted under
// `surface` but not logged, since the transition log describes a pair's present.
func (g *SubstanceGate) AllowedAt(ctx context.Context, base, quote canonical.Asset, at time.Time, surface string) bool {
	v := g.MeasureAt(ctx, base, quote, at)
	countVerdict(surface, v.Allowed, v.Measured, v.Floor)
	return v.Allowed || !v.Measured
}

// MeasureAt is [SubstanceGate.AllowedAt]'s verdict with its evidence, uncounted.
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
	// Truncating changes no verdict's upper edge, makes the window whole buckets and the key shareable.
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

// pairCacheKey is direction-insensitive: (A,B) and (B,A) share a verdict.
func pairCacheKey(base, quote canonical.Asset) string {
	b, q := base.String(), quote.String()
	if b > q {
		b, q = q, b
	}
	return b + "\x00" + q
}
