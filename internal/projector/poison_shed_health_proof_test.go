package projector

import (
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/consumer"
	"github.com/Stellar-Index/StellarIndex/internal/sources/sorobanevents"
)

// TestCycle_PoisonRowWithNoSinkHealthProofHoldsUntilTheNoProgressBudget is the
// SECOND rail RLT-131 asks for, and the only part of the finding this package
// cannot ship on its own. It is committed skipped so the contract and its
// reproduction are on the branch rather than in a report: un-skip it and it
// fails against the code as shipped, which is the evidence that the gap is
// real.
//
// The rail: the per-cycle cap below ([PermanentSkipPerCycle]) bounds the RATE
// at which a global class-22/23 fault sheds rows, but not the fact of it. The
// discriminator between "this row's values are bad" and "the sink rejects
// everything right now" is the same proof the quarantine arm already takes —
// `madeProgress` (some OTHER event of this cycle durably committed). Without
// it, [QuarantineAfterCyclesNoProgress] is the budget: hold ~1 hour of cycles,
// far longer than the lag/sink_retry alerts take to fire, and only then give
// up one row per cycle. Passing `eventsEmitted > 0` into
// [permanentSkipCandidate] the way [quarantineCandidate] takes it is the whole
// change; what blocks it is that two existing cases pin the superseded
// contract (a lone poison row sheds on cycle one with no health proof), and
// both live outside this unit's assigned files:
//
//   - cycle_wedge_test.go, TestCycle_ValidationErrorStillAdvancesAcrossCycles
//   - trade_drop_outcome_test.go, TestCycle_DroppedTradeIsNotReportedOK
//
// Both would need their expectation moved from "cursor advances on cycle one"
// to "cursor holds for the no-progress budget, then advances" — a change to
// what those tests assert, which is not this fixer's to make unilaterally.
func TestCycle_PoisonRowWithNoSinkHealthProofHoldsUntilTheNoProgressBudget(t *testing.T) {
	t.Skip("RLT-131 rail 2 (madeProgress proof) not shipped: it changes what cycle_wedge_test.go and trade_drop_outcome_test.go assert, both outside this unit's file set — see the test doc")

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
