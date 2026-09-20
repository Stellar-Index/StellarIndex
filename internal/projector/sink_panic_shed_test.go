package projector

import (
	"fmt"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/consumer"
	"github.com/Stellar-Index/StellarIndex/internal/pipeline"
	"github.com/Stellar-Index/StellarIndex/internal/sources/sorobanevents"
)

// TestCycle_SinkPanicIsShedLikeAPermanentFault pins the projector side of
// the panic contract: pipeline.HandleEvent recovers a sink panic and returns
// it wrapped in pipeline.ErrSinkPanic, which the sink's own classifier drops.
// The projector must read it the same way — with a sink-health proof the row
// is shed on cycle one under the RLT-131 cap and counted as sink_permanent —
// rather than as an unclassified fault that re-runs the panicking decode
// every cycle for the whole quarantine budget (Q053).
func TestCycle_SinkPanicIsShedLikeAPermanentFault(t *testing.T) {
	const source = "q053-sink-panic"
	rows := []sorobanevents.Row{lakeRow(101, 1), lakeRow(102, 2)}
	beforePermanent := decodedCount(t, source, "sink_permanent")
	beforeRetry := decodedCount(t, source, "sink_retry")

	h := newWedgeHarness(t, source, rows, 105, func(ev consumer.Event) error {
		if ev.(ledgerEvent).ledger == 101 {
			return fmt.Errorf("%w for %s/echo: runtime error: nil map write", pipeline.ErrSinkPanic, source)
		}
		return nil // ledger 102 commits — the health proof
	})

	h.cycle()
	if got := h.store.cursor(); got != 105 {
		t.Fatalf("cycle 1: cursor = %d, want 105 — a recovered sink panic is deterministic for the event and must be shed like a class-22/23 verdict, not held", got)
	}
	if got := decodedCount(t, source, "sink_permanent") - beforePermanent; got != 1 {
		t.Errorf("sink_permanent delta = %v, want 1 (the panicking row, counted as a permanent drop)", got)
	}
	if got := decodedCount(t, source, "sink_retry") - beforeRetry; got != 0 {
		t.Errorf("sink_retry delta = %v, want 0 (nothing was held for retry)", got)
	}
}
