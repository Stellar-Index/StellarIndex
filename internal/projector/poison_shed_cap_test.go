package projector

import (
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Stellar-Index/StellarIndex/internal/consumer"
	"github.com/Stellar-Index/StellarIndex/internal/events"
	"github.com/Stellar-Index/StellarIndex/internal/sources/sorobanevents"
)

// notNullViolation is the verbatim shape a migration that adds a NOT NULL
// column (or a CHECK the live rows violate) puts on EVERY insert into the
// affected hypertable: SQLSTATE 23502, class 23, which
// timescale.IsPermanentDataError reports as a POSITIVE permanent data fault.
//
// That is the trap RLT-131 names: the classifier's verdict is "these VALUES
// are bad", but the same SQLSTATE arrives from a fault that is GLOBAL rather
// than row-local, in which case every row of the window is "poison" at once.
func notNullViolation() error {
	return &pgconn.PgError{
		Code:    "23502",
		Message: `null value in column "quote_asset_id" of relation "trades" violates not-null constraint`,
	}
}

// TestCycle_GlobalPermanentFaultShedsAtMostOneRowPerCycle is the RLT-131
// regression. A bad migration makes the sink reject EVERY row of the window
// with a class-23 error. The projector must not answer that by dropping the
// whole backlog: it holds every row below the cursor (a visible stall — rising
// lag, runs_total{outcome="sink_retry"}) for as long as the cycle cannot prove
// the sink is otherwise healthy, then bleeds at most one row per cycle, and
// still drains rather than wedging (COR-11).
//
// Before the fix the skip arm ran inline per row with no cap: the first cycle
// counted all five, forgot them and let the cursor advance to the window's end
// (110), so an operator's next look showed a caught-up source with five rows
// missing from the served tier and nothing but an ERROR log to say so.
//
// The cap alone (rail 1) still shed the first row on cycle ONE, ~1 hour before
// anything could page. This case now pins BOTH rails: no row leaves the served
// tier until the no-progress budget is spent, because a window in which
// nothing at all committed is the global-fault shape by construction. See
// poison_shed_health_proof_test.go for the single-row form.
func TestCycle_GlobalPermanentFaultShedsAtMostOneRowPerCycle(t *testing.T) {
	const source = "rlt131-global-not-null"
	rows := []sorobanevents.Row{
		lakeRow(101, 1), lakeRow(102, 2), lakeRow(103, 3), lakeRow(104, 4), lakeRow(105, 5),
	}
	okRunsBefore := runsCount(t, source, "ok")
	retryRunsBefore := runsCount(t, source, "sink_retry")

	h := newWedgeHarness(t, source, rows, 110, func(consumer.Event) error {
		return notNullViolation()
	})

	h.cycle()

	if got := h.store.cursor(); got != 100 {
		t.Fatalf("cycle 1: cursor = %d, want 100 — a class-23502 fault rejecting EVERY row of the window has no sink-health proof, so NOTHING may be shed yet; 101 sheds a row an hour before an operator can see the fault, 110 drops the whole backlog in a single pass", got)
	}
	if got := runsCount(t, source, "sink_retry") - retryRunsBefore; got != 1 {
		t.Errorf("runs_total{outcome=sink_retry} delta = %v, want 1 — a cycle that held rows back must not be reported as a clean run", got)
	}
	if got := runsCount(t, source, "ok") - okRunsBefore; got != 0 {
		t.Errorf("runs_total{outcome=ok} delta = %v, want 0 — all five rows are still held", got)
	}

	// The stall lasts the whole no-progress budget: the operator's window to
	// fix the migration before any row leaves the served tier.
	for i := 2; i < QuarantineAfterCyclesNoProgress; i++ {
		h.cycle()
		if got := h.store.cursor(); got != 100 {
			t.Fatalf("cycle %d: cursor = %d, want 100 — the stall must last the whole no-progress budget", i, got)
		}
	}

	// Budget spent: the held rows are re-read and bled off one per cycle —
	// bounded and loud, never a permanent stall (COR-11: a poison row must not
	// wedge a sole-writer domain).
	for _, want := range []uint32{101, 102, 103, 104} {
		h.cycle()
		if got := h.store.cursor(); got != want {
			t.Fatalf("cursor = %d, want %d — the backlog must drain at exactly one poison row per cycle", got, want)
		}
	}

	// Last poison row: nothing is left to hold, so the cursor catches up to
	// the tip in the same cycle.
	h.cycle()
	if got := h.store.cursor(); got != 110 {
		t.Fatalf("final cycle: cursor = %d, want 110 — once the last poison row is shed the source must catch up to the tip, not wedge", got)
	}
}

// TestCycle_PoisonOutputOnAHeldRowDoesNotResetItsRetryBudget guards the
// interaction the shed cap introduces, rather than the defect it fixes: a
// multi-output row (RLT-132) can carry BOTH a permanently dropped output and a
// fault that holds the cursor. Such a row is retried whole, poison outputs
// included, so it must not also be a shed candidate — shedding it would forget
// the row identity, and the consecutive-cycle count the quarantine budget is
// made of would restart every cycle, so the row could never be quarantined and
// the sole-writer source would wedge forever (COR-11/COR-01).
func TestCycle_PoisonOutputOnAHeldRowDoesNotResetItsRetryBudget(t *testing.T) {
	const source = "rlt131-drop-plus-held"
	quarantinedBefore := decodedCount(t, source, "sink_quarantined")

	h := newWedgeHarness(t, source, []sorobanevents.Row{lakeRow(101, 1)}, 105, nil)
	h.src.Decoder = &scriptedDecoder{build: []func(events.Event) consumer.Event{poisonTrade, echoOutput}}
	h.proj.sink = productionTradeSink(func(consumer.Event) error {
		// Unclassified: neither a positively permanent data fault nor a
		// positively transient infra one, so it is held under the budget.
		return errors.New("timescale: InsertSEP41TransferBatch: row 0 transfer negative Amount -1")
	})

	// Nothing else committed this cycle, so the long no-progress budget is the
	// one that applies.
	for i := 1; i < QuarantineAfterCyclesNoProgress; i++ {
		h.cycle()
		if got := h.store.cursor(); got != 100 {
			t.Fatalf("cycle %d: cursor = %d, want 100 — the held fault must keep the cursor below its ledger for the whole budget", i, got)
		}
	}

	h.cycle()

	if got := decodedCount(t, source, "sink_quarantined") - quarantinedBefore; got != 1 {
		t.Errorf("outcome=sink_quarantined delta = %v, want 1 — the budget must accumulate across cycles even though the row also drops a poison output", got)
	}
	if got := h.store.cursor(); got != 105 {
		t.Fatalf("cursor = %d, want 105 — the row quarantines once its budget is spent; a reset budget would wedge the source forever", got)
	}
}

// TestCycle_PoisonShedTakesTheLowestLedgerAndKeepsGoodRowsFlowing pins the
// other half of the cap: it is the LOWEST-ledger poison row that is shed, so
// the cursor watermark still advances in ledger order, and the valid rows
// between two poison rows commit on the way past.
//
// Rows: 101 poison, 102 valid, 103 poison, 104 valid. The cycle may shed 101
// only, which puts the watermark at 102 (the last fully-committed ledger below
// the row still held) — not at 110, which is what shedding both poison rows in
// one pass produced.
func TestCycle_PoisonShedTakesTheLowestLedgerAndKeepsGoodRowsFlowing(t *testing.T) {
	const source = "rlt131-lowest-ledger-first"
	rows := []sorobanevents.Row{lakeRow(101, 1), lakeRow(102, 2), lakeRow(103, 3), lakeRow(104, 4)}
	okBefore := decodedCount(t, source, "ok")

	h := newWedgeHarness(t, source, rows, 110, func(ev consumer.Event) error {
		switch ev.(ledgerEvent).ledger {
		case 101, 103:
			return notNullViolation()
		default:
			return nil
		}
	})

	h.cycle()

	if got := h.store.cursor(); got != 102 {
		t.Fatalf("cycle 1: cursor = %d, want 102 — only the lowest poison row (101) may be shed, so the watermark stops below the one still held (103)", got)
	}
	if got := decodedCount(t, source, "ok") - okBefore; got != 2 {
		t.Errorf("outcome=ok delta = %v, want 2 — holding a poison row must not stop the window's valid rows reaching the sink", got)
	}

	// Ledger 103 is re-read next cycle, shed, and 104 is already durable, so
	// the source catches up.
	h.cycle()
	if got := h.store.cursor(); got != 110 {
		t.Fatalf("cycle 2: cursor = %d, want 110", got)
	}
}
