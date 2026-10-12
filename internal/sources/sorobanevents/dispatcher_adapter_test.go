package sorobanevents

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"math/big"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/Stellar-Index/StellarIndex/internal/events"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
)

// blockableWriter holds a release channel — InsertSorobanEventsBatch
// blocks until the test calls release(). Records the row batches it
// successfully writes so assertions can verify durability.
type blockableWriter struct {
	mu      sync.Mutex
	written [][]Row
	release chan struct{}
}

func newBlockableWriter() *blockableWriter {
	return &blockableWriter{release: make(chan struct{})}
}

func (w *blockableWriter) InsertSorobanEventsBatch(ctx context.Context, rows []Row) error {
	select {
	case <-w.release:
	case <-ctx.Done():
		return ctx.Err()
	}
	w.mu.Lock()
	cp := make([]Row, len(rows))
	copy(cp, rows)
	w.written = append(w.written, cp)
	w.mu.Unlock()
	return nil
}

func (w *blockableWriter) WrittenRows() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	n := 0
	for _, b := range w.written {
		n += len(b)
	}
	return n
}

// captureableEvent builds a minimal events.Event whose Capture
// succeeds without depending on the full xdr-encoded fixture set
// used by events_test.go.
func captureableEvent(t *testing.T, ledger uint32) events.Event {
	t.Helper()
	contract := mkContractStrkey(t, byte(ledger%256))
	topic := b64SV(t, symbolSV("swap"))
	body := b64SV(t, i128SV(big.NewInt(int64(ledger))))
	return events.Event{
		Type:           "contract",
		Ledger:         ledger,
		LedgerClosedAt: "2026-05-26T12:00:00Z",
		ContractID:     contract,
		OperationIndex: 0,
		TxHash:         mkTxHashHex(byte(ledger % 256)),
		Topic:          []string{topic},
		Value:          body,
	}
}

// TestAsyncSink_PushEventBacksPressure_BufferFull_NoDrops verifies that a full channel makes
// PushEvent block rather than drop the row. The backfill cursor advances per produced ledger,
// so drops would leave unrecoverable gaps.
func TestAsyncSink_PushEventBacksPressure_BufferFull_NoDrops(t *testing.T) {
	t.Parallel()

	w := newBlockableWriter()
	const buf = 2
	const batchSz = 2
	sink := NewAsyncSink(w, AsyncSinkOptions{
		BufferSize:    buf,
		BatchSize:     batchSz,
		FlushInterval: 10 * time.Second, // disable time-based flush in test
		WriteTimeout:  time.Second,
		// Generous drain grace: the default (2x WriteTimeout) is a real-time bound a loaded CI scheduler under the
		// race detector can exceed, losing the last batch; the invariant is back-pressure with no drops, not drain
		// latency.
		DrainGrace: 30 * time.Second,
	})
	sink.Start()

	const totalRows = 8
	pushed := make(chan struct{})
	go func() {
		defer close(pushed)
		for i := 0; i < totalRows; i++ {
			sink.PushEvent(captureableEvent(t, uint32(1_000_000+i)))
		}
	}()

	// Give the producer time to fill the buffer and batch and block on the next send; under non-blocking semantics
	// all 8 pushes would return instantly and DroppedCount would jump.
	select {
	case <-pushed:
		t.Fatalf("PushEvent returned before writer was released — back-pressure not applied")
	case <-time.After(100 * time.Millisecond):
		// expected — producer is blocked waiting for the writer.
	}
	if got := sink.DroppedCount(); got != 0 {
		t.Errorf("DroppedCount = %d before release, want 0 (back-pressure must not drop)", got)
	}

	// Release the writer; producer should now drain all rows.
	close(w.release)

	select {
	case <-pushed:
	case <-time.After(2 * time.Second):
		t.Fatalf("producer never finished after writer release")
	}

	sink.Stop()

	if got := sink.WrittenCount(); got != totalRows {
		t.Errorf("WrittenCount = %d, want %d", got, totalRows)
	}
	if got := sink.DroppedCount(); got != 0 {
		t.Errorf("DroppedCount = %d after Stop, want 0", got)
	}
	if got := w.WrittenRows(); got != totalRows {
		t.Errorf("writer received %d rows, want %d", got, totalRows)
	}
}

// TestAsyncSink_StopDrainsPendingRows_NoChannelClose verifies that Stop signals via the
// stopping channel and the worker drains buffered rows without panicking. Stop does NOT close
// the input channel, which would race with concurrent producers.
func TestAsyncSink_StopDrainsPendingRows_NoChannelClose(t *testing.T) {
	t.Parallel()

	w := newBlockableWriter()
	close(w.release) // writer is always ready

	sink := NewAsyncSink(w, AsyncSinkOptions{
		BufferSize:    16,
		BatchSize:     4,
		FlushInterval: 10 * time.Second,
		WriteTimeout:  time.Second,
	})
	sink.Start()

	const total = 10
	for i := 0; i < total; i++ {
		sink.PushEvent(captureableEvent(t, uint32(2_000_000+i)))
	}
	sink.Stop()

	if got := sink.WrittenCount(); got != total {
		t.Errorf("WrittenCount = %d, want %d", got, total)
	}
	if got := w.WrittenRows(); got != total {
		t.Errorf("writer received %d rows, want %d", got, total)
	}
	if got := sink.DroppedCount(); got != 0 {
		t.Errorf("DroppedCount = %d, want 0", got)
	}
}

// TestAsyncSink_StopReleasesBlockedProducers pins the shutdown race: a PushEvent already blocked on a full channel
// must return (counted dropped) once Stop is signalled, not deadlock.
func TestAsyncSink_StopReleasesBlockedProducers(t *testing.T) {
	t.Parallel()

	w := newBlockableWriter() // writer never releases — sink stays full

	sink := NewAsyncSink(w, AsyncSinkOptions{
		BufferSize:    1,
		BatchSize:     1,
		FlushInterval: 10 * time.Second,
		WriteTimeout:  time.Second,
	})
	sink.Start()

	// Fire enough pushes that one will block — the writer never
	// finishes a batch so after the first row is consumed into a
	// batch the buffer fills and the next pushes block.
	const total = 4
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < total; i++ {
			sink.PushEvent(captureableEvent(t, uint32(3_000_000+i)))
		}
	}()

	// Producer should be blocked.
	select {
	case <-done:
		t.Fatalf("producer finished before Stop — back-pressure not applied")
	case <-time.After(100 * time.Millisecond):
	}

	// Stop must unblock the producer. The writer is not released: the worker's pending batch hits the 1s
	// WriteTimeout and the worker exits, so Stop returns.
	stopReturned := make(chan struct{})
	go func() {
		sink.Stop()
		close(stopReturned)
	}()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatalf("blocked producer not released by Stop")
	}
	select {
	case <-stopReturned:
	case <-time.After(3 * time.Second):
		t.Fatalf("Stop did not return")
	}

	// At least one drop should be recorded (the producer that was
	// past the stopping check at close time).
	if got := sink.DroppedCount(); got == 0 {
		t.Errorf("DroppedCount = 0, want >0 after shutdown-race")
	}
}

// flakyWriter fails its first failN calls with failWith, then
// succeeds. failN == -1 means "always fail" (used to pin the
// permanent-fault path, which must never retry).
type flakyWriter struct {
	mu      sync.Mutex
	failErr error
	failN   int
	calls   int
	written [][]Row
}

func (w *flakyWriter) InsertSorobanEventsBatch(_ context.Context, rows []Row) error {
	w.mu.Lock()
	w.calls++
	call := w.calls
	w.mu.Unlock()
	if w.failN < 0 || call <= w.failN {
		return w.failErr
	}
	w.mu.Lock()
	cp := make([]Row, len(rows))
	copy(cp, rows)
	w.written = append(w.written, cp)
	w.mu.Unlock()
	return nil
}

func (w *flakyWriter) callCount() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.calls
}

// TestAsyncSink_FlushBatch_RetriesInfraFaultUntilItLands guards against giving up on the first error, which would
// silently eat raw soroban_events rows on a Postgres blip. An unclassified fault is retried with backpressure
// until it lands (WrittenCount full, LostCount zero); drop requires positive proof of permanence.
func TestAsyncSink_FlushBatch_RetriesInfraFaultUntilItLands(t *testing.T) {
	t.Parallel()

	w := &flakyWriter{failErr: errors.New("dial tcp: connection refused"), failN: 2}
	sink := NewAsyncSink(w, AsyncSinkOptions{
		BufferSize:    4,
		BatchSize:     2,
		FlushInterval: 10 * time.Second, // disable time-based flush in test
		WriteTimeout:  time.Second,
	})
	sink.Start()

	for i := 0; i < 2; i++ {
		sink.PushEvent(captureableEvent(t, uint32(4_000_000+i)))
	}

	deadline := time.Now().Add(3 * time.Second)
	for sink.WrittenCount() != 2 {
		if time.Now().After(deadline) {
			t.Fatalf("WrittenCount = %d after 3s, want 2 — infra fault was not retried until it landed (LostCount=%d, writer calls=%d)",
				sink.WrittenCount(), sink.LostCount(), w.callCount())
		}
		time.Sleep(10 * time.Millisecond)
	}
	sink.Stop()

	if got := sink.LostCount(); got != 0 {
		t.Errorf("LostCount = %d, want 0 — a retryable infra fault must never be counted as a permanent loss", got)
	}
	if got := w.callCount(); got < 3 {
		t.Errorf("writer called %d times, want >= 3 (2 induced failures + the attempt that landed)", got)
	}
}

// TestAsyncSink_FlushBatch_PermanentFaultCountsLostNotRetried pins that a positively classified permanent fault
// (pq class 23, e.g. unique violation) is counted on LostCount and not retried, since retrying would wedge the
// sink on one poison batch.
func TestAsyncSink_FlushBatch_PermanentFaultCountsLostNotRetried(t *testing.T) {
	t.Parallel()

	var logBuf bytes.Buffer
	permErr := &pgconn.PgError{Code: "23505", Message: "duplicate key value violates unique constraint"}
	w := &flakyWriter{failErr: permErr, failN: -1} // always fails
	sink := NewAsyncSink(w, AsyncSinkOptions{
		BufferSize:    4,
		BatchSize:     2,
		FlushInterval: 10 * time.Second,
		WriteTimeout:  time.Second,
		Logger:        slog.New(slog.NewTextHandler(&logBuf, nil)),
		// The app layer injects IsPermanentFault in production (a source package must not import
		// internal/storage), so wire the pq class-23 predicate here; unset, the pq error would retry until
		// shutdown and the count assertions would pass for the wrong reason.
		IsPermanentFault: func(err error) bool {
			var pgErr *pgconn.PgError
			return errors.As(err, &pgErr) && pgErr.Code[:2] == "23"
		},
	})
	sink.Start()

	for i := 0; i < 2; i++ {
		sink.PushEvent(captureableEvent(t, uint32(5_000_000+i)))
	}
	sink.Stop()

	if got := sink.LostCount(); got != 2 {
		t.Errorf("LostCount = %d, want 2 — a permanent data fault must be counted, not silently dropped with no signal", got)
	}
	if got := sink.WrittenCount(); got != 0 {
		t.Errorf("WrittenCount = %d, want 0", got)
	}
	// One attempt on the pair, then one per row to isolate the fault —
	// bisection, never a backoff retry of the same batch.
	if got := w.callCount(); got != 3 {
		t.Errorf("writer called %d times, want exactly 3 (batch, then each half) — a permanent fault must not be retried", got)
	}
	if out := logBuf.String(); strings.Contains(out, "retrying with backpressure") {
		t.Errorf("log output = %q; a permanent fault must never enter the backoff retry loop", out)
	}
	if out := logBuf.String(); !strings.Contains(out, `reason="permanent data fault"`) {
		t.Errorf("log output = %q; want it to contain reason=\"permanent data fault\" — a wired IsPermanentFault must abandon on the FIRST attempt, not retry until shutdown", out)
	}
}

// TestAsyncSink_FlushBatch_UnwiredIsPermanentFault_RetriesEvenAPqError pins the safe default: a caller that does
// not wire AsyncSinkOptions.IsPermanentFault never drops a row, even a real permanent one, so unwired fails toward
// retry. It is proven via Stop()'s shutdown-abandon path (LostCount rises) so it does not block on a real DB.
func TestAsyncSink_FlushBatch_UnwiredIsPermanentFault_RetriesEvenAPqError(t *testing.T) {
	t.Parallel()

	permErr := &pgconn.PgError{Code: "23505", Message: "duplicate key value violates unique constraint"}
	w := &flakyWriter{failErr: permErr, failN: -1} // always fails
	sink := NewAsyncSink(w, AsyncSinkOptions{
		BufferSize:    4,
		BatchSize:     2,
		FlushInterval: 10 * time.Second,
		WriteTimeout:  time.Second,
		// IsPermanentFault deliberately left unset.
	})
	sink.Start()

	for i := 0; i < 2; i++ {
		sink.PushEvent(captureableEvent(t, uint32(6_000_000+i)))
	}
	sink.Stop()

	if got := sink.WrittenCount(); got != 0 {
		t.Errorf("WrittenCount = %d, want 0", got)
	}
	// The unwired default must have retried at least once (backoff
	// window) before the shutdown-abandon path took over — proving it
	// did NOT immediately treat the pq error as permanent.
	if got := w.callCount(); got < 1 {
		t.Errorf("writer called %d times, want >= 1", got)
	}
	if got := sink.LostCount(); got != 2 {
		t.Errorf("LostCount = %d, want 2 (abandoned at shutdown, not immediately dropped as permanent)", got)
	}
}

// transientOnceWriter fails its first batch write with a transient error and succeeds afterwards, distinguishing a
// live retry loop during the shutdown drain from a drain that abandons on first error, without scheduler races.
type transientOnceWriter struct {
	mu      sync.Mutex
	calls   int
	written int
}

func (w *transientOnceWriter) InsertSorobanEventsBatch(_ context.Context, rows []Row) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.calls++
	if w.calls == 1 {
		return errors.New("transient: connection reset by peer")
	}
	w.written += len(rows)
	return nil
}

func (w *transientOnceWriter) WrittenRows() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.written
}

// TestAsyncSink_ShutdownDrainRetriesTransientFailure pins that a transient write failure during the shutdown drain
// is retried, not abandoned. flushBatch's abort signal must not be s.stopping, since the drain runs after it
// closes and would discard the final buffered rows; the drain gets its own grace window.
func TestAsyncSink_ShutdownDrainRetriesTransientFailure(t *testing.T) {
	t.Parallel()

	w := &transientOnceWriter{}
	const total = 10
	sink := NewAsyncSink(w, AsyncSinkOptions{
		BufferSize: 64,
		// Big batch + long interval: nothing flushes before Stop, so the
		// ONLY flush in this test is the shutdown drain's.
		BatchSize:     1000,
		FlushInterval: time.Hour,
		WriteTimeout:  time.Second,
		DrainGrace:    5 * time.Second,
	})
	sink.Start()
	for i := 0; i < total; i++ {
		sink.PushEvent(captureableEvent(t, uint32(3_000_000+i)))
	}
	sink.Stop()

	if got := sink.LostCount(); got != 0 {
		t.Errorf("LostCount = %d, want 0 — the drain abandoned rows on a transient error "+
			"instead of retrying within the grace window", got)
	}
	if got := sink.WrittenCount(); got != total {
		t.Errorf("WrittenCount = %d, want %d", got, total)
	}
	if got := w.WrittenRows(); got != total {
		t.Errorf("writer received %d rows, want %d", got, total)
	}
	if w.calls < 2 {
		t.Errorf("writer saw %d attempts, want >=2 — the retry never happened, "+
			"so this test is not exercising the drain retry path", w.calls)
	}
}

// stopRacedWriter models a batch write in flight when Stop() fires: the first call parks until its ctx is
// cancelled and returns ctx.Err(), as a real pgx INSERT does; later calls succeed. It is deterministic, with no
// dependence on select scheduling.
type stopRacedWriter struct {
	entered chan struct{} // closed when the first (in-flight) call starts
	mu      sync.Mutex
	calls   int
	written int
}

func newStopRacedWriter() *stopRacedWriter {
	return &stopRacedWriter{entered: make(chan struct{})}
}

func (w *stopRacedWriter) InsertSorobanEventsBatch(ctx context.Context, rows []Row) error {
	w.mu.Lock()
	w.calls++
	first := w.calls == 1
	w.mu.Unlock()
	if first {
		close(w.entered)
		<-ctx.Done()
		return ctx.Err()
	}
	w.mu.Lock()
	w.written += len(rows)
	w.mu.Unlock()
	return nil
}

func (w *stopRacedWriter) WrittenRows() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.written
}

func (w *stopRacedWriter) Calls() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.calls
}

// TestAsyncSink_StopRacingInFlightSteadyStateFlush_RowsLandNotLost pins that a batch write in flight when Stop()
// fires is not abandoned as lost (which would discard accepted rows on every restart) but carried into the
// shutdown drain and retried under DrainGrace.
func TestAsyncSink_StopRacingInFlightSteadyStateFlush_RowsLandNotLost(t *testing.T) {
	t.Parallel()

	w := newStopRacedWriter()
	sink := NewAsyncSink(w, AsyncSinkOptions{
		BufferSize:    16,
		BatchSize:     4,
		FlushInterval: 10 * time.Second,
		// Long enough that the in-flight write can only end via the
		// stopping-triggered cancel, never its own deadline.
		WriteTimeout: 10 * time.Second,
		DrainGrace:   5 * time.Second,
	})
	sink.Start()

	const total = 10
	for i := 0; i < total; i++ {
		sink.PushEvent(captureableEvent(t, uint32(4_000_000+i)))
	}

	// Wait until the first batch's write is in flight, then Stop while
	// it is still parked — the exact race the CI runner hit.
	select {
	case <-w.entered:
	case <-time.After(5 * time.Second):
		t.Fatalf("first batch write never started")
	}
	sink.Stop()

	if got := sink.LostCount(); got != 0 {
		t.Errorf("LostCount = %d, want 0 — the in-flight batch was abandoned because Stop raced it", got)
	}
	if got := sink.WrittenCount(); got != total {
		t.Errorf("WrittenCount = %d, want %d", got, total)
	}
	if got := w.WrittenRows(); got != total {
		t.Errorf("writer received %d rows, want %d", got, total)
	}
	if got := sink.DroppedCount(); got != 0 {
		t.Errorf("DroppedCount = %d, want 0 — no producer was blocked at Stop", got)
	}
	if got := w.Calls(); got < 2 {
		t.Errorf("writer saw %d calls, want >=2 — the interrupted batch was never retried", got)
	}
}

// TestAsyncSink_StartTwice_IsNoOp pins Start's idempotency: without the once-guard a second run() goroutine would
// race to close(s.done) on Stop and panic with "close of closed channel".
func TestAsyncSink_StartTwice_IsNoOp(t *testing.T) {
	t.Parallel()

	w := newBlockableWriter()
	close(w.release)
	sink := NewAsyncSink(w, AsyncSinkOptions{
		BufferSize:    4,
		BatchSize:     2,
		FlushInterval: 10 * time.Second,
		WriteTimeout:  time.Second,
	})
	sink.Start()
	sink.Start()
	sink.PushEvent(captureableEvent(t, 6_000_000))
	sink.Stop()
	// The duplicate worker's panic surfaces asynchronously after Stop
	// returns; give it the chance to, so the un-guarded code fails here.
	time.Sleep(50 * time.Millisecond)

	if got := sink.WrittenCount(); got != 1 {
		t.Errorf("WrittenCount = %d, want 1", got)
	}
}

// TestAsyncSink_PushEventAfterStop_NeverOrphansARow pins that a PushEvent after Stop is counted dropped, never
// enqueued: a select with a free slot and a closed stopping picks at random, orphaning about half the late rows,
// counted neither written, dropped nor lost.
func TestAsyncSink_PushEventAfterStop_NeverOrphansARow(t *testing.T) {
	t.Parallel()

	w := newBlockableWriter()
	close(w.release)
	sink := NewAsyncSink(w, AsyncSinkOptions{
		BufferSize:    128,
		BatchSize:     8,
		FlushInterval: 10 * time.Second,
		WriteTimeout:  time.Second,
	})
	sink.Start()
	sink.Stop()

	const late = 64
	for i := 0; i < late; i++ {
		sink.PushEvent(captureableEvent(t, uint32(7_000_000+i)))
	}

	if got := len(sink.ch); got != 0 {
		t.Errorf("%d rows orphaned in the buffer after Stop — neither written nor dropped", got)
	}
	if got := sink.DroppedCount(); got != late {
		t.Errorf("DroppedCount = %d, want %d — every post-Stop row must be counted dropped", got, late)
	}
	if got := sink.WrittenCount() + sink.DroppedCount() + sink.LostCount(); got != late {
		t.Errorf("written+dropped+lost = %d, want %d", got, late)
	}
}

// TestAsyncSink_PushEventDuringStop_EveryRowAccounted hammers PushEvent from several producers during Stop and
// checks the ledger balances: every row is written, dropped or lost, and the drain's final poll waits for every
// in-flight producer to leave PushEvent.
func TestAsyncSink_PushEventDuringStop_EveryRowAccounted(t *testing.T) {
	t.Parallel()

	w := newBlockableWriter()
	close(w.release)
	sink := NewAsyncSink(w, AsyncSinkOptions{
		BufferSize:    16,
		BatchSize:     4,
		FlushInterval: time.Millisecond,
		WriteTimeout:  time.Second,
	})
	sink.Start()

	const producers, perProducer = 8, 200
	var wg sync.WaitGroup
	start := make(chan struct{})
	for p := 0; p < producers; p++ {
		wg.Add(1)
		go func(p int) {
			defer wg.Done()
			<-start
			for i := 0; i < perProducer; i++ {
				sink.PushEvent(captureableEvent(t, uint32(8_000_000+p*perProducer+i)))
			}
		}(p)
	}
	close(start)
	time.Sleep(2 * time.Millisecond)
	sink.Stop()
	wg.Wait()

	const pushed = producers * perProducer
	if got := len(sink.ch); got != 0 {
		t.Errorf("%d rows orphaned in the buffer after Stop", got)
	}
	if got := sink.WrittenCount() + sink.DroppedCount() + sink.LostCount(); got != pushed {
		t.Errorf("written+dropped+lost = %d, want %d (written=%d dropped=%d lost=%d)",
			got, pushed, sink.WrittenCount(), sink.DroppedCount(), sink.LostCount())
	}
	if got := w.WrittenRows(); uint64(got) != sink.WrittenCount() {
		t.Errorf("writer saw %d rows, WrittenCount = %d", got, sink.WrittenCount())
	}
}

// poisonWriter rejects any batch containing a row at poisonLedger with a
// pq class-22 data fault — what Postgres does to a whole multi-row
// INSERT when one row is malformed — and writes every other batch.
type poisonWriter struct {
	mu           sync.Mutex
	poisonLedger uint32
	written      []Row
}

func (w *poisonWriter) InsertSorobanEventsBatch(_ context.Context, rows []Row) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, r := range rows {
		if r.Ledger == w.poisonLedger {
			return &pgconn.PgError{Code: "22P02", Message: "invalid input syntax"}
		}
	}
	w.written = append(w.written, rows...)
	return nil
}

// TestAsyncSink_FlushBatch_PermanentFaultIsolatesPoisonRow pins batch splitting: one poison row loses exactly that
// row, not the whole batch's good rows.
func TestAsyncSink_FlushBatch_PermanentFaultIsolatesPoisonRow(t *testing.T) {
	t.Parallel()

	const base, batch = uint32(9_000_000), 8
	const poison = base + 5
	var logBuf bytes.Buffer
	w := &poisonWriter{poisonLedger: poison}
	sink := NewAsyncSink(w, AsyncSinkOptions{
		BufferSize:    batch,
		BatchSize:     batch,
		FlushInterval: 10 * time.Second,
		WriteTimeout:  time.Second,
		Logger:        slog.New(slog.NewTextHandler(&logBuf, nil)),
		IsPermanentFault: func(err error) bool {
			var pgErr *pgconn.PgError
			return errors.As(err, &pgErr) && pgErr.Code[:2] == "22"
		},
	})
	sink.Start()
	for i := uint32(0); i < batch; i++ {
		sink.PushEvent(captureableEvent(t, base+i))
	}
	sink.Stop()

	if got := sink.LostCount(); got != 1 {
		t.Errorf("LostCount = %d, want 1 — only the poison row is a permanent fault", got)
	}
	if got := sink.WrittenCount(); got != batch-1 {
		t.Errorf("WrittenCount = %d, want %d — the good rows must land", got, batch-1)
	}
	w.mu.Lock()
	seen := make(map[uint32]bool, len(w.written))
	for _, r := range w.written {
		seen[r.Ledger] = true
	}
	w.mu.Unlock()
	for i := uint32(0); i < batch; i++ {
		if want := base+i != poison; seen[base+i] != want {
			t.Errorf("ledger %d written = %v, want %v", base+i, seen[base+i], want)
		}
	}
	if out := logBuf.String(); !strings.Contains(out, "rows=1") || !strings.Contains(out, `reason="permanent data fault"`) {
		t.Errorf("log output = %q; want exactly one row abandoned with reason=\"permanent data fault\"", out)
	}
}

// TestAsyncSink_LowestUnlandedLedger_CoversShutdownDropsAndAbandons pins the signal backfill caps its resume
// cursor on: the lowest ledger with rows dropped at graceful Stop or in an abandoned batch must be reported, or
// the cursor passes rows that never reached soroban_events.
func TestAsyncSink_LowestUnlandedLedger_CoversShutdownDropsAndAbandons(t *testing.T) {
	t.Parallel()

	w := newBlockableWriter() // never releases: Postgres is the bottleneck
	sink := NewAsyncSink(w, AsyncSinkOptions{
		BufferSize:    1,
		BatchSize:     1,
		FlushInterval: 10 * time.Second,
		WriteTimeout:  100 * time.Millisecond,
	})
	sink.Start()
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := uint32(0); i < 4; i++ {
			sink.PushEvent(captureableEvent(t, 4_000_000+i))
		}
	}()
	time.Sleep(100 * time.Millisecond) // producer is now blocked in PushEvent
	sink.Stop()
	<-done

	if sink.DroppedCount() == 0 {
		t.Fatal("precondition: no row was dropped at Stop")
	}
	got, ok := sink.LowestUnlandedLedger()
	if !ok || got != 4_000_000 {
		t.Fatalf("LowestUnlandedLedger() = (%d, %t), want (4000000, true): nothing was written, so the lowest pushed ledger is the first one not durable", got, ok)
	}
}

func TestAsyncSink_LowestUnlandedLedger_NoneOnCleanStop(t *testing.T) {
	t.Parallel()

	w := newBlockableWriter()
	close(w.release)
	sink := NewAsyncSink(w, AsyncSinkOptions{BatchSize: 2, WriteTimeout: time.Second})
	sink.Start()
	for i := uint32(0); i < 3; i++ {
		sink.PushEvent(captureableEvent(t, 5_000_000+i))
	}
	sink.Stop()
	if got, ok := sink.LowestUnlandedLedger(); ok {
		t.Fatalf("LowestUnlandedLedger() = %d after every row landed; want none", got)
	}
}

// This sink deliberately breaks worker.Recover's "contain and keep running" contract: the dispatcher calls
// PushEvent synchronously, so a dead drain would block every source. run() must let the panic propagate so systemd
// restarts from the durable ledger cursor. The test calls run() in a recovering goroutine.
func TestAsyncSink_PanickingWriterCrashesRatherThanWedges(t *testing.T) {
	const workerName = "soroban-events-sink-drain"
	before := testutil.ToFloat64(obs.WorkerPanicsTotal.WithLabelValues(workerName))

	sink := NewAsyncSink(panickingWriter{}, AsyncSinkOptions{
		BufferSize:    4,
		BatchSize:     1,
		FlushInterval: 10 * time.Millisecond,
	})
	sink.PushEvent(captureableEvent(t, 1)) // buffered; BatchSize=1 flushes it as soon as run() reads it

	recovered := make(chan any, 1)
	go func() {
		defer func() { recovered <- recover() }()
		sink.run()
	}()

	select {
	case r := <-recovered:
		if r == nil {
			t.Fatal("run() returned without panicking: the drain panic was contained instead of " +
				"propagating, so PushEvent's channel is left undrained and every producer wedges forever")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("run() neither panicked nor returned: the drain deadlocked")
	}

	select {
	case <-sink.done:
	default:
		t.Fatal("s.done was not closed even though run() unwound via panic — Stop() would block forever on it")
	}

	if after := testutil.ToFloat64(obs.WorkerPanicsTotal.WithLabelValues(workerName)); after != before+1 {
		t.Errorf("stellarindex_worker_panics_total{worker=%q} = %v, want %v — the panic must still "+
			"be reported before it propagates, so the existing page fires either way",
			workerName, after, before+1)
	}
}

// panickingWriter is a BatchWriter that has gone wrong in a way no error
// return describes — a nil pool, a driver bug, a bad type assertion.
type panickingWriter struct{}

func (panickingWriter) InsertSorobanEventsBatch(context.Context, []Row) error {
	panic("batch writer blew up")
}

// Sync is the projector's soroban_events completeness barrier: it must not
// return while a row PushEvent already accepted is still unwritten, and must
// return once it lands.
func TestAsyncSink_SyncWaitsForAcceptedRowsToCommit(t *testing.T) {
	w := newBlockableWriter()
	s := NewAsyncSink(w, AsyncSinkOptions{BatchSize: 1, FlushInterval: 10 * time.Millisecond})
	s.Start()
	defer s.Stop()

	for l := uint32(1); l <= 3; l++ {
		s.PushEvent(captureableEvent(t, l))
	}

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	err := s.Sync(ctx)
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Sync with 3 unwritten rows = %v, want context.DeadlineExceeded", err)
	}

	close(w.release)
	ctx, cancel = context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.Sync(ctx); err != nil {
		t.Fatalf("Sync after the writer released = %v, want nil", err)
	}
	if got := w.WrittenRows(); got != 3 {
		t.Errorf("rows written when Sync returned = %d, want 3", got)
	}
}

// An idle sink has nothing to wait for.
func TestAsyncSink_SyncIdleReturnsImmediately(t *testing.T) {
	s := NewAsyncSink(newBlockableWriter(), AsyncSinkOptions{})
	s.Start()
	defer s.Stop()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := s.Sync(ctx); err != nil {
		t.Fatalf("Sync on an idle sink = %v, want nil", err)
	}
}
