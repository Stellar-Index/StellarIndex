package pipeline

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"math/big"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
)

// fakeTradeStore is a tradeWriter that fails on demand so the resilient
// sink's retry / buffer / drop paths can be exercised without a real
// Postgres. Landed trades are recorded so tests assert no-loss.
type fakeTradeStore struct {
	mu         sync.Mutex
	landed     []canonical.Trade
	batchCalls int
	rowCalls   int
	// healthy=false → every insert returns an infrastructure error
	// (the 2026-07-06 signature). Flip to true to simulate recovery.
	healthy atomic.Bool
	// dataErr, when set, makes every insert return a permanent DATA
	// fault (non-infra) regardless of healthy — the error-and-skip path.
	dataErr bool
}

var errInfra = errors.New("dial tcp 127.0.0.1:5432: connect: connection refused")

var errData = errors.New("pq: null value in column \"quote_asset\" violates not-null constraint")

func (f *fakeTradeStore) BatchInsertTrades(_ context.Context, trades []canonical.Trade) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.batchCalls++
	if f.dataErr {
		return errData
	}
	if !f.healthy.Load() {
		return errInfra
	}
	f.landed = append(f.landed, trades...)
	return nil
}

func (f *fakeTradeStore) InsertTrade(_ context.Context, t canonical.Trade) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rowCalls++
	if f.dataErr {
		return errData
	}
	if !f.healthy.Load() {
		return errInfra
	}
	f.landed = append(f.landed, t)
	return nil
}

func (f *fakeTradeStore) WouldPopulateUSDVolume(_ context.Context, _ canonical.Trade) bool {
	return false
}

func (f *fakeTradeStore) landedCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.landed)
}

func mkTrade(source string, ledger uint32) canonical.Trade {
	native := canonical.NativeAsset()
	return canonical.Trade{
		Source:      source,
		Ledger:      ledger,
		TxHash:      "tx",
		OpIndex:     0,
		Timestamp:   time.Unix(1_700_000_000, 0).UTC(),
		Pair:        canonical.Pair{Base: native, Quote: native},
		BaseAmount:  canonical.NewAmount(big.NewInt(1)),
		QuoteAmount: canonical.NewAmount(big.NewInt(1)),
	}
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func counter(t *testing.T, vec *prometheus.CounterVec, labels ...string) float64 {
	t.Helper()
	return testutil.ToFloat64(vec.WithLabelValues(labels...))
}

// TestFlushTradeBatch_InfraRetryThenSuccess is the load-bearing
// property (2026-07-06 outage): an on-chain batch that hits an
// infrastructure fault must BLOCK (retry with backpressure — the drain
// goroutine is stalled, so the ledger cursor can't advance) and then
// land every trade exactly once when Postgres recovers — never drop.
func TestFlushTradeBatch_InfraRetryThenSuccess(t *testing.T) {
	retryBefore := counter(t, obs.TradeInsertRetriesTotal, "retry")
	recoveredBefore := counter(t, obs.TradeInsertRetriesTotal, "recovered")

	store := &fakeTradeStore{} // healthy=false → keeps erroring
	batch := []canonical.Trade{mkTrade("sdex", 100), mkTrade("sdex", 101), mkTrade("sdex", 102)}

	done := make(chan struct{})
	go func() {
		defer close(done)
		flushTradeBatch(context.Background(), discardLogger(), store, nil, batch, 0)
	}()

	// While Postgres is down the flush MUST NOT return — this is the
	// backpressure that gates the cursor.
	select {
	case <-done:
		t.Fatal("flushTradeBatch returned while store was unhealthy — it dropped/skipped instead of blocking")
	case <-time.After(200 * time.Millisecond):
	}
	if n := store.landedCount(); n != 0 {
		t.Fatalf("landed %d trades while store unhealthy; want 0", n)
	}

	// Postgres recovers.
	store.healthy.Store(true)
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("flushTradeBatch did not complete within 3s after recovery")
	}

	if n := store.landedCount(); n != len(batch) {
		t.Fatalf("landed %d trades after recovery; want %d (no loss, no dup)", n, len(batch))
	}
	if got := counter(t, obs.TradeInsertRetriesTotal, "retry") - retryBefore; got < 1 {
		t.Errorf("retry counter delta = %v; want >= 1", got)
	}
	if got := counter(t, obs.TradeInsertRetriesTotal, "recovered") - recoveredBefore; got != 1 {
		t.Errorf("recovered counter delta = %v; want 1", got)
	}
}

// TestFlushTradeBatch_DataErrorSkips — a permanent DATA fault (not an
// outage) must be error-and-skipped per row, counted, and must NOT loop
// forever. No trade lands; each row bumps source_insert_errors.
func TestFlushTradeBatch_DataErrorSkips(t *testing.T) {
	before := counter(t, obs.SourceInsertErrorsTotal, "sdex", "trade")
	store := &fakeTradeStore{dataErr: true}
	store.healthy.Store(true)
	batch := []canonical.Trade{mkTrade("sdex", 200), mkTrade("sdex", 201), mkTrade("sdex", 202)}

	done := make(chan struct{})
	go func() {
		defer close(done)
		flushTradeBatch(context.Background(), discardLogger(), store, nil, batch, 0)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("flushTradeBatch blocked on a data error; must skip, not retry")
	}

	if n := store.landedCount(); n != 0 {
		t.Fatalf("landed %d on data error; want 0", n)
	}
	if got := counter(t, obs.SourceInsertErrorsTotal, "sdex", "trade") - before; got != float64(len(batch)) {
		t.Errorf("source_insert_errors{sdex,trade} delta = %v; want %d", got, len(batch))
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

// TestFlushTradeBatch_ExternalInfraRoutesToBufferNoBlock — an external
// CEX batch that hits an infra fault must be handed to the async buffer
// and return immediately (no pipeline block), unlike on-chain trades.
func TestFlushTradeBatch_ExternalInfraRoutesToBufferNoBlock(t *testing.T) {
	store := &fakeTradeStore{} // stays unhealthy
	buf := newExternalRetryBuffer(store, discardLogger(), 1000)
	batch := []canonical.Trade{mkTrade("binance", 400), mkTrade("binance", 401)}

	done := make(chan struct{})
	go func() {
		defer close(done)
		flushTradeBatch(context.Background(), discardLogger(), store, buf, batch, 0)
	}()
	select {
	case <-done:
	case <-time.After(1 * time.Second):
		t.Fatal("flushTradeBatch blocked on external trades; external must not block (bounded buffer)")
	}

	buf.mu.Lock()
	depth := len(buf.ring)
	buf.mu.Unlock()
	if depth != len(batch) {
		t.Errorf("external buffer depth = %d; want %d (routed, not blocked)", depth, len(batch))
	}
}

// TestPersistTrade_AbandonOnShutdown — if the context is cancelled while
// an infra fault persists (shutdown), persistTrade must give up (not
// hang) and count the loss; the row is recoverable from the CH lake.
func TestPersistTrade_AbandonOnShutdown(t *testing.T) {
	before := counter(t, obs.SourceInsertErrorsTotal, "sdex", "trade")
	store := &fakeTradeStore{} // stays unhealthy
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		defer close(done)
		persistTrade(ctx, discardLogger(), store, mkTrade("sdex", 500))
	}()
	// Let it enter the retry loop, then cancel (shutdown).
	time.Sleep(120 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("persistTrade did not abandon after ctx cancel — it hung")
	}
	if n := store.landedCount(); n != 0 {
		t.Fatalf("landed %d on abandon; want 0", n)
	}
	if got := counter(t, obs.SourceInsertErrorsTotal, "sdex", "trade") - before; got != 1 {
		t.Errorf("source_insert_errors{sdex,trade} delta = %v; want 1", got)
	}
}
