package completeness

import (
	"math/big"
	"sort"
)

// This file adds the fourth ADR-0033 integrity check: the DERIVED-CHECKPOINT
// reconcile. The substrate / recognition / projection reconciles prove raw rows
// are captured and became table rows; this one proves that an
// *incrementally-maintained running total* folded from those rows still equals
// the authoritative re-sum of them.
//
// `sep41_supply_rollup` (migration 0085) holds a per-contract mint/burn/clawback
// running total advanced by a watermark worker (AdvanceSEP41SupplyRollup: sum
// only `ledger > last_ledger`). A history re-derive that rewrote raw
// `sep41_supply_events` BELOW an existing checkpoint, without resetting it, made
// the worker re-fold counted history (served supply exactly 2×). The row-count
// reconciles (reconcile.go) could not catch it: the raw rows were correct.
//
// The invariant: for every checkpointed contract, checkpoint.total == Σ(raw
// rows this checkpoint folds). A double-fold shows up as checkpoint = k×truth;
// a dropped-below-checkpoint edit as checkpoint != truth. Zero tolerance
// catches both exactly.

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

// ReconcileRunningTotals is the DERIVED-CHECKPOINT reconcile: it diffs a served
// incremental checkpoint (`checkpoint`, e.g. sep41_supply_rollup's per-contract
// totals) against the AUTHORITATIVE re-sum of the exact rows it folds (`truth`),
// up to the checkpoint's last_ledger. It returns every (contract, kind) whose
// values differ by strictly more than tolerance (abs), sorted by (contract, kind).
//
// TRUTH SOURCE. `truth` MUST be the same-source re-sum of the rows the
// checkpoint folds, NOT the network-wide ClickHouse `supply_flows` lake. The PG
// SEP-41 observer is watched-set-gated and bare-i128-only; the CH lake is
// network-wide and map-variant-aware, so their totals legitimately differ
// (migration 0085) and comparing to the lake would false-positive on every
// map-variant token. The projection reconcile (reconcile.go) separately proves
// `sep41_supply_events` faithful to the lake, so checkpoint == PG re-sum ⇒
// checkpoint == lake truth.
//
// tolerance nil is treated as exact (zero): the rollup sums the same integer
// amounts the re-sum does. A caller MAY pass a small tolerance to absorb an
// in-flight advance racing the re-sum snapshot. Pure, no IO.
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
