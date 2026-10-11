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
// v1.TriangulatedPriceLooker. It reads the cachekeys VWAP value plus its provenance, observed-at
// and coverage keys.
//
// "Triangulated" means the aggregator's triangulation worker wrote the value. A missing marker
// yields isTriangulated=false but the value is still served, because aggregator-rewritten pairs
// (XLM/fiat:USD) have no prices_1m row to fall back on.
//
// A cache miss, or a value with no readable observed-at stamp, returns (found=false, no error):
// its age is unknowable and a freeze keeps a value alive for its whole hold. Read errors
// propagate so the handler can fall through to its other fallbacks.
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
// aggregator writes alongside a triangulated composite (cachekeys.VWAPCompositeMeta), so
// /v1/price can surface flags.diverged / flags.rerouted. A miss returns (nil, false, nil).
// Best-effort: the handler leaves the flags unset on any error, so meta never fails the request.
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

// GlobalPriceReader adapts *timescale.Store + the Redis triangulated looker to
// aggregate.GlobalPriceReader. Each method is one ComputeGlobalPrice tier: LatestVWAP (tier 1, the
// raw ratio, decimals-corrected by v1's decimalsCorrectedGlobalReader), LatestAggregatorPrices
// (tier 2) and LookupTriangulated (tier 3, via RedisTriangulatedLooker).
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
	// Thin-market substance gate ([pricing_guard]): a pair whose whole market is attacker-authorable
	// must not publish a headline price. Withheld reads degrade to "no data" and the caller falls
	// through to its aggregator tier, which applies its own min-USD-volume floor.
	if PriceWithheld(ctx, g.Substance, g.Scam, base, quote, "asset_headline") != pricingguard.NotWithheld {
		return "", time.Time{}, 0, nil, false, nil
	}
	// Same serving-sanity guard as StorePriceReader.LatestPrice: the bare Σ(quote)/Σ(base) bucket
	// bypasses the orchestrator's outlier filter, volume gate and freeze protection. The headline
	// has no stale flag, so an unvalidated (no baseline) or held bucket reads as "no data" and the
	// caller falls through, where the change shows in price_authority instead of a silently
	// frozen vwap_native.
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
	// Scam-issuer gate, through the chokepoint. A Stellar-only token's literal <asset>/fiat:USD pair
	// has no prices_1m rows, so tier 1 misses by construction and this tier serves its headline;
	// without the gate a flagged issuer would be served. Withheld degrades to "no data".
	//
	// The substance gate is passed nil DELIBERATELY: its floor is measured over the pair's alias
	// union, and a triangulated pair has zero rows in its literal form, so it would withhold every
	// Stellar-only token for lacking a literal market. Substance is asked in tier 1 and in the
	// per-asset listing gate, the same split /v1/twap, /v1/vwap and /v1/price make.
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
// It reads the closed 1m VWAP bucket first and falls back to the last trade in the trades hypertable.
// defaultVWAPFreshness: a closed bucket older than this is served stale=true. It sits well above
// the structural 1-2min closed-bucket floor, so active pairs stay fresh and dormant ones flag.
const defaultVWAPFreshness = 15 * time.Minute

// PriceWithheld is THE withholding chokepoint for every price-serving read seam. One function keeps
// the scam and substance gates symmetric: hand-written call sites let
// disable_substance_gate=true also un-withhold flagged issuers. TestPriceServingSeamsAreGated
// derives the seam set from calls to it, and TestWithholdingGatesAreSpelledOnlyAtTheChokepoint
// fails if a call site spells a gate out itself. The logic lives in pricingguard.Gate.
//
// Both gates are nil-receiver safe (nil == allow-everything). It returns WHICH gate fired so
// v1.PriceWithheldError can name the cause. [AsOfInstant] measures the thin-market half over the
// window ending at that instant; it is an option, not a second chokepoint.
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

// bucketIsStale is the staleness rule: a closed 1m bucket is stale once its CLOSE (bucket start
// + 1 minute) is older than the freshness window, or whenever the read was low-confidence.
//
// It is separate from LatestPrice, which needs a live *timescale.Store, so a unit test exercises
// the real rule. It measures from the CLOSE: from the start, every bucket would read a minute
// older than it is.
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
	// Stale is judged on the bucket actually served (the candidate, or the older last-known-good
	// after a guard rejection), so a dormant pair's old VWAP is never stale=false.
	//
	// A pair's first served minute has no trailing baseline, so the guard fails OPEN and would
	// accept a lone manipulated print; lowConfidence serves that as stale, never as confident.
	// A substituted bucket is a held value standing in for the current minute, so it is stale
	// however recent it is.
	stale := r.bucketIsStale(served.Bucket, lowConfidence || substituted)
	snap := v1.VWAP1mToSnapshot(asset.String(), quote.String(), served.VWAP, served.Bucket)
	// Substituted means the guard swapped in an older last-known-good bucket for `row`. The handler's
	// confidence/composite-flags come from a separate cache keyed by (pair, window) with no as-of,
	// so they describe the current tick, not this older bucket; snap.Substituted tells the handler
	// to withhold them rather than attribute a live read to the substituted value.
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

	// Primary path: most-recent CLOSED 1-minute VWAP from the prices_1m CAGG (ADR-0015). It is a
	// bare Σ(quote)/Σ(base), NOT the orchestrator's filtered VWAP: outlier filter, volume gate and
	// freeze protection live on the orchestrator path, and callers only restore freeze. A pair
	// with no prices_1m rows (pure-synthetic fiat like native/fiat:USD) misses here (ErrNoRows)
	// and the handler's Redis-VWAP fallback serves it. GuardServedVWAP1mConfidence therefore
	// bounds the latest bucket against the trailing closed buckets and serves last-known-good
	// when it is grossly off.
	row, err := r.S.LatestClosedVWAP1mForPair(ctx, pair)
	if err == nil {
		// Thin-market substance gate ([pricing_guard]), checked before the trailing-baseline guard
		// because they stop different attacks: the baseline guard rejects one bad bucket in a healthy
		// market; the substance gate refuses a market whose whole history, baseline included, is
		// attacker-authored. ErrPriceWithheld deliberately bypasses the handler's fallback chain.
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

	// Fast-path the synthetic-fiat case: no on-chain trades exist for fiat: / crypto: quotes (the
	// aggregator's triangulation worker synthesises them). Skipping LatestTradesForPair avoids a
	// full hypertable chunk-walk that is known to return zero rows; the handler's
	// tryRedisVWAPFallback picks up the synthesised value via Redis on ErrPriceNotFound.
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
	// Last-trade arm: a pair with no closed 1m bucket inside the gate window has no trailing
	// substance, so an on-chain pair is withheld; "the last trade was P" for a substanceless
	// market is the manipulable claim (the raw trade stays on /v1/observations). Off-chain pairs,
	// never substance-gated, keep the fallback.
	//
	// This arm consults BOTH gates via the chokepoint: substance alone would let
	// disable_substance_gate=true publish a flagged issuer's last trade as its price.
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

// RecentClosedSnapshots is the SEP-40 prices(asset, records) passthrough: the most-recent N closed
// 1-minute VWAP buckets. A pair with none yet returns an empty slice and nil error; "asset unknown"
// is the handler's asset-existence check, not this reader's.
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

// RecentClosedVWAP1mExists is the optional gate the /v1/price stablecoin-proxy fallback uses to
// skip empty proxy pairs before the unbounded last-trade walk. It delegates to the store's
// bounded, both-directions probe and satisfies the unexported `proxyPairGate` in internal/api/v1.
func (r StorePriceReader) RecentClosedVWAP1mExists(ctx context.Context, base, quote canonical.Asset) (bool, error) {
	pair, err := canonical.NewPair(base, quote)
	if err != nil {
		return false, err
	}
	return r.S.RecentClosedVWAP1mExists(ctx, pair)
}
