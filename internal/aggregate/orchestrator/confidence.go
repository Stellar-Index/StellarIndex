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

// defaultDivergenceMinSources is the fallback applied when
// Config.DivergenceMinSources is unset (<= 0) — mirrors
// divergence.NewService's own fallback and the API's
// defaultDivergenceMinSources so an operator who never touches
// `divergence.min_sources_for_warning` gets the same quorum on all
// three consumers.
const defaultDivergenceMinSources = 2

// divergenceMinSources returns the floor on a cached divergence
// result's SuccessCount before its DivergencePct is trusted as a
// confidence input. Below this we pass the "no cross-oracle data"
// sentinel — safer to neutralise the factor than to score a single
// reference's hiccup as a multi-source signal.
//
// Sourced from Config.DivergenceMinSources (GH-1046) — previously a
// hardcoded const, independent of the operator's
// `divergence.min_sources_for_warning`. Raising that knob moved the
// worker's own WarningFired gate and the API's divergence_checked
// predicate, but not this one: a pair below the RAISED quorum still
// counted as corroborated here, so Phase 2's freeze could hold and
// release on a cross-oracle signal the API had stopped publishing.
func (o *Orchestrator) divergenceMinSources() int {
	if o.cfg.DivergenceMinSources > 0 {
		return o.cfg.DivergenceMinSources
	}
	return defaultDivergenceMinSources
}

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

// confidenceCacheTTL — the TTL is identical to VWAP (derived from this
// orchestrator's own cadence) so a stale confidence record can't
// outlive the price it scored.
func (o *Orchestrator) confidenceCacheTTL(window time.Duration) time.Duration {
	return o.vwapTTL(window)
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
	// ZWindow is the baseline window whose z is ZScore, so a freeze
	// record can say which scale fired.
	ZWindow time.Duration

	// CrossOracleMedian is the reference median price (0 when fewer
	// references than the trust floor answered). See
	// crossOracleSignal.median for why the freeze release path needs
	// the raw median rather than the cached divergencePct.
	CrossOracleMedian float64

	// TriangulationChecked / TriangulationDivergencePct carry the FRESH
	// composite comparison for this bucket's own VWAP (the same values
	// fed to confidence.Compute). The freeze release path reads these
	// rather than confidence.Factors, which folds the pct into a scored
	// agreement factor and does not retain the raw divergence.
	TriangulationChecked       bool
	TriangulationDivergencePct float64
}

// computeConfidence runs the multi-factor confidence math for the
// freshly-computed (pair, window) bucket, scoring the largest z among rets —
// the returns of the closed minutes this decision scores (see
// marginalBuckets). Returns (_, false) when the inputs aren't ready yet — no
// comparable prior bucket, no baseline, baseline in full bootstrap.
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
		BootstrapReleased:          o.bootstrapReleased[pair.String()],
	}, confidence.DefaultWeights())
	o.bootstrapReleased[pair.String()] = !score.Factors.BootstrapCapped
	exportBootstrapState(pair, score.Factors)

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

// crossOracleSignal is what [Orchestrator.lookupCrossOracle] extracts
// from the cached divergence result for the confidence step. The
// no-data state (unchecked) is both fields at their -1
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
	// median — the cross-reference median PRICE itself (0 below the
	// trust floor). Carried so the freeze release path can compare a
	// mid-freeze RELEASE CANDIDATE against the references directly:
	// during a freeze the refresh is pinned to the last-known-good, so
	// divergencePct and agreementCount are unchecked while the median
	// stays set — it is the one field that says anything about the
	// refused fresh print.
	median float64
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
// Reads the per-PAIR key (`div:<base>/<quote>`), not a per-base key:
// the confidence score for XLM/USDT must use XLM/USDT's own divergence,
// not one computed against a different quote.
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
//   - DEX + FX (soroswap + massive) → 2
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
// The quote amount's smallest-unit scale is a per-SOURCE property, not a
// constant. Off-chain CEX / aggregator quotes use 1e8 (8 decimals — see
// each poller's externalAmountDecimals), the FX pollers 1e6, on-chain
// legs 1e7. A fixed 1e7 divisor would overstate every 8dp CEX quote by
// 10×, and every pair this function values (fiat:USD + the abstract
// crypto:* stablecoin tickers) is that off-chain 8dp convention.
//
// That error is not benign: the LiquidityFactor is log-LINEAR across its
// band, so a 10× (one-decade) volume error shifts the factor by
// ln(10)/ln(ceiling/floor) for any true volume inside it (a third of the
// full [0,1] range on the [1e3, 1e6] band), materially distorting the
// per-pair confidence ranking. Resolve the scale the same way the
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

// baselineAgeDays returns how much real history backs the 30d
// baseline, in DAYS-EQUIVALENT of 1-minute buckets: the number of 1m
// buckets that fed the median/MAD divided by 1440 (1m buckets per
// 24h). Returns -1 (the [confidence.BaselineQualityFactor] sentinel)
// when the 30d window is in bootstrap.
//
// The bucket count is Day30.N+1, not Day30.N: N counts bucket-to-
// bucket RETURNS and N returns span N+1 buckets. Dividing N itself
// makes this measure structurally unable to reach 30: a window in which
// the pair traded in all 43,200 minutes yields 43,199 returns and would
// read 29.99931, under every threshold in [confidence], leaving the
// bootstrap cap engaged forever. A completely-observed window reads
// exactly 30.0, and no window can read higher.
//
// This is sample DENSITY, not calendar age, despite the name. It deliberately takes no wall-clock input:
// LatestBaseline's computedAt says when the refresher last WROTE the
// row, which is a property of the refresh loop's cadence rather than
// of the asset, and nothing available here carries the asset's
// first-observation time — so a true calendar maturity is not
// derivable at this layer. Density is also the better signal: a
// baseline is trustworthy in proportion to the observations behind
// it, so a calendar-mature but sparsely-traded pair SHOULD score low.
// See [confidence.Inputs.BaselineAgeDays] for the consumer-side
// framing.
//
// Density is kept over calendar age on purpose: (1) confidence in a price
// BASELINE is a function of how many observations back it, so a mature
// pair that barely trades has a genuinely thin baseline and scoring it
// conservatively is correct; (2) un-capping by calendar age raises
// confidence on thin baselines, the LESS-safe direction for a
// money-adjacent signal. The current behaviour errs LOW = safe. A calendar
// maturity signal must be ADDITIVE (never a replacement for density) and
// plumbed from storage's first-observation time.
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
