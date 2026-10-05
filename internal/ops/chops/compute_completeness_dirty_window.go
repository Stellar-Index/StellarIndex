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

// projectionPlan is src's projection reconcile floor, and whether this run
// defers src's pending dirty window instead of reconciling over it. A -pass
// defers for a source whose re-proof outlasts the pass (sdex, sep41): a
// backfill-sized window would hit -source-timeout every night, so its
// dedicated timer re-proves it from genesis instead. The deferral ignores the
// prior verdict, so the false that a deferred pass publishes cannot pull the
// next pass into that same from-genesis re-proof.
func projectionPlan(src reconSource, pass bool, prior priorProjection, priorWatermark uint32, fromLedger uint, win timescale.ProjectionDirtyWindow, hasDirty bool) (projFrom uint32, deferDirty bool) {
	projFrom = sourceProjectionFloor(src, pass, prior, priorWatermark, fromLedger)
	if !hasDirty {
		return projFrom, false
	}
	if pass && src.outlastsPass() {
		return projFrom, true
	}
	return dirtyReconcileFloor(projFrom, src.genesis, win), false
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
