package chops

import (
	"github.com/Stellar-Index/StellarIndex/internal/completeness"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// dirtyReconcileFloor lowers the floor to a pending dirty window's genesis-clamped bottom (migration 0125) regardless of -from:
// projectionClaim rule 3 assumes served rows are immutable, a rewrite breaks that.
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

// passDirtySpanLedgers is the widest re-check a -pass takes on for a source whose re-proof outlasts it: one day of ledgers at the 5 s
// close, what the nightly pass reconciles per source inside -source-timeout (45 min).
const passDirtySpanLedgers uint32 = 17_280

// projectionPlan returns src's reconcile floor and whether this run defers its pending dirty window: only -pass, only sources whose re-
// proof outlasts it (sdex, sep41), only when the span to hi cannot fit. Span decides, not Reason (a widened window keeps its last
// writer's). Ignores the prior verdict so a deferred false cannot force a from-genesis re-proof.
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

// servedAxisVerdict is a CH-path source's published watermark: a deferred dirty window caps it below the window whatever the reconcile
// said (nothing re-verified the rewrite); otherwise complete needs the lake axis and this run's projection claim.
func servedAxisVerdict(srW completeness.Watermark, servedOK, deferDirty bool, win timescale.ProjectionDirtyWindow) completeness.Watermark {
	if deferDirty {
		return deferredDirtyWatermark(srW, win)
	}
	return combineWatermark(srW, servedOK)
}

// deferredDirtyWatermark stops the watermark below the window's bottom because the rewritten rows were not re-verified; FirstProblem stays
// the lake's (the window is pending, not a found failure).
func deferredDirtyWatermark(srW completeness.Watermark, win timescale.ProjectionDirtyWindow) completeness.Watermark {
	out := srW
	out.Complete = false
	capped := completeness.ComputeWatermark(srW.Genesis, srW.Tip, []uint32{max(win.From, srW.Genesis)})
	if capped.Ledger < srW.Ledger {
		out.Ledger, out.CoveragePct = capped.Ledger, capped.CoveragePct
	}
	return out
}

// dirtyWindowSatisfied reports whether this run earned clearing a replay-rewind window: projOK from its own reconcile (never a carried
// claim) over a range covering the window's genesis-clamped bottom through its top.
func dirtyWindowSatisfied(w timescale.ProjectionDirtyWindow, projOK bool, reconcileFloor, genesis, hi uint32) bool {
	lo := w.From
	if lo < genesis {
		lo = genesis
	}
	return projOK && reconcileFloor <= lo && hi >= w.To
}
