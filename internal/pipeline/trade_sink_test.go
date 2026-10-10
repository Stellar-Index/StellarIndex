package pipeline

import (
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
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
	// (the outage signature). Flip to true to simulate recovery.
	healthy atomic.Bool
	// dataErr, when set, makes every insert return a permanent DATA
	// fault (non-infra) regardless of healthy — the error-and-skip path.
	dataErr bool
	// failErr overrides the error returned while unhealthy. Zero value
	// keeps the historical errInfra signature; set it before starting
	// any goroutine (it is read under f.mu but never written after).
	failErr error
}

// unhealthyErr is the error an unhealthy fake returns.
func (f *fakeTradeStore) unhealthyErr() error {
	if f.failErr != nil {
		return f.failErr
	}
	return errInfra
}

var errInfra = errors.New("dial tcp 127.0.0.1:5432: connect: connection refused")

// errData is a PERMANENT data fault as the driver actually reports one:
// SQLSTATE 23502 (not_null_violation, integrity-constraint class 23).
// A bare errors.New whose *text* imitated pq would be
// indistinguishable from an unclassifiable driver error,
// which the classifier deliberately routes to block-and-retry
// rather than to the drop path. So the fault is typed, letting the
// classifier positively recognise it.
var errData = &pgconn.PgError{
	Code:    "23502",
	Message: `null value in column "quote_asset" violates not-null constraint`,
}

// errUnclassified is a driver error NEITHER predicate recognises — the
// shape of a disk-full (53100), OOM (53200) or class-58 I/O fault
// reaching the sink today: these must block-and-retry, never
// drop.
var errUnclassified = &pgconn.PgError{
	Code:    "53100",
	Message: "could not extend file \"base/16384/24576\": No space left on device",
}

func (f *fakeTradeStore) BatchInsertTrades(_ context.Context, trades []canonical.Trade) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.batchCalls++
	if f.dataErr {
		return errData
	}
	if !f.healthy.Load() {
		return f.unhealthyErr()
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
		return f.unhealthyErr()
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
// property: an on-chain batch that hits an
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

// ─── infra-vs-data classification ────────
//
// The hazard: retrying ONLY what timescale.IsInfraError
// positively recognises and dropping everything else would treat a Postgres
// infrastructure fault it does not enumerate (53100 disk_full, 53200
// out_of_memory, class-58 I/O, XX000) as a permanent data
// fault — the trade dropped while the enqueue-advanced cursor sails
// past its ledger. These tests pin the asymmetric policy: a
// DROP needs positive proof of permanence; everything else blocks and
// retries.

// TestClassifyFault_Policy pins the classification table itself.
func TestClassifyFault_Policy(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want faultClass
	}{
		{"connection refused (recognised infra)", errInfra, faultInfra},
		{"disk full 53100 (unrecognised infra)", errUnclassified, faultInfra},
		{"out of memory 53200", &pgconn.PgError{Code: "53200", Message: "out of memory"}, faultInfra},
		{"io error 58030", &pgconn.PgError{Code: "58030", Message: "could not read block"}, faultInfra},
		{"internal error XX000", &pgconn.PgError{Code: "XX000", Message: "internal error"}, faultInfra},
		{"deadlock 40P01 (transient contention)", &pgconn.PgError{Code: "40P01", Message: "deadlock detected"}, faultInfra},
		{"bare driver error (unclassifiable)", errors.New("sql: unexpected message from server"), faultInfra},
		{"not-null violation 23502", errData, faultData},
		{"numeric out of range 22003", &pgconn.PgError{Code: "22003", Message: "numeric field overflow"}, faultData},
		{"canonical trade validation", fmt.Errorf("timescale: InsertTrade: %w", canonical.ErrInvalidTrade), faultData},
		{"canonical oracle validation", fmt.Errorf("%w: zero timestamp", canonical.ErrInvalidOracle), faultData},
		{"recovered sink panic", fmt.Errorf("%w for band/band.update: boom", ErrSinkPanic), faultData},
		{"context canceled", context.Canceled, faultShutdown},
		{"deadline exceeded", context.DeadlineExceeded, faultShutdown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyFault(tc.err); got != tc.want {
				t.Errorf("classifyFault(%v) = %v; want %v", tc.err, got, tc.want)
			}
		})
	}
}

// TestClassifyFault_StoragePredicatesAgree — the pipeline's policy must
// never contradict the storage layer's own predicates: anything
// timescale positively calls infra classifies as faultInfra, anything it
// positively calls a permanent data fault classifies as faultData. (The
// converse does NOT hold, by design: faultInfra is the default, so it
// also absorbs everything neither predicate recognises.)
func TestClassifyFault_StoragePredicatesAgree(t *testing.T) {
	errs := []error{
		errInfra,
		errUnclassified,
		errData,
		driver.ErrBadConn,
		&pgconn.PgError{Code: "08006", Message: "connection failure"},
		&pgconn.PgError{Code: "57P03", Message: "the database system is starting up"},
		&pgconn.PgError{Code: "53300", Message: "too many connections"},
		&pgconn.PgError{Code: "23505", Message: "duplicate key"},
		&pgconn.PgError{Code: "40001", Message: "serialization failure"},
		errors.New("something nobody classified"),
	}
	for _, err := range errs {
		got := classifyFault(err)
		if timescale.IsInfraError(err) && got != faultInfra {
			t.Errorf("IsInfraError(%v) but classifyFault = %v; want faultInfra", err, got)
		}
		if timescale.IsPermanentDataError(err) && got != faultData {
			t.Errorf("IsPermanentDataError(%v) but classifyFault = %v; want faultData", err, got)
		}
	}
}

// TestRetryInfra_UnclassifiedFaultRetriesUntilItLands — the core
// property at the retry choke point: a fault nobody has positively
// classified (disk-full here) must BLOCK and retry until it lands, not
// return for the caller to drop.
func TestRetryInfra_UnclassifiedFaultRetriesUntilItLands(t *testing.T) {
	var attempts atomic.Int32
	err := retryInfra(context.Background(), discardLogger(), "test", func(context.Context) error {
		if attempts.Add(1) < 4 {
			return errUnclassified
		}
		return nil
	})
	if err != nil {
		t.Fatalf("retryInfra returned %v on an unclassified fault; want nil after retrying until success (a return here is the caller's cue to DROP the write)", err)
	}
	if got := attempts.Load(); got != 4 {
		t.Errorf("attempts = %d; want 4 (retried until the fault cleared)", got)
	}
}

// TestRetryInfra_PermanentDataFaultReturnsImmediately — the other half:
// a positively-permanent fault must NOT be retried, so one poison row
// can never wedge the drain.
func TestRetryInfra_PermanentDataFaultReturnsImmediately(t *testing.T) {
	var attempts atomic.Int32
	err := retryInfra(context.Background(), discardLogger(), "test", func(context.Context) error {
		attempts.Add(1)
		return errData
	})
	if !errors.Is(err, errData) {
		t.Fatalf("retryInfra returned %v; want the data fault surfaced for error-and-skip", err)
	}
	if got := attempts.Load(); got != 1 {
		t.Errorf("attempts = %d; want 1 (a permanent data fault must never be retried)", got)
	}
}

// TestFlushTradeBatch_UnclassifiedInfraNeverDropsOnChain — end-to-end at
// the batch path: an on-chain batch hitting a disk-full fault must block
// (cursor gating) and land every trade on recovery, with ZERO drops
// counted. Returning instantly would count three
// dropped trades while the cursor kept moving.
func TestFlushTradeBatch_UnclassifiedInfraNeverDropsOnChain(t *testing.T) {
	dropsBefore := counter(t, obs.SourceInsertErrorsTotal, "sdex", "trade")
	store := &fakeTradeStore{failErr: errUnclassified} // healthy=false → disk full
	batch := []canonical.Trade{mkTrade("sdex", 700), mkTrade("sdex", 701), mkTrade("sdex", 702)}

	done := make(chan struct{})
	go func() {
		defer close(done)
		flushTradeBatch(context.Background(), discardLogger(), store, nil, batch, 0)
	}()

	select {
	case <-done:
		t.Fatal("flushTradeBatch returned while Postgres was out of disk — it dropped the on-chain trades instead of blocking the drain (REL-08)")
	case <-time.After(300 * time.Millisecond):
	}
	if n := store.landedCount(); n != 0 {
		t.Fatalf("landed %d trades while the store was failing; want 0", n)
	}

	store.healthy.Store(true) // operator frees disk
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("flushTradeBatch did not complete within 5s after recovery")
	}

	if n := store.landedCount(); n != len(batch) {
		t.Fatalf("landed %d trades after recovery; want %d (no loss)", n, len(batch))
	}
	if got := counter(t, obs.SourceInsertErrorsTotal, "sdex", "trade") - dropsBefore; got != 0 {
		t.Errorf("source_insert_errors{sdex,trade} delta = %v; want 0 — an infrastructure fault must never be counted as a dropped trade", got)
	}
}

// TestFlushTradeBatch_ExternalUnclassifiedRoutesToBufferNoBlock — the
// mirror-image invariant: making unclassified faults
// blocking must NOT put external CEX/FX trades on the blocking path.
// They have no cursor and are vendor-refillable (ADR-0041 acceptance
// caveat), so they go to the bounded buffer and the drain keeps moving.
func TestFlushTradeBatch_ExternalUnclassifiedRoutesToBufferNoBlock(t *testing.T) {
	store := &fakeTradeStore{failErr: errUnclassified}
	buf := newExternalRetryBuffer(store, discardLogger(), 1000)
	batch := []canonical.Trade{mkTrade("binance", 800), mkTrade("binance", 801)}

	done := make(chan struct{})
	go func() {
		defer close(done)
		flushTradeBatch(context.Background(), discardLogger(), store, buf, batch, 0)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("flushTradeBatch blocked on external trades under an unclassified fault; external must never block the pipeline")
	}

	buf.mu.Lock()
	depth := len(buf.ring)
	buf.mu.Unlock()
	if depth != len(batch) {
		t.Errorf("external buffer depth = %d; want %d (buffered for async retry, not blocked and not dropped)", depth, len(batch))
	}
}

// scriptedStore is a tradeWriter whose per-row outcome is scripted by
// ledger, so the external buffer's per-row isolation pass can be driven
// through a MIXED batch: one permanently-bad row plus rows that fail
// with an infrastructure fault.
type scriptedStore struct {
	mu       sync.Mutex
	batchErr error
	rowErr   map[uint32]error
	landed   []canonical.Trade
}

func (s *scriptedStore) BatchInsertTrades(_ context.Context, _ []canonical.Trade) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.batchErr
}

func (s *scriptedStore) InsertTrade(_ context.Context, t canonical.Trade) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.rowErr[t.Ledger]; err != nil {
		return err
	}
	s.landed = append(s.landed, t)
	return nil
}

func (s *scriptedStore) WouldPopulateUSDVolume(_ context.Context, _ canonical.Trade) bool {
	return false
}

// TestFlushTradeBatch_CtxCancelledMidWrite_ReturnsWholeBatch pins the
// contract the carry depends on: a batch write that fails with the
// ctx's own cancellation is handed back to the caller in full — not
// isolated per-row against the dead ctx (which can only fail every
// row instantly and count each one lost).
func TestFlushTradeBatch_CtxCancelledMidWrite_ReturnsWholeBatch(t *testing.T) {
	droppedBefore := counter(t, obs.SourceInsertErrorsTotal, "sdex", "trade")

	store := newShutdownRacedTradeStore()
	batch := []canonical.Trade{mkTrade("sdex", 10), mkTrade("sdex", 11), mkTrade("sdex", 12)}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-store.entered
		cancel()
	}()
	got := flushTradeBatch(ctx, discardLogger(), store, nil, batch, 0)

	if len(got) != len(batch) {
		t.Fatalf("flushTradeBatch returned %d trades, want the whole batch of %d", len(got), len(batch))
	}
	for i := range batch {
		if got[i].Ledger != batch[i].Ledger {
			t.Errorf("returned[%d].Ledger = %d, want %d", i, got[i].Ledger, batch[i].Ledger)
		}
	}
	if n := store.landedCount(); n != 0 {
		t.Errorf("landed %d, want 0 (the write was cancelled)", n)
	}
	if _, rows := store.calls(); rows != 0 {
		t.Errorf("InsertTrade called %d times, want 0 — a ctx-cancelled batch must not be isolated per-row against the dead ctx", rows)
	}
	if got := counter(t, obs.SourceInsertErrorsTotal, "sdex", "trade") - droppedBefore; got != 0 {
		t.Errorf("source_insert_errors{sdex,trade} delta = %v, want 0", got)
	}
}
