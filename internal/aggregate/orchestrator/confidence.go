package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"math/big"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/Stellar-Index/StellarIndex/internal/aggregate/baseline"
	"github.com/Stellar-Index/StellarIndex/internal/aggregate/confidence"
	"github.com/Stellar-Index/StellarIndex/internal/cachekeys"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/divergence"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/sources/external"
)

// divergenceMinSources is the floor on a cached divergence result's
// SuccessCount before its DivergencePct is trusted as a confidence
// input. Below this we pass the "no cross-oracle data" sentinel —
// safer to neutralise the factor than to score a single reference's
// hiccup as a multi-source signal.
//
// Matches the divergence Service's default minSources gate.
const divergenceMinSources = 2

// BaselineSource is the read-side interface the confidence step
// uses to look up a per-pair MultiBaseline. Production wiring
// adapts `*timescale.Store.LatestBaseline`. Nil = confidence step
// runs with z-factor in bootstrap (no baseline available).
type BaselineSource interface {
	// LatestBaseline returns the current per-pair baseline plus the
	// wall-clock timestamp it was computed at. Implementations
	// return ([baseline.MultiBaseline]{}, zero time, error) when
	// the pair has no baseline yet.
	LatestBaseline(ctx context.Context, pair canonical.Pair) (baseline.MultiBaseline, time.Time, error)
}

// confidenceCacheTTL — the TTL is identical to VWAP so a stale
// confidence record can't outlive the price it scored.
func confidenceCacheTTL(window time.Duration) time.Duration {
	return cachekeys.ConfidenceTTL(window)
}

// confidenceComputation bundles the score with the z-score that
// produced it. Returned by [Orchestrator.computeConfidence] so the
// Phase 2 freeze check can read both without recomputing.
//
// ZScore is deliberately the OBSERVATION-based score
// ([baseline.MultiBaseline.MaxZScore] of this bucket's return), not
// the wider score that fed [confidence.Compute]. The two differ when
// the sustained-drift signal is the larger of the pair — see
// [Orchestrator.computeConfidence] for why the freeze leg must stay
// observation-based.
type confidenceComputation struct {
	Score  confidence.Score
	ZScore float64
}

// computeConfidence runs the multi-factor confidence math for the
// freshly-computed (pair, window) bucket. Returns (_, false) when
// the inputs aren't ready yet — first tick, no baseline, baseline
// in full bootstrap.
//
// The split between compute and cache exists so the Phase 2 freeze
// check (ADR-0019) can read the score before deciding whether to
// publish — frozen buckets must NOT cache confidence either, or
// the next API read would surface a stale score from a refused
// bucket.
func (o *Orchestrator) computeConfidence(
	ctx context.Context,
	pair canonical.Pair,
	window time.Duration,
	vwap *big.Rat,
	prevVWAP *big.Rat,
	trades []canonicalTrade,
) (confidenceComputation, bool) {
	if o.cfg.Baselines == nil || prevVWAP == nil {
		obs.AggregatorConfidenceComputeTotal.WithLabelValues("skipped").Inc()
		return confidenceComputation{}, false
	}

	// computedAt (when the refresher last WROTE the row) is
	// deliberately unused: it measures the refresh loop's cadence, not
	// the asset's maturity — see [baselineAgeDays].
	multi, _, err := o.cfg.Baselines.LatestBaseline(ctx, pair)
	if err != nil {
		obs.AggregatorConfidenceComputeTotal.WithLabelValues("baseline_missing").Inc()
		return confidenceComputation{}, false
	}

	currF, _ := vwap.Float64()
	prevF, _ := prevVWAP.Float64()
	if prevF == 0 {
		obs.AggregatorConfidenceComputeTotal.WithLabelValues("skipped").Inc()
		return confidenceComputation{}, false
	}
	returnPct := (currF - prevF) / prevF

	observedZ, _, valid := multi.MaxZScore(returnPct)
	if !valid {
		obs.AggregatorConfidenceComputeTotal.WithLabelValues("baseline_missing").Inc()
		return confidenceComputation{}, false
	}

	// Frog-boiling (ADR-0019 §"Multi-window safeguard"): MaxZScore
	// only asks whether THIS bucket's return is unusual, so a slow
	// sustained push — every bucket individually unremarkable, the
	// window medians drifting along with the attack — scores ~0 at
	// every window length. MaxDriftZScore scores the sustained drift
	// itself, and the larger of the two becomes the confidence input.
	//
	// It feeds the SCORE ONLY — never the freeze leg below. That is a
	// hard structural constraint, not a tuning preference:
	// MaxDriftZScore takes no observation, so it is a pure function of
	// the hourly-refreshed baseline row. A publication decision gated
	// on it cannot self-clear when the current bucket is fine — it
	// stays engaged until the drift ages out of all three windows.
	// Measured on the real path (SplitByLookback -> NewMultiBaseline),
	// one genuine +50%-over-7-days repricing of a quiet asset holds
	// driftZ above 5 for 30 days, 24 of them AFTER the move finished:
	// a month of serving a pre-repricing last-known-good price.
	//
	// Nor can that be repaired by demanding the current bucket
	// corroborate before drift may freeze — corroboration means
	// observedZ is itself over the threshold, which is exactly the
	// pre-existing spike check. A window-level statistic can either
	// latch or add nothing to a per-bucket decision, so drift stays
	// out of the freeze path entirely and expresses itself as reduced
	// confidence, which is graded, self-correcting and non-destructive.
	scoringZ := observedZ
	if dz, _, ok := multi.MaxDriftZScore(); ok && dz > scoringZ {
		scoringZ = dz
	}

	xo := o.lookupCrossOracle(ctx, pair)
	triPct, triChecked := o.triangulationDivergencePct(pair, window, vwap)
	score := confidence.Compute(confidence.Inputs{
		ZScore: scoringZ,
		// SourceCount counts CONTRIBUTING SOURCES only. The composite
		// comparison below is corroboration, not a source, and must
		// never be folded in here — this is the input the freeze's
		// `source_count <= 1` leg reads, and a derived path that shares
		// our legs and our pipeline cannot satisfy an independence test
		// (see triangulate_corroborate.go's header).
		SourceCount:               distinctSourceCount(trades),
		SourceClassCount:          distinctSourceClassCount(trades),
		LiquidityUSD:              approxUSDVolume(trades, pair),
		CrossOracleDivergencePct:  xo.divergencePct,
		CrossOracleAgreementCount: xo.agreementCount,
		// Unchecked for every pair without a fresh chain output — which
		// is every pair until an operator configures
		// `[[aggregate.triangulations]]`, and by design leaves the score
		// bit-identical to its pre-corroboration value.
		TriangulationChecked:       triChecked,
		TriangulationDivergencePct: triPct,
		BaselineAgeDays:            baselineAgeDays(multi),
	}, confidence.DefaultWeights())

	// ZScore carries the OBSERVATION-based score to the Phase 2
	// freeze. Widening it to scoringZ would let drift alone freeze a
	// pair: the drift statistic takes no observation, so a freeze
	// gated on it cannot self-clear when the current bucket is fine —
	// it stays engaged until the drift ages out of all three windows
	// (see the paragraph above and [baseline.Baseline.DriftZScore]).
	//
	// Until COR-14 that widening was worse still, because the
	// confidence leg of the 3-signal AND was pinned true for a large
	// population by construction: approxUSDVolume returned 0 for every
	// non-USD-quoted pair, LiquidityFactor(0) is 0, and one zero factor
	// drives the geometric mean to 0 — so the `confidence < threshold` leg held
	// there regardless of every other input, and 8 of the 12 pairs in
	// defaultPairs() are non-USD-quoted. That leg is live again now
	// that an unvaluable pair passes the [confidence.LiquidityUnmeasured]
	// sentinel instead, but the latching argument above stands on its
	// own and drift stays out of the freeze path.
	return confidenceComputation{Score: score, ZScore: observedZ}, true
}

// cacheConfidence writes a previously-computed [confidence.Score]
// to Redis. Called only when the bucket is being published (i.e.
// neither Phase 1 nor Phase 2 has refused). Failures are logged +
// counted but don't propagate — confidence is enrichment.
func (o *Orchestrator) cacheConfidence(
	ctx context.Context,
	pair canonical.Pair,
	window time.Duration,
	score confidence.Score,
) {
	body, err := json.Marshal(score)
	if err != nil {
		obs.AggregatorConfidenceComputeTotal.WithLabelValues("marshal_error").Inc()
		return
	}
	key := cachekeys.Confidence(pair.Base, pair.Quote, window)
	if err := o.cache.Set(ctx, key.String(), body, confidenceCacheTTL(window)).Err(); err != nil {
		obs.AggregatorConfidenceComputeTotal.WithLabelValues("write_error").Inc()
		o.logger.Warn("confidence cache write failed",
			"pair", pair.String(), "window", window.String(), "err", err)
		return
	}
	obs.AggregatorConfidenceComputeTotal.WithLabelValues("ok").Inc()
}

// canonicalTrade is a local alias to avoid an import cycle while
// the orchestrator's Trade-handling helpers live next to the
// canonical package import. Same shape; same semantics.
type canonicalTrade = canonical.Trade

// crossOracleSignal is what [Orchestrator.lookupCrossOracle] extracts
// from the cached divergence result for the confidence step. The
// no-data state (unchecked per CS-087) is both fields at their -1
// sentinels — [confidence.Compute] then uses the neutral factor and
// serves CrossOracleChecked=false.
type crossOracleSignal struct {
	// divergencePct — % deviation from the cross-reference median,
	// or -1 ("no cross-oracle data", the [confidence.CrossOracleFactor]
	// sentinel).
	divergencePct float64
	// agreementCount — references corroborating our VWAP within the
	// divergence threshold (ADR-0019 Phase 3), or -1 when unchecked.
	agreementCount int
}

// noCrossOracle is the unchecked-state signal (both sentinels).
var noCrossOracle = crossOracleSignal{divergencePct: -1, agreementCount: -1}

// lookupCrossOracle reads the cached divergence result for the
// specific `pair` and returns its DivergencePct + AgreementCount
// when the SuccessCount meets the trust floor
// (`divergenceMinSources`). Otherwise (no key, decode error,
// transient cache failure, single-source success) returns
// [noCrossOracle] — the "no cross-oracle data" sentinels.
//
// F-1344 (G16-03): reads the per-PAIR key (`div:<base>/<quote>`),
// not a per-base key. The confidence score for XLM/USDT must use
// XLM/USDT's own divergence, not whatever pair last wrote the base
// key — the pre-fix per-base key meant the score consumed a
// divergence computed against a different quote.
//
// Best-effort: divergence is enrichment, not a publish-blocker.
// Read failures don't propagate; the confidence step continues with
// the neutral sentinels.
func (o *Orchestrator) lookupCrossOracle(ctx context.Context, pair canonical.Pair) crossOracleSignal {
	raw, err := o.cache.Get(ctx, cachekeys.Divergence(pair).String()).Bytes()
	if errors.Is(err, redis.Nil) {
		return noCrossOracle // no cache entry; treat as "no data"
	}
	if err != nil {
		// Transient Redis read failure — log debug-level (not warn)
		// because we don't want a Redis blip to flood logs every tick;
		// the metric label captures this cleanly enough.
		obs.AggregatorConfidenceComputeTotal.WithLabelValues("divergence_read_error").Inc()
		return noCrossOracle
	}
	var cached divergence.CachedResult
	if err := json.Unmarshal(raw, &cached); err != nil {
		obs.AggregatorConfidenceComputeTotal.WithLabelValues("divergence_decode_error").Inc()
		return noCrossOracle
	}
	if cached.SuccessCount < divergenceMinSources {
		// Single-reference signal: don't trust as a multi-source
		// divergence input. Pass "no data" sentinels.
		return noCrossOracle
	}
	return crossOracleSignal{
		divergencePct:  cached.DivergencePct,
		agreementCount: cached.AgreementCount,
	}
}

// distinctSourceClassCount returns the count of distinct
// (Class, Subclass) buckets represented in the trade slice. Used
// by the confidence diversity factor (ADR-0019): sources of the
// same economic kind agree less informatively than sources from
// different kinds.
//
// Bucket key is `Class:Subclass`. This means:
//   - two CEXes (binance + coinbase) → both `exchange:cex` → 1
//   - CEX + DEX (binance + soroswap) → `exchange:cex` + `exchange:dex` → 2
//   - CEX + Oracle (binance + reflector-dex) → 2
//   - DEX + FX (soroswap + polygon-forex) → 2
//
// Sources outside ClassExchange typically have empty Subclass —
// their parent Class already captures the economic distinction
// (oracles, aggregators, authority anchors don't sub-partition).
//
// Sources missing from the registry fall into the [external.Lookup]
// fallback (`exchange:`, no subclass) — an unknown source name
// doesn't get its own bucket.
func distinctSourceClassCount(trades []canonicalTrade) int {
	if len(trades) == 0 {
		return 0
	}
	seen := make(map[string]struct{}, 4)
	for i := range trades {
		md := external.Lookup(trades[i].Source)
		key := string(md.Class) + ":" + string(md.Subclass)
		seen[key] = struct{}{}
	}
	return len(seen)
}

// approxUSDVolume returns an approximation of bucket USD volume.
// Best when the pair quotes in fiat:USD or a USD-pegged stablecoin
// — sums each trade's QuoteAmount scaled by ITS SOURCE's decimals.
//
// For non-USD-quoted pairs it returns [confidence.LiquidityUnmeasured]
// — NOT 0 (COR-14). Zero is a measurement ("this bucket carried no
// dollars"), and a zero LiquidityFactor drives the geometric mean to
// zero, so returning it for every pair we simply cannot value in USD
// served a permanently-0 confidence for 8 of the 12 pairs in
// defaultPairs() and pinned the Phase 2 freeze's confidence leg true
// for them. The sentinel routes to the neutral factor instead, so the
// score reflects the factors we DID measure.
//
// Scale correctness (M13 / CS-040): the quote amount's smallest-unit
// scale is a per-SOURCE property, not a constant. Off-chain CEX /
// aggregator quotes use 1e8 (8 decimals — see each poller's
// externalAmountDecimals), the FX pollers 1e6, on-chain legs 1e7. A
// fixed 1e7 divisor (the prior implementation) overstated every 8dp
// CEX quote by 10× — and every pair this function values (fiat:USD +
// the abstract crypto:* stablecoin tickers) is that off-chain 8dp
// convention, so the whole valued set was systematically 10×-inflated.
//
// That is NOT the benign error the old comment claimed: the
// LiquidityFactor is log-LINEAR across its band, so a 10× (one-decade)
// volume error shifts the factor by ln(10)/ln(ceiling/floor) for any
// true volume inside it — a third of the full [0,1] range on today's
// [1e3, 1e6] band, and HALF of it on the [1e3, 1e5] band that shipped
// until 2026-07-25 (it only vanishes once the true volume already
// saturates at the ceiling). That materially distorts the per-pair
// confidence ranking. Resolve the scale the same way the
// contribution-sink USD valuation does —
// external.Metadata.AmountScaleDecimals.
//
// Refines once L2.2 (`usd_volume` column populated per trade) ships
// and the trade carries an authoritative USD figure.
func approxUSDVolume(trades []canonicalTrade, pair canonical.Pair) float64 {
	if !isUSDQuoted(pair) {
		return confidence.LiquidityUnmeasured
	}
	total := new(big.Rat)
	for i := range trades {
		amt := trades[i].QuoteAmount.BigInt()
		if amt == nil || amt.Sign() <= 0 {
			continue
		}
		dec := external.Lookup(trades[i].Source).AmountScaleDecimals()
		scale := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(dec)), nil)
		total.Add(total, new(big.Rat).SetFrac(amt, scale))
	}
	usd, _ := total.Float64()
	return usd
}

// isUSDQuoted reports whether the pair's quote is fiat:USD or a
// canonical USD-pegged stablecoin. Used to gate USD-magnitude
// approximations in [approxUSDVolume].
func isUSDQuoted(pair canonical.Pair) bool {
	switch pair.Quote.String() {
	case "fiat:USD",
		"crypto:USDT",
		"crypto:USDC",
		"crypto:DAI",
		"crypto:PYUSD",
		"crypto:USDP":
		return true
	}
	return false
}

// baselineAgeDays returns how much real history backs the 30d
// baseline, in DAYS-EQUIVALENT of 1-minute buckets: Day30.N (the
// count of bucket-to-bucket returns that fed the median/MAD) divided
// by 1440 (1m buckets per 24h). Returns -1 (the
// [confidence.BaselineQualityFactor] sentinel) when the 30d window is
// in bootstrap.
//
// This is sample DENSITY, not calendar age, and the name is
// historical (COR-14). It deliberately takes no wall-clock input:
// LatestBaseline's computedAt says when the refresher last WROTE the
// row, which is a property of the refresh loop's cadence rather than
// of the asset, and nothing available here carries the asset's
// first-observation time — so a true calendar maturity is not
// derivable at this layer. Density is also the better signal: a
// baseline is trustworthy in proportion to the observations behind
// it, so a calendar-mature but sparsely-traded pair SHOULD score low.
// See [confidence.Inputs.BaselineAgeDays] for the consumer-side
// framing.
func baselineAgeDays(multi baseline.MultiBaseline) float64 {
	if multi.Day30 == nil {
		return -1
	}
	// 1440 = minutes per day. Day30.N is the number of bucket-to-
	// bucket returns in the window (one per 1m bucket pair).
	return float64(multi.Day30.N) / 1440.0
}
