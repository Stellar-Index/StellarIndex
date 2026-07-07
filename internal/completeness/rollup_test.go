package completeness

import (
	"math/big"
	"testing"
)

func bi(s string) *big.Int {
	v, ok := new(big.Int).SetString(s, 10)
	if !ok {
		panic("bad bigint literal: " + s)
	}
	return v
}

func rt(mint, burn, clawback string) RunningTotals {
	return RunningTotals{Mint: bi(mint), Burn: bi(burn), Clawback: bi(clawback)}
}

func TestReconcileRunningTotals_ExactMatch(t *testing.T) {
	cp := map[string]RunningTotals{
		"C1": rt("690000000000000", "1", "0"),
		"C2": rt("34200000000000000000", "13767314110027", "97691830539"),
	}
	// truth identical → no drift.
	if got := ReconcileRunningTotals(cp, cp, nil); got != nil {
		t.Fatalf("expected no drift on exact match, got %+v", got)
	}
}

func TestReconcileRunningTotals_KaleDoubleFold(t *testing.T) {
	// The incident: a re-derive re-folded history below the checkpoint,
	// doubling the served total. Checkpoint = 2×Truth ⇒ Delta = +Truth.
	truth := map[string]RunningTotals{"KALE": rt("34200000000000000000", "5", "0")}
	cp := map[string]RunningTotals{"KALE": rt("68400000000000000000", "10", "0")}

	got := ReconcileRunningTotals(cp, truth, nil)
	if len(got) != 2 {
		t.Fatalf("expected 2 drifts (mint+burn), got %d: %+v", len(got), got)
	}
	// Deterministic order: kinds appear mint before burn.
	if got[0].Kind != "mint" || got[1].Kind != "burn" {
		t.Fatalf("expected mint then burn, got %s then %s", got[0].Kind, got[1].Kind)
	}
	if got[0].Delta.Cmp(bi("34200000000000000000")) != 0 {
		t.Errorf("mint Delta = %s, want +truth 34200000000000000000 (2× signature)", got[0].Delta)
	}
	if got[0].Checkpoint.Cmp(bi("68400000000000000000")) != 0 || got[0].Truth.Cmp(bi("34200000000000000000")) != 0 {
		t.Errorf("mint drift carries wrong checkpoint/truth: %+v", got[0])
	}
	if got[1].Delta.Sign() <= 0 {
		t.Errorf("burn Delta should be a positive over-count, got %s", got[1].Delta)
	}
}

func TestReconcileRunningTotals_Undercount(t *testing.T) {
	// A below-checkpoint edit the incremental watermark never re-summed:
	// checkpoint < truth ⇒ Delta < 0.
	truth := map[string]RunningTotals{"C1": rt("100", "0", "0")}
	cp := map[string]RunningTotals{"C1": rt("60", "0", "0")}
	got := ReconcileRunningTotals(cp, truth, nil)
	if len(got) != 1 || got[0].Kind != "mint" {
		t.Fatalf("expected one mint drift, got %+v", got)
	}
	if got[0].Delta.Cmp(big.NewInt(-40)) != 0 {
		t.Errorf("Delta = %s, want -40 (undercount)", got[0].Delta)
	}
}

func TestReconcileRunningTotals_MissingContractEitherSide(t *testing.T) {
	truth := map[string]RunningTotals{"ONLY_TRUTH": rt("5", "0", "0")}
	cp := map[string]RunningTotals{"ONLY_CP": rt("0", "7", "0")}
	got := ReconcileRunningTotals(cp, truth, nil)
	if len(got) != 2 {
		t.Fatalf("expected 2 drifts (one per orphaned contract), got %+v", got)
	}
	// Sorted by contract id: ONLY_CP < ONLY_TRUTH.
	if got[0].ContractID != "ONLY_CP" || got[0].Kind != "burn" || got[0].Delta.Cmp(big.NewInt(7)) != 0 {
		t.Errorf("ONLY_CP burn drift wrong: %+v", got[0])
	}
	if got[1].ContractID != "ONLY_TRUTH" || got[1].Kind != "mint" || got[1].Delta.Cmp(big.NewInt(-5)) != 0 {
		t.Errorf("ONLY_TRUTH mint drift wrong: %+v", got[1])
	}
}

func TestReconcileRunningTotals_ToleranceBoundary(t *testing.T) {
	truth := map[string]RunningTotals{"C1": rt("1000", "0", "0")}
	cpAtTol := map[string]RunningTotals{"C1": rt("1005", "0", "0")}   // drift 5 == tol
	cpOverTol := map[string]RunningTotals{"C1": rt("1006", "0", "0")} // drift 6 > tol

	tol := big.NewInt(5)
	if got := ReconcileRunningTotals(cpAtTol, truth, tol); got != nil {
		t.Errorf("drift == tolerance must NOT be reported, got %+v", got)
	}
	got := ReconcileRunningTotals(cpOverTol, truth, tol)
	if len(got) != 1 || got[0].Delta.Cmp(big.NewInt(6)) != 0 {
		t.Errorf("drift > tolerance must be reported with Delta 6, got %+v", got)
	}
}

func TestReconcileRunningTotals_NilFieldsAreZero(t *testing.T) {
	// A checkpoint row with nil clawback vs a truth row with an actual
	// clawback: nil reads as zero, so this is a real drift.
	cp := map[string]RunningTotals{"C1": {Mint: bi("100")}} // burn, clawback nil
	truth := map[string]RunningTotals{"C1": rt("100", "0", "42")}
	got := ReconcileRunningTotals(cp, truth, nil)
	if len(got) != 1 || got[0].Kind != "clawback" || got[0].Delta.Cmp(big.NewInt(-42)) != 0 {
		t.Fatalf("expected one clawback drift -42, got %+v", got)
	}

	// Both sides nil/zero for every kind → no drift, no panic.
	if got := ReconcileRunningTotals(
		map[string]RunningTotals{"C1": {}},
		map[string]RunningTotals{"C1": {}},
		nil,
	); got != nil {
		t.Fatalf("all-nil totals should reconcile clean, got %+v", got)
	}
}

// TestReconcileRunningTotals_DoesNotMutateInputs guards the pure-function
// contract — callers reuse the maps across ticks.
func TestReconcileRunningTotals_DoesNotMutateInputs(t *testing.T) {
	truth := map[string]RunningTotals{"C1": rt("100", "0", "0")}
	cp := map[string]RunningTotals{"C1": rt("200", "0", "0")}
	_ = ReconcileRunningTotals(cp, truth, nil)
	if cp["C1"].Mint.Cmp(big.NewInt(200)) != 0 || truth["C1"].Mint.Cmp(big.NewInt(100)) != 0 {
		t.Fatal("ReconcileRunningTotals mutated its inputs")
	}
}
