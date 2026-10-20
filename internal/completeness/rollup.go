package completeness

import (
	"math/big"
	"sort"
)

// This file adds the fourth ADR-0033 integrity check, the DERIVED-CHECKPOINT reconcile: an
// incrementally maintained total must still equal the authoritative re-sum of the rows it folds.
//
// `sep41_supply_rollup` advances by a watermark worker summing only `ledger > last_ledger`. A
// history re-derive rewriting raw rows BELOW an existing checkpoint, without resetting it, makes
// the worker re-fold history (served supply exactly 2x), which row-count reconciles cannot see.
//
// Invariant: checkpoint.total == the sum of the raw rows it folds, with zero tolerance, so a
// double-fold (k*truth) and a below-checkpoint edit are both caught.

// RunningTotals is the per-kind supply total a SEP-41 contract's incremental checkpoint carries; fields are *big.Int per ADR-0003 (a mint sum can exceed i128) and nil reads as zero.
type RunningTotals struct {
	Mint     *big.Int
	Burn     *big.Int
	Clawback *big.Int
}

// TotalsDrift is one (contract, kind) where a served checkpoint disagrees with the authoritative re-sum beyond tolerance; Delta = Checkpoint - Truth, positive an OVER-count and negative an UNDER-count, either meaning the rollup must be TRUNCATEd and re-folded.
type TotalsDrift struct {
	ContractID string   // SEP-41 contract C-strkey
	Kind       string   // "mint" | "burn" | "clawback"
	Checkpoint *big.Int // value the served rollup carries
	Truth      *big.Int // value the authoritative re-sum carries
	Delta      *big.Int // Checkpoint − Truth (>0 over-count, <0 under-count)
}

// ReconcileRunningTotals diffs a checkpoint against the authoritative re-sum (`truth`) and returns every (contract, kind) differing by strictly more than tolerance, sorted; TRUTH SOURCE: `truth` MUST be the same-source re-sum of the rows folded, NOT the network-wide ClickHouse `supply_flows` lake, which would false-positive on map-variant tokens.
func ReconcileRunningTotals(checkpoint, truth map[string]RunningTotals, tolerance *big.Int) []TotalsDrift {
	tol := tolerance
	if tol == nil {
		tol = big.NewInt(0)
	}

	// Union of both sides: a contract on only one side is a drift against zero.
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
