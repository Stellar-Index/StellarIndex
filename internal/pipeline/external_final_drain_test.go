package pipeline

import (
	"context"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/Stellar-Index/StellarIndex/internal/obs"
)

// TestExternalRetryBuffer_FinalDrainCountsUndrainedRows pins the third
// shutdown-loss site: external trades still in the retry ring after the
// final bounded pass have nowhere left to go, and must be counted on
// SinkUndrainedRowsTotal by row exactly like an abandoned on-chain batch
// (reportAbandonedTrades) — a Warn line alone would leave the alert
// blind to a vendor-refillable loss.
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

// TestExternalRetryBuffer_OverflowDropsOldest — external CEX trades are
// vendor-refillable and must never block: on overflow the buffer drops
// the OLDEST, counts every drop, and holds the newest maxDepth entries.
func TestExternalRetryBuffer_OverflowDropsOldest(t *testing.T) {
	before := counter(t, obs.SourceInsertErrorsTotal, "binance", "dropped")
	const maxDepth = 5
	buf := newExternalRetryBuffer(&fakeTradeStore{}, discardLogger(), maxDepth)

	const total = 8
	for i := 0; i < total; i++ {
		buf.enqueue(mkTrade("binance", uint32(300+i)))
	}

	buf.mu.Lock()
	depth := len(buf.ring)
	first := buf.ring[0].Ledger
	last := buf.ring[len(buf.ring)-1].Ledger
	buf.mu.Unlock()

	if depth != maxDepth {
		t.Fatalf("ring depth = %d; want %d (drop-oldest)", depth, maxDepth)
	}
	// Oldest 3 (ledgers 300,301,302) dropped; newest 5 (303..307) kept.
	if first != 303 || last != 307 {
		t.Errorf("ring holds ledgers [%d..%d]; want [303..307] (newest kept)", first, last)
	}
	if got := counter(t, obs.SourceInsertErrorsTotal, "binance", "dropped") - before; got != float64(total-maxDepth) {
		t.Errorf("dropped counter delta = %v; want %d", got, total-maxDepth)
	}
	if got := testutil.ToFloat64(obs.TradeInsertBufferDepth); got != maxDepth {
		t.Errorf("buffer-depth gauge = %v; want %d", got, maxDepth)
	}
}

// TestExternalRetryBuffer_InfraDuringIsolationRequeues — when
// the batch fails with a data fault the buffer isolates per-row, and if
// Postgres goes away DURING that pass the un-landed rows must be
// re-queued for the next tick, not counted as permanent drops. Before
// the fix every failing row in the isolation pass was dropped and
// counted "dropped" — recoverable infra failures mislabelled as
// permanent loss, and for external trades (no cursor, no lake) that loss
// was final.
func TestExternalRetryBuffer_InfraDuringIsolationRequeues(t *testing.T) {
	droppedBefore := counter(t, obs.SourceInsertErrorsTotal, "kraken", "dropped")
	store := &scriptedStore{
		batchErr: errData, // forces the per-row isolation pass
		rowErr: map[uint32]error{
			901: errData,         // genuinely poison → drop + count
			902: errUnclassified, // PG went away mid-pass → must be kept
			903: errInfra,        // ditto
		},
	}
	buf := newExternalRetryBuffer(store, discardLogger(), 1000)
	for _, l := range []uint32{900, 901, 902, 903} {
		buf.enqueue(mkTrade("kraken", l))
	}

	buf.drainOnce(context.Background())

	buf.mu.Lock()
	var kept []uint32
	for _, tr := range buf.ring {
		kept = append(kept, tr.Ledger)
	}
	buf.mu.Unlock()

	if len(kept) != 2 || kept[0] != 902 || kept[1] != 903 {
		t.Errorf("ring after isolation = %v; want [902 903] — rows that hit an infrastructure fault mid-isolation must be re-queued, not dropped", kept)
	}
	store.mu.Lock()
	landed := len(store.landed)
	store.mu.Unlock()
	if landed != 1 {
		t.Errorf("landed %d rows; want 1 (ledger 900 was fine)", landed)
	}
	if got := counter(t, obs.SourceInsertErrorsTotal, "kraken", "dropped") - droppedBefore; got != 1 {
		t.Errorf("dropped counter delta = %v; want exactly 1 (only the permanently-bad row 901)", got)
	}
}
