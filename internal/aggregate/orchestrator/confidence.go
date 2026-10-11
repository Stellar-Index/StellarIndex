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

// defaultDivergenceMinSources is the fallback for an unset Config.DivergenceMinSources; it mirrors divergence.NewService and the API so all three consumers share one quorum.
const defaultDivergenceMinSources = 2

// divergenceMinSources is the floor on a cached divergence result's SuccessCount before its DivergencePct is a confidence input; below it the "no cross-oracle data" sentinel is passed.
// Config-sourced, not a const, so it follows `divergence.min_sources_for_warning`; otherwise a pair under the raised quorum would still count as corroborated here and Phase 2 could hold or release on a signal the API stopped publishing.
func (o *Orchestrator) divergenceMinSources() int {
	if o.cfg.DivergenceMinSources > 0 {
		return o.cfg.DivergenceMinSources
	}
	return defaultDivergenceMinSources
}

// BaselineSource looks up a per-pair MultiBaseline for the confidence step; nil means the z-factor stays in bootstrap.
type BaselineSource interface {
	// LatestBaseline returns the current per-pair baseline plus the
	// wall-clock timestamp it was computed at. Implementations
	// return ([baseline.MultiBaseline]{}, zero time, error) when
	// the pair has no baseline yet.
	LatestBaseline(ctx context.Context, pair canonical.Pair) (baseline.MultiBaseline, time.Time, error)
}

// confidenceCacheTTL — the TTL is identical to VWAP (derived from this
// orchestrator's own cadence) so a stale confidence record can't
// outlive the price it scored.
func (o *Orchestrator) confidenceCacheTTL(window time.Duration) time.Duration {
	return o.vwapTTL(window)
}

// confidenceComputation bundles the score with the z-score that produced it, so the Phase 2 freeze check reads both without recomputing.
// ZScore is deliberately the OBSERVATION-based score (MaxZScore of this bucket's return), not the wider score fed to [confidence.Compute]; see [Orchestrator.computeConfidence].
type confidenceComputation struct {
	Score  confidence.Score
	ZScore float64
	// ZWindow is the baseline window whose z is ZScore, so a freeze
	// record can say which scale fired.
	ZWindow time.Duration

	// CrossOracleMedian is the reference median price (0 when fewer
	// references than the trust floor answered). See
	// crossOracleSignal.median for why the freeze release path needs
	// the raw median rather than the cached divergencePct.
	CrossOracleMedian float64

	// TriangulationChecked / TriangulationDivergencePct carry the FRESH composite comparison for this bucket's VWAP; the freeze release path reads these because confidence.Factors folds the pct into a score and drops the raw divergence.
	TriangulationChecked       bool
	TriangulationDivergencePct float64
}

// computeConfidence runs the confidence math for the fresh (pair, window) bucket, scoring the largest z among rets (see marginalBuckets). Returns (_, false) when inputs aren't ready: no comparable prior bucket, no baseline, or full bootstrap.
// Compute is split from caching so the Phase 2 freeze (ADR-0019) can read the score first; a frozen bucket must NOT cache confidence or the API would serve a stale score.
func (o *Orchestrator) computeConfidence(
	ctx context.Context,
	pair canonical.Pair,
	window time.Duration,
	vwap *big.Rat,
	rets []baseline.BucketReturn,
	trades []canonicalTrade,
	now time.Time,
) (confidenceComputation, bool) {
	if o.cfg.Baselines == nil || len(rets) == 0 {
		obs.AggregatorConfidenceComputeTotal.WithLabelValues("skipped").Inc()
		return confidenceComputation{}, false
	}

	// computedAt measures the refresh loop, not the asset's maturity (see
	// [baselineAgeDays]), so it gates freshness only.
	multi, computedAt, err := o.cfg.Baselines.LatestBaseline(ctx, pair)
	if err != nil {
		obs.AggregatorConfidenceComputeTotal.WithLabelValues("baseline_missing").Inc()
		return confidenceComputation{}, false
	}
	if !baselineFresh(pair, computedAt, now) {
		obs.AggregatorConfidenceComputeTotal.WithLabelValues("baseline_stale").Inc()
		return confidenceComputation{}, false
	}

	observedZ, zWindow, valid := maxBucketZ(multi, rets)
	if !valid {
		obs.AggregatorConfidenceComputeTotal.WithLabelValues("baseline_missing").Inc()
		return confidenceComputation{}, false
	}

	// Frog-boiling (ADR-0019 multi-window safeguard): MaxZScore misses a slow sustained push, so MaxDriftZScore scores the drift and the larger of the two is the confidence input.
	// It feeds the SCORE ONLY, never the freeze leg: drift takes no observation, so a freeze gated on it cannot self-clear when the current bucket is fine (one +50%-over-7-days repricing held driftZ above 5 for 30 days).
	// Requiring the current bucket to corroborate just restates the spike check, so drift is expressed only as graded, self-correcting reduced confidence.
	scoringZ := observedZ
	if dz, _, ok := multi.MaxDriftZScore(); ok && dz > scoringZ {
		scoringZ = dz
	}

	xo := o.lookupCrossOracle(ctx, pair)
	triPct, triChecked := o.triangulationDivergencePct(pair, window, vwap)
	score := confidence.Compute(confidence.Inputs{
		ZScore: scoringZ,
		// SourceCount counts CONTRIBUTING SOURCES only. The composite comparison is corroboration, never folded in: this feeds the freeze's `source_count <= 1` leg (see triangulate_corroborate.go).
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
		BootstrapReleased:          o.bootstrapReleased[pair.String()],
	}, confidence.DefaultWeights())
	o.bootstrapReleased[pair.String()] = !score.Factors.BootstrapCapped
	exportBootstrapState(pair, score.Factors)

	// ZScore carries the OBSERVATION-based score to the Phase 2 freeze; widening it to scoringZ would let drift alone freeze a pair, and the freeze would not self-clear until the drift ages out of all three windows.
	// Worse, the confidence leg is pinned true for non-USD-quoted pairs by construction (zero liquidity factor zeroes the geometric mean); the [confidence.LiquidityUnmeasured] sentinel fixes that leg, but drift stays out of the freeze regardless.
	return confidenceComputation{
		Score:                      score,
		ZScore:                     observedZ,
		ZWindow:                    zWindow,
		CrossOracleMedian:          xo.median,
		TriangulationChecked:       triChecked,
		TriangulationDivergencePct: triPct,
	}, true
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
	if err := o.cache.Set(ctx, key.String(), body, o.confidenceCacheTTL(window)).Err(); err != nil {
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

// crossOracleSignal is what [Orchestrator.lookupCrossOracle] extracts for the confidence step; no data means both fields at their -1 sentinels (neutral factor, CrossOracleChecked=false).
type crossOracleSignal struct {
	// divergencePct — % deviation from the cross-reference median,
	// or -1 ("no cross-oracle data", the [confidence.CrossOracleFactor]
	// sentinel).
	divergencePct float64
	// agreementCount — references corroborating our VWAP within the
	// divergence threshold (ADR-0019 Phase 3), or -1 when unchecked.
	agreementCount int
	// median is the cross-reference median PRICE (0 below the trust floor). During a freeze divergencePct and agreementCount are unchecked while median stays set, so it is the only field that judges a mid-freeze RELEASE CANDIDATE against the references.
	median float64
}

// noCrossOracle is the unchecked-state signal (both sentinels).
var noCrossOracle = crossOracleSignal{divergencePct: -1, agreementCount: -1}

// lookupCrossOracle reads the cached divergence result for this exact pair (`div:<base>/<quote>`, never a per-base key, so XLM/USDT uses its own divergence) and returns it when SuccessCount meets the trust floor.
// Otherwise (no key, decode error, cache failure, single-source) returns [noCrossOracle]. Best-effort: failures never propagate.
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
	if cached.SuccessCount < o.divergenceMinSources() {
		// Single-reference signal: don't trust as a multi-source
		// divergence input. Pass "no data" sentinels.
		return noCrossOracle
	}
	if cached.Pinned {
		// Measured against the frozen pair's pinned LKG, so it scores the
		// freeze, not this bucket; only the reference median is ours to use.
		pinned := noCrossOracle
		pinned.median = cached.Median
		return pinned
	}
	return crossOracleSignal{
		divergencePct:  cached.DivergencePct,
		agreementCount: cached.AgreementCount,
		median:         cached.Median,
	}
}

// distinctSourceClassCount returns the count of distinct (Class, Subclass) buckets in the trade slice, for the confidence diversity factor (ADR-0019): same-kind sources agree less informatively.
// The key is `Class:Subclass` (two CEXes = 1, CEX + DEX = 2); non-exchange classes have empty Subclass; unknown sources fall into the [external.Lookup] fallback (`exchange:`).
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

// approxUSDVolume approximates bucket USD volume for fiat:USD or USD-pegged quotes, summing QuoteAmount scaled by each trade's SOURCE decimals (CEX/aggregator 1e8, FX 1e6, on-chain 1e7; a fixed 1e7 would overstate 8dp quotes 10x). Resolve via external.Metadata.AmountScaleDecimals.
// Non-USD-quoted pairs return [confidence.LiquidityUnmeasured], NOT 0: zero is a measurement and zeroes the geometric mean; the sentinel routes to the neutral factor.
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
	usd, _ := total.Float64() // i128:ok USD volume as a confidence-score input, not served
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

// baselineAgeDays returns how much real history backs the 30d baseline in days-equivalent of 1m buckets (-1 = bootstrap sentinel for [confidence.BaselineQualityFactor]).
// Bucket count is Day30.N+1: N counts RETURNS, so dividing N could never reach 30 and the bootstrap cap would stay engaged forever.
// It is sample DENSITY, not calendar age (un-capping a sparse pair by age would raise confidence on a thin baseline); see [confidence.Inputs.BaselineAgeDays].
func baselineAgeDays(multi baseline.MultiBaseline) float64 {
	if multi.Day30 == nil {
		return -1
	}
	// 1440 = minutes per day. Day30.N is the number of bucket-to-
	// bucket returns in the window (one per 1m bucket pair), so the
	// buckets behind them number N+1.
	return float64(multi.Day30.N+1) / 1440.0
}

// maxBucketZ is the largest [baseline.MultiBaseline.MaxZScore] over rets, so
// the worst minute of a multi-minute decision is the one that is judged, with
// the baseline window that scored it.
func maxBucketZ(multi baseline.MultiBaseline, rets []baseline.BucketReturn) (float64, time.Duration, bool) {
	var best float64
	var bestWindow time.Duration
	for i, r := range rets {
		z, window, valid := multi.MaxZScore(r)
		if !valid {
			return 0, 0, false
		}
		if i == 0 || z > best {
			best, bestWindow = z, window
		}
	}
	return best, bestWindow, len(rets) > 0
}

// maxBaselineAge is the oldest baseline row still scored against. Past one
// day the stored 1d window no longer overlaps the last day at all, and the
// hourly refresher has missed 24 cycles for this pair.
const maxBaselineAge = baseline.Window1d

// baselineFresh exports the pair's baseline age and reports whether the row
// is young enough to score against. A zero computedAt is unknown freshness
// and fails closed; a row stamped ahead of now (clock skew) is fresh.
func baselineFresh(pair canonical.Pair, computedAt, now time.Time) bool {
	if computedAt.IsZero() {
		return false
	}
	age := now.Sub(computedAt)
	obs.AggregatorBaselineAgeSeconds.WithLabelValues(pair.String()).Set(age.Seconds())
	return age <= maxBaselineAge
}

// exportBootstrapState publishes the pair's bootstrap-cap state and the
// baseline density it gated on, so the cap's release is observable.
func exportBootstrapState(pair canonical.Pair, f confidence.Factors) {
	capped := 0.0
	if f.BootstrapCapped {
		capped = 1
	}
	obs.AggregatorBootstrapCapped.WithLabelValues(pair.String()).Set(capped)
	obs.AggregatorBaselineDensityDays.WithLabelValues(pair.String()).Set(f.BaselineAgeDays)
}

// baselineWindowLabel renders a [baseline.MultiBaseline] lookback as the
// 1d / 7d / 30d name the freeze record uses.
func baselineWindowLabel(w time.Duration) string {
	switch w {
	case baseline.Window1d:
		return "1d"
	case baseline.Window7d:
		return "7d"
	case baseline.Window30d:
		return "30d"
	default:
		return w.String()
	}
}
