package pipeline

import (
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/obs"
)

// TestExternalRetryBuffer_FinalDrainCountsUndrainedRows pins the third
// shutdown-loss site: external trades still in the retry ring after the
// final bounded pass have nowhere left to go, and must be counted on
// SinkUndrainedRowsTotal by row exactly like an abandoned on-chain batch
// (reportAbandonedTrades) — previously only a Warn line recorded them, so
// the alert never saw a vendor-refillable loss (RLT-190).
func TestExternalRetryBuffer_FinalDrainCountsUndrainedRows(t *testing.T) {
	before := counter(t, obs.SinkUndrainedRowsTotal, obs.SinkPersistEvents, "trade")
	store := &fakeTradeStore{} // stays unhealthy: every insert is an infra fault
	buf := newExternalRetryBuffer(store, discardLogger(), 8)
	for i := 0; i < 3; i++ {
		buf.enqueue(mkTrade("kraken", 0))
	}

	buf.finalDrain()

	if got := counter(t, obs.SinkUndrainedRowsTotal, obs.SinkPersistEvents, "trade") - before; got != 3 {
		t.Errorf("undrained trade rows delta = %v, want 3 (one per external trade left in the ring after the final pass)", got)
	}
}

// TestExternalRetryBuffer_FinalDrainDoesNotCountLandedRows is the guard on
// the other side: a final pass that lands the ring is not a loss.
func TestExternalRetryBuffer_FinalDrainDoesNotCountLandedRows(t *testing.T) {
	before := counter(t, obs.SinkUndrainedRowsTotal, obs.SinkPersistEvents, "trade")
	store := &fakeTradeStore{}
	store.healthy.Store(true)
	buf := newExternalRetryBuffer(store, discardLogger(), 8)
	buf.enqueue(mkTrade("kraken", 0))
	buf.enqueue(mkTrade("coinbase", 0))

	buf.finalDrain()

	if got := counter(t, obs.SinkUndrainedRowsTotal, obs.SinkPersistEvents, "trade") - before; got != 0 {
		t.Errorf("undrained trade rows delta = %v, want 0 (the final pass landed every row)", got)
	}
	store.mu.Lock()
	landed := len(store.landed)
	store.mu.Unlock()
	if landed != 2 {
		t.Errorf("landed = %d, want 2", landed)
	}
}
