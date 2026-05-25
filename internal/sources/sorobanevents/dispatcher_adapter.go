package sorobanevents

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/RatesEngine/rates-engine/internal/events"
)

// RawEventSink is the contract a sink must satisfy to receive
// captured rows from the dispatcher's raw-event hook. Mirrors the
// shape of [dispatcher.DiscoverySink] — non-blocking Push from the
// hot path, drain asynchronously in a worker goroutine.
//
// The dispatcher's raw-event hook (added in ADR-0029) calls
// PushEvent for every Soroban contract event it sees, regardless
// of whether a per-source decoder claimed it. The sink converts to
// a [Row] via [Capture] and writes batched.
//
// Push MUST be non-blocking — a slow Push back-pressures the entire
// ingest pipeline. The standard [AsyncSink] implementation drops
// on buffer-full and increments DroppedCount; operators alert on
// sustained climb.
type RawEventSink interface {
	PushEvent(ev events.Event)
}

// BatchWriter is the storage seam the [AsyncSink] uses to drain its
// buffer. The Timescale implementation
// (`internal/storage/timescale.Store.InsertSorobanEventsBatch`)
// writes the batch via a single multi-row INSERT with ON CONFLICT
// DO NOTHING; idempotent across replays.
type BatchWriter interface {
	InsertSorobanEventsBatch(ctx context.Context, rows []Row) error
}

// AsyncSinkOptions configures a [NewAsyncSink].
type AsyncSinkOptions struct {
	// BufferSize is the channel depth. Defaults to 4096. Sized so a
	// few seconds of peak Soroban event volume (typically 100-500
	// events/s) fits even during a Postgres write hiccup.
	BufferSize int

	// BatchSize is the number of rows the worker accumulates before
	// firing an InsertSorobanEventsBatch. Defaults to 1000 per the
	// task spec — large enough to amortise the COPY-equivalent
	// statement-prepare cost across many rows, small enough that one
	// failed batch only loses ~10s of events at peak.
	BatchSize int

	// FlushInterval bounds how long a partial batch can wait before
	// being flushed. Defaults to 1 second. Without this, a low-volume
	// indexer (or the tail of a backfill) could leave the last
	// few-hundred rows stranded in the channel forever.
	FlushInterval time.Duration

	// WriteTimeout caps how long one InsertSorobanEventsBatch call
	// may block. Defaults to 10 seconds — generous because the
	// batched write is many-row.
	WriteTimeout time.Duration

	// Logger is used for warn/error lines from the worker. nil falls
	// through to slog.Default().
	Logger *slog.Logger
}

// AsyncSink is the non-blocking adapter between the dispatcher's
// raw-event hook and a batched storage writer. Construct with
// [NewAsyncSink] + Start; Stop drains the buffer and shuts down
// the worker.
//
// Concurrency: PushEvent is safe for concurrent callers (the
// dispatcher is single-threaded but parallel backfill chunks each
// run their own dispatcher into the same sink). The worker
// goroutine is single, so writes don't compete with each other.
type AsyncSink struct {
	w       BatchWriter
	logger  *slog.Logger
	timeout time.Duration

	ch       chan Row
	flush    time.Duration
	batchSz  int
	stopOnce sync.Once
	done     chan struct{}

	mu      sync.Mutex
	dropped uint64
	skipped uint64
	written uint64
}

// NewAsyncSink constructs an AsyncSink. Returns the sink in stopped
// state — callers must call Start before PushEvent will drain.
func NewAsyncSink(w BatchWriter, opts AsyncSinkOptions) *AsyncSink {
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
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &AsyncSink{
		w:       w,
		logger:  logger,
		timeout: opts.WriteTimeout,
		ch:      make(chan Row, opts.BufferSize),
		flush:   opts.FlushInterval,
		batchSz: opts.BatchSize,
		done:    make(chan struct{}),
	}
}

// Start launches the drain worker. Idempotent; calling twice is a
// no-op. Caller must Stop before the process exits to flush pending
// rows.
func (s *AsyncSink) Start() {
	go s.run()
}

// PushEvent captures `ev` into a Row and enqueues it for the
// batched writer. Behaviour:
//   - Capture returns ErrSkip → silently skipped (SkippedCount
//     incremented). Defence in depth for non-contract events that
//     should never reach this hook.
//   - Capture returns any other error → logged + counted as skipped.
//   - Channel full → dropped (DroppedCount incremented). Discovery's
//     buffer-full pattern: dispatch never stalls.
//   - Otherwise → enqueued.
//
// Implements [RawEventSink] (structurally — circular import means
// dispatcher declares its own interface and this method satisfies
// it).
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
	select {
	case s.ch <- row:
	default:
		s.mu.Lock()
		s.dropped++
		s.mu.Unlock()
	}
}

// Stop closes the input channel and waits for the worker to finish
// draining. Pending rows that fit within the worker's per-batch
// timeout are flushed; any that error are logged. Idempotent.
func (s *AsyncSink) Stop() {
	s.stopOnce.Do(func() {
		close(s.ch)
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

// run drains the channel and flushes batches.
//
// Deliberately uses a fresh context per batch rather than a
// parent-derived one — the whole reason this sink exists is to
// keep writing past the dispatcher's lifetime (shutdown drain),
// matching the pattern in [internal/canonical/discovery.AsyncSink].
//
//nolint:contextcheck // intentional fresh context; see godoc above.
func (s *AsyncSink) run() {
	defer close(s.done)
	batch := make([]Row, 0, s.batchSz)
	ticker := time.NewTicker(s.flush)
	defer ticker.Stop()

	flush := func() {
		if len(batch) == 0 {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), s.timeout)
		err := s.w.InsertSorobanEventsBatch(ctx, batch)
		cancel()
		if err != nil {
			s.logger.Warn("sorobanevents: batch insert failed",
				"err", err, "rows", len(batch))
		} else {
			s.mu.Lock()
			s.written += uint64(len(batch))
			s.mu.Unlock()
		}
		batch = batch[:0]
	}

	for {
		select {
		case row, ok := <-s.ch:
			if !ok {
				// Channel closed — final flush and exit.
				flush()
				return
			}
			batch = append(batch, row)
			if len(batch) >= s.batchSz {
				flush()
			}
		case <-ticker.C:
			flush()
		}
	}
}
