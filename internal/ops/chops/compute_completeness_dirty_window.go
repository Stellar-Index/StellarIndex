package chops

import (
	"github.com/Stellar-Index/StellarIndex/internal/completeness"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// dirtyReconcileFloor lowers the reconcile floor to a pending dirty window's genesis-clamped
// bottom (migration 0125), regardless of -from. projectionClaim rule 3 assumes served rows are
// immutable; a rewrite breaks that, so the range must be re-proved.
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

// projectionPlan returns src's reconcile floor and whether this run defers src's pending dirty
// window. Only a -pass defers, only for sources whose re-proof outlasts the pass (sdex, sep41),
// and only when the span from the window's bottom to hi cannot fit. Span decides, not Reason,
// because a widened window keeps only its last writer's Reason. The deferral ignores the prior
// verdict so a deferred pass's false cannot drag the next pass into a from-genesis re-proof.
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

// deferredDirtyWatermark is the verdict for a deferred dirty window: the rewritten rows were
// not re-verified, so the watermark stops below the window's bottom. FirstProblem stays the
// lake's because the window is pending, not a found failure.
func deferredDirtyWatermark(srW completeness.Watermark, win timescale.ProjectionDirtyWindow) completeness.Watermark {
	out := srW
	out.Complete = false
	capped := completeness.ComputeWatermark(srW.Genesis, srW.Tip, []uint32{max(win.From, srW.Genesis)})
	if capped.Ledger < srW.Ledger {
		out.Ledger, out.CoveragePct = capped.Ledger, capped.CoveragePct
	}
	return out
}

// dirtyWindowSatisfied reports whether this run earned clearing a pending replay-rewind window:
// projOK is clean from its own reconcile (never a carried claim), and the reconciled range
// covers the window's genesis-clamped bottom through its top. Anything less keeps it pending,
// since a failing or short-scoped run proved nothing about the window.
func dirtyWindowSatisfied(w timescale.ProjectionDirtyWindow, projOK bool, reconcileFloor, genesis, hi uint32) bool {
	lo := w.From
	if lo < genesis {
		lo = genesis
	}
	return projOK && reconcileFloor <= lo && hi >= w.To
}
