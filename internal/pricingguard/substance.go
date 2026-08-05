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
	store  MarketSubstanceReader
	policy SubstancePolicy
	logger *slog.Logger
	now    func() time.Time // nil → time.Now

	mu    sync.Mutex
	cache map[string]substanceVerdict
}

// SubstanceGateOptions configures [NewSubstanceGate]. Zero-valued
// policy fields fall back to the package defaults; a nil Logger
// disables warn logging (decisions are unaffected).
type SubstanceGateOptions struct {
	Policy SubstancePolicy
	Logger *slog.Logger
}

// NewSubstanceGate builds a gate over the store.
func NewSubstanceGate(store MarketSubstanceReader, opts SubstanceGateOptions) *SubstanceGate {
	return &SubstanceGate{
		store:  store,
		policy: opts.Policy.withDefaults(),
		logger: opts.Logger,
		cache:  make(map[string]substanceVerdict),
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
func (g *SubstanceGate) Allowed(ctx context.Context, base, quote canonical.Asset, surface string) bool {
	if g == nil {
		return true
	}
	if !SubstanceGated(base, quote) {
		return true
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
		return prior.allowed
	}
	g.mu.Unlock()

	allowed, measured := g.measure(ctx, base, quote)
	if !measured {
		// Fail-open on infrastructure error — and do NOT cache, so the
		// next request re-measures.
		return true
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
	return allowed
}

// measure runs the alias-union substance measurement. measured=false
// means an infrastructure error prevented a verdict (caller fails
// open).
func (g *SubstanceGate) measure(ctx context.Context, base, quote canonical.Asset) (allowed, measured bool) {
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
			sub, err := g.store.PairMarketSubstance(ctx, pair, g.policy.Window)
			if err != nil {
				if g.logger != nil && ctx.Err() == nil {
					g.logger.Warn("substance gate: measurement failed — serving unguarded",
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
	return SubstanceOK(totalVol, buckets, span, g.policy), true
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
