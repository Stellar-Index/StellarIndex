package projector

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/lib/pq"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// TestClassifySinkFault pins the three-valued taxonomy that replaced the
// permanent/transient boolean (COR-11 / COR-01, audit-2026-07-23). The
// load-bearing rows are the DETERMINISTIC ones that carry no *pq.Error: they
// used to fall into "transient" and hold a sole-writer cursor forever.
func TestClassifySinkFault(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want sinkDisposition
	}{
		{
			name: "canonical oracle validation (COR-11: InsertOracleUpdate returns Validate verbatim)",
			err:  fmt.Errorf("%w: price must be positive, got 0", canonical.ErrInvalidOracle),
			want: dispositionSkip,
		},
		{
			name: "canonical trade validation",
			err:  fmt.Errorf("timescale: InsertTrade: %w: base_amount must be positive", canonical.ErrInvalidTrade),
			want: dispositionSkip,
		},
		{
			name: "canonical asset validation nested behind two wraps",
			err:  fmt.Errorf("sink: %w", fmt.Errorf("%w: unknown crypto code %q", canonical.ErrInvalidAsset, "ZZZ")),
			want: dispositionSkip,
		},
		{
			name: "postgres CHECK violation (class 23)",
			err:  &pq.Error{Code: "23514", Message: "new row violates check constraint"},
			want: dispositionSkip,
		},
		{
			name: "postgres numeric out of range (class 22)",
			err:  &pq.Error{Code: "22003", Message: "numeric field overflow"},
			want: dispositionSkip,
		},
		{
			name: "postgres unreachable",
			err:  errors.New("dial tcp 127.0.0.1:5432: connect: connection refused"),
			want: dispositionRetry,
		},
		{
			name: "postgres shutting down (57P01)",
			err:  &pq.Error{Code: "57P01", Message: "terminating connection due to administrator command"},
			want: dispositionRetry,
		},
		{
			name: "cycle deadline",
			err:  fmt.Errorf("sink: %w", context.DeadlineExceeded),
			want: dispositionRetry,
		},
		{
			name: "shutdown",
			err:  context.Canceled,
			want: dispositionRetry,
		},
		{
			// COR-01: the store rejects the row BEFORE the statement runs, so
			// there is no SQLSTATE to read and no sentinel to match. It must
			// NOT be classified transient-forever; the budget arm bounds it.
			name: "store pre-SQL validation, no sentinel (COR-01 negative SEP-41 amount)",
			err:  errors.New("timescale: InsertSEP41TransferBatch: row 0 transfer negative Amount -1"),
			want: dispositionUnclassified,
		},
		{
			name: "deadlock (retryable, but not positively infra)",
			err:  &pq.Error{Code: "40P01", Message: "deadlock detected"},
			want: dispositionUnclassified,
		},
		{
			name: "a brand-new error type nobody enumerated",
			err:  errors.New("some future sink returned something we have never seen"),
			want: dispositionUnclassified,
		},
		{
			// Registry/catalogue state can change between cycles, so this is
			// deliberately NOT treated as a row verdict.
			name: "unknown asset is not a value-shape verdict",
			err:  fmt.Errorf("%w: CXYZ", canonical.ErrUnknownAsset),
			want: dispositionUnclassified,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := classifySinkFault(c.err); got != c.want {
				t.Errorf("classifySinkFault(%v) = %s, want %s", c.err, got, c.want)
			}
		})
	}
}

// TestPoisonTracker_CountsConsecutiveCycles checks the budget counts cycles in
// which the SAME row re-failed, and that a healed row's history is dropped —
// otherwise an unrelated flap would eventually quarantine a healthy row.
func TestPoisonTracker_CountsConsecutiveCycles(t *testing.T) {
	var tr poisonTracker
	a := rowIdentity{ledger: 10, txHash: "aa", opIndex: 0, eventIndex: 0}
	b := rowIdentity{ledger: 10, txHash: "bb", opIndex: 1, eventIndex: 0}

	if got := tr.fail(a); got != 1 {
		t.Fatalf("first failure count = %d, want 1", got)
	}
	if got := tr.fail(a); got != 2 {
		t.Fatalf("second failure count = %d, want 2", got)
	}
	// A different row at the same ledger keeps its own budget.
	if got := tr.fail(b); got != 1 {
		t.Fatalf("sibling row count = %d, want 1 (budget is per row identity, not per ledger)", got)
	}

	// Cycle in which only `a` failed: `b` healed and must be forgotten.
	tr.retain(map[rowIdentity]bool{a: true})
	if got := tr.fail(b); got != 1 {
		t.Errorf("healed row count = %d, want 1 (a healed row restarts its budget)", got)
	}

	tr.forget(a)
	if got := tr.fail(a); got != 1 {
		t.Errorf("forgotten row count = %d, want 1", got)
	}
}

// TestQuarantineCandidate pins the give-up rule: only unclassified rows, only
// past the budget, at most one per cycle (lowest ledger first), and the long
// budget when the cycle could not prove the sink healthy.
func TestQuarantineCandidate(t *testing.T) {
	id := func(ledger uint32) rowIdentity {
		return rowIdentity{ledger: ledger, txHash: fmt.Sprintf("tx%d", ledger)}
	}

	t.Run("nothing past budget", func(t *testing.T) {
		held := []heldRow{{id: id(5), disposition: dispositionUnclassified, fails: QuarantineAfterCycles - 1}}
		if got := quarantineCandidate(held, true); got != -1 {
			t.Errorf("index = %d, want -1 (budget not yet spent)", got)
		}
	})

	t.Run("infra fault is never quarantined", func(t *testing.T) {
		held := []heldRow{{id: id(5), disposition: dispositionRetry, fails: QuarantineAfterCyclesNoProgress * 10}}
		if got := quarantineCandidate(held, true); got != -1 {
			t.Errorf("index = %d, want -1 (a positively-identified infra fault is retried forever)", got)
		}
	})

	t.Run("lowest ledger wins, one per cycle", func(t *testing.T) {
		held := []heldRow{
			{id: id(9), disposition: dispositionUnclassified, fails: QuarantineAfterCycles},
			{id: id(4), disposition: dispositionUnclassified, fails: QuarantineAfterCycles},
		}
		if got := quarantineCandidate(held, true); got != 1 {
			t.Errorf("index = %d, want 1 (the lowest-ledger held row)", got)
		}
	})

	t.Run("no health proof uses the long budget", func(t *testing.T) {
		held := []heldRow{{id: id(5), disposition: dispositionUnclassified, fails: QuarantineAfterCycles}}
		if got := quarantineCandidate(held, false); got != -1 {
			t.Errorf("index = %d, want -1 (without a committed sibling this could be a global outage)", got)
		}
		held[0].fails = QuarantineAfterCyclesNoProgress
		if got := quarantineCandidate(held, false); got != 0 {
			t.Errorf("index = %d, want 0 (the long budget eventually gives up, so a sparse source still self-heals)", got)
		}
	})
}

// TestLowestHeldLedger checks the cursor watermark input.
func TestLowestHeldLedger(t *testing.T) {
	if _, ok := lowestHeldLedger(nil); ok {
		t.Error("empty held set must report nothing held")
	}
	held := []heldRow{
		{id: rowIdentity{ledger: 30}},
		{id: rowIdentity{ledger: 12}},
		{id: rowIdentity{ledger: 44}},
	}
	got, ok := lowestHeldLedger(held)
	if !ok || got != 12 {
		t.Errorf("lowestHeldLedger = (%d, %v), want (12, true)", got, ok)
	}
}
