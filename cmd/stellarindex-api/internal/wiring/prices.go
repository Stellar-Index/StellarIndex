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

// RedisTriangulatedLooker adapts the shared Redis client to v1.TriangulatedPriceLooker, reading the cachekeys VWAP value plus its provenance, observed-at and coverage keys.
type RedisTriangulatedLooker struct{ RDB redis.UniversalClient }

func (r RedisTriangulatedLooker) LookupTriangulatedVWAP(
	ctx context.Context, base, quote canonical.Asset, window time.Duration,
) (v1.CachedVWAP, bool, error) {
	if r.RDB == nil {
		return v1.CachedVWAP{}, false, nil
	}
	// One MGET: the aggregator writes all four keys in one MULTI/EXEC, and separate GETs could
	// straddle a write and pair a value with another write's provenance or stamp.
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

// LookupCompositeMeta reads the router quality-flags blob written alongside a triangulated composite; best-effort, any error leaves the flags unset rather than failing the request.
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

// GlobalPriceStore is the storage seam [GlobalPriceReader] reads; *timescale.Store satisfies it.
type GlobalPriceStore interface {
	pricingguard.TrailingReader
	LatestClosedVWAP1mForPair(ctx context.Context, p canonical.Pair) (timescale.Vwap1mRow, error)
	LatestAggregatorPricesForPair(ctx context.Context, base, quote canonical.Asset, sources []string) ([]canonical.OracleUpdate, error)
}

// GlobalPriceReader adapts *timescale.Store + the Redis triangulated looker to aggregate.GlobalPriceReader.
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
		// An invalid pair is "no data" from this seam; the caller falls through to the aggregator tier.
		return "", time.Time{}, 0, nil, false, nil //nolint:nilerr // intentional: invalid pair → "no data" from this tier
	}
	row, err := g.S.LatestClosedVWAP1mForPair(ctx, pair)
	if errors.Is(err, sql.ErrNoRows) {
		return "", time.Time{}, 0, nil, false, nil
	}
	if err != nil {
		return "", time.Time{}, 0, nil, false, err
	}
	// A pair whose whole market is attacker-authorable must not publish a headline price. Withheld
	// reads degrade to "no data"; the aggregator tier applies its own min-USD-volume floor.
	if PriceWithheld(ctx, g.Substance, g.Scam, base, quote, "asset_headline") != pricingguard.NotWithheld {
		return "", time.Time{}, 0, nil, false, nil
	}
	// Same serving-sanity guard as StorePriceReader.LatestPrice: the bare bucket bypasses the
	// orchestrator's outlier filter, volume gate and freeze protection. The headline has no stale
	// flag, so an unvalidated or held bucket reads as "no data" and the change shows in
	// price_authority instead of a silently frozen vwap_native.
	served, lowConfidence, substituted := pricingguard.GuardServedVWAP1mConfidence(ctx, g.S, g.Logger, pair, row)
	if lowConfidence || substituted {
		return "", time.Time{}, 0, nil, false, nil
	}
	// row.Bucket is the bucket's start; the closed-bucket contract (ADR-0015) serves its end.
	asOf := served.Bucket.Add(time.Minute)
	return served.VWAP, asOf, served.TradeCount, served.Sources, true, nil
}

func (g GlobalPriceReader) LatestAggregatorPrices(ctx context.Context, base, quote canonical.Asset, sources []string) ([]canonical.OracleUpdate, error) {
	// Scam gate: without it a flagged issuer's headline, withheld by tier 1, resurfaces here.
	if PriceWithheld(ctx, nil, g.Scam, base, quote, "asset_headline") != pricingguard.NotWithheld {
		return nil, nil
	}
	return g.S.LatestAggregatorPricesForPair(ctx, base, quote, sources)
}

func (g GlobalPriceReader) LookupTriangulated(ctx context.Context, base, quote canonical.Asset, window time.Duration) (string, time.Time, bool, error) {
	// A Stellar-only token's literal <asset>/fiat:USD pair has no prices_1m rows, so tier 1 misses
	// by construction and this tier serves its headline; without the scam gate a flagged issuer
	// would be served.
	//
	// The substance gate is passed nil DELIBERATELY: its floor is measured over the pair's alias
	// union, and a triangulated pair has zero rows in its literal form, so it would withhold every
	// Stellar-only token. Substance is asked in tier 1 and the per-asset listing gate instead.
	if PriceWithheld(ctx, nil, g.Scam, base, quote, "asset_headline") != pricingguard.NotWithheld {
		return "", time.Time{}, false, nil
	}
	v, found, err := g.Tri.LookupTriangulatedVWAP(ctx, base, quote, window)
	if err != nil || !found || !v.Triangulated {
		// found && !Triangulated is a direct value, which must not be served as this tier.
		return "", time.Time{}, false, err
	}
	return v.Value, v.ObservedAt, true, nil
}

// StorePriceReader adapts *timescale.Store to v1.PriceReader: the closed 1m VWAP bucket first, then the last trade in the trades
// hypertable. defaultVWAPFreshness: an older closed bucket is served stale=true; it sits well above the structural 1-2min floor.
const defaultVWAPFreshness = 15 * time.Minute

// PriceWithheld is THE withholding chokepoint for every price-serving read seam, keeping the scam and substance gates symmetric; both are nil-receiver safe (nil allows everything).
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

// withholdingQuery is what a seam may tell the chokepoint about its read. pointInTime is an
// explicit flag, not "at is non-zero", so the zero time a failed parse yields can never quietly
// select the live measurement.
type withholdingQuery struct {
	pointInTime bool
	at          time.Time
}

type WithholdingOption func(*withholdingQuery)

// AsOfInstant marks the gated read as point-in-time: the thin-market half is measured over the window ending at ts.
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

// bucketIsStale: a closed 1m bucket is stale once its CLOSE (start + 1 minute, else every bucket
// would read a minute older than it is) passes the freshness window, or when low-confidence. It is
// separate from LatestPrice so a unit test exercises the real rule without a store.
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
	// Stale is judged on the bucket actually served, so a dormant pair's old VWAP is never
	// stale=false. A pair's first served minute has no trailing baseline, so the guard fails OPEN
	// and would accept a lone manipulated print; lowConfidence serves that as stale. A substituted
	// bucket is a held value standing in for the current minute, so it is stale however recent.
	stale := r.bucketIsStale(served.Bucket, lowConfidence || substituted)
	snap := v1.VWAP1mToSnapshot(asset.String(), quote.String(), served.VWAP, served.Bucket)
	// The handler's confidence/composite-flags come from a cache keyed by (pair, window) with no
	// as-of, so they describe the current tick, not a substituted older bucket; snap.Substituted
	// tells the handler to withhold them.
	snap.Substituted = substituted
	return snap, stale
}

// lastTradeSince is the last-trade walk's lower bound, floored to a UTC midnight so the statement text
// holds for a day: Postgres then reuses a cached generic plan (~5ms) instead of planning ~100 trades chunks per miss (~50ms per alias).
func (r StorePriceReader) lastTradeSince() time.Time {
	return r.clock().Add(-timescale.LatestVWAPWindow).UTC().Truncate(24 * time.Hour)
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

	// Primary path: latest CLOSED 1m VWAP from the prices_1m CAGG (ADR-0015), a bare ratio without
	// the orchestrator's outlier filter, volume gate and freeze protection, so
	// GuardServedVWAP1mConfidence bounds it against trailing buckets and serves last-known-good
	// when grossly off. A pair with no prices_1m rows (native/fiat:USD) misses with ErrNoRows and
	// the handler's Redis-VWAP fallback serves it.
	row, err := r.S.LatestClosedVWAP1mForPair(ctx, pair)
	if err == nil {
		// The substance gate runs before the baseline guard because they stop different attacks: the
		// baseline guard rejects one bad bucket in a healthy market; the substance gate refuses a
		// market whose whole history is attacker-authored. ErrPriceWithheld bypasses the fallbacks.
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

	// No on-chain trades exist for fiat:/crypto: quotes (the triangulation worker synthesises
	// them); skipping LatestTradesForPair avoids a full chunk-walk that returns zero rows. The
	// handler's tryRedisVWAPFallback serves the synthesised value on ErrPriceNotFound.
	if quote.Type == canonical.AssetFiat || quote.Type == canonical.AssetCrypto {
		return v1.PriceSnapshot{}, nil, false, v1.ErrPriceNotFound
	}

	// Fallback: latest trade, for a pair with no closed 1m bucket yet (a brand-new listing). Always
	// served stale=true. Bounded by the primary path's 400-day lookback: with the substance gate on
	// (24h window by default) a pair idle that long is withheld anyway, and the unbounded walk cost
	// a miss up to 1.6s.
	trades, err := r.S.LatestTradesForPair(ctx, pair, 1, r.lastTradeSince())
	if err != nil {
		return v1.PriceSnapshot{}, nil, false, err
	}
	if len(trades) == 0 {
		return v1.PriceSnapshot{}, nil, false, v1.ErrPriceNotFound
	}
	// A pair with no closed 1m bucket inside the gate window has no trailing substance, so an
	// on-chain pair is withheld: "the last trade was P" for a substanceless market is the
	// manipulable claim (the raw trade stays on /v1/observations). Off-chain pairs keep the
	// fallback. BOTH gates run via the chokepoint, or disable_substance_gate=true would publish a
	// flagged issuer's last trade.
	if withheld := PriceWithheld(ctx, r.Substance, r.Scam, asset, quote, "price_read"); withheld != pricingguard.NotWithheld {
		return v1.PriceSnapshot{}, nil, false, v1.PriceWithheldError(withheld)
	}
	// decimals=7 is Stellar's default stroop scale.
	snap, ok := v1.LastTradeToSnapshot(trades[0], 7)
	if !ok {
		return v1.PriceSnapshot{}, nil, false, v1.ErrPriceNotFound
	}
	return snap, []string{trades[0].Source}, true, nil
}

// RecentClosedSnapshots is the SEP-40 prices(asset, records) passthrough: the most-recent N closed 1-minute VWAP buckets.
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
	// A snapshot SERIES is an aggregated price claim per bucket; the SEP-40 oracle surface is the
	// last place a substanceless market's rate belongs.
	if withheld := PriceWithheld(ctx, r.Substance, r.Scam, asset, quote, "oracle"); withheld != pricingguard.NotWithheld {
		return nil, v1.PriceWithheldError(withheld)
	}
	// Each bucket is the same bare CAGG ratio /v1/price guards; a manipulated minute is dropped.
	rows = pricingguard.GuardServedVWAP1mSeries(r.Logger, pair, rows, n)
	out := make([]v1.PriceSnapshot, len(rows))
	for i, row := range rows {
		out[i] = v1.VWAP1mToSnapshot(asset.String(), quote.String(), row.VWAP, row.Bucket)
	}
	return out, nil
}

// RecentClosedVWAP1mExists is the optional gate the /v1/price stablecoin-proxy fallback uses to skip empty proxy pairs before the unbounded last-trade walk.
func (r StorePriceReader) RecentClosedVWAP1mExists(ctx context.Context, base, quote canonical.Asset) (bool, error) {
	pair, err := canonical.NewPair(base, quote)
	if err != nil {
		return false, err
	}
	return r.S.RecentClosedVWAP1mExists(ctx, pair)
}
