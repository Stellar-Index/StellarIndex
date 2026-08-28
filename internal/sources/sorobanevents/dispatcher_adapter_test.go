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

	"github.com/Stellar-Index/StellarIndex/internal/events"
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

// TestAsyncSink_PushEventBacksPressure_BufferFull_NoDrops verifies
// the post-2026-05-26 contract: when the channel is full, PushEvent
// blocks rather than dropping the row. This is the invariant the
// backfill cursor relies on (cursor advances per produced ledger;
// drops would leave un-recoverable gaps).
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
		// Explicit generous drain grace: the default (2xWriteTimeout =
		// 2s) is a REAL-TIME bound, and this test's final Stop() drain
		// races it against a loaded scheduler — under CI + race
		// detector the release->flush cycle exceeded 2s and the drain
		// correctly gave up, losing the last batch and failing the
		// no-drops assertion (observed: WrittenCount 6/8, one flake in
		// CI run 30178983905, 2026-07-25; 8/8 green locally). The
		// invariant under test is back-pressure/no-drops, not drain
		// latency, so the grace is not load-bearing here — pin it far
		// above any plausible scheduler stall.
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

	// Give the producer time to fill the buffer + batch (cap = buf +
	// batchSz at most) and block on the next send. If the old
	// non-blocking semantics were in play, all 8 pushes would return
	// near-instantly and DroppedCount would jump.
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

// TestAsyncSink_StopDrainsPendingRows_NoChannelClose verifies that
// Stop signals via the stopping channel and the worker drains
// remaining buffered rows without panicking (Stop intentionally does
// NOT close the input channel — that would race with concurrent
// producers).
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

// TestAsyncSink_StopReleasesBlockedProducers verifies the
// shutdown-race semantics: a PushEvent already blocked on a full
// channel must return (counted as dropped) once Stop is signalled,
// rather than deadlocking the producer.
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

	// Stop must unblock the producer. We don't release the writer:
	// the worker's pending InsertSorobanEventsBatch will hit the
	// per-batch WriteTimeout (1s) and the worker will exit; Stop
	// returns once the worker is done.
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

// TestAsyncSink_FlushBatch_RetriesInfraFaultUntilItLands is the
// regression test for audit-2026-07-23 REL-02/DAT-09: before the
// fix, [AsyncSink.run]'s flush closure made exactly ONE
// InsertSorobanEventsBatch attempt and, on ANY error — including a
// plain transient infra fault like "connection refused" — logged a
// Warn and permanently discarded the batch. There was no retry, no
// lost-rows counter, and no ERROR-level signal: a sustained Postgres
// blip silently ate a window of raw soroban_events rows with nothing
// for an operator to alert on or a range to re-derive.
//
// Asserts the CORRECTED behaviour: an unclassified/infra fault is
// retried with backpressure until it lands — WrittenCount reaches the
// full row count and LostCount stays zero — matching the ADR-0041
// asymmetric policy the trades path already has (retry is the
// default; drop requires positive proof of permanence).
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

// TestAsyncSink_FlushBatch_PermanentFaultCountsLostNotRetried pins
// the other half of the REL-02/DAT-09 contract: a POSITIVELY
// classified permanent data fault (pq class 23, e.g. a unique
// constraint violation) must be counted on LostCount and must NOT be
// retried forever — retrying a deterministic constraint violation
// can never succeed and would wedge the sink on a single poison
// batch.
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
		// IsPermanentFault is injected by the app layer in production
		// (the indexer / ops backfill wire timescale.IsPermanentDataError
		// — see ARCH-import-boundaries, 9b033ff0: a source package must
		// not import internal/storage itself). Wire the same pq class-23
		// predicate here so this test exercises the intended contract —
		// left unset, IsPermanentFault defaults to "nothing is permanent"
		// and this pq error would instead retry-until-shutdown, which
		// would make the count-only assertions below pass for the WRONG
		// reason (racing sink.Stop()'s shutdown-abandon path — see
		// TestAsyncSink_FlushBatch_UnwiredIsPermanentFault_RetriesEvenAPqError)
		// instead of proving immediate, deterministic abandonment on a
		// positively-classified permanent fault. The log-reason
		// assertion below pins that distinction directly.
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
	if got := w.callCount(); got != 1 {
		t.Errorf("writer called %d times, want exactly 1 — a permanent fault must not be retried", got)
	}
	if out := logBuf.String(); !strings.Contains(out, `reason="permanent data fault"`) {
		t.Errorf("log output = %q; want it to contain reason=\"permanent data fault\" — a wired IsPermanentFault must abandon on the FIRST attempt, not retry until shutdown", out)
	}
}

// TestAsyncSink_FlushBatch_UnwiredIsPermanentFault_RetriesEvenAPqError
// pins the safe-default half of the ARCH-import-boundaries injection
// (9b033ff0): a caller that does NOT wire AsyncSinkOptions.IsPermanentFault
// must never drop a row — even one that a real IsPermanentDataError
// predicate would classify as permanent — because leaving it unwired
// must fail toward "retry forever", not "silently discard rows from
// ADR-0029's catch-all landing zone". Proves this via Stop()'s
// shutdown-abandon path (LostCount rises) rather than infinite
// retry — this test does not block forever waiting for a real DB.
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

// transientOnceWriter fails its first batch write with a transient
// (non-permanent) error and succeeds on every attempt after that.
// This is the shape that distinguishes "the retry loop is alive during
// the shutdown drain" from "the drain abandons on first error", with no
// dependence on scheduler races.
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

// TestAsyncSink_ShutdownDrainRetriesTransientFailure pins the fix for the
// shutdown-drain data-loss bug.
//
// flushBatch aborts an in-flight/retrying write when its abort signal
// fires, so a stuck write cannot hold shutdown hostage. That signal used
// to be s.stopping — but the shutdown DRAIN runs entirely AFTER
// s.stopping is closed, so every batch the drain flushed hit the
// abandon branch the moment its first attempt returned any error. A
// single transient blip (a Postgres restart, a reset connection) at
// shutdown therefore discarded the final buffered rows, logged them as
// lost, and exited 0. That is the ADR-0029 catch-all table: the rows the
// completeness census reconciles against.
//
// The drain now gets its own bounded grace window, so "stop was
// requested" and "stop has waited long enough" are finally distinct.
//
// Proven red: with abortFlush left as s.stopping, this fails with
// WrittenCount = 0 / LostCount = 10 and the "shutdown before the write
// landed" abandon log.
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

// stopRacedWriter models the exact shape of the 2026-08-28 CI failure
// (run 33168844647): a steady-state batch write is IN FLIGHT when
// Stop() fires. The first call parks until its ctx is cancelled (which
// the sink does the instant s.stopping closes) and returns ctx.Err(),
// exactly as a real pgx INSERT does when its context is cancelled
// mid-statement. Every later call succeeds. Deterministic: no
// dependence on which of two ready select cases the scheduler picks.
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

// TestAsyncSink_StopRacingInFlightSteadyStateFlush_RowsLandNotLost pins
// the shutdown data-loss class caught by main CI run 33168844647
// (2026-08-28): TestAsyncSink_StopDrainsPendingRows_NoChannelClose
// failed with "WrittenCount = 6, want 10" — 10 rows minus one
// BatchSize=4 batch — on a slow -race runner.
//
// Mechanism: a steady-state flushBatch used s.stopping as its abort
// signal. When Stop() closed it while a batch write was in flight, the
// write's ctx was cancelled, the write returned an error, and the
// abort branch ABANDONED the batch as lost — rows already accepted into
// the sink, discarded on every indexer restart/deploy, in the
// soroban_events raw landing zone. The interrupted batch must instead
// be carried into the shutdown drain and retried under DrainGrace.
//
// Proven red on the pre-fix code: WrittenCount = 6, LostCount = 4,
// writer received 6 rows.
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
