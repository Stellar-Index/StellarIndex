package orchestrator

import (
	"context"
	"fmt"
	"math"
	"math/big"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/Stellar-Index/StellarIndex/internal/aggregate/anomaly"
	"github.com/Stellar-Index/StellarIndex/internal/aggregate/confidence"
	"github.com/Stellar-Index/StellarIndex/internal/aggregate/freeze"
	"github.com/Stellar-Index/StellarIndex/internal/cachekeys"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
)

// Phase 2 freeze thresholds per ADR-0019: all three signals must agree, so legitimate multi-source events (high z) don't freeze but the single-source, large-deviation, low-confidence attack shape does.
// Defaults apply when [Config.Phase2Thresholds] is zero; operators tune via `[anomaly.phase2]`.
const (
	// 0.45, NOT ADR-0019's original 0.10: confidence is a geometric mean that decays gently in z (single-source, mature baseline: z=0 0.524, z=5 0.467, z=6 0.422), so at 0.10 the freeze needed z ~ 14.95 (a ~30% one-minute move) and the control was decorative.
	// 0.45 triggers just past z=5 (~5.4 mature, ~4.2 sparse) as the ADR specifies. It relies on the unmeasured-liquidity sentinel: approxUSDVolume returning 0 for non-USD quotes used to pin the confidence leg true.
	DefaultPhase2ConfidenceMaxFreeze  = 0.45 // freeze when confidence < this
	DefaultPhase2ZScoreMinFreeze      = 5.0  // freeze when z > this
	DefaultPhase2SourceCountMaxFreeze = 1    // freeze when source_count <= this
)

// Phase2Thresholds is the operator-tunable Phase 2 freeze condition and the ADR-0019 lifecycle it feeds; zero values take defaults.
// The first three fields decide WHETHER a bucket fires; the [freeze.Policy] fields decide HOW LONG it holds and what ends it.
type Phase2Thresholds struct {
	ConfidenceMaxFreeze  float64
	ZScoreMinFreeze      float64
	SourceCountMaxFreeze int

	// Lifecycle is the ADR-0019 freeze-duration policy (initial holds, extension size and cap, auto-unfreeze); zero fields fall back to the `freeze.Default*` constants.
	Lifecycle freeze.Policy
}

// withDefaults replaces zero-valued fields with package defaults so a partial operator override merges over them.
// Zero ConfidenceMaxFreeze means "use default" (a literal 0.0 would never freeze on confidence); to disable that leg set >= 1.0 (z and source_count must still pass).
func (p Phase2Thresholds) withDefaults() Phase2Thresholds {
	if p.ConfidenceMaxFreeze == 0 {
		p.ConfidenceMaxFreeze = DefaultPhase2ConfidenceMaxFreeze
	}
	if p.ZScoreMinFreeze == 0 {
		p.ZScoreMinFreeze = DefaultPhase2ZScoreMinFreeze
	}
	// SourceCountMaxFreeze: zero is a valid threshold ("freeze only
	// when zero contributing sources") — we can't use the zero
	// sentinel here. Operators who want to override this MUST set a
	// non-zero value; the documented default fires below.
	if p.SourceCountMaxFreeze == 0 {
		p.SourceCountMaxFreeze = DefaultPhase2SourceCountMaxFreeze
	}
	return p
}

// phase2FreezeFires reports whether the ADR-0019 3-signal AND holds for a bucket: confidence < ConfidenceMaxFreeze AND z_score > ZScoreMinFreeze AND source_count <= SourceCountMaxFreeze.
// Two-of-three (e.g. anomalous z + low confidence but multi-source) does NOT freeze; those surface via flags.divergence_warning.
func phase2FreezeFires(c confidenceWithSourceCount, t Phase2Thresholds) bool {
	t = t.withDefaults()
	return c.Confidence < t.ConfidenceMaxFreeze &&
		c.ZScore > t.ZScoreMinFreeze &&
		c.SourceCount <= t.SourceCountMaxFreeze
}

// releaseAgreementMaxPct bounds how far a mid-freeze RELEASE CANDIDATE may sit from a corroborating lens and still count as agreeing (Signal.ReleaseCorroborated).
// 5%: a genuine repricing carries the references with it, while held-manipulation offsets (~5-40%) stay blocked and walk the ladder to the operator.
const releaseAgreementMaxPct = 5.0

// releaseCorroborated reports whether a lens produced a reading THIS bucket agreeing with the fresh price: the triangulation composite (prior-tick samples can't fire mid-freeze); the CURRENT-BUCKET composite reference, against its tighter [CompositeReferenceConfig.ReleaseBandPct] band (a reference that resolved and disagrees holds the streak at zero); or the cross-oracle median compared against the fresh candidate here, not the cached DivergencePct, which certifies the pinned LKG.
// No lens reading means false: the streak holds at zero and the ladder escalates.
func releaseCorroborated(c confidenceComputation, candidate *big.Rat, ref compositeReference, compositeBandPct float64) bool {
	switch {
	case ref.resolved():
		// Exact Rat compare, matching how the verdict was decided; a float64 compare would put the release boundary of a possibly-manipulated held price at the mercy of binary rounding.
		band := new(big.Rat).SetFloat64(compositeBandPct / 100.0)
		if band != nil && ref.deviationRatio != nil && ref.deviationRatio.Cmp(band) <= 0 {
			return true
		}
	case c.TriangulationChecked && math.Abs(c.TriangulationDivergencePct) <= releaseAgreementMaxPct:
		return true
	}
	if crossOracleMedian := c.CrossOracleMedian; crossOracleMedian > 0 && candidate != nil {
		cand, _ := candidate.Float64() // i128:ok candidate price for a percentage agreement compare, not served
		if cand > 0 {
			pct := math.Abs(cand-crossOracleMedian) / crossOracleMedian * 100
			if pct <= releaseAgreementMaxPct {
				return true
			}
		}
	}
	return false
}

// confidenceWithSourceCount is the Phase 2 input bundle. Pulled
// out of [confidenceComputation] so [phase2FreezeFires] is a pure
// function on three floats — easy to unit-test exhaustively.
type confidenceWithSourceCount struct {
	Confidence  float64
	ZScore      float64
	SourceCount int
}

// classOf resolves the asset class label for a pair, using the
// Phase 1 checker's classifier when it's wired so the per-class
// metric stays consistent across both phases.
func (o *Orchestrator) classOf(pair canonical.Pair) anomaly.AssetClass {
	if o.cfg.Anomaly != nil {
		return o.cfg.Anomaly.ClassOf(pair.Base)
	}
	return anomaly.ClassDefault
}

// unpricedBucket is decideBucket's exit for a bucket with no price (empty after filtering, under [Config.MinUSDVolume], or no computable VWAP). It publishes nothing, but a frozen window still advances its lifecycle: marker TTL and ladder liveness are refreshed only there, and skipping it let thin buckets lapse the freeze, which the next priced bucket read as the operator override.
// The signal is unscored: it resets the unfreeze streak, slides an expired hold without spending an extension, and never releases.
func (o *Orchestrator) unpricedBucket(
	ctx context.Context,
	pair canonical.Pair,
	window time.Duration,
	now time.Time,
) (*publishedBucket, error) {
	stateKey := pair.String() + ":" + window.String()
	if st, cached := o.freezeStates[stateKey]; !cached || st.Active() {
		decision := anomaly.Decision{
			Action: anomaly.ActionFreeze,
			Class:  o.classOf(pair),
			Reason: "phase2:unscored",
		}
		o.stepFreezeLifecycle(ctx, pair, window, stateKey, freeze.Signal{Now: now}, decision)
	}
	return nil, nil
}

// stepPhase2Freeze translates one scored bucket into a [freeze.Signal] and advances the ADR-0019 lifecycle; returns true when publication must be refused. Called on EVERY bucket, since a pair inside its hold stays frozen through quiet buckets.
// confOK=false (no baseline, no prev VWAP, bootstrap) yields an UNSCORED signal that can neither fire nor earn an unfreeze streak: "could not measure" is not evidence of recovery, and right after a restart that is the state.
func (o *Orchestrator) stepPhase2Freeze(
	ctx context.Context,
	pair canonical.Pair,
	window time.Duration,
	stateKey string,
	now time.Time,
	conf confidenceComputation,
	confOK bool,
	sourceCount int,
	vwap *big.Rat,
	compositeRef compositeReference,
) bool {
	sig := freeze.Signal{Now: now}
	decision := anomaly.Decision{
		Action: anomaly.ActionFreeze,
		Class:  o.classOf(pair),
		// Phase 2 doesn't compute a raw class deviation — the per-
		// asset baseline replaces the per-class threshold. Keep
		// DeviationPct zero; the Reason field carries the source.
		Reason: "phase2:unscored",
	}
	if confOK {
		input := confidenceWithSourceCount{
			Confidence:  conf.Score.Confidence,
			ZScore:      conf.ZScore,
			SourceCount: sourceCount,
		}
		sig.Scored = true
		sig.Confidence = input.Confidence
		sig.ZScore = input.ZScore
		sig.Fires = phase2FreezeFires(input, o.cfg.Phase2Thresholds)
		sig.Corroborated = corroborated(conf.Score.Factors)
		sig.ReleaseCorroborated = releaseCorroborated(conf, vwap, compositeRef,
			o.cfg.CompositeReference.withDefaults().ReleaseBandPct)
		// `sources=` is the distinct-VENUE count the AND read; neither the
		// composite reference nor router corroboration ever changes it.
		// route_corroboration= records the router's count beside it, so the
		// audit trail can tell "two venues" from "two paths through our graph".
		decision.Reason = fmt.Sprintf("phase2:3_signal_AND confidence=%.3f z=%.2f z_window=%s sources=%d",
			input.Confidence, input.ZScore, baselineWindowLabel(conf.ZWindow), input.SourceCount)
		if n, ok := o.routeCorroborationCount(pair, window); ok {
			decision.Reason += fmt.Sprintf(" route_corroboration=%d", n)
		}
		if compositeRef.verdict != "" {
			decision.Reason += compositeRef.reasonSuffix()
			if sig.Fires && compositeRef.verdict == compositeVerdictCorroborated {
				// The deep-market composite moved WITH the venue on this bucket: the move is market-wide, so the single-source AND does not engage (mid-hold, it does not reset the release streak). Refuted/unavailable leave Fires as computed.
				sig.Fires = false
				obs.AggregatorCompositeFreezeSuppressedTotal.WithLabelValues(pair.String(), windowLabel(window)).Inc()
				o.logger.Info("phase2 freeze suppressed: composite reference corroborates the move",
					"pair", pair.String(),
					"window", window.String(),
					"reason", decision.Reason)
			}
		}
	}
	return o.stepFreezeLifecycle(ctx, pair, window, stateKey, sig, decision)
}

// corroborated reports whether a lens produced a reading for this bucket, i.e. whether a second opinion was available; it selects ADR-0019's 30-minute initial hold over the shorter uncorroborated one (freeze.DefaultUncorroboratedInitialHold).
// "Was a lens consulted", NOT "did it agree": a DISAGREEING composite or reference is the strongest evidence the freeze is true, so counting only agreement would give the best-evidenced freezes the shortest hold. Agreement is already priced in via the confidence leg of the fire condition.
func corroborated(f confidence.Factors) bool {
	return f.TriangulationChecked || f.CrossOracleChecked
}

// stepFreezeLifecycle is the single owner of a pair's freeze lifecycle and its `freeze:<asset>:<quote>` marker: evaluates the ADR-0019 policy, persists state, drives the marker, emits metrics and keeps the LKG VWAP alive. Returns true when the bucket must be refused.
// Both freeze phases funnel here: two writers with different TTL semantics would let a Phase 1 fire truncate a Phase 2 hold, and a marker written without advancing the ladder would hold forever without escalating to an operator.
func (o *Orchestrator) stepFreezeLifecycle(
	ctx context.Context,
	pair canonical.Pair,
	window time.Duration,
	stateKey string,
	sig freeze.Signal,
	decision anomaly.Decision,
) bool {
	prev, overridden, err := o.loadFreezeState(ctx, pair, window, stateKey)
	if err != nil {
		// Unknown is not "never frozen": this may be the first bucket
		// after a restart, unscored and unable to re-fire a live freeze,
		// so withhold it and leave marker and state untouched.
		o.logger.Warn("freeze lifecycle: marker read failed on cold key; withholding bucket",
			"pair", pair.String(), "window", window.String(), "err", err)
		return true
	}
	if overridden {
		o.releaseFreeze(ctx, pair, window, stateKey, prev, freeze.TransitionOverridden)
		return false
	}

	out := o.cfg.Phase2Thresholds.Lifecycle.Evaluate(prev, sig)
	if !out.Frozen {
		o.releaseFreeze(ctx, pair, window, stateKey, prev, out.Transition)
		return false
	}
	if !prev.Active() && !out.State.OverriddenAt.IsZero() {
		obs.AnomalyFreezeRefiredAfterOverrideTotal.Inc()
	}
	o.engageFreeze(ctx, pair, window, stateKey, decision, out)
	return true
}

// loadFreezeState returns the working lifecycle state for a (pair, window) key plus whether the operator force-unfroze it out of band. Two cheap Redis reads:
// a cold key (first evaluation in this process) re-hydrates THIS window's ladder from the marker ([Orchestrator.loadMarkerState]), else every deploy restarts the 2-hour escalation clock and the window publishes the bucket the freeze was withholding; an absent marker is the operator override (the read is window-AGNOSTIC, the marker is pair-scoped), else the in-memory ladder would re-write it.
// A cold-key read error returns err and caches nothing; the caller must withhold the bucket.
func (o *Orchestrator) loadFreezeState(
	ctx context.Context,
	pair canonical.Pair,
	window time.Duration,
	stateKey string,
) (freeze.State, bool, error) {
	st, cached := o.freezeStates[stateKey]
	if o.cfg.FreezeWriter == nil {
		return st, false, nil
	}

	if !cached {
		marked, ok, err := o.loadMarkerState(ctx, pair, window)
		if err != nil {
			return freeze.State{}, false, err
		}
		if ok && marked.Active() {
			st = marked
			o.logger.Info("freeze lifecycle rehydrated from marker",
				"pair", pair.String(),
				"window", window.String(),
				"fired_at", marked.FiredAt,
				"hold_until", marked.HoldUntil,
				"extensions_used", marked.ExtensionsUsed,
				"escalated", marked.Escalated)
		}
		o.freezeStates[stateKey] = st
		return st, false, nil
	}

	if st.Active() {
		if _, ok, err := o.cfg.FreezeWriter.LoadState(ctx, pair.Base, pair.Quote); err == nil && !ok {
			return st, true, nil
		}
	}
	return st, false, nil
}

// loadMarkerState reads the lifecycle the marker records for (pair, window), via the window-aware reader when the writer has one.
// The pair-wide [FreezeMarker.LoadState] is only the fallback: it returns whichever ladder the marker carries, the cross-window bleed [WindowedFreezeMarker] exists to stop. A writer that can't scope a marker errs towards holding a freeze rather than dropping one.
func (o *Orchestrator) loadMarkerState(
	ctx context.Context,
	pair canonical.Pair,
	window time.Duration,
) (freeze.State, bool, error) {
	if o.windowedFreeze != nil {
		return o.windowedFreeze.LoadStateForWindow(ctx, pair.Base, pair.Quote, window)
	}
	return o.cfg.FreezeWriter.LoadState(ctx, pair.Base, pair.Quote)
}

// engageFreeze applies a still-frozen [freeze.Outcome]: persist the
// state, count it, log the transition, protect the last-known-good
// value, and refresh the marker with the remaining hold.
func (o *Orchestrator) engageFreeze(
	ctx context.Context,
	pair canonical.Pair,
	window time.Duration,
	stateKey string,
	decision anomaly.Decision,
	out freeze.Outcome,
) {
	o.freezeStates[stateKey] = out.State

	o.mu.Lock()
	o.freezesEngaged++
	o.mu.Unlock()

	// Per FROZEN TICK, not per fire transition: anomaly.yml rules read a sustained freeze as a sustained rate() on this counter, so incrementing only on the fire would silently disarm stellarindex_anomaly_freeze_engaged (severity:ticket).
	obs.AnomalyFreezeEngagedTotal.WithLabelValues(string(decision.Class)).Inc()

	o.logFreezeTransition(pair, window, decision, out)

	// This pair's LKG stays in cache for the rest of the tick;
	// record the refusal so triangulation can't republish it as a
	// derived price.
	o.markFrozenThisTick(pair, window)

	// A freeze skips the VWAP cache write, so the
	// prior bucket's value must outlive the marker. Refresh regardless
	// of whether a FreezeWriter is wired — the LKG keeps serving
	// either way.
	o.keepFrozenVWAPAlive(ctx, pair, window, out.MarkerTTL)
	o.reseedFrozenVWAP(ctx, pair, window, stateKey, out.MarkerTTL)

	if o.cfg.FreezeWriter == nil {
		return
	}
	// LKG VWAP we're freezing on: the prior bucket's value (which
	// stays in cache because the caller skips the cache write).
	// Empty string when no prior bucket exists (first-tick freeze).
	var frozenValue string
	if held := o.prevVWAPs[stateKey]; held != nil {
		frozenValue = formatRatFixed(held, 12)
	}
	// Record the ladder against the window that owns it: the marker is pair-scoped (its presence is the pair-wide flags.frozen) but the lifecycle in it is this window's alone, and a cold sibling would otherwise adopt it ([WindowedFreezeMarker]).
	if err := o.markHoldForWindow(ctx, pair, window,
		frozenValue, decision, out.State, out.MarkerTTL); err != nil {
		o.logger.Warn("freeze marker write failed",
			"pair", pair.String(), "window", window, "err", err)
		// Soft-fail: the anomaly was detected, the marker write
		// failed, the API won't see flags.frozen. Operators alert on
		// AnomalyFreezeEngagedTotal vs the API-side flag rate; a
		// sustained gap = the writer is broken. Don't fail the tick.
	}
}

// reseedValueWithCoverageLua sets KEYS[1] to ARGV[1] only if absent and, only
// then, sets KEYS[2] to ARGV[2] (or deletes it when ARGV[2] is empty); PX ARGV[3].
const reseedValueWithCoverageLua = `
if redis.call('SET', KEYS[1], ARGV[1], 'NX', 'PX', ARGV[3]) then
  if ARGV[2] == '' then
    redis.call('DEL', KEYS[2])
  else
    redis.call('SET', KEYS[2], ARGV[2], 'PX', ARGV[3])
  end
  return 1
end
return 0
`

// reseedFrozenVWAP writes the held comparator back as the window's VWAP, with its observed-at stamp, when the keys are absent (cache flushed mid-hold); otherwise the API reads frozen=true with nothing servable and answers 503.
// SetNX never overwrites a surviving value or stamp. The stamp is the held bucket's own end (skipped if unknown) so served age stays honest; coverage is written only when the held value lands, so a surviving composite never gains the held window's coverage.
func (o *Orchestrator) reseedFrozenVWAP(ctx context.Context, pair canonical.Pair, window time.Duration, stateKey string, ttl time.Duration) {
	// Read the last PUBLISHED value, not the caller's comparator: mid-freeze
	// that is the previous refused bucket and must never be served as held.
	held := o.prevVWAPs[stateKey]
	heldEnd, ok := o.prevVWAPBucketEnd[stateKey]
	if held == nil || !ok {
		return
	}
	if ttl <= 0 {
		ttl = cachekeys.FreezeTTL
	}
	key := cachekeys.VWAP(pair.Base, pair.Quote, window).String()
	atKey := cachekeys.VWAPObservedAt(pair.Base, pair.Quote, window).String()
	covKey := cachekeys.VWAPCoverage(pair.Base, pair.Quote, window).String()
	var coverageArg string
	if coverage, known := o.prevVWAPCoverage[stateKey]; known {
		coverageArg = cachekeys.FormatVWAPCoverage(coverage)
	}
	if _, err := o.cache.TxPipelined(ctx, func(p redis.Pipeliner) error {
		p.SetNX(ctx, atKey, cachekeys.FormatVWAPObservedAt(heldEnd), ttl)
		p.Eval(ctx, reseedValueWithCoverageLua, []string{key, covKey},
			formatRatFixed(held, 12), coverageArg, ttl.Milliseconds())
		return nil
	}); err != nil {
		o.logger.Debug("freeze: held VWAP reseed failed",
			"pair", pair.String(), "window", window, "key", key, "err", err)
	}
}

// markHoldForWindow writes the freeze marker for (pair, window),
// recording the owning window when the configured writer can carry it
// and falling back to the pair-wide write when it cannot.
func (o *Orchestrator) markHoldForWindow(
	ctx context.Context,
	pair canonical.Pair,
	window time.Duration,
	frozenValue string,
	decision anomaly.Decision,
	state freeze.State,
	ttl time.Duration,
) error {
	if o.windowedFreeze != nil {
		return o.windowedFreeze.MarkHoldForWindow(ctx, pair.Base, pair.Quote, window,
			frozenValue, decision, state, ttl)
	}
	return o.cfg.FreezeWriter.MarkHold(ctx, pair.Base, pair.Quote,
		frozenValue, decision, state, ttl)
}

// logFreezeTransition emits the per-transition operator signal and extension/escalation counters. Escalation logs at ERROR: ADR-0019 holds it "until manual unfreeze", so /v1/price stays pinned to the LKG until a human acts.
func (o *Orchestrator) logFreezeTransition(
	pair canonical.Pair,
	window time.Duration,
	decision anomaly.Decision,
	out freeze.Outcome,
) {
	switch out.Transition {
	case freeze.TransitionFired:
		o.logger.Info("freeze engaged",
			"pair", pair.String(),
			"window", window.String(),
			"class", string(decision.Class),
			"reason", decision.Reason,
			"hold_until", out.State.HoldUntil,
			"corroborated", out.State.Corroborated,
			"extensions_used", out.State.ExtensionsUsed,
			"resumed_override_ladder", !out.State.OverriddenAt.IsZero(),
			"writer_wired", o.cfg.FreezeWriter != nil)
	case freeze.TransitionExtended:
		obs.AnomalyFreezeExtensionsTotal.Inc()
		o.logger.Warn("freeze hold extended",
			"pair", pair.String(),
			"window", window.String(),
			"class", string(decision.Class),
			"extensions_used", out.State.ExtensionsUsed,
			"max_extensions", o.cfg.Phase2Thresholds.Lifecycle.WithDefaults().MaxExtensions,
			"hold_until", out.State.HoldUntil,
			"reason", decision.Reason)
	case freeze.TransitionEscalated:
		obs.AnomalyFreezeEscalatedTotal.Inc()
		o.logger.Error("freeze escalated to operator review — will NOT auto-unfreeze",
			"pair", pair.String(),
			"window", window.String(),
			"class", string(decision.Class),
			"fired_at", out.State.FiredAt,
			"extensions_used", out.State.ExtensionsUsed,
			"resumed_override_ladder", !out.State.OverriddenAt.IsZero(),
			"reason", decision.Reason)
	case freeze.TransitionHeldUnscored:
		// The hold expired on a bucket the scorer could not evaluate (restart bootstrap, scoring outage). ADR-0019 says slide, not extend, but a sustained run means scoring is stuck while extension/escalation counters stay zero, so it gets its own counter and WARN.
		obs.AnomalyFreezeHeldUnscoredTotal.Inc()
		o.logger.Warn("freeze hold slid — bucket could not be scored",
			"pair", pair.String(),
			"window", window.String(),
			"hold_until", out.State.HoldUntil,
			"extensions_used", out.State.ExtensionsUsed)
	case freeze.TransitionHeld, freeze.TransitionNone,
		freeze.TransitionReleased, freeze.TransitionOverridden:
		// Held is the steady state of a live freeze — logging it every
		// tick would bury the transitions above. The other three never
		// reach this function (they are not frozen outcomes).
	default:
		o.logger.Debug("freeze: unhandled lifecycle transition",
			"pair", pair.String(), "transition", string(out.Transition))
	}
}

// releaseFreeze ends ONE window's freeze: drops its ladder, counts the release by mode, and once no sibling window is still frozen deletes the shared marker so `flags.frozen` clears immediately.
// The state entry stays present-but-inactive so [Orchestrator.loadFreezeState]'s hydrate stays once-per-process.
func (o *Orchestrator) releaseFreeze(
	ctx context.Context,
	pair canonical.Pair,
	window time.Duration,
	stateKey string,
	prev freeze.State,
	transition freeze.Transition,
) {
	if !prev.Active() {
		// Nothing was frozen; keep any ladder an earlier override left.
		o.freezeStates[stateKey] = prev
		return
	}

	mode := "auto"
	o.freezeStates[stateKey] = freeze.State{}
	if transition == freeze.TransitionOverridden {
		mode = o.overrideMode(ctx, pair)
		o.freezeStates[stateKey] = prev.Overridden(o.clock())
	}
	obs.AnomalyFreezeReleasedTotal.WithLabelValues(mode).Inc()
	o.logger.Info("freeze released",
		"pair", pair.String(),
		"window", window.String(),
		"mode", mode,
		"fired_at", prev.FiredAt,
		"extensions_used", prev.ExtensionsUsed,
		"escalated", prev.Escalated)

	if o.cfg.FreezeWriter == nil {
		return
	}

	// The marker and durable ladder are keyed by (asset, quote) but windows run independently, and the marker's presence is the pair-wide `flags.frozen`, so it must stay set while ANY window is frozen.
	// Clearing it when a short window releases would let the next tick read the absent marker as an operator override (loadFreezeState) and republish a still-anomalous VWAP with frozen=false, with no self-heal. Retire it only when the LAST frozen window releases.
	// A genuine operator override (`freeze-unfreeze` deletes the marker out of band) is unaffected: each window observes it independently.
	if o.siblingWindowFrozen(pair, window) {
		// The marker stays for the sibling, but THIS window's ladder must not: a restart would rehydrate it as a freeze that had already released and re-pin a healthy window to its LKG. Retiring it keeps the per-window ladders a faithful record of what is frozen.
		if o.windowedFreeze != nil {
			if err := o.windowedFreeze.RetireWindowLadder(ctx, pair.Base, pair.Quote, window); err != nil {
				o.logger.Warn("freeze ladder retire failed — this window's released ladder stays in the marker",
					"pair", pair.String(), "window", window.String(), "err", err)
			}
		}
		return
	}

	o.clearReleasedMarker(ctx, pair, window)
}

// overrideMode labels a release the orchestrator detected as a missing
// marker: "operator" when freeze-unfreeze left its tombstone (or the writer
// cannot say), "lapsed" when the marker and ladder expired with no operator
// involved, so the operator series counts only human force-unfreezes.
func (o *Orchestrator) overrideMode(ctx context.Context, pair canonical.Pair) string {
	if o.overrideReader == nil {
		return "operator"
	}
	recorded, err := o.overrideReader.OverrideRecorded(ctx, pair.Base, pair.Quote)
	if err != nil {
		o.logger.Warn("freeze override tombstone read failed — counting the release as operator",
			"pair", pair.String(), "err", err)
		return "operator"
	}
	if recorded {
		return "operator"
	}
	return "lapsed"
}

// clearReleasedMarker deletes the pair's marker when what THIS PROCESS believes is its last frozen window releases.
// [Orchestrator.siblingWindowFrozen] only sees windows that reached the freeze step in this process; a window under [Config.MinUSDVolume], or any window after a restart, is invisible to it. So a window-aware writer releases against the marker's own record ([WindowedFreezeMarker.ReleaseWindow]) and keeps a still-frozen (e.g. ESCALATED) sibling's marker and ladder; a writer that can't scope clears outright.
// An operator override already deleted the marker; clearing again is an idempotent DEL.
func (o *Orchestrator) clearReleasedMarker(ctx context.Context, pair canonical.Pair, window time.Duration) {
	if o.windowedFreeze == nil {
		if err := o.cfg.FreezeWriter.Clear(ctx, pair.Base, pair.Quote); err != nil {
			o.logger.Warn("freeze marker clear failed — flags.frozen stays set until its TTL",
				"pair", pair.String(), "err", err)
		}
		return
	}
	kept, err := o.windowedFreeze.ReleaseWindow(ctx, pair.Base, pair.Quote, window)
	if err != nil {
		o.logger.Warn("freeze marker release failed — flags.frozen stays set until its TTL",
			"pair", pair.String(), "window", window.String(), "err", err)
		return
	}
	if kept {
		o.logger.Info("freeze marker kept — it still records a sibling window's freeze this process has not evaluated",
			"pair", pair.String(), "released_window", window.String())
	}
}

// siblingWindowFrozen reports whether any window OTHER than `window` still holds a live freeze for `pair`, keeping the shared marker alive until the last releases.
// Reads o.freezeStates on the single-Tick goroutine (no lock, as loadFreezeState). Rebuilds each sibling's `pair:window` stateKey exactly as refreshPairWindow does instead of prefix-matching, since asset ids contain ':' (`fiat:USD`).
func (o *Orchestrator) siblingWindowFrozen(pair canonical.Pair, window time.Duration) bool {
	for _, w := range o.cfg.Windows {
		if w == window {
			continue
		}
		if o.freezeStates[pair.String()+":"+w.String()].Active() {
			return true
		}
	}
	return false
}
