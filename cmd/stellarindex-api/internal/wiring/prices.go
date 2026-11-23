package wiring

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/Stellar-Index/StellarIndex/internal/aggregate"
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
	// Exclusions: nil → no source-exclusion check.
	Exclusions *pricingguard.SourceExclusionGate
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
	if w := g.Exclusions.BucketWithholding(ctx, base, quote, served); w != pricingguard.NotWithheld {
		return "", time.Time{}, 0, nil, false, tierWithheld(w)
	}
	// row.Bucket is the bucket's start; the closed-bucket contract (ADR-0015) serves its end.
	asOf := served.Bucket.Add(time.Minute)
	return served.VWAP, asOf, served.TradeCount, served.Sources, true, nil
}

// LatestAggregatorPrices is [GlobalPriceReader.LatestAggregatorPricesTrimmed] judged without a freshness filter; the headline
// tier calls the trimmed form so it judges fresh rows only.
func (g GlobalPriceReader) LatestAggregatorPrices(ctx context.Context, base, quote canonical.Asset, sources []string) ([]canonical.OracleUpdate, error) {
	kept, excluded, err := g.LatestAggregatorPricesTrimmed(ctx, base, quote, sources)
	if err != nil {
		return nil, err
	}
	if len(excluded) > 0 && !aggregate.ExcludedSurvivorsAgree(append(append([]canonical.OracleUpdate(nil), kept...), excluded...), kept) {
		return nil, tierWithheld(pricingguard.WithheldSourceExcluded)
	}
	return kept, nil
}

var _ aggregate.ExclusionTrimmingReader = GlobalPriceReader{}

// LatestAggregatorPricesTrimmed splits the latest aggregator rows into survivors and operator-excluded rows; the aggregator tier
// checks the survivors against the full set after its freshness filter ([aggregate.ExclusionTrimmingReader]).
func (g GlobalPriceReader) LatestAggregatorPricesTrimmed(
	ctx context.Context, base, quote canonical.Asset, sources []string,
) (kept, excluded []canonical.OracleUpdate, err error) {
	// Scam gate: without it a flagged issuer's headline, withheld by tier 1, resurfaces here.
	if PriceWithheld(ctx, nil, g.Scam, base, quote, "asset_headline") != pricingguard.NotWithheld {
		return nil, nil, nil
	}
	rows, err := g.S.LatestAggregatorPricesForPair(ctx, base, quote, sources)
	if err != nil || g.Exclusions == nil {
		return rows, nil, err
	}
	// Each row is one source's own observation, so the tier is recomputed from the survivors.
	kept = rows[:0:0]
	for _, u := range rows {
		switch w := g.Exclusions.InstantWithholding(ctx, base, quote, u.Source, u.Timestamp); w {
		case pricingguard.NotWithheld:
			kept = append(kept, u)
		case pricingguard.WithheldSourceExcluded:
			excluded = append(excluded, u)
		default:
			return nil, nil, tierWithheld(w)
		}
	}
	if len(kept) == 0 && len(rows) > 0 {
		return nil, excluded, tierWithheld(pricingguard.WithheldSourceExcluded)
	}
	return kept, excluded, nil
}

// tierWithheld is the headline tiers' refusal under an exclusion: distinct from "no data", so no ungated fallback fills it.
func tierWithheld(w pricingguard.Withholding) error {
	return &aggregate.TierWithheldError{Unread: w == pricingguard.WithheldExclusionsUnread}
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
	// A triangulated value records neither sources nor legs; the last tier fails closed like the cache layer of /v1/price.
	if w := g.Exclusions.MultiHopWithholding(ctx, v.ObservedAt.Add(-window), time.Now().Add(time.Nanosecond)); w != pricingguard.NotWithheld {
		return "", time.Time{}, false, tierWithheld(w)
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
	if q.shared {
		adm = nil
	}
	gate := pricingguard.Gate{Substance: substance, Scam: scam}
	v := gate.Judge(ctx, base, quote, surface, pricingguard.Query{
		PointInTime: q.pointInTime,
		At:          q.at,
		AdmitThin:   adm.Requested() && adm.Covers(base, quote),
	})
	adm.Record(base, quote, v)
	if q.shared && v.Unmeasured && v.Withholding == pricingguard.NotWithheld {
		// One caller's read may fail open; an answer kept for every caller may not.
		return pricingguard.WithheldThinMarket
	}
	return v.Withholding
}

// withholdingQuery is what a seam may tell the chokepoint about its read. pointInTime is an
// explicit flag, not "at is non-zero", so the zero time a failed parse yields can never quietly
// select the live measurement.
type withholdingQuery struct {
	pointInTime bool
	at          time.Time
	shared      bool
}

type WithholdingOption func(*withholdingQuery)

// AsOfInstant marks the gated read as point-in-time: the thin-market half is measured over the window ending at ts.
func AsOfInstant(ts time.Time) WithholdingOption {
	return func(q *withholdingQuery) {
		q.pointInTime = true
		q.at = ts
	}
}

// ForSharedRead marks a read whose answer is kept for every caller: the calling request's `include_thin` opt-in
// does not admit it, its verdict is not recorded as that request's evidence, and a market the gate could not
// measure is withheld.
func ForSharedRead() WithholdingOption {
	return func(q *withholdingQuery) { q.shared = true }
}

// PriceStore is the storage seam [StorePriceReader] reads; *timescale.Store satisfies it.
type PriceStore interface {
	pricingguard.TrailingReader
	LatestClosedVWAP1mForPair(ctx context.Context, p canonical.Pair) (timescale.Vwap1mRow, error)
	RecentClosedVWAP1mForPair(ctx context.Context, p canonical.Pair, limit int) ([]timescale.Vwap1mRow, error)
	RecentClosedVWAP1mExists(ctx context.Context, p canonical.Pair) (bool, error)
	LatestTradesForPair(ctx context.Context, p canonical.Pair, limit int, since time.Time) ([]canonical.Trade, error)
	FreezeWindowsForPair(ctx context.Context, assetIDs []string, quoteID string, since time.Time) ([]timescale.FreezeWindow, error)
}

type StorePriceReader struct {
	S             PriceStore
	VWAPFreshness time.Duration               // 0 → defaultVWAPFreshness
	Now           func() time.Time            // nil → time.Now
	Logger        *slog.Logger                // nil → no guard logging
	Substance     *pricingguard.SubstanceGate // nil → no thin-market gate
	Scam          *pricingguard.ScamGate      // nil → no scam-issuer gate
	// Exclusions: nil → no source-exclusion check.
	Exclusions *pricingguard.SourceExclusionGate
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
		if w := r.Exclusions.BucketWithholding(ctx, asset, quote, served); w != pricingguard.NotWithheld {
			return v1.PriceSnapshot{}, nil, false, v1.PriceWithheldError(w)
		}
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
	if w := r.Exclusions.InstantWithholding(ctx, asset, quote, trades[0].Source, trades[0].Timestamp); w != pricingguard.NotWithheld {
		return v1.PriceSnapshot{}, nil, false, v1.PriceWithheldError(w)
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
	// An excluded source's minutes are dropped before the guard so they neither serve nor set the baseline.
	clean, w := r.Exclusions.CleanBuckets(ctx, asset, quote, rows)
	if w != pricingguard.NotWithheld {
		return nil, v1.PriceWithheldError(w)
	}
	// Each bucket is the same bare CAGG ratio /v1/price guards; a manipulated minute is dropped.
	served := pricingguard.GuardServedVWAP1mSeries(r.Logger, pair, clean, n)
	holed, err := seriesHoledByExclusion(rows, clean, served)
	if err != nil {
		return nil, err
	}
	out := make([]v1.PriceSnapshot, len(served))
	for i, row := range served {
		out[i] = v1.VWAP1mToSnapshot(asset.String(), quote.String(), row.VWAP, row.Bucket)
		out[i].FillsWithheld = holed
		out[i].BucketSources = row.Sources
	}
	return out, nil
}

// seriesHoledByExclusion reports whether a bucket the exclusion dropped (in read, not in clean) is no older than the oldest
// served one, so the served series has a hole in it; dropped buckets that leave nothing to serve are a refusal, not "no records".
func seriesHoledByExclusion(read, clean, served []timescale.Vwap1mRow) (bool, error) {
	if len(clean) == len(read) {
		return false, nil
	}
	if len(served) == 0 {
		// Only an exclusion that emptied the series alone is `source_excluded`; the guard dropping the rest is "no records".
		if len(clean) == 0 {
			return false, v1.PriceWithheldError(pricingguard.WithheldSourceExcluded)
		}
		return false, nil
	}
	kept := make(map[time.Time]bool, len(clean))
	oldest := served[0].Bucket
	for _, row := range clean {
		kept[row.Bucket] = true
	}
	for _, row := range served {
		if row.Bucket.Before(oldest) {
			oldest = row.Bucket
		}
	}
	for _, row := range read {
		if !kept[row.Bucket] && !row.Bucket.Before(oldest) {
			return true, nil
		}
	}
	return false, nil
}

// FreezeWindows is the optional [v1.FreezeWindowReader]: the recorded freezes of any of assets against quote still open at since.
func (r StorePriceReader) FreezeWindows(ctx context.Context, assets []canonical.Asset, quote canonical.Asset, since time.Time) ([]timescale.FreezeWindow, error) {
	ids := make([]string, len(assets))
	for i, a := range assets {
		ids[i] = a.String()
	}
	return r.S.FreezeWindowsForPair(ctx, ids, quote.String(), since)
}

// RecentClosedVWAP1mExists is the optional gate the /v1/price stablecoin-proxy fallback uses to skip empty proxy pairs before the unbounded last-trade walk.
func (r StorePriceReader) RecentClosedVWAP1mExists(ctx context.Context, base, quote canonical.Asset) (bool, error) {
	pair, err := canonical.NewPair(base, quote)
	if err != nil {
		return false, err
	}
	return r.S.RecentClosedVWAP1mExists(ctx, pair)
}
