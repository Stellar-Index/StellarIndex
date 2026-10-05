package chops

import "github.com/Stellar-Index/StellarIndex/internal/storage/timescale"

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

// passDefersDirtyWindow reports whether a -pass leaves src's pending dirty
// window to src's dedicated timer (compute-completeness-sdex/-sep41, which
// re-prove from genesis and clear it) instead of lowering its floor now. A
// backfill-sized sdex window is millions of ledgers of census re-derive: it
// would hit -source-timeout every pass and fail the unit daily while the
// prior verdict stood regardless.
func passDefersDirtyWindow(src reconSource, pass bool) bool {
	return pass && src.outlastsPass()
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
