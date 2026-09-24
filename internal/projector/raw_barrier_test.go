package projector

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/consumer"
	"github.com/Stellar-Index/StellarIndex/internal/sources/sorobanevents"
)

// In soroban_events mode the ledgerstream cursor (the scan bound) advances
// when a ledger's raw rows are enqueued, before they commit. A cycle that
// scans to the tip while the tip's rows are still buffered finds nothing
// there, commits the cursor past them, and never reads them again. The
// barrier must land those rows before the scan.
func TestCycle_SorobanEventsModeWaitsForBufferedRawRows(t *testing.T) {
	var sunk []uint32
	h := newWedgeHarness(t, "raw-barrier", []sorobanevents.Row{lakeRow(150, 1)}, 200,
		func(ev consumer.Event) error {
			sunk = append(sunk, ev.(ledgerEvent).ledger)
			return nil
		})
	buffered := []sorobanevents.Row{lakeRow(200, 2)}
	h.proj.SetRawEventBarrier(func(context.Context) error {
		h.store.mu.Lock()
		h.store.rows = append(h.store.rows, buffered...)
		h.store.mu.Unlock()
		buffered = nil
		return nil
	})

	h.cycle()

	if h.store.cursor() != 200 {
		t.Fatalf("cursor = %d, want 200", h.store.cursor())
	}
	if !slices.Equal(sunk, []uint32{150, 200}) {
		t.Errorf("projected ledgers = %v, want [150 200]: the tip's buffered row was skipped", sunk)
	}
}

// A barrier that cannot settle (Postgres down, a stuck flush) fails the
// cycle and holds the cursor: advancing past unsettled rows is the loss.
func TestCycle_SorobanEventsBarrierFailureHoldsCursor(t *testing.T) {
	const src = "raw-barrier-fail"
	h := newWedgeHarness(t, src, []sorobanevents.Row{lakeRow(150, 1)}, 200,
		func(consumer.Event) error { return nil })
	h.proj.SetRawEventBarrier(func(context.Context) error {
		return errors.New("sorobanevents: waiting for accepted rows to settle: context deadline exceeded")
	})
	errBefore := runsCount(t, src, "error")

	h.cycle()

	if h.store.cursor() != 100 {
		t.Errorf("cursor = %d, want it held at 100", h.store.cursor())
	}
	if d := runsCount(t, src, "error") - errBefore; d != 1 {
		t.Errorf("runs_total{error} delta = %v, want 1", d)
	}
	if got := lagOf(src); got != 100 {
		t.Errorf("lag = %v, want 100 (tip 200 - cursor 100)", got)
	}
}
