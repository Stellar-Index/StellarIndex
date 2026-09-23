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
// unit-testable without a database.
type MarketSubstanceReader interface {
	PairMarketSubstance(ctx context.Context, p canonical.Pair, window time.Duration) (timescale.MarketSubstance, error)
}

// MarketSubstanceAtReader is the storage seam for the POINT-IN-TIME
// question ([SubstanceGate.AllowedAt]): the same three legs, measured
// over the window ending at `asOf` rather than at now, at the named
// grain. It is a separate interface so the live gate's seam (and every
// fake of it) is untouched; [NewSubstanceGate] picks it up from the
// same store value.
type MarketSubstanceAtReader interface {
	PairMarketSubstanceAt(ctx context.Context, p canonical.Pair, asOf time.Time, window time.Duration, g timescale.HistoryGranularity) (timescale.MarketSubstance, error)
}

// The production store MUST answer the point-in-time question. The gate
// discovers the capability by type assertion, and a store that silently
// stopped satisfying it would send every /v1/price/at read back to the
// trailing-from-now verdict (finding T038) with nothing failing — so
// the assertion is made here, at compile time, instead.
var _ MarketSubstanceAtReader = (*timescale.Store)(nil)

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
// returns nil) also falls back to the default floor — never zero.
func SubstancePolicyFromValues(minVolumeUSD float64, minBuckets, minSpanMinutes, windowHours int) SubstancePolicy {
	pol := SubstancePolicy{
		MinBuckets: int64(minBuckets),
		MinSpan:    time.Duration(minSpanMinutes) * time.Minute,
		Window:     time.Duration(windowHours) * time.Hour,
	}
	if minVolumeUSD > 0 {
		pol.MinVolumeUSD = new(big.Rat).SetFloat64(minVolumeUSD)
	}
	return pol
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
	store   MarketSubstanceReader
	atStore MarketSubstanceAtReader // nil → store cannot answer point-in-time; see AllowedAt
	policy  SubstancePolicy
	logger  *slog.Logger
	now     func() time.Time // nil → time.Now

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

// NewSubstanceGate builds a gate over the store. A store that also
// implements [MarketSubstanceAtReader] (the production *timescale.Store
// does — asserted at compile time above) additionally answers the
// point-in-time question behind [SubstanceGate.AllowedAt].
func NewSubstanceGate(store MarketSubstanceReader, opts SubstanceGateOptions) *SubstanceGate {
	atStore, _ := store.(MarketSubstanceAtReader)
	return &SubstanceGate{
		store:   store,
		atStore: atStore,
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
// while SDEX writes under native), and the pair's real market breadth
// is their sum. Without the union, /v1/price?asset=native&quote=fiat:USD
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
		func(ctx context.Context, pair canonical.Pair) (timescale.MarketSubstance, error) {
			return g.store.PairMarketSubstance(ctx, pair, g.policy.Window)
		})
}

// measureUnion is the alias-union fold shared by the live and the
// point-in-time measurement: `read` supplies one spelling's substance,
// and the union is held to `policy`. One fold, so the two questions
// cannot come to disagree about what "the pair's market" is.
func (g *SubstanceGate) measureUnion(
	ctx context.Context, base, quote canonical.Asset, policy SubstancePolicy,
	read func(context.Context, canonical.Pair) (timescale.MarketSubstance, error),
) (allowed, measured bool) {
	totalVol := new(big.Rat)
	var buckets, span int64
	for _, a := range canonical.AssetAliases(base) {
		for _, q := range canonical.AssetAliases(quote) {
			pair, err := canonical.NewPair(a, q)
			if err != nil {
				// Degenerate alias combination (e.g. native/crypto:XLM
				// collapsing to an identity pair) — skip.
				continue
			}
			sub, err := read(ctx, pair)
			if err != nil {
				if g.logger != nil && ctx.Err() == nil {
					g.logger.Warn("substance gate: measurement failed — no verdict for the pair",
						"pair", pair.String(), "err", err)
				}
				return true, false
			}
			if v, ok := new(big.Rat).SetString(sub.VolumeUSD); ok {
				totalVol.Add(totalVol, v)
			}
			buckets += sub.Buckets
			if sub.SpanSeconds > span {
				span = sub.SpanSeconds
			}
		}
	}
	return SubstanceOK(totalVol, buckets, span, policy), true
}

// substanceHourGrainMinBuckets is the distinct-bucket leg of the floor
// when the legs are counted at HOUR grain (a historical instant — see
// [SubstanceGate.AllowedAt]).
//
// The minute floor's number cannot be inherited: it counts distinct
// minutes, of which a 24-hour window holds 1440, and an hour-grain
// window holds 24 buckets in total. Two is not a guess. It is the
// LARGEST hour floor that never refuses a market the minute floor
// admits: 20 distinct minutes spread over a 6-hour span are guaranteed
// to touch two hour buckets, and can touch exactly two and no more
// (ten minutes in one hour, ten in an hour six later). Any larger number
// would withhold, in history, a market this gate serves live. It is
// also the number the sibling per-day floor already uses for the same
// reason (internal/api/v1 rwaPremiumDayFloor). The volume and span legs
// are grain-independent and carry over unchanged — an hour-bucket span
// of N hours is implied by a minute span of N hours, never the reverse.
const substanceHourGrainMinBuckets = 2

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
	hourly := g.policy
	if hourly.MinBuckets > substanceHourGrainMinBuckets {
		hourly.MinBuckets = substanceHourGrainMinBuckets
	}
	return timescale.Granularity1h, hourly
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
// A gate whose store cannot answer the point-in-time question falls
// back to the trailing verdict rather than serving unguarded. That is
// unreachable in production: *timescale.Store is asserted to implement
// [MarketSubstanceAtReader] at compile time.
//
// Nil-receiver safe: a nil gate allows everything.
func (g *SubstanceGate) AllowedAt(ctx context.Context, base, quote canonical.Asset, at time.Time, surface string) bool {
	if g == nil {
		return true
	}
	if !SubstanceGated(base, quote) {
		return true
	}
	if g.atStore == nil {
		return g.Allowed(ctx, base, quote, surface)
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
		func(ctx context.Context, pair canonical.Pair) (timescale.MarketSubstance, error) {
			return g.atStore.PairMarketSubstanceAt(ctx, pair, asOf, policy.Window, grain)
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

// PriceWithheldAt is [PriceWithheld] for a point-in-time read: the same
// one expression over the same two gates, with the substance half asked
// about the instant being served instead of about now. The scam half is
// deliberately NOT moved in time — a directory flag is an owner-level
// trust decision about the issuer, and it withholds that issuer's
// history along with its present.
//
// It sits beside [SubstanceGate.AllowedAt] rather than being spelled by
// a caller for the reason [PriceWithheld] exists at all: a hand-written
// call site can consult one gate and forget the other (MSP-07).
func PriceWithheldAt(
	ctx context.Context,
	substance *SubstanceGate,
	scam *ScamGate,
	base, quote canonical.Asset,
	at time.Time,
	surface string,
) bool {
	return PriceWithholdingAt(ctx, substance, scam, base, quote, at, surface) != NotWithheld
}

// PriceWithholdingAt is [PriceWithheldAt] reporting which gate fired,
// in the same scam-first order as [PriceWithholding].
func PriceWithholdingAt(
	ctx context.Context,
	substance *SubstanceGate,
	scam *ScamGate,
	base, quote canonical.Asset,
	at time.Time,
	surface string,
) Withholding {
	return WithholdingFor(scam.WithheldPair(ctx, base, quote, surface), substance.AllowedAt(ctx, base, quote, at, surface))
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
