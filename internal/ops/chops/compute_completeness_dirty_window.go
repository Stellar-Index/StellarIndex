package chops

import (
	"github.com/Stellar-Index/StellarIndex/internal/completeness"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// dirtyReconcileFloor lowers an incremental run's projection reconcile floor
// to a pending dirty window's genesis-clamped bottom (migration 0125),
// regardless of -from. projectionClaim rule 3 carries a prior clean verdict
// over the skipped prefix on the premise that served rows there are
// immutable; a rewrite breaks that premise, so the range must be re-earned.
func dirtyReconcileFloor(projFrom, genesis uint32, w timescale.ProjectionDirtyWindow) uint32 {
	lo := w.From
	if lo < genesis {
		lo = genesis // nothing exists below the source's genesis to re-verify
	}
	if lo < projFrom {
		return lo
	}
	return projFrom
}

// passDirtySpanLedgers is the widest re-check a -pass takes on for a source
// whose re-proof outlasts the pass: one day of ledgers at the 5 s close, the
// range the nightly pass already reconciles per source from its prior
// watermark inside -source-timeout (45 min default, computeCompleteness).
const passDirtySpanLedgers uint32 = 17_280

// projectionPlan is src's projection reconcile floor, and whether this run
// defers src's pending dirty window instead of reconciling over it. Only a
// -pass defers, only for a source whose re-proof outlasts the pass (sdex,
// sep41), and only when the re-check cannot fit: the reconcile runs from the
// window's bottom to hi (this run's reconcile top), so that span, not the
// window's own width, is the cost. Span, not Reason, decides because a widened
// window keeps only its last writer's Reason. A deferred window is re-proved
// by the source's dedicated timer. The deferral ignores the prior verdict, so
// the false a deferred pass publishes cannot pull the next pass into a
// from-genesis re-proof; the span only grows with hi, so it stays deferred.
func projectionPlan(src reconSource, pass bool, prior priorProjection, priorWatermark uint32, fromLedger uint, hi uint32, win timescale.ProjectionDirtyWindow, hasDirty bool) (projFrom uint32, deferDirty bool) {
	projFrom = sourceProjectionFloor(src, pass, prior, priorWatermark, fromLedger)
	if !hasDirty {
		return projFrom, false
	}
	if pass && src.outlastsPass() && hi > max(win.From, src.genesis)+passDirtySpanLedgers {
		return projFrom, true
	}
	return dirtyReconcileFloor(projFrom, src.genesis, win), false
}

// servedAxisVerdict is a CH-path source's published watermark. A deferred
// dirty window caps it below the window whatever the reconcile said, because
// nothing re-verified the rewritten rows; otherwise complete needs both the
// lake axis and this run's projection claim.
func servedAxisVerdict(srW completeness.Watermark, servedOK, deferDirty bool, win timescale.ProjectionDirtyWindow) completeness.Watermark {
	if deferDirty {
		return deferredDirtyWatermark(srW, win)
	}
	return combineWatermark(srW, servedOK)
}

// deferredDirtyWatermark is the verdict for a source whose dirty window this
// run deferred: nothing re-verified the rewritten rows, so the served axis is
// not complete and the watermark stops below the window's genesis-clamped
// bottom. FirstProblem stays the lake's: the window is pending, not a found
// failure.
func deferredDirtyWatermark(srW completeness.Watermark, win timescale.ProjectionDirtyWindow) completeness.Watermark {
	out := srW
	out.Complete = false
	capped := completeness.ComputeWatermark(srW.Genesis, srW.Tip, []uint32{max(win.From, srW.Genesis)})
	if capped.Ledger < srW.Ledger {
		out.Ledger, out.CoveragePct = capped.Ledger, capped.CoveragePct
	}
	return out
}

// dirtyWindowSatisfied reports whether THIS run earned the right to clear a
// pending replay-rewind window: its projection verdict is CLEAN (projOK —
// which per projectionClaim requires this run's own reconcile to have found
// nothing, never a carried claim over an unchecked range) AND the run's
// reconcile floor reached the window's genesis-clamped bottom AND the
// reconciled range reached the window's top. Anything less keeps the window
// pending — a failing verdict must not erase the obligation, and a run whose
// scope stopped short of the window proved nothing about it. Pure —
// unit-testable.
func dirtyWindowSatisfied(w timescale.ProjectionDirtyWindow, projOK bool, reconcileFloor, genesis, hi uint32) bool {
	lo := w.From
	if lo < genesis {
		lo = genesis
	}
	return projOK && reconcileFloor <= lo && hi >= w.To
}
