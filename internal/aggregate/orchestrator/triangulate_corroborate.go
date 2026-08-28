package orchestrator

import (
	"math"
	"math/big"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// Composite corroboration (2026-07-25).
//
// A triangulation chain does two things once it is configured: it
// publishes an implied price for its target pair, and — the part this
// file adds — it produces a SECOND, independently-routed opinion about
// a pair we also price directly. On the pairs chains are deployed for
// that second opinion is worth more than the first: XLM/EUR ran 87.5%
// single-source minutes and XLM/GBP 100% (measured 2026-07-25), while
// XLM/USD ran 0.0% — so a composite through USD reaches those pairs via
// markets that are not thin, not single-venue, and not the venue an
// attacker would pick.
//
// What it feeds and what it must not. The single-chain DIVERGENCE
// comparison feeds ONE confidence factor
// ([confidence.Inputs.TriangulationDivergencePct]). It deliberately
// does not touch [confidence.Inputs.SourceCount]: a single composite is
// not a second source — it re-uses our own leg VWAPs, our own
// outlier/class filters and usually our own upstream venues, so counting
// ONE derived path as a corroborating source would let it silently
// satisfy an independence test it does not meet. The confidence leg is
// graded and self-correcting; the source-count leg is a structural claim
// about independence, and one derived path is not it.
//
// The router's MULTI-ROUTE corroboration count is a distinct signal and
// is treated differently, deliberately (Step 2, 2026-08). When the
// graph router (internal/aggregate/router.go) reaches a target by ≥2
// SHORTEST routes that (a) each clear the min_route_confidence floor,
// (b) mutually AGREE within the TIGHT corroboration band
// (routerCorroborationAgreePct — much tighter than the 40% outlier band;
// merely surviving outlier omission is NOT agreement), (c) do not come
// from a diverged combine, and (d) are genuinely INDEPENDENT — the count
// is the maximum set of PAIRWISE EDGE-DISJOINT such routes, so a set that
// all funnels through one shared (potentially wash-traded) leg collapses
// to a single confirmation. That aggregate.CombineRoutes corroborationCount
// (NOT the raw survivor pathCount) is genuine multi-path independence:
// distinct hub assets, distinct intermediary markets, each edge already
// gated by its own quality signal, and any single manipulated leg either
// drops out on confidence, shows up as a divergent route the combiner
// rejects, or is revealed as the shared bottleneck it is. So a
// corroborationCount ≥ 2 DOES widen the freeze's `source_count` leg (via
// [Orchestrator.effectiveSourceCount]) — but ONLY the freeze leg, never
// [confidence.Inputs.SourceCount], and only as a MAX against the direct
// trade-source count, so corroborationCount ≤ 1 can never RAISE it: the
// invariant above still holds for one path. The signal is also
// staleness-bounded ([compositeMaxAgeTicks]): if the corroborating routes
// dry up, the widening ages out within two ticks and the freeze falls
// back to the direct source count, exactly like the divergence factor.
//
// Direct-market corroboration (D1, 2026-08-28). In the production graph
// every target has exactly ONE hub route (through USD), so the router
// alone could never count above 1 and the widening never executed —
// the freeze kept firing on structurally single-venue crosses
// (crypto:XLM/fiat:GBP: one venue) whose served price the deep
// XLM/USD × USD/GBP route reproduced to well within the tight band. The
// target's own direct market is therefore entered into the corroboration
// clique as one more EDGE-DISJOINT route (it is excluded from routing, so
// it can share no edge with any hub route): a confident hub route that
// tightly agrees with the direct print corroborates it → 2. This does not
// weaken the invariant above — the count is still "N independent,
// confidence-gated, tightly-agreeing routes", the direct print merely
// becomes one of the N — and a divergent composite (outside
// routerCorroborationAgreePct) confirms nothing, leaving the count at 1.
// See [aggregate.CombineRoutesWithDirect] and [Orchestrator.routeTarget].

// compositeSample is the most recent composite (triangulated) price the
// chain pass published for one target (pair, window), with the time it was
// published at and the router corroboration that backed it.
//
// `at` retains the process MONOTONIC clock reading (stamped with
// time.Now(), NOT time.Now().UTC() — see [Orchestrator.recordComposite]): the
// staleness checks below read it only through time.Since, which is immune to
// a backward wall-clock (NTP/VM) step so long as the monotonic reading
// survives (M1).
//
// Mirrors [Orchestrator.prevVWAPs]: bounded by len(Triangulations) ×
// len(Windows), read and written only from within a single Tick (which
// the ticker serialises), so it needs no lock of its own.
type compositeSample struct {
	price *big.Rat
	at    time.Time

	// corroborationCount is the number of INDEPENDENT, tightly-agreeing,
	// non-diverged router routes that backed this composite
	// (aggregate.CombineRoutes' corroborationCount return — the maximum
	// edge-disjoint subset of the routes that agree within the tight band,
	// 0 when diverged or when no two routes tightly agree). This is the
	// count [Orchestrator.effectiveSourceCount] reads (one tick behind,
	// freshness-bounded) into the freeze's source_count leg. 2 for a
	// single-hub-route target whose confident composite tightly agrees with
	// its own direct print (D1), ≤ 1 otherwise — so it never widens the
	// freeze on a target the composite does not confirm.
	corroborationCount int

	// combinedConfidence / diverged are the router's quality flags for
	// this composite, carried so downstream (Step 3: market-cap gating,
	// /v1/price flags) can respect them. A low-confidence composite is
	// NOT recorded here at all — it is never published over the direct
	// price — so a sample present in this map is always a CONFIDENT one.
	combinedConfidence float64
	diverged           bool
}

// compositeMaxAgeTicks bounds how old a recorded composite may be
// before the confidence step stops trusting it, expressed in ticks
// because that is the cadence the chain pass republishes at.
//
// Two ticks (60s at the default interval) tolerates a single missed
// chain — a leg whose window happened to be empty, an FX snap blip —
// without tolerating a chain that has actually stopped. That
// distinction is the whole point: a composite that stopped updating
// while the direct price kept moving would drift into apparent
// disagreement and quietly drag a healthy pair's confidence down, so
// staleness must read as "unchecked", never as "disagrees".
const compositeMaxAgeTicks = 2

// recordComposite stores the composite price a chain just published for
// (target, window), together with the router corroboration that backed
// it. Called from [Orchestrator.triangulateOne] only on a CONFIDENT
// publish (a low-confidence composite is never published over the direct
// price and never recorded, so a sample here always corroborates).
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
		// time.Now(), NOT .UTC(): the staleness readers ([routeCorroborationCount],
		// [triangulationDivergencePct]) compare this only via time.Since, so it
		// must RETAIN the monotonic clock reading. .UTC() strips it, dropping
		// those comparisons to wall-clock arithmetic where a backward NTP/VM
		// step could latch a stale, freeze-suppressing corroboration count (M1).
		at:                 time.Now(),
		corroborationCount: corroborationCount,
		combinedConfidence: combinedConfidence,
		diverged:           diverged,
	}
}

// routeCorroborationCount returns the number of INDEPENDENT, tightly-
// agreeing, non-diverged router routes that backed (pair, window)'s
// composite on the chain pass — the corroboration count
// [Orchestrator.effectiveSourceCount] folds into the freeze's
// source_count leg. This is aggregate.CombineRoutes' corroborationCount
// (edge-disjoint + tight-agreement + non-diverged), NOT the raw survivor
// pathCount: two routes that merely fall inside the loose outlier band,
// or that share a single manipulable bottleneck leg, do not widen the
// freeze here.
//
// Returns (_, false) — unchecked, fail-closed — under exactly the same
// conditions [triangulationDivergencePct] rejects a composite: no chain
// output recorded for this pair, or the last publish is older than
// [compositeMaxAgeTicks]. Staleness must read as "no corroboration"
// (fall back to the direct source count), never latch a freeze-
// suppressing count after the corroborating routes have gone. A count
// ≤ 1 is returned as-is (a single route corroborates nothing and, being
// MAX-ed against the direct count in effectiveSourceCount, cannot lower
// it either).
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

// effectiveSourceCount is the independence signal both freeze phases'
// `source_count` leg reads: the GREATER of the distinct contributing
// trade sources and the router's corroboration count for this
// (pair, window). Widening (never narrowing — it is a MAX) means a thin
// direct print that is nonetheless reproduced by ≥2 agreeing,
// confidence-gated router routes is no longer treated as single-source,
// so the freeze stops false-firing on it. When the router has no fresh
// corroboration (unchecked / stale / single-route), this is exactly the
// legacy distinctSourceCount(trades).
func (o *Orchestrator) effectiveSourceCount(pair canonical.Pair, window time.Duration, trades []canonical.Trade) int {
	n := distinctSourceCount(trades)
	if pc, ok := o.routeCorroborationCount(pair, window); ok && pc > n {
		n = pc
	}
	return n
}

// compositeKey is the [Orchestrator.lastComposites] key. Its own key
// space, deliberately not shared with prevVWAPs / frozenThisTick even
// though the shape matches — those maps answer different questions and
// a future change to one must not silently re-key the others.
func compositeKey(pair canonical.Pair, window time.Duration) string {
	return pair.String() + ":" + window.String()
}

// triangulationDivergencePct returns the % absolute deviation between
// the direct VWAP just computed for (pair, window) and the most recent
// composite the chain pass published for the same pair, plus whether a
// comparison happened at all — the
// [confidence.Inputs.TriangulationDivergencePct] /
// [confidence.Inputs.TriangulationChecked] pair.
//
// Returns (_, false) when there is no chain output to compare against:
// no chain configured for this pair, the chain has not published yet,
// its last publish is older than [compositeMaxAgeTicks], or either
// price is non-positive. Unchecked is the fail-closed answer here —
// [confidence.Compute] drops the factor's weight entirely in that
// case, so a pair with no usable composite scores exactly as it did
// before this input existed.
//
// The composite is one tick old by construction. The chain pass runs
// AFTER the per-pair refresh loop inside [Orchestrator.Tick] (it has
// to: its legs read the VWAPs that loop just wrote), so the freshest
// composite available while scoring this bucket is the previous tick's.
// That is the right trade — the alternative is re-deriving every chain
// mid-refresh, which would double the FX-snap query load and
// double-count [obs.AggregatorFXSnapFallbackTotal], the metric whose
// ratio drives the fx-snap-fallback alert. One tick of skew (30s
// default) is well inside this score's existing precedent: the
// cross-oracle input it sits beside is refreshed every 5 minutes
// (DivergenceMinInterval).
//
// Freeze interaction (MNY-22): a leg frozen this tick makes the chain
// refuse to publish, so no sample is recorded and the composite ages
// out into "unchecked" within two ticks. A frozen leg's last-known-good
// value therefore cannot reach the confidence score any more than it
// can reach the published price.
func (o *Orchestrator) triangulationDivergencePct(
	pair canonical.Pair,
	window time.Duration,
	direct *big.Rat,
) (float64, bool) {
	if len(o.lastComposites) == 0 || direct == nil || direct.Sign() <= 0 {
		return 0, false
	}
	sample, ok := o.lastComposites[compositeKey(pair, window)]
	if !ok || sample.price == nil {
		return 0, false
	}
	if time.Since(sample.at) > compositeMaxAgeTicks*o.tickInterval() {
		return 0, false
	}
	compositeF, _ := sample.price.Float64()
	directF, _ := direct.Float64()
	if compositeF <= 0 || directF <= 0 || math.IsInf(compositeF, 0) || math.IsInf(directF, 0) {
		return 0, false
	}
	// Same orientation as divergence.Compare's DivergencePct: deviation
	// measured AGAINST the reference, so the two corroboration factors
	// are fed comparable numbers. The composite is the reference here —
	// it is the route through the deep markets.
	return math.Abs(directF-compositeF) / compositeF * 100.0, true
}

// tickInterval returns the configured tick cadence, falling back to
// [DefaultInterval] for an Orchestrator built without one. New()
// already normalises Config.Interval, so the fallback only covers a
// zero-valued struct assembled directly in a test.
func (o *Orchestrator) tickInterval() time.Duration {
	if o.cfg.Interval > 0 {
		return o.cfg.Interval
	}
	return DefaultInterval
}
