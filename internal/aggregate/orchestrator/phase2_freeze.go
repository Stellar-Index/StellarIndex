package orchestrator

import (
	"context"
	"fmt"

	"github.com/RatesEngine/rates-engine/internal/aggregate/anomaly"
	"github.com/RatesEngine/rates-engine/internal/canonical"
	"github.com/RatesEngine/rates-engine/internal/obs"
)

// Phase 2 freeze thresholds per ADR-0019 §"Freeze policy". All
// three signals must agree to fire — keeps the freeze decision
// robust against legitimate market events (those have multi-source
// corroboration even if z is high) while still catching the
// USTRY-shape attack pattern (single source, large deviation,
// confidence-killing combination of factors).
//
// Hardcoded for the MVP. Operator-tunable via TOML lands as a
// follow-up; the current values match the ADR's documented
// stop-gap.
const (
	phase2ConfidenceMaxFreeze  = 0.10 // freeze when confidence < this
	phase2ZScoreMinFreeze      = 5.0  // freeze when z > this
	phase2SourceCountMaxFreeze = 1    // freeze when source_count <= this
)

// phase2FreezeFires reports whether the Phase 2 freeze condition
// (3-signal AND) holds for a bucket given its confidence score,
// raw z-score, and contributing source count.
//
// Per ADR-0019 §"Freeze policy":
//
//	freeze_condition = (
//	  confidence  < 0.10
//	  AND z_score > 5.0
//	  AND source_count <= 1
//	)
//
// All three must be true. A two-of-three pattern (e.g. anomalous
// z + low confidence but multi-source) does NOT freeze — those
// scenarios surface via flags.divergence_warning instead, set by
// the API's read-side divergence check.
func phase2FreezeFires(c confidenceWithSourceCount) bool {
	return c.Confidence < phase2ConfidenceMaxFreeze &&
		c.ZScore > phase2ZScoreMinFreeze &&
		c.SourceCount <= phase2SourceCountMaxFreeze
}

// confidenceWithSourceCount is the Phase 2 input bundle. Pulled
// out of [confidenceComputation] so [phase2FreezeFires] is a pure
// function on three floats — easy to unit-test exhaustively.
type confidenceWithSourceCount struct {
	Confidence  float64
	ZScore      float64
	SourceCount int
}

// markPhase2Freeze records a Phase 2 freeze decision via the
// configured FreezeWriter (when wired) and emits the orchestrator's
// freeze-engaged Prometheus counter. Reuses the [anomaly.Decision]
// shape so downstream readers (the API's freeze-flag lookup) don't
// need to distinguish Phase 1 vs Phase 2 — both look identical on
// the wire.
//
// The [anomaly.Decision] carries Reason="phase2:3_signal_AND" so
// log lines + Redis marker JSON make the source legible without
// adding a new wire field.
func (o *Orchestrator) markPhase2Freeze(
	ctx context.Context,
	pair canonical.Pair,
	c confidenceWithSourceCount,
) {
	o.mu.Lock()
	o.freezesEngaged++
	o.mu.Unlock()

	// Class label uses the same Phase 1 checker's classifier when
	// it's wired (so the per-class metric stays consistent across
	// both phases). When Phase 1 isn't configured, default class.
	class := anomaly.ClassDefault
	if o.cfg.Anomaly != nil {
		class = o.cfg.Anomaly.ClassOf(pair.Base)
	}
	obs.AnomalyFreezeEngagedTotal.WithLabelValues(string(class)).Inc()

	if o.cfg.FreezeWriter == nil {
		return
	}
	decision := anomaly.Decision{
		Action: anomaly.ActionFreeze,
		Class:  class,
		// Phase 2 doesn't compute a raw class deviation — the per-
		// asset baseline replaces the per-class threshold. Keep
		// DeviationPct zero; the Reason field carries the source.
		Reason: fmt.Sprintf("phase2:3_signal_AND confidence=%.3f z=%.2f sources=%d",
			c.Confidence, c.ZScore, c.SourceCount),
	}
	if err := o.cfg.FreezeWriter.Mark(ctx, pair.Base, pair.Quote, decision); err != nil {
		o.logger.Warn("phase2 freeze marker write failed",
			"pair", pair.String(), "err", err)
	}
}
