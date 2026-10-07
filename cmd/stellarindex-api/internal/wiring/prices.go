package wiring

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/redis/go-redis/v9"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/cachekeys"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/pricingguard"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// RedisTriangulatedLooker adapts the shared Redis client to
// v1.TriangulatedPriceLooker. Reads:
//
//   - cachekeys.VWAP(base, quote, window) — the value
//   - cachekeys.VWAPProvenance(...)        — the marker
//   - cachekeys.VWAPObservedAt(...)        — when the value was observed
//   - cachekeys.VWAPCoverage(...)          — how much of the window it read
//
// Per the marker contract, "triangulated" means the aggregator's
// triangulation worker wrote this value (vs. the direct per-pair
// refresh, which doesn't write the marker). Absence of the marker
// → isTriangulated=false; the handler still serves the cached value,
// labelled flags.triangulated=false, because aggregator-rewritten
// pairs (XLM/fiat:USD) have no prices_1m row to fall back on.
//
// Cache miss returns (found=false, no error), and so does a value with
// no readable observed-at stamp: its age is unknowable, and a freeze
// keeps a value alive for its whole hold. Read errors propagate so the
// handler can log and fall through to its other fallbacks.
type RedisTriangulatedLooker struct{ RDB redis.UniversalClient }

func (r RedisTriangulatedLooker) LookupTriangulatedVWAP(
	ctx context.Context, base, quote canonical.Asset, window time.Duration,
) (v1.CachedVWAP, bool, error) {
	if r.RDB == nil {
		return v1.CachedVWAP{}, false, nil
	}
	// One MGET: the aggregator writes value, marker, stamp and coverage in one
	// MULTI/EXEC, and separate GETs could straddle a write and pair a
	// value with another write's provenance or stamp.
	valKey := cachekeys.VWAP(base, quote, window)
	provKey := cachekeys.VWAPProvenance(base, quote, window)
	atKey := cachekeys.VWAPObservedAt(base, quote, window)
	covKey := cachekeys.VWAPCoverage(base, quote, window)
	got, err := r.RDB.MGet(ctx, valKey.String(), provKey.String(), atKey.String(), covKey.String()).Result()
	if err != nil {
		return v1.CachedVWAP{}, false, fmt.Errorf("vwap cache mget %s: %w", valKey, err)
	}
	val, ok := got[0].(string)
	if !ok {
		return v1.CachedVWAP{}, false, nil
	}
	rawAt, _ := got[2].(string)
	observedAt, err := cachekeys.ParseVWAPObservedAt(rawAt)
	if err != nil {
		return v1.CachedVWAP{}, false, nil //nolint:nilerr // no readable stamp is a miss, not a Redis error
	}
	// A missing marker means a direct VWAP (per the marker contract):
	// found=true, isTriangulated=false — served, but not labelled triangulated.
	prov, _ := got[1].(string)
	out := v1.CachedVWAP{
		Value:        val,
		Triangulated: prov == cachekeys.VWAPProvenanceTriangulated,
		ObservedAt:   observedAt,
	}
	// Absent or unreadable coverage stays unknown, never "complete".
	if rawCov, ok := got[3].(string); ok {
		if c, err := cachekeys.ParseVWAPCoverage(rawCov); err == nil {
			out.Coverage = &c
		}
	}
	return out, true, nil
}

// LookupCompositeMeta reads the router quality-flags blob the
// aggregator writes alongside a triangulated composite
// (cachekeys.VWAPCompositeMeta), satisfying the optional
// v1.CompositeMetaLooker capability so /v1/price can surface
// flags.diverged / flags.rerouted. Cache miss → (nil, false, nil);
// read errors propagate. Best-effort: the handler leaves the flags
// unset on any error, so a missing/failed meta never fails the request.
func (r RedisTriangulatedLooker) LookupCompositeMeta(
	ctx context.Context, base, quote canonical.Asset, window time.Duration,
) ([]byte, bool, error) {
	if r.RDB == nil {
		return nil, false, nil
	}
	key := cachekeys.VWAPCompositeMeta(base, quote, window)
	val, err := r.RDB.Get(ctx, key.String()).Bytes()
	countRedisRead("prices_redis", "composite_meta", err)
	if errors.Is(err, redis.Nil) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("composite meta cache get %s: %w", key, err)
	}
	return val, true, nil
}

// GlobalPriceStore is the storage seam [GlobalPriceReader] reads;
// *timescale.Store satisfies it.
type GlobalPriceStore interface {
	pricingguard.TrailingReader
	LatestClosedVWAP1mForPair(ctx context.Context, p canonical.Pair) (timescale.Vwap1mRow, error)
	LatestAggregatorPricesForPair(ctx context.Context, base, quote canonical.Asset, sources []string) ([]canonical.OracleUpdate, error)
}

// GlobalPriceReader adapts *timescale.Store + the existing Redis
// triangulated looker to aggregate.GlobalPriceReader (R-018 Phase
// 1.4a). Each method maps to one tier of ComputeGlobalPrice:
//
//   - LatestVWAP → Store.LatestClosedVWAP1mForPair (tier 1); the raw
//     ratio, decimals-corrected by v1's decimalsCorrectedGlobalReader
//   - LatestAggregatorPrices → Store.LatestAggregatorPricesForPair (tier 2)
//   - LookupTriangulated → wraps RedisTriangulatedLooker (tier 3)
//
// Constructed once at startup and passed via v1.Options.GlobalPrice.
type GlobalPriceReader struct {
	S         GlobalPriceStore
	Tri       RedisTriangulatedLooker
	PKPairFor func(base, quote canonical.Asset) (canonical.Pair, error)
	Logger    *slog.Logger                // nil → no guard logging
	Substance *pricingguard.SubstanceGate // nil → no thin-market gate
	Scam      *pricingguard.ScamGate      // nil → no scam gate
}

func (g GlobalPriceReader) LatestVWAP(ctx context.Context, base, quote canonical.Asset) (string, time.Time, int64, []string, bool, error) {
	pair, err := g.PKPairFor(base, quote)
	if err != nil {
		// Invalid pair (e.g. quote == base, or any other allow-list
		// violation) is the same as "no data" from this seam's
		// perspective — caller falls through to the aggregator tier.
		return "", time.Time{}, 0, nil, false, nil //nolint:nilerr // intentional: invalid pair → "no data" from this tier
	}
	row, err := g.S.LatestClosedVWAP1mForPair(ctx, pair)
	if errors.Is(err, sql.ErrNoRows) {
		return "", time.Time{}, 0, nil, false, nil
	}
	if err != nil {
		return "", time.Time{}, 0, nil, false, err
	}
	// Thin-market substance gate ([pricing_guard]): a headline price on
	// the GlobalAssetView is an aggregated claim, and a pair whose whole
	// market is attacker-authorable must not publish one. Withheld reads
	// degrade to "no data" here — the caller falls through to its
	// aggregator tier, whose orchestrator applies its own min-USD-volume
	// floor.
	if PriceWithheld(ctx, g.Substance, g.Scam, base, quote, "asset_headline") != pricingguard.NotWithheld {
		return "", time.Time{}, 0, nil, false, nil
	}
	// Same raw-CAGG serving-sanity guard as /v1/price
	// (StorePriceReader.LatestPrice): LatestClosedVWAP1mForPair returns a
	// bare Σ(quote)/Σ(base) closed bucket that bypasses the orchestrator's
	// σ-outlier filter / min-USD-volume gate / freeze protection, so the
	// GlobalAssetView headline price carries the identical unfiltered
	// fat-finger / manipulation vector. The guard serves last-known-good
	// when the latest bucket is grossly off its recent trailing baseline,
	// and is a byte-identical pass-through on a healthy bucket. The
	// headline has no stale flag to carry an unvalidated bucket (no
	// trailing baseline) or a held last-known-good one, so either reads as
	// "no data" and the caller falls through to its other tiers, where the
	// change shows in price_authority instead of a silently frozen vwap_native.
	served, lowConfidence, substituted := pricingguard.GuardServedVWAP1mConfidence(ctx, g.S, g.Logger, pair, row)
	if lowConfidence || substituted {
		return "", time.Time{}, 0, nil, false, nil
	}
	// row.Bucket is the bucket's *start*; the closed-bucket contract
	// (ADR-0015) means the bucket's served observation_at is the
	// bucket end. Add one minute to surface the consumer-facing
	// timestamp matching every other closed-bucket surface.
	asOf := served.Bucket.Add(time.Minute)
	return served.VWAP, asOf, served.TradeCount, served.Sources, true, nil
}

func (g GlobalPriceReader) LatestAggregatorPrices(ctx context.Context, base, quote canonical.Asset, sources []string) ([]canonical.OracleUpdate, error) {
	// Scam gate: without it a flagged issuer's headline, withheld by tier
	// 1, resurfaces from tier 2. Substance is not asked, as on tier 3.
	if PriceWithheld(ctx, nil, g.Scam, base, quote, "asset_headline") != pricingguard.NotWithheld {
		return nil, nil
	}
	return g.S.LatestAggregatorPricesForPair(ctx, base, quote, sources)
}

func (g GlobalPriceReader) LookupTriangulated(ctx context.Context, base, quote canonical.Asset, window time.Duration) (string, time.Time, bool, error) {
	// Scam-issuer gate, through the chokepoint. Tier 1 above withholds a
	// flagged issuer's headline; this tier served it, and it is the tier a
	// Stellar-only token actually reaches — its literal <asset>/fiat:USD
	// pair has no prices_1m rows, so tier 1 misses by construction and the
	// headline comes from the aggregator's cache instead. A flagged
	// issuer's asset page carrying a price and a market cap IS the
	// decision this gate was built for. Withheld
	// degrades to "no data", exactly as tier 1 does: the caller falls
	// through, and the on-chain headline fallback it lands on is gated by
	// the listing's own substance screen.
	//
	// The substance gate is passed nil DELIBERATELY — it is not a gate
	// this tier can ask. Its floor is measured over the pair's alias
	// union, and a triangulated pair has zero rows in its literal form by
	// construction (that absence is why this tier exists), so asking it
	// here would withhold every Stellar-only token's headline for
	// absence-of-a-literal-market rather than for a thin one. The
	// substance question about the REAL underlying market is asked where
	// it can be answered — tier 1, and the per-asset listing gate behind
	// the on-chain fallback. Same split /v1/twap, /v1/vwap and
	// /v1/price's cache-backed fallback chain make.
	if PriceWithheld(ctx, nil, g.Scam, base, quote, "asset_headline") != pricingguard.NotWithheld {
		return "", time.Time{}, false, nil
	}
	v, found, err := g.Tri.LookupTriangulatedVWAP(ctx, base, quote, window)
	if err != nil || !found || !v.Triangulated {
		// `found && !Triangulated` means the cache had a direct (non-
		// triangulated) value — per the marker contract we shouldn't
		// serve that as the triangulation tier. Tell the caller the
		// tier missed.
		return "", time.Time{}, false, err
	}
	return v.Value, v.ObservedAt, true, nil
}

// StorePriceReader adapts *timescale.Store to v1.PriceReader.
//
// This MVP impl always falls back to "last trade in the trades
// hypertable" and reports stale=true. Once the aggregator ships,
// swap this for an adapter that reads `price:<asset>` from Redis
// first and this trade-based path becomes the second-level
// fallback.
// defaultVWAPFreshness: a closed 1m VWAP bucket whose close is older than
// this is served with stale=true. Well above the structural
// 1-2min closed-bucket floor so active pairs stay stale=false, but decisive
// on genuinely dormant pairs (the bug: a 200-day-old VWAP was served
// stale=false for the ~250k dormant/delisted long-tail).
const defaultVWAPFreshness = 15 * time.Minute

// PriceWithheld is THE withholding chokepoint for every price-serving
// read seam (MSP cluster, wave D). The scam/substance decision lives
// here rather than inline in each reader, because a new seam has no
// obligation to remember it. Every seam that serves a
// number derived from a closed VWAP bucket routes through here, and
// TestPriceServingSeamsAreGated enumerates those seams so a new
// ungated one fails CI rather than shipping.
//
// Routing through one function is also what keeps the two gates
// SYMMETRIC. Hand-written call sites drifted apart: the last-trade arm
// of LatestPrice consulted substance but not scam, so an operator who
// set disable_substance_gate=true to widen pricing coverage silently
// also un-withheld every directory-flagged issuer (MSP-07). Callers
// cannot make that mistake here — there is one expression, and
// TestWithholdingGatesAreSpelledOnlyAtTheChokepoint fails if a future
// call site spells either gate out again.
//
// The expression itself lives in pricingguard.Gate, shared with the
// aggregator's customer-webhook surfaces. This function stays because
// the seam guard above derives its subject set from calls to it by name.
//
// Both gates are nil-receiver safe (nil == allow-everything), so an
// operator who disabled [pricing_guard] keeps today's behaviour.
//
// It returns WHICH gate fired, and a reader seam hands that to
// v1.PriceWithheldError so the response names the real cause: a
// flagged issuer's market must not be described as merely thin.
//
// A seam that serves the price AS OF a past instant says so with
// [AsOfInstant], and the thin-market half is then measured over the
// window ending at that instant instead of at now. It is an
// option on THIS function rather than a second chokepoint so that both
// structural guards keep holding by construction: there is still one
// name a seam must call, and still no gate method spelled outside it.
func PriceWithheld(
	ctx context.Context,
	substance *pricingguard.SubstanceGate,
	scam *pricingguard.ScamGate,
	base, quote canonical.Asset,
	surface string,
	opts ...WithholdingOption,
) pricingguard.Withholding {
	var q withholdingQuery
	for _, opt := range opts {
		opt(&q)
	}
	adm := v1.ThinAdmissionFrom(ctx)
	gate := pricingguard.Gate{Substance: substance, Scam: scam}
	v := gate.Judge(ctx, base, quote, surface, pricingguard.Query{
		PointInTime: q.pointInTime,
		At:          q.at,
		AdmitThin:   adm.Requested() && adm.Covers(base, quote),
	})
	adm.Record(base, quote, v)
	return v.Withholding
}

// withholdingQuery is what a seam may tell the chokepoint about the
// read it is gating. pointInTime is an explicit flag rather than "at is
// non-zero" so that a zero time.Time — what a caller gets from a failed
// parse — can never quietly select the live measurement.
type withholdingQuery struct {
	pointInTime bool
	at          time.Time
}

type WithholdingOption func(*withholdingQuery)

// AsOfInstant marks the gated read as point-in-time: the number being
// served is the bucket at-or-before ts, so the market whose substance
// matters is the one that existed in the window ending at ts.
func AsOfInstant(ts time.Time) WithholdingOption {
	return func(q *withholdingQuery) {
		q.pointInTime = true
		q.at = ts
	}
}

type StorePriceReader struct {
	S             *timescale.Store
	VWAPFreshness time.Duration               // 0 → defaultVWAPFreshness
	Now           func() time.Time            // nil → time.Now
	Logger        *slog.Logger                // nil → no guard logging
	Substance     *pricingguard.SubstanceGate // nil → no thin-market gate
	Scam          *pricingguard.ScamGate      // nil → no scam-issuer gate
}

func (r StorePriceReader) freshnessWindow() time.Duration {
	if r.VWAPFreshness > 0 {
		return r.VWAPFreshness
	}
	return defaultVWAPFreshness
}

// bucketIsStale is the staleness rule: a closed 1m bucket is
// stale once its CLOSE (bucket start + 1 minute) is older than the
// freshness window, or whenever the read was low-confidence.
//
// Separate from LatestPrice so a test can exercise the REAL rule: a test
// that re-implemented the expression locally would stay green after
// deleting the `> r.freshnessWindow()` term while /v1/price served
// months-old buckets with stale=false, which IS the bug it exists to stop.
// LatestPrice needs a live *timescale.Store, so calling it from a unit
// test is not possible; calling this is.
//
// Measured from the CLOSE, not the bucket start: a 1-minute CAGG bucket
// is not closed until its minute elapses, so measuring from the start
// would report every bucket a minute older than it is.
func (r StorePriceReader) bucketIsStale(bucket time.Time, lowConfidence bool) bool {
	return lowConfidence || r.clock().Sub(bucket.Add(time.Minute)) > r.freshnessWindow()
}

// guardedSnapshot builds the snapshot /v1/price serves from the
// serving-sanity guard's verdict on the latest closed bucket.
func (r StorePriceReader) guardedSnapshot(
	asset, quote canonical.Asset,
	served timescale.Vwap1mRow,
	lowConfidence, substituted bool,
) (v1.PriceSnapshot, bool) {
	// The bucket closes at Bucket+1min; flag stale when that
	// close is older than the freshness window, so a dormant pair's
	// months-old VWAP is not served as stale=false. Applied to the
	// bucket we actually serve (candidate, or the older last-known-good
	// on a guard rejection).
	//
	// W6-fresh-1: a pair's first-ever served minute has NO trailing
	// baseline, so the guard fails OPEN (accepts any value, even a lone
	// manipulated/fat-finger print). lowConfidence marks that unvalidated
	// case; serve the value but as stale, never as a confident price.
	//
	// A substituted bucket is a held value standing in for the current
	// minute, as a frozen serve is, so it is stale however recent it is.
	stale := r.bucketIsStale(served.Bucket, lowConfidence || substituted)
	snap := v1.VWAP1mToSnapshot(asset.String(), quote.String(), served.VWAP, served.Bucket)
	// RNC27: substituted means the guard swapped in an older
	// last-known-good bucket for `row`. The handler's confidence/
	// composite-flags staples are looked up from a SEPARATE cache keyed
	// by (pair, window) with no as-of of their own, so they answer for
	// the current tick, not for this older bucket — snap.Substituted
	// tells the handler to withhold them rather than mis-attribute a
	// live read to the substituted value.
	snap.Substituted = substituted
	return snap, stale
}

func (r StorePriceReader) clock() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r StorePriceReader) LatestPrice(ctx context.Context, asset, quote canonical.Asset) (v1.PriceSnapshot, []string, bool, error) {
	pair, err := canonical.NewPair(asset, quote)
	if err != nil {
		return v1.PriceSnapshot{}, nil, false, err
	}

	// Primary path: most-recent CLOSED 1-minute VWAP from the prices_1m
	// CAGG (per ADR-0015 we serve only closed buckets). Note the CAGG is a
	// bare Σ(quote)/Σ(base) per bucket — it is NOT the orchestrator's
	// filtered VWAP. The σ-outlier filter, the min-USD-volume gate, and
	// freeze value-protection all live on the ORCHESTRATOR path that writes
	// the filtered value to Redis (which this CAGG bypasses). Freeze is the
	// one of the three this reader's callers make good: on a pair with a
	// live freeze marker /v1/price and /v1/price/batch discard this bucket
	// for the value the freeze is holding (v1.Server.resolveFrozenServe)
	// — the other surfaces that read this bucket do not. A pair with no
	// prices_1m rows at all (pure-synthetic fiat like native/fiat:USD —
	// SDEX native trades are quoted in issuer-stablecoins, never fiat:USD)
	// misses here (ErrNoRows) and the handler's Redis-VWAP fallback — which
	// IS filtered — serves it. But any pair with real prices_1m rows serves
	// this raw bucket: that includes directly-quoted DEX/CEX pairs (a
	// Soroban token priced in USDC-GA5Z…, crypto:BTC/crypto:USDT) AND
	// headline pairs with a real fiat CEX market (crypto:XLM/fiat:USD via
	// Kraken/Coinbase). A single fat-finger / manipulation trade in the
	// served minute would otherwise corrupt the price with stale=false, no
	// outlier rejection, no volume floor. pricingguard.GuardServedVWAP1mConfidence
	// applies a robust sanity bound over the pair's recent trailing closed
	// buckets and serves last-known-good when the latest is grossly off
	// (adversarial-review HIGH). It is a pass-through (byte-identical) on a
	// healthy bucket — a liquid pair like crypto:XLM/fiat:USD sits tightly
	// clustered and always passes — so it only ever changes the served value
	// for a manipulated bucket.
	row, err := r.S.LatestClosedVWAP1mForPair(ctx, pair)
	if err == nil {
		// Thin-market substance gate ([pricing_guard]) — checked before
		// the trailing-baseline guard because the two protect against
		// DIFFERENT attacks: the baseline guard rejects one bad bucket
		// in a healthy market; the substance gate refuses a market
		// whose entire history (baseline included) is attacker-authored
		// (the valuation incident). ErrPriceWithheld deliberately
		// bypasses the handler's fallback chain — see its doc comment.
		if withheld := PriceWithheld(ctx, r.Substance, r.Scam, asset, quote, "price_read"); withheld != pricingguard.NotWithheld {
			return v1.PriceSnapshot{}, nil, false, v1.PriceWithheldError(withheld)
		}
		served, lowConfidence, substituted := pricingguard.GuardServedVWAP1mConfidence(ctx, r.S, r.Logger, pair, row)
		snap, stale := r.guardedSnapshot(asset, quote, served, lowConfidence, substituted)
		return snap, served.Sources, stale, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return v1.PriceSnapshot{}, nil, false, err
	}

	// Fast-path the synthetic-fiat case: no on-chain trades ever
	// exist for fiat: / crypto: quotes (those pairs are synthesised
	// by the aggregator's triangulation worker from the underlying
	// stablecoin pairs). Skipping LatestTradesForPair here saves a
	// full hypertable chunk-walk against an index condition that's
	// known to return zero rows; the handler's tryRedisVWAPFallback
	// picks up the synthesised value via Redis on the back of
	// ErrPriceNotFound.
	if quote.Type == canonical.AssetFiat || quote.Type == canonical.AssetCrypto {
		return v1.PriceSnapshot{}, nil, false, v1.ErrPriceNotFound
	}

	// Fallback: latest-trade. Hit when no closed 1m bucket exists for
	// the pair — typical for a brand-new listing that just got its
	// first trade in the in-progress bucket. Marks the response
	// stale=true; clients expecting freshness treat this as degraded.
	trades, err := r.S.LatestTradesForPair(ctx, pair, 1)
	if err != nil {
		return v1.PriceSnapshot{}, nil, false, err
	}
	if len(trades) == 0 {
		return v1.PriceSnapshot{}, nil, false, v1.ErrPriceNotFound
	}
	// Withholding gate, last-trade arm: a pair with no closed 1m bucket
	// inside the gate window by definition has no trailing substance, so
	// for an on-chain pair this withholds. That is the intended policy —
	// "the last trade was P" for a substanceless market is exactly the
	// manipulable claim the gate exists to stop (the raw trade stays
	// visible on /v1/observations). Off-chain pairs (never
	// substance-gated) keep the last-trade fallback.
	//
	// This arm consults BOTH gates via the chokepoint. Checking substance
	// only would let `disable_substance_gate=true` — an operator relaxing the
	// thin-market floor to diagnose a coverage complaint — silently also
	// publish a directory-flagged issuer's last trade as its price, reversing
	// a separate owner-level trust decision the operator never touched.
	if withheld := PriceWithheld(ctx, r.Substance, r.Scam, asset, quote, "price_read"); withheld != pricingguard.NotWithheld {
		return v1.PriceSnapshot{}, nil, false, v1.PriceWithheldError(withheld)
	}
	// decimals=7 matches Stellar's default stroop scale. A future
	// revision reads per-asset decimals from internal/metadata.
	snap, ok := v1.LastTradeToSnapshot(trades[0], 7)
	if !ok {
		return v1.PriceSnapshot{}, nil, false, v1.ErrPriceNotFound
	}
	return snap, []string{trades[0].Source}, true, nil
}

// RecentClosedSnapshots is the SEP-40 prices(asset, records)
// passthrough — most-recent N closed 1-minute VWAP buckets. Empty
// slice + nil error when the pair has no closed buckets yet (the
// "asset unknown" distinction is the API handler's job via the
// asset-existence check, not this reader's).
func (r StorePriceReader) RecentClosedSnapshots(ctx context.Context, asset, quote canonical.Asset, n int) ([]v1.PriceSnapshot, error) {
	pair, err := canonical.NewPair(asset, quote)
	if err != nil {
		return nil, err
	}
	// SampleFetch extra buckets so each of the n served buckets has its own
	// trailing baseline for the serving-sanity guard below.
	rows, err := r.S.RecentClosedVWAP1mForPair(ctx, pair, n+pricingguard.SampleFetch)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return []v1.PriceSnapshot{}, nil
	}
	// Thin-market substance gate: a snapshot SERIES is an aggregated
	// price claim per bucket, and the SEP-40 oracle surface is the last
	// place a substanceless market's rate belongs.
	if withheld := PriceWithheld(ctx, r.Substance, r.Scam, asset, quote, "oracle"); withheld != pricingguard.NotWithheld {
		return nil, v1.PriceWithheldError(withheld)
	}
	// Each bucket is the same bare CAGG ratio /v1/price guards; a
	// manipulated minute is dropped from the series rather than published
	// as an oracle record.
	rows = pricingguard.GuardServedVWAP1mSeries(r.Logger, pair, rows, n)
	out := make([]v1.PriceSnapshot, len(rows))
	for i, row := range rows {
		out[i] = v1.VWAP1mToSnapshot(asset.String(), quote.String(), row.VWAP, row.Bucket)
	}
	return out, nil
}

// RecentClosedVWAP1mExists implements the optional gate the /v1/price
// stablecoin-proxy fallback uses to skip empty proxy pairs before the
// unbounded last-trade walk (the empty-alias latency incident,
// proxy layer). Delegates to the bounded, both-directions probe on the
// store. Satisfies the unexported `proxyPairGate` interface in
// internal/api/v1.
func (r StorePriceReader) RecentClosedVWAP1mExists(ctx context.Context, base, quote canonical.Asset) (bool, error) {
	pair, err := canonical.NewPair(base, quote)
	if err != nil {
		return false, err
	}
	return r.S.RecentClosedVWAP1mExists(ctx, pair)
}
