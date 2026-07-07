package completeness

import (
	"math/big"
	"sort"
)

// This file adds the fourth ADR-0033 integrity check, complementing the
// substrate / recognition / projection reconciles: the DERIVED-CHECKPOINT
// reconcile. The first three prove the raw rows are captured and became
// table rows; this one proves that an *incrementally-maintained running
// total* folded from those rows still equals the authoritative re-sum of
// them.
//
// Why it exists (incident 2026-07-06). `sep41_supply_rollup` (migration
// 0085) holds a per-contract mint/burn/clawback running total advanced
// INCREMENTALLY by a watermark worker (AdvanceSEP41SupplyRollup: sum only
// `ledger > last_ledger`, add in). A full-history re-derive rewrote a
// contract's raw `sep41_supply_events` BELOW an existing checkpoint
// without the mandated `TRUNCATE sep41_supply_rollup`, so the worker
// re-folded already-counted history and the served supply came out
// exactly 2×. The row-count reconciles (reconcile.go) could NOT catch it:
// the raw rows were correct — only the derived checkpoint was doubled.
//
// The guard here is the missing invariant, made auditable: for every
// checkpointed contract, checkpoint.total == Σ(raw rows this checkpoint
// folds). A double-fold shows up as checkpoint = k×truth (Delta > 0); a
// dropped-below-checkpoint edit the worker never re-summed shows up as
// checkpoint > truth or < truth. Zero tolerance catches both exactly.

// RunningTotals is a per-kind i128-safe supply total (mint / burn /
// clawback) a SEP-41 contract's incremental checkpoint carries. Each
// field is *big.Int per ADR-0003 (Σmint alone can exceed i128, so a
// fixed-width int would truncate); a nil field reads as zero.
type RunningTotals struct {
	Mint     *big.Int
	Burn     *big.Int
	Clawback *big.Int
}

// TotalsDrift is one (contract, kind) where a served incremental
// checkpoint disagrees with the authoritative re-sum of the same rows by
// more than tolerance. Delta = Checkpoint − Truth: a positive Delta is an
// OVER-count (the KALE double-fold: Checkpoint = 2×Truth ⇒ Delta =
// +Truth); a negative Delta is an UNDER-count (a below-checkpoint edit
// the incremental watermark never re-summed). Either means the rollup
// must be TRUNCATEd and re-folded from zero.
type TotalsDrift struct {
	ContractID string   // SEP-41 contract C-strkey
	Kind       string   // "mint" | "burn" | "clawback"
	Checkpoint *big.Int // value the served rollup carries
	Truth      *big.Int // value the authoritative re-sum carries
	Delta      *big.Int // Checkpoint − Truth (>0 over-count, <0 under-count)
}

// ReconcileRunningTotals is the DERIVED-CHECKPOINT reconcile: it diffs a
// served incremental checkpoint (`checkpoint`, e.g. sep41_supply_rollup's
// stored per-contract totals) against the AUTHORITATIVE re-sum of the
// exact rows that checkpoint folds (`truth`, e.g. Σ sep41_supply_events
// .amount FILTER (event_kind=…) up to the checkpoint's last_ledger). It
// returns every (contract, kind) whose values differ by strictly more
// than tolerance (abs), sorted by (contract, kind) so the output is
// deterministic and diffable across runs.
//
// TRUTH SOURCE — read this before wiring a caller. `truth` MUST be the
// same-source re-sum of the rows the checkpoint folds, NOT the
// network-wide ClickHouse `supply_flows` lake. The PG SEP-41 observer is
// watched-set-gated and bare-i128-only; the CH lake is network-wide and
// map-variant-aware, so their per-contract totals legitimately differ
// (migration 0085 spells this out). Comparing the checkpoint straight to
// the lake would false-positive on every map-variant token. The
// composition that IS sound: the projection reconcile (reconcile.go)
// separately proves `sep41_supply_events` faithful to the lake row-for-
// row, so checkpoint == PG re-sum ⇒ checkpoint == lake truth transitively
// — without importing the lake's methodology into this check.
//
// tolerance nil is treated as exact (zero) — the expected posture, since
// the rollup sums the same integer amounts the re-sum does and any
// nonzero difference is a real fold error, not rounding. A caller MAY
// pass a small nonzero tolerance to absorb an in-flight advance racing
// the re-sum snapshot.
//
// Pure and deterministic (like ComputeWatermark / ReconcileCounts): same
// inputs → same drifts, so the check is re-runnable and auditable. It
// does no IO; a caller fetches `checkpoint` and `truth` and hands them in.
func ReconcileRunningTotals(checkpoint, truth map[string]RunningTotals, tolerance *big.Int) []TotalsDrift {
	tol := tolerance
	if tol == nil {
		tol = big.NewInt(0)
	}

	// Union of contract IDs from both sides — a contract present on only
	// one side is a drift against zero (phantom checkpoint, or a folded
	// contract the re-sum found nothing for).
	seen := make(map[string]struct{}, len(checkpoint)+len(truth))
	ids := make([]string, 0, len(checkpoint)+len(truth))
	for id := range checkpoint {
		if _, ok := seen[id]; !ok {
			seen[id] = struct{}{}
			ids = append(ids, id)
		}
	}
	for id := range truth {
		if _, ok := seen[id]; !ok {
			seen[id] = struct{}{}
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)

	var drifts []TotalsDrift
	for _, id := range ids {
		cp := checkpoint[id]
		tr := truth[id]
		// Kinds in a fixed order so per-contract output is stable.
		drifts = appendKindDrift(drifts, id, "mint", cp.Mint, tr.Mint, tol)
		drifts = appendKindDrift(drifts, id, "burn", cp.Burn, tr.Burn, tol)
		drifts = appendKindDrift(drifts, id, "clawback", cp.Clawback, tr.Clawback, tol)
	}
	return drifts
}

// appendKindDrift appends a TotalsDrift for one (contract, kind) iff
// |checkpoint − truth| > tolerance. nil operands read as zero.
func appendKindDrift(drifts []TotalsDrift, contractID, kind string, cp, tr, tol *big.Int) []TotalsDrift {
	c := orZero(cp)
	t := orZero(tr)
	delta := new(big.Int).Sub(c, t)
	if new(big.Int).Abs(delta).Cmp(tol) <= 0 {
		return drifts // within tolerance — not a drift
	}
	return append(drifts, TotalsDrift{
		ContractID: contractID,
		Kind:       kind,
		Checkpoint: new(big.Int).Set(c),
		Truth:      new(big.Int).Set(t),
		Delta:      delta,
	})
}

// orZero returns v, or a fresh zero when v is nil — a nil per-kind total
// (missing contract / kind never observed) reconciles as zero.
func orZero(v *big.Int) *big.Int {
	if v == nil {
		return big.NewInt(0)
	}
	return v
}
