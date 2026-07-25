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
// What it feeds and what it must not. The comparison feeds ONE
// confidence factor ([confidence.Inputs.TriangulationDivergencePct]).
// It deliberately does not touch [confidence.Inputs.SourceCount], and
// therefore cannot touch the `source_count <= 1` leg of ADR-0019's
// 3-signal freeze AND. A composite is not a second source: it re-uses
// our own leg VWAPs, our own outlier/class filters and usually our own
// upstream venues, so counting it as corroborating SOURCE would let a
// derived path silently satisfy an independence test it does not meet —
// on exactly the thin pairs where the freeze's `source_count` leg is
// the only thing standing between a single manipulated venue and a
// published price. The confidence leg is graded and self-correcting;
// the source-count leg is a structural claim about independence. Only
// the first is ours to move.

// compositeSample is the most recent composite (triangulated) price the
// chain pass published for one target (pair, window), with the
// wall-clock time it was published at.
//
// Mirrors [Orchestrator.prevVWAPs]: bounded by len(Triangulations) ×
// len(Windows), read and written only from within a single Tick (which
// the ticker serialises), so it needs no lock of its own.
type compositeSample struct {
	price *big.Rat
	at    time.Time
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
// (target, window). Called from [Orchestrator.triangulateOne] only on a
// successful publish.
func (o *Orchestrator) recordComposite(target canonical.Pair, window time.Duration, price *big.Rat) {
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
		at:    time.Now().UTC(),
	}
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
