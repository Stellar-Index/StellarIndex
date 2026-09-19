package projector

import (
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/consumer"
	"github.com/Stellar-Index/StellarIndex/internal/sources/sorobanevents"
)

// TestCycle_PoisonRowWithNoSinkHealthProofHoldsUntilTheNoProgressBudget is the
// SECOND rail RLT-131 asks for, committed skipped by the first half of the fix
// and un-skipped by this one.
//
// The rail: the per-cycle cap ([PermanentSkipPerCycle]) bounds the RATE at
// which a global class-22/23 fault sheds rows, but not the FACT of it. The
// discriminator between "this row's values are bad" and "the sink rejects
// everything right now" is the same proof the quarantine arm already takes —
// `madeProgress` (some OTHER event of this cycle durably committed). Without
// it, [QuarantineAfterCyclesNoProgress] is the budget: hold ~1 hour of cycles,
// far longer than the lag / sink_retry alerts take to fire, and only then give
// up one row per cycle.
//
// Two existing cases pinned the superseded contract — a lone poison row sheds
// on cycle one with no health proof at all — and both were moved rather than
// exempted:
//
//   - cycle_wedge_test.go, TestCycle_ValidationErrorStillAdvancesAcrossCycles
//     now supplies the health proof it always meant to (a second row that
//     commits) and keeps asserting the anti-wedge property;
//   - trade_drop_outcome_test.go, TestCycle_DroppedTradeIsNotReportedOK now
//     asserts the hold on cycle one AND the eventual self-heal, so RLT-132's
//     "a drop must not wedge a sole-writer source" survives intact.
func TestCycle_PoisonRowWithNoSinkHealthProofHoldsUntilTheNoProgressBudget(t *testing.T) {
	const source = "rlt131-no-health-proof"
	// One row, and it is poison: nothing else commits, so this cycle has NO
	// evidence the sink is healthy. A bad migration and a genuinely bad row
	// look identical from here, and the safe reading is the pessimistic one.
	h := newWedgeHarness(t, source, []sorobanevents.Row{lakeRow(101, 1)}, 105, func(consumer.Event) error {
		return notNullViolation()
	})

	for i := 1; i < QuarantineAfterCyclesNoProgress; i++ {
		h.cycle()
		if got := h.store.cursor(); got != 100 {
			t.Fatalf("cycle %d: cursor = %d, want 100 — with no proof the sink is healthy, a class-23502 verdict must stall visibly instead of shedding the row", i, got)
		}
	}

	// Budget spent: the source self-heals rather than wedging, one row per
	// cycle, exactly as the unclassified arm does.
	h.cycle()
	if got := h.store.cursor(); got != 105 {
		t.Fatalf("cursor = %d, want 105 — after the no-progress budget the poison row is shed so a sparse source still self-heals", got)
	}
}

// TestCycle_PoisonRowWithSinkHealthProofShedsOnCycleOne is the other side of
// the same rail, and the reason the proof is a DISCRIMINATOR rather than a
// blanket delay: a scattered poison row sits beside rows that commit, so the
// proof is present and the row costs exactly one cycle — the COR-11 behaviour
// this arm exists for is unchanged.
func TestCycle_PoisonRowWithSinkHealthProofShedsOnCycleOne(t *testing.T) {
	const source = "rlt131-with-health-proof"
	rows := []sorobanevents.Row{lakeRow(101, 1), lakeRow(102, 2)}

	h := newWedgeHarness(t, source, rows, 105, func(ev consumer.Event) error {
		if ev.(ledgerEvent).ledger == 101 {
			return notNullViolation()
		}
		return nil // ledger 102 commits — THE health proof
	})

	h.cycle()
	if got := h.store.cursor(); got != 105 {
		t.Fatalf("cycle 1: cursor = %d, want 105 — with another event durably committed the class-23502 verdict is row-local and sheds at once", got)
	}
}
