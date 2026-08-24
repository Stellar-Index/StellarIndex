package orchestrator

import (
	"context"
	"fmt"
	"math"
	"math/big"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/aggregate/anomaly"
	"github.com/Stellar-Index/StellarIndex/internal/aggregate/confidence"
	"github.com/Stellar-Index/StellarIndex/internal/aggregate/freeze"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
)

// Phase 2 freeze thresholds per ADR-0019 §"Freeze policy". All
// three signals must agree to fire — keeps the freeze decision
// robust against legitimate market events (those have multi-source
// corroboration even if z is high) while still catching the
// USTRY-shape attack pattern (single source, large deviation,
// confidence-killing combination of factors).
//
// The package-level defaults match the ADR-0019 documented values
// and are used when [Config.Phase2Thresholds] is the zero value
// (operator hasn't overridden via TOML). Operators tune via the
// `[anomaly.phase2]` config block.
const (
	// 0.45, NOT ADR-0019's original 0.10 — operator decision of
	// 2026-07-25, taken together with the COR-14 fix as this repo's
	// remediation ledger requires ("R-072/R-078 ... must be fixed as ONE
	// decision").
	//
	// Why the ADR's own z>5 needs a number this high: confidence is a
	// weighted geometric mean, so it decays gently in z. Measured on the
	// real combiner for a single-source bucket, mature baseline:
	//   z=0 -> 0.5241   z=5 -> 0.4674   z=6 -> 0.4215
	// and sparse/bootstrap-capped: z=0 -> 0.4767, z=5 -> 0.4252.
	// At 0.10 the freeze needs z ~= 14.95 — roughly a 30% move in one
	// 1-minute bucket for XLM (return_mad ~2%), i.e. the control was
	// decorative. 0.45 puts the trigger just past z=5 (~5.4 mature,
	// ~4.2 sparse), which is what ADR-0019 §"Freeze fires only when all
	// three of the following hold" actually specifies.
	//
	// On the false-fire history: markPhase2Freeze's comment records
	// "Phase 2 false-fires across many pairs" from the era when
	// approxUSDVolume returned 0 for non-USD-quoted pairs. That is NOT
	// evidence against this number — those pairs froze on (z>5 AND
	// source_count<=1) with the confidence leg pinned permanently true,
	// so the documented 3-signal AND was really a 2-signal one. With
	// COR-14 fixed, confidence is a genuine third gate and this is
	// strictly stricter than the configuration that false-fired.
	DefaultPhase2ConfidenceMaxFreeze  = 0.45 // freeze when confidence < this
	DefaultPhase2ZScoreMinFreeze      = 5.0  // freeze when z > this
	DefaultPhase2SourceCountMaxFreeze = 1    // freeze when source_count <= this
)

// Phase2Thresholds is the operator-tunable shape of the Phase 2
// freeze condition AND of the ADR-0019 freeze lifecycle it feeds.
// The orchestrator's [Config.Phase2Thresholds] holds an instance of
// this struct; zero values fall back to documented defaults so unset
// fields do the right thing.
//
// The two halves answer different questions and are tuned
// independently: the first three fields decide WHETHER a bucket fires
// a freeze, the [freeze.Policy] fields decide HOW LONG the resulting
// freeze holds, when it re-evaluates, and what it takes to end.
type Phase2Thresholds struct {
	ConfidenceMaxFreeze  float64
	ZScoreMinFreeze      float64
	SourceCountMaxFreeze int

	// Lifecycle is the ADR-0019 §"Freeze duration" policy — initial
	// hold (corroborated / uncorroborated), extension size, extension
	// cap before operator escalation, and the auto-unfreeze condition.
	// Zero-valued fields fall back to the `freeze.Default*` constants,
	// which are the ADR's own numbers.
	Lifecycle freeze.Policy
}

// withDefaults returns a Phase2Thresholds with any zero-valued
// field replaced by the package default. Lets the orchestrator
// merge an operator's partial override over the documented
// stop-gap without forcing the operator to repeat every value.
//
// The zero value sentinel: ConfidenceMaxFreeze == 0 means "use
// default" because a literal 0.0 would mean "never freeze on
// confidence" (any value < 0 is impossible since confidence is
// clamped [0, 1]) — equivalent to disabling that condition. We
// treat the zero value as "use default"; operators who genuinely
// want to disable the confidence check set a value >= 1.0 (always
// fires the confidence sub-condition; freeze still requires z +
// source_count to also pass).
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

// phase2FreezeFires reports whether the Phase 2 freeze condition
// (3-signal AND) holds for a bucket given its confidence score,
// raw z-score, and contributing source count, evaluated against
// the supplied (or default-merged) thresholds.
//
// Per ADR-0019 §"Freeze policy":
//
//	freeze_condition = (
//	  confidence  < ConfidenceMaxFreeze
//	  AND z_score > ZScoreMinFreeze
//	  AND source_count <= SourceCountMaxFreeze
//	)
//
// All three must be true. A two-of-three pattern (e.g. anomalous
// z + low confidence but multi-source) does NOT freeze — those
// scenarios surface via flags.divergence_warning instead, set by
// the API's read-side divergence check.
func phase2FreezeFires(c confidenceWithSourceCount, t Phase2Thresholds) bool {
	t = t.withDefaults()
	return c.Confidence < t.ConfidenceMaxFreeze &&
		c.ZScore > t.ZScoreMinFreeze &&
		c.SourceCount <= t.SourceCountMaxFreeze
}

// releaseAgreementMaxPct bounds how far a mid-freeze RELEASE CANDIDATE
// may sit from a corroborating lens's reading and still count as
// "agreeing" for the auto-unfreeze streak (Signal.ReleaseCorroborated).
// 5%: a genuine repricing carries the references with it (candidate vs
// reference median lands within low single digits), while the held-
// manipulation band the 2026-08-24 corroborated-release panel measured
// (~5-40% held offsets releasing under per-tick calmness) stays
// blocked and walks the ladder to the operator.
const releaseAgreementMaxPct = 5.0

// releaseCorroborated reports whether a corroborating lens produced a
// reading THIS bucket that agrees with the bucket's own fresh price:
//   - the triangulation composite (computed against the fresh VWAP by
//     construction — triangulationDivergencePct takes it as input), or
//   - the cross-oracle reference median, compared against the fresh
//     candidate HERE — deliberately not the cached DivergencePct, which
//     mid-freeze was computed against the pinned served price and
//     therefore certifies the LKG, not the candidate.
//
// No lens reading ⇒ false ⇒ the streak holds at zero and the ladder
// escalates to an operator, matching the pre-shadow-comparator posture
// for uncorroboratable pairs.
func releaseCorroborated(c confidenceComputation, candidate *big.Rat) bool {
	if c.TriangulationChecked && math.Abs(c.TriangulationDivergencePct) <= releaseAgreementMaxPct {
		return true
	}
	if crossOracleMedian := c.CrossOracleMedian; crossOracleMedian > 0 && candidate != nil {
		cand, _ := candidate.Float64()
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

// stepPhase2Freeze translates one scored bucket into a [freeze.Signal]
// and advances the ADR-0019 lifecycle. Returns true when the bucket
// must be refused publication.
//
// Called on EVERY bucket, not only on buckets the 3-signal AND fires
// for — a pair inside its hold stays frozen through quiet buckets,
// and only the ADR's auto-unfreeze condition ends it.
//
// confOK=false (no baseline, no previous VWAP, bootstrap) yields an
// UNSCORED signal: it cannot fire a freeze and it cannot earn an
// auto-unfreeze streak. That asymmetry is deliberate — "we could not
// measure this bucket" is not evidence the pair recovered, and the
// state right after an aggregator restart is exactly that.
func (o *Orchestrator) stepPhase2Freeze(
	ctx context.Context,
	pair canonical.Pair,
	window time.Duration,
	stateKey string,
	now time.Time,
	conf confidenceComputation,
	confOK bool,
	sourceCount int,
	prevVWAP *big.Rat,
	vwap *big.Rat,
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
		sig.ReleaseCorroborated = releaseCorroborated(conf, vwap)
		decision.Reason = fmt.Sprintf("phase2:3_signal_AND confidence=%.3f z=%.2f sources=%d",
			input.Confidence, input.ZScore, input.SourceCount)
	}
	return o.stepFreezeLifecycle(ctx, pair, window, stateKey, sig, decision, prevVWAP)
}

// corroborated reports whether a corroborating lens produced a
// reading for this bucket — i.e. whether the freeze decision was
// taken with a second opinion available at all. It selects between
// ADR-0019's 30-minute initial hold and the shorter uncorroborated
// one (see freeze.DefaultUncorroboratedInitialHold).
//
// "Was a lens consulted", NOT "did the lens agree". A composite or an
// external reference that DISAGREES is the strongest evidence a
// freeze is true, so reading only the agreeing case as corroboration
// would hand the shortest hold to the best-evidenced freezes — the
// exact inversion of what the duration is for. The agreeing case is
// already priced in elsewhere: agreement raises the confidence score,
// which is a leg of the fire condition, so a well-corroborated
// healthy pair mostly does not reach this code at all.
func corroborated(f confidence.Factors) bool {
	return f.TriangulationChecked || f.CrossOracleChecked
}

// stepFreezeLifecycle is the single owner of a pair's freeze
// lifecycle and of its `freeze:<asset>:<quote>` marker: it evaluates
// the ADR-0019 policy, persists the resulting state, drives the
// marker (write / refresh / delete), emits metrics + logs, and keeps
// the last-known-good VWAP alive for the hold's duration.
//
// Returns true when the bucket must be refused publication.
//
// Both freeze phases funnel through here. One owner per marker key is
// a correctness requirement, not tidiness: two writers with different
// TTL semantics would let a Phase 1 fire truncate a Phase 2 hold, and
// a phase that wrote the marker without advancing the ladder would
// hold a freeze forever without ever escalating it to an operator.
func (o *Orchestrator) stepFreezeLifecycle(
	ctx context.Context,
	pair canonical.Pair,
	window time.Duration,
	stateKey string,
	sig freeze.Signal,
	decision anomaly.Decision,
	prevVWAP *big.Rat,
) bool {
	prev, overridden := o.loadFreezeState(ctx, pair, stateKey)
	if overridden {
		o.releaseFreeze(ctx, pair, window, stateKey, prev, freeze.TransitionOverridden)
		return false
	}

	out := o.cfg.Phase2Thresholds.Lifecycle.Evaluate(prev, sig)
	if !out.Frozen {
		o.releaseFreeze(ctx, pair, window, stateKey, prev, out.Transition)
		return false
	}
	o.engageFreeze(ctx, pair, window, stateKey, decision, prevVWAP, out)
	return true
}

// loadFreezeState returns the working lifecycle state for a
// (pair, window) key, plus whether the operator has force-unfrozen it
// out of band.
//
// Two Redis reads, both deliberate and both cheap:
//
//   - Cold key (first evaluation in this process): re-hydrate the
//     ladder from the marker. Without it, every deploy would restart
//     the 2-hour escalation clock, so a rolling restart cadence
//     shorter than 2 hours could hold a pair frozen indefinitely
//     while never paging anyone.
//   - Live freeze: confirm the marker still exists. ADR-0019 requires
//     "operator override always available: force unfreeze", and
//     deleting the marker is that override — but without this check
//     the orchestrator's in-memory ladder would simply re-write the
//     marker on the next tick and the override would not stick.
//
// Healthy pairs cost nothing steady-state: an inactive-but-present
// entry short-circuits both reads.
func (o *Orchestrator) loadFreezeState(
	ctx context.Context,
	pair canonical.Pair,
	stateKey string,
) (freeze.State, bool) {
	st, cached := o.freezeStates[stateKey]
	if o.cfg.FreezeWriter == nil {
		return st, false
	}

	if !cached {
		marked, ok, err := o.cfg.FreezeWriter.LoadState(ctx, pair.Base, pair.Quote)
		switch {
		case err != nil:
			// Transient Redis failure on a cold key. Start clean rather
			// than fail the tick; the pair re-freezes on its own signal
			// if the anomaly is still live.
			o.logger.Debug("freeze lifecycle: marker read failed on cold key",
				"pair", pair.String(), "err", err)
		case ok && marked.Active():
			st = marked
			o.logger.Info("freeze lifecycle rehydrated from marker",
				"pair", pair.String(),
				"fired_at", marked.FiredAt,
				"hold_until", marked.HoldUntil,
				"extensions_used", marked.ExtensionsUsed,
				"escalated", marked.Escalated)
		}
		o.freezeStates[stateKey] = st
		return st, false
	}

	if st.Active() {
		if _, ok, err := o.cfg.FreezeWriter.LoadState(ctx, pair.Base, pair.Quote); err == nil && !ok {
			return st, true
		}
	}
	return st, false
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
	prevVWAP *big.Rat,
	out freeze.Outcome,
) {
	o.freezeStates[stateKey] = out.State

	o.mu.Lock()
	o.freezesEngaged++
	o.mu.Unlock()

	// Per FROZEN TICK, not per fire transition. The pre-existing
	// anomaly.yml rules read a sustained freeze as a sustained rate()
	// on this counter ("a sustained freeze keeps the counter
	// incrementing"), and a lifecycle hold is precisely a sustained
	// freeze — incrementing only on the fire would silently disarm
	// stellarindex_anomaly_freeze_sustained, a severity:page rule.
	obs.AnomalyFreezeEngagedTotal.WithLabelValues(string(decision.Class)).Inc()

	o.logFreezeTransition(pair, window, decision, out)

	// MNY-22: this pair's LKG stays in cache for the rest of the tick;
	// record the refusal so triangulation can't republish it as a
	// derived price.
	o.markFrozenThisTick(pair, window)

	// F-1345 (G13-03): a freeze skips the VWAP cache write, so the
	// prior bucket's value must outlive the marker. Refresh regardless
	// of whether a FreezeWriter is wired — the LKG keeps serving
	// either way.
	o.keepFrozenVWAPAlive(ctx, pair, window, out.MarkerTTL)

	if o.cfg.FreezeWriter == nil {
		return
	}
	// LKG VWAP we're freezing on: the prior bucket's value (which
	// stays in cache because the caller skips the cache write).
	// Empty string when no prior bucket exists (first-tick freeze).
	var frozenValue string
	if prevVWAP != nil {
		frozenValue = formatRatFixed(prevVWAP, 12)
	}
	if err := o.cfg.FreezeWriter.MarkHold(ctx, pair.Base, pair.Quote,
		frozenValue, decision, out.State, out.MarkerTTL); err != nil {
		o.logger.Warn("freeze marker write failed",
			"pair", pair.String(), "window", window, "err", err)
		// Soft-fail: the anomaly was detected, the marker write
		// failed, the API won't see flags.frozen. Operators alert on
		// AnomalyFreezeEngagedTotal vs the API-side flag rate; a
		// sustained gap = the writer is broken. Don't fail the tick.
	}
}

// logFreezeTransition emits the per-transition operator signal and
// the extension / escalation counters.
//
// Escalation logs at ERROR: it is the one transition that will not
// resolve itself. ADR-0019 holds an escalated freeze "until manual
// unfreeze", so the pair's /v1/price is pinned to a last-known-good
// price until a human acts.
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
			"reason", decision.Reason)
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

// releaseFreeze ends ONE window's freeze: drop that window's ladder,
// count the release by mode, and — once no sibling window for the pair
// is still frozen — delete the shared marker so `flags.frozen` clears
// immediately rather than after the remaining hold's TTL.
//
// The state entry is kept present-but-inactive rather than deleted,
// so [Orchestrator.loadFreezeState]'s hydrate-on-cold-key path stays
// a once-per-process cost.
func (o *Orchestrator) releaseFreeze(
	ctx context.Context,
	pair canonical.Pair,
	window time.Duration,
	stateKey string,
	prev freeze.State,
	transition freeze.Transition,
) {
	o.freezeStates[stateKey] = freeze.State{}
	if !prev.Active() {
		return // nothing was frozen; nothing to release
	}

	mode := "auto"
	if transition == freeze.TransitionOverridden {
		mode = "operator"
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

	// W3-freeze-1: the freeze marker AND the durable ladder are keyed by
	// (asset, quote), but a pair's lifecycle runs independently per
	// window ([DefaultWindows] is 5m/1h/24h). That shared marker's
	// presence is the single flag the API serves as `flags.frozen` for
	// the WHOLE pair, so it must stay set while ANY window is still
	// frozen. Clearing it when a short window auto-releases mid-hold on a
	// sibling deletes the marker out from under the still-frozen window;
	// the next tick reads the absent marker as an operator override
	// (loadFreezeState) and republishes that window's still-anomalous
	// VWAP with flags.frozen=false — defeating the freeze in exactly the
	// thin/single-source case it exists for, and it does not self-heal.
	// So retire the shared marker + durable ladder only when the LAST
	// frozen window for the pair releases.
	//
	// A genuine operator override is unaffected: `freeze-unfreeze`
	// deletes the marker (and stamps recovered_at) out of band, and each
	// window observes that independently in loadFreezeState — so every
	// window releases regardless of which one's releaseFreeze reaches the
	// Clear below.
	if o.siblingWindowFrozen(pair, window) {
		return
	}

	// An operator override already deleted the marker; clearing again
	// is a harmless idempotent DEL and keeps the two paths identical.
	if err := o.cfg.FreezeWriter.Clear(ctx, pair.Base, pair.Quote); err != nil {
		o.logger.Warn("freeze marker clear failed — flags.frozen stays set until its TTL",
			"pair", pair.String(), "err", err)
	}
}

// siblingWindowFrozen reports whether any window OTHER than `window`
// still holds a live freeze for `pair`. It is what keeps the shared
// (asset, quote) freeze marker + durable ladder alive until the last
// window releases (W3-freeze-1).
//
// Reads o.freezeStates on the single-Tick goroutine — the same
// no-lock invariant loadFreezeState relies on. Reconstructs each
// sibling's stateKey with the exact shape refreshPairWindow uses
// (`pair:window`) rather than prefix-matching map keys, so an asset id
// that itself contains the ':' separator (e.g. `fiat:USD`) can't
// produce a false match.
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
