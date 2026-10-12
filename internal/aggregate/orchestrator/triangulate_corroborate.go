package orchestrator

import (
	"math"
	"math/big"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
)

// Composite corroboration: a triangulation chain gives a SECOND, independently-routed opinion about a directly priced pair, which matters on thin pairs (XLM/EUR ran 87.5% single-source minutes).
// The divergence comparison feeds ONE confidence factor ([confidence.Inputs.TriangulationDivergencePct]) and deliberately not [confidence.Inputs.SourceCount]: a composite re-uses our own leg VWAPs, so counting it would claim independence it lacks.
// The router's multi-route corroborationCount is held to the same rule (ADR-0019 amendment 2) and is audit-only; both freeze phases read [distinctSourceCount] alone. A composite stands down a single-venue freeze only via the SAME-bucket composite reference (composite_reference.go).

// compositeSample is the latest composite price the chain pass published for one target (pair, window), with publish time and router
// corroboration.
// `at` MUST keep the MONOTONIC clock reading (time.Now(), not .UTC()) so time.Since staleness survives a backward wall-clock step. Single-
// Tick access like [Orchestrator.prevVWAPs], so lock-free.
type compositeSample struct {
	price *big.Rat
	at    time.Time

	// corroborationCount is the number of INDEPENDENT, tightly-agreeing, non-diverged router routes behind this composite (CombineRoutes' maximum edge-disjoint agreeing subset; 0 when diverged). Audit only: never feeds a source count.
	corroborationCount int

	// combinedConfidence / diverged are the router's quality flags for this composite; a low-confidence composite is never recorded (never published over the direct price), so a sample here is always CONFIDENT.
	combinedConfidence float64
	diverged           bool
}

// compositeMaxAgeTicks bounds how old a recorded composite may be before the confidence step stops trusting it, in chain-pass ticks.
// Two ticks tolerate one missed chain without tolerating a stopped one: a stale composite would drift into apparent disagreement and drag a healthy pair's confidence down, so staleness must read as "unchecked", never "disagrees".
const compositeMaxAgeTicks = 2

// recordComposite stores the composite a chain just published for (target, window) with its router corroboration, only on a CONFIDENT publish from [Orchestrator.triangulateOne], so a sample here always corroborates.
func (o *Orchestrator) recordComposite(
	target canonical.Pair, window time.Duration, price *big.Rat,
	corroborationCount int, combinedConfidence float64, diverged bool,
) {
	if price == nil || price.Sign() <= 0 {
		return
	}
	if o.lastComposites == nil {
		o.lastComposites = make(map[string]compositeSample, len(o.cfg.Triangulations)*max(len(o.cfg.Windows), 1))
	}
	o.lastComposites[compositeKey(target, window)] = compositeSample{
		// Defensive copy: the caller's *big.Rat is the chain's own
		// working value and must not alias into next tick's comparison.
		price: new(big.Rat).Set(price),
		// time.Now(), NOT .UTC(): staleness readers compare via time.Since, so the monotonic clock reading must be retained; .UTC() strips it and a backward NTP/VM step could latch a stale composite as fresh.
		at:                 time.Now(),
		corroborationCount: corroborationCount,
		combinedConfidence: combinedConfidence,
		diverged:           diverged,
	}
	obs.AggregatorRouteCorroborationCount.WithLabelValues(target.String(), windowLabel(window)).
		Set(float64(corroborationCount))
}

// routeCorroborationCount returns the number of INDEPENDENT, tightly-agreeing, non-diverged router routes behind (pair, window)'s last composite, reported beside the venue count in the freeze reason and never folded into it.
// Returns (_, false) when [triangulationDivergencePct] would reject the composite: none recorded, or older than [compositeMaxAgeTicks].
func (o *Orchestrator) routeCorroborationCount(pair canonical.Pair, window time.Duration) (int, bool) {
	if len(o.lastComposites) == 0 {
		return 0, false
	}
	sample, ok := o.lastComposites[compositeKey(pair, window)]
	if !ok {
		return 0, false
	}
	if time.Since(sample.at) > compositeMaxAgeTicks*o.tickInterval() {
		return 0, false
	}
	return sample.corroborationCount, true
}

// compositeKey is the [Orchestrator.lastComposites] key, deliberately its own key space (not shared with prevVWAPs / frozenThisTick):
// those maps answer different questions and must not be silently re-keyed together.
func compositeKey(pair canonical.Pair, window time.Duration) string {
	return pair.String() + ":" + window.String()
}

// triangulationDivergencePct returns the % absolute deviation between the direct VWAP just computed and the latest composite, plus whether
// a comparison happened; (_, false) when nothing is comparable
// (no chain, none published, older than [compositeMaxAgeTicks], non-positive price), which is fail-closed ([confidence.Compute] drops the
// factor's weight). A RESOLVED composite-reference reading is returned instead so confidence, freeze and release read ONE sample.
func (o *Orchestrator) triangulationDivergencePct(
	pair canonical.Pair,
	window time.Duration,
	direct *big.Rat,
) (float64, bool) {
	if direct == nil || direct.Sign() <= 0 {
		return 0, false
	}
	if ref, ok := o.currentCompositeReference(pair, window); ok && ref.verdict != compositeVerdictUnavailable {
		return ref.divergencePct, true
	}
	if len(o.lastComposites) == 0 {
		return 0, false
	}
	sample, ok := o.lastComposites[compositeKey(pair, window)]
	if !ok || sample.price == nil {
		return 0, false
	}
	if time.Since(sample.at) > compositeMaxAgeTicks*o.tickInterval() {
		return 0, false
	}
	compositeF, _ := sample.price.Float64() // i128:ok prices for a percentage divergence compare, not served
	directF, _ := direct.Float64()          // i128:ok prices for a percentage divergence compare, not served
	if compositeF <= 0 || directF <= 0 || math.IsInf(compositeF, 0) || math.IsInf(directF, 0) {
		return 0, false
	}
	// Same orientation as divergence.Compare's DivergencePct: deviation
	// measured AGAINST the reference, so the two corroboration factors
	// are fed comparable numbers. The composite is the reference here —
	// it is the route through the deep markets.
	return math.Abs(directF-compositeF) / compositeF * 100.0, true
}

// tickInterval returns the configured tick cadence, falling back to [DefaultInterval] for a zero-valued struct built directly in a test
// (New() normalises Config.Interval).
func (o *Orchestrator) tickInterval() time.Duration {
	if o.cfg.Interval > 0 {
		return o.cfg.Interval
	}
	return DefaultInterval
}
