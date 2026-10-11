package sorobanevents

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/events"
	"github.com/Stellar-Index/StellarIndex/internal/worker"
)

// RawEventSink receives every Soroban contract event from the dispatcher's raw-event hook,
// whether or not a per-source decoder claimed it; the sink converts to a [Row] via [Capture].
//
// PushEvent MAY block to apply back-pressure: cursor advance must not outrun durable writes,
// otherwise -resume cannot recover the gap, so a full buffer must never drop rows.
type RawEventSink interface {
	PushEvent(ev events.Event)
}

// BatchWriter is the storage seam the [AsyncSink] drains into. The Timescale implementation
// uses one multi-row INSERT with ON CONFLICT DO NOTHING, so replays are idempotent.
type BatchWriter interface {
	InsertSorobanEventsBatch(ctx context.Context, rows []Row) error
}

// AsyncSinkOptions configures a [NewAsyncSink].
type AsyncSinkOptions struct {
	// IsPermanentFault positively classifies an error as a PERMANENT data fault (a row that will
	// never land however often retried). Only such an error is isolated and dropped; everything
	// else blocks and retries (ADR-0041).
	//
	// Injected because a source package must not import internal/storage (rule L/sources-app-purity);
	// the app layer supplies timescale.IsPermanentDataError. Defaults to "nothing is permanent":
	// an unclassified error must never silently discard a row from the catch-all landing zone.
	IsPermanentFault func(error) bool

	// BufferSize is the channel depth (default 4096), enough for a few seconds of peak Soroban
	// volume (100-500 events/s) during a Postgres write hiccup. When full, PushEvent blocks, so
	// sustained over-production slows the dispatcher instead of dropping rows.
	BufferSize int

	// BatchSize is the rows accumulated before one InsertSorobanEventsBatch (default 1000):
	// large enough to amortise statement cost, small enough that one failed batch is cheap.
	BatchSize int

	// FlushInterval bounds how long a partial batch waits (default 1s), so a low-volume
	// indexer or a backfill tail cannot strand rows in the channel.
	FlushInterval time.Duration

	// WriteTimeout caps one InsertSorobanEventsBatch call (default 10s; it is a many-row write).
	WriteTimeout time.Duration

	// DrainGrace bounds how long flushBatch may keep retrying rows still buffered when Stop()
	// fired (default 2x WriteTimeout). It is separate from s.stopping because the shutdown drain
	// runs entirely after s.stopping closes, so reusing that signal abandoned every drained batch
	// on arrival. Stop() still terminates promptly: the drain is bounded by this grace.
	DrainGrace time.Duration

	// Logger is used for warn/error lines from the worker. nil falls
	// through to slog.Default().
	Logger *slog.Logger
}

// AsyncSink is the back-pressuring adapter between the dispatcher's raw-event hook and a batched
// storage writer. Construct with [NewAsyncSink] + Start; Stop drains the buffer and stops the worker.
//
// PushEvent blocks when the channel is full so rows are never silently lost, a precondition for
// the backfill driver's cursor-advance contract. DroppedCount goes non-zero only when a producer
// races past the stopping check after Stop.
//
// PushEvent is safe for concurrent callers; the single worker goroutine serialises writes.
type AsyncSink struct {
	w                BatchWriter
	isPermanentFault func(error) bool
	logger           *slog.Logger
	timeout          time.Duration

	ch         chan Row
	flush      time.Duration
	batchSz    int
	drainGrace time.Duration

	// abortFlush interrupts an in-flight or retrying flush. It is s.stopping in steady state;
	// drainOnStop swaps in a bounded grace timer so the final batches can land. Interrupted rows
	// are returned to the worker for the drain to retry, or abandoned once the grace expires.
	// abortFlush and draining are touched only by the single worker goroutine.
	abortFlush <-chan struct{}
	draining   bool
	startOnce  sync.Once
	stopOnce   sync.Once
	stopping   chan struct{}
	// quiesced closes once Stop has seen every in-flight PushEvent return; only then may
	// drainOnStop take its final poll of ch, since a producer past the stopping check can still
	// win the send. pushMu is that barrier: producers hold it shared, Stop takes it exclusively.
	quiesced chan struct{}
	pushMu   sync.RWMutex
	done     chan struct{}

	mu      sync.Mutex
	dropped uint64
	skipped uint64
	written uint64
	lost    uint64
	// minUnlanded is the lowest ledger of any row dropped or abandoned at
	// shutdown (0 = none); see [AsyncSink.LowestUnlandedLedger].
	minUnlanded uint32
	// accepted counts rows that entered ch. Every one ends written or
	// lost, in channel order, which is what [AsyncSink.Sync] relies on.
	accepted uint64
}

// NewAsyncSink constructs an AsyncSink. Returns the sink in stopped
// state — callers must call Start before PushEvent will drain.
func NewAsyncSink(w BatchWriter, opts AsyncSinkOptions) *AsyncSink {
	if opts.IsPermanentFault == nil {
		opts.IsPermanentFault = func(error) bool { return false }
	}
	if opts.BufferSize <= 0 {
		opts.BufferSize = 4096
	}
	if opts.BatchSize <= 0 {
		opts.BatchSize = 1000
	}
	if opts.FlushInterval <= 0 {
		opts.FlushInterval = time.Second
	}
	if opts.WriteTimeout <= 0 {
		opts.WriteTimeout = 10 * time.Second
	}
	if opts.DrainGrace <= 0 {
		opts.DrainGrace = 2 * opts.WriteTimeout
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &AsyncSink{
		w:                w,
		isPermanentFault: opts.IsPermanentFault,
		logger:           logger,
		timeout:          opts.WriteTimeout,
		ch:               make(chan Row, opts.BufferSize),
		flush:            opts.FlushInterval,
		batchSz:          opts.BatchSize,
		drainGrace:       opts.DrainGrace,
		stopping:         make(chan struct{}),
		quiesced:         make(chan struct{}),
		done:             make(chan struct{}),
	}
}

// Start launches the drain worker. Idempotent; calling twice is a
// no-op. Caller must Stop before the process exits to flush pending
// rows.
func (s *AsyncSink) Start() {
	s.startOnce.Do(func() { go s.run() })
}

// PushEvent captures ev into a Row and enqueues it. A Capture error is counted as skipped
// (ErrSkip silently, anything else logged). A full channel BLOCKS until a slot frees or Stop
// fires, in which case the row is counted as dropped; DroppedCount is non-zero only in that
// shutdown race. Satisfies [RawEventSink] structurally to avoid a circular import.
func (s *AsyncSink) PushEvent(ev events.Event) {
	row, err := Capture(ev)
	if err != nil {
		s.mu.Lock()
		s.skipped++
		s.mu.Unlock()
		if !errors.Is(err, ErrSkip) {
			s.logger.Warn("sorobanevents: capture failed",
				"err", err,
				"contract_id", ev.ContractID,
				"tx_hash", ev.TxHash,
				"ledger", ev.Ledger,
			)
		}
		return
	}
	s.pushMu.RLock()
	defer s.pushMu.RUnlock()
	// Checked before the blocking select: once Stop has fired a row must never enter ch,
	// because the drain's final poll may already be past.
	select {
	case <-s.stopping:
		s.countDropped(row.Ledger)
		return
	default:
	}
	select {
	case s.ch <- row:
		s.mu.Lock()
		s.accepted++
		s.mu.Unlock()
	case <-s.stopping:
		s.countDropped(row.Ledger)
	}
}

// syncPoll is how often [AsyncSink.Sync] re-checks the settled count.
const syncPoll = 20 * time.Millisecond

// ErrSinkStopped is returned by [AsyncSink.Sync] when the drain worker
// exited with rows it accepted still unsettled.
var ErrSinkStopped = errors.New("sorobanevents: sink stopped before its rows settled")

// Sync blocks until every row whose PushEvent returned before the call has settled (committed to
// soroban_events, or counted lost) or ctx ends. PushEvent returns before the ledgerstream cursor
// is written, so after Sync every ledger at or below a previously read cursor is readable unless
// LostCount says otherwise. The worker settles batches in channel order, so the count settles a prefix.
func (s *AsyncSink) Sync(ctx context.Context) error {
	s.mu.Lock()
	target := s.accepted
	s.mu.Unlock()
	t := time.NewTicker(syncPoll)
	defer t.Stop()
	settled := func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.written+s.lost >= target
	}
	for !settled() {
		select {
		case <-ctx.Done():
			return fmt.Errorf("sorobanevents: waiting for accepted rows to settle: %w", ctx.Err())
		case <-s.done:
			if settled() {
				return nil
			}
			return ErrSinkStopped
		case <-t.C:
		}
	}
	return nil
}

func (s *AsyncSink) countDropped(ledger uint32) {
	s.mu.Lock()
	s.dropped++
	s.noteUnlandedLocked(ledger)
	s.mu.Unlock()
}

func (s *AsyncSink) noteUnlandedLocked(ledger uint32) {
	if s.minUnlanded == 0 || ledger < s.minUnlanded {
		s.minUnlanded = ledger
	}
}

// LowestUnlandedLedger returns the lowest ledger of any row this sink
// dropped or abandoned at shutdown, so a caller checkpointing a resume
// cursor can stop short of it. Rows isolated as a permanent data fault
// are excluded: a re-walk cannot land them either (ADR-0041 skip-class).
func (s *AsyncSink) LowestUnlandedLedger() (uint32, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.minUnlanded, s.minUnlanded != 0
}

// Stop closes the stopping channel so blocked producers return their row as dropped, then waits
// for the worker to drain and flush the final batch. The input channel is deliberately NOT
// closed: that would race with concurrent producers and panic on send-to-closed. Pending rows,
// including an in-flight steady-state batch, are flushed within DrainGrace; failures are logged
// as lost. Idempotent.
//
// Producers racing past the stopping check are counted as dropped, so the drop count reflects
// shutdown-race loss only. Rows already accepted are never counted lost merely because Stop
// raced their flush.
func (s *AsyncSink) Stop() {
	s.stopOnce.Do(func() {
		close(s.stopping)
		// Every producer inside PushEvent is now unblocked; wait for
		// them all to leave so nothing can enter ch after the drain's
		// final poll.
		s.pushMu.Lock()
		close(s.quiesced)
		s.pushMu.Unlock()
		<-s.done
	})
}

// DroppedCount returns the number of rows dropped because the
// channel was full. Operators alert when this counter rises
// monotonically — it indicates Postgres write throughput can't
// keep up with peak event rate.
func (s *AsyncSink) DroppedCount() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.dropped
}

// SkippedCount returns the number of events skipped by Capture
// (non-contract events or malformed inputs). A low non-zero value
// is normal (defence-in-depth); a sustained rise means the
// dispatcher is feeding us garbage and worth investigating.
func (s *AsyncSink) SkippedCount() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.skipped
}

// WrittenCount returns the total number of rows successfully
// persisted via InsertSorobanEventsBatch. Useful as a
// well-it's-working signal in operator dashboards.
func (s *AsyncSink) WrittenCount() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.written
}

// LostCount returns the rows permanently lost: batches that hit a positively-classified permanent
// data fault or never landed before shutdown gave up. Without this signal a sustained infra fault
// would silently eat whole windows of the raw landing zone, so operators alert on it rising.
func (s *AsyncSink) LostCount() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lost
}

// asyncSinkRetryInitialBackoff / asyncSinkRetryMaxBackoff bound the capped exponential backoff
// [flushBatch] applies to an insert failure not positively classified as permanent, mirroring the
// trades path (ADR-0041: a drop requires positive proof of permanence; retry is the default).
const (
	asyncSinkRetryInitialBackoff = 100 * time.Millisecond
	asyncSinkRetryMaxBackoff     = 5 * time.Second
)

// run drains the channel and flushes batches.
//
// It uses a fresh context per batch, not a parent-derived one, because the sink must keep
// writing past the dispatcher's lifetime (shutdown drain).
//
//nolint:contextcheck // intentional fresh context; see godoc above.
func (s *AsyncSink) run() {
	defer close(s.done)
	// Deliberately NOT worker.Recover: run() is the sole drain of the raw landing zone and the
	// dispatcher calls PushEvent synchronously, so a swallowed panic would leave the channel
	// undrained and block every source's PushEvent with no restart. Report (keeps the
	// stellarindex_worker_panics_total page) then re-panic so the supervisor restarts from the
	// durable ledger cursor. Registered after close(s.done) so that defer still runs on the way out.
	defer func() {
		if r := recover(); r != nil {
			worker.Report(s.logger, "soroban-events-sink-drain", r)
			panic(r)
		}
	}()
	s.abortFlush = s.stopping
	batch := make([]Row, 0, s.batchSz)
	ticker := time.NewTicker(s.flush)
	defer ticker.Stop()

	flush := func() {
		if len(batch) == 0 {
			return
		}
		if !s.draining {
			select {
			case <-s.stopping:
				// Stop already fired: a steady-state attempt would be
				// cancelled on arrival, so leave the rows in batch for
				// the stopping branch below to carry into drainOnStop.
				return
			default:
			}
		}
		b := batch
		batch = make([]Row, 0, s.batchSz)
		// A steady-state flush interrupted by Stop hands its rows back
		// (batch is empty here because flush is synchronous), and the
		// stopping branch below carries them into drainOnStop.
		batch = append(batch, s.flushBatch(b)...)
	}

	for {
		select {
		case row := <-s.ch:
			batch = append(batch, row)
			if len(batch) >= s.batchSz {
				flush()
			}
		case <-ticker.C:
			flush()
		case <-s.stopping:
			s.drainOnStop(&batch, flush)
			return
		}
	}
}

// drainOnStop is the shutdown-drain branch of run: it collects rows producers enqueued after the
// stopping-close and does a final flush. s.ch is NOT closed, which would panic in-flight
// producers. The final non-blocking poll waits for Stop to report quiesced: a producer that won
// the send would otherwise leave its row in a channel nobody reads, counted neither written nor dropped.
func (s *AsyncSink) drainOnStop(batch *[]Row, flush func()) {
	// s.stopping is already closed, so leaving abortFlush on it would abandon every drained
	// batch. The grace timer bounds the drain so Stop() still returns promptly.
	grace := make(chan struct{})
	timer := time.AfterFunc(s.drainGrace, func() { close(grace) })
	defer timer.Stop()
	s.abortFlush = grace
	s.draining = true

	for {
		select {
		case row := <-s.ch:
			*batch = append(*batch, row)
			if len(*batch) >= s.batchSz {
				flush()
			}
		case <-s.quiesced:
			for {
				select {
				case row := <-s.ch:
					*batch = append(*batch, row)
					if len(*batch) >= s.batchSz {
						flush()
					}
				default:
					flush()
					return
				}
			}
		}
	}
}

// flushBatch writes one batch under the ADR-0041 asymmetric failure policy: a write failure
// blocks and retries with capped backoff by default; only a POSITIVELY-classified permanent data
// fault (IsPermanentFault, pq class 22/23) is isolated and dropped, since dropping on any error
// would silently lose raw soroban_events rows during a transient Postgres outage.
//
// The retry loop is bounded by s.abortFlush. In steady state that is s.stopping: Stop() cancels
// the attempt and the unlanded rows are RETURNED for drainOnStop to retry under DrainGrace,
// otherwise a Stop() racing a healthy write would count a whole batch as lost on every deploy
// (TestAsyncSink_StopDrainsPendingRows_NoChannelClose). During the drain abortFlush is the grace
// timer, and a batch still unlanded is abandoned loudly via [AsyncSink.abandonBatch].
//
// Returns the rows neither written nor abandoned (nil in the drain).
func (s *AsyncSink) flushBatch(rows []Row) []Row {
	abort := s.abortFlush
	backoff := asyncSinkRetryInitialBackoff
	for {
		ctx, cancel := context.WithTimeout(context.Background(), s.timeout)
		attemptDone := make(chan struct{})
		go func() {
			// An unrecovered panic in this detached watchdog would take the whole process down.
			// cancel() is idempotent, so recovering cannot strand the write.
			defer worker.Recover(s.logger, "soroban-events-flush-abort-watch")
			select {
			case <-abort:
				cancel() // unblock a write stuck past its own WriteTimeout
			case <-attemptDone:
			}
		}()
		err := s.w.InsertSorobanEventsBatch(ctx, rows)
		close(attemptDone)
		cancel()

		if err == nil {
			s.mu.Lock()
			s.written += uint64(len(rows))
			s.mu.Unlock()
			return nil
		}
		if s.isPermanentFault(err) {
			return s.isolatePermanentFault(rows, err)
		}
		select {
		case <-abort:
			if !s.draining {
				return rows // Stop raced this flush: the drain retries it.
			}
			s.abandonBatch(rows, err, "shutdown before the write landed")
			lo, _ := rowLedgerRange(rows)
			s.mu.Lock()
			s.noteUnlandedLocked(lo)
			s.mu.Unlock()
			return nil
		case <-time.After(backoff):
		}
		s.logger.Warn("sorobanevents: batch insert failed — retrying with backpressure",
			"err", err, "rows", len(rows), "backoff", backoff)
		backoff = min(backoff*2, asyncSinkRetryMaxBackoff)
	}
}

// isolatePermanentFault narrows a batch-wide permanent fault to the rows carrying it. Postgres
// rejects the whole multi-row INSERT for one bad row, so abandoning the batch would lose up to
// BatchSize-1 good rows; bisecting costs log2(BatchSize) extra round trips per poison row and
// only a batch of one that still faults is abandoned. Returns rows handed back as in [flushBatch].
func (s *AsyncSink) isolatePermanentFault(rows []Row, err error) []Row {
	if len(rows) <= 1 {
		s.abandonBatch(rows, err, "permanent data fault")
		return nil
	}
	mid := len(rows) / 2
	left := s.flushBatch(rows[:mid])
	right := s.flushBatch(rows[mid:])
	if len(left) == 0 && len(right) == 0 {
		return nil
	}
	// Fresh backing array: left and right alias rows, and appending
	// one into the other's spare capacity would overwrite live rows.
	carried := make([]Row, 0, len(left)+len(right))
	carried = append(carried, left...)
	return append(carried, right...)
}

// abandonBatch counts + loudly logs rows[] as permanently lost, with
// the ledger range so an operator knows exactly what to re-derive
// (the raw ops are durable in the CH lake per ADR-0034).
func (s *AsyncSink) abandonBatch(rows []Row, err error, reason string) {
	lo, hi := rowLedgerRange(rows)
	s.mu.Lock()
	s.lost += uint64(len(rows))
	s.mu.Unlock()
	s.logger.Error("sorobanevents: batch insert failed — rows lost, re-derive this range from the CH lake (ADR-0034)",
		"reason", reason, "err", err, "rows", len(rows),
		"ledger_from", lo, "ledger_to", hi)
}

// rowLedgerRange returns the min/max Ledger across rows, for the
// re-derive hint in [AsyncSink.abandonBatch]. lo==0 means rows was
// empty.
func rowLedgerRange(rows []Row) (lo, hi uint32) {
	for _, r := range rows {
		if lo == 0 || r.Ledger < lo {
			lo = r.Ledger
		}
		if r.Ledger > hi {
			hi = r.Ledger
		}
	}
	return lo, hi
}
