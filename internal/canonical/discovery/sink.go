package discovery

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// AsyncSink is a non-blocking [DiscoverySink]-compatible adapter
// over a [Recorder]. The dispatcher's hot path Push call enqueues
// to a buffered channel; a worker goroutine drains the channel and
// calls Recorder.Record at production-grade rates without backing
// up dispatch.
//
// In-process dedup: the Recorder upserts on (contract_id, event_type),
// so re-pushing the same key is a no-op write that pointlessly
// occupies a buffer slot. AsyncSink keeps a process-local set of
// (ContractID, EventType) keys it has already enqueued and silently
// skips repeats. The skip counter is exposed via [AsyncSink.SkippedCount]
// alongside [AsyncSink.DroppedCount] so operators can see how much
// of the pre-dedup volume was duplicates. A process restart resets
// the set; the first Push for any key after restart still records
// (the recorder's upsert handles the already-known case).
//
// Buffer-full policy: when the channel is full, Push silently
// drops the new Hit. Discovery is best-effort — losing one record
// for a contract that already produced 10,000 events is acceptable;
// stalling the dispatch loop is not. The drop counter is exposed
// via [AsyncSink.DroppedCount] for operator monitoring.
//
// Construct via [NewAsyncSink] + Start; Stop drains the buffer and
// shuts down the worker. Safe for concurrent Push from multiple
// goroutines (the dispatcher itself is single-threaded but the
// indexer may run multiple dispatchers in the future).
type AsyncSink struct {
	rec     Recorder
	logger  *slog.Logger
	timeout time.Duration

	ch       chan Hit
	stopOnce sync.Once
	done     chan struct{}

	mu      sync.Mutex
	stopped bool
	dropped uint64
	skipped uint64
	failed  uint64
	seen    map[string]struct{}
}

// seenKey is the in-process dedup key for a Hit. Single definition
// because THREE call sites must agree on it byte-for-byte — Push's
// mark, Push's buffer-full rollback, and run's record-failure rollback
// — and a drifted key silently disables the rollback rather than
// failing loudly.
//
// The key combines Kind + both symbol-carrying fields rather than just
// EventType: KindSEP41 hits (from [Sniff]) populate EventType;
// KindOracleEvent/KindOracleCall hits (from
// [SniffOracleEvent]/[SniffOracleCall]) populate only Symbol.
// Concatenating both keeps the legacy SEP-41 dedup key byte-for-byte
// unchanged (Kind/Symbol are empty on any hand-built Hit that only
// sets EventType, e.g. existing tests) while still giving every
// (contract, kind, symbol) tuple its own key for the two new lanes.
func seenKey(hit Hit) string {
	return hit.ContractID + "\x00" + string(hit.Kind) + "\x00" + string(hit.EventType) + "\x00" + hit.Symbol
}

// AsyncSinkOptions configures a [NewAsyncSink].
type AsyncSinkOptions struct {
	// BufferSize is the channel depth. Must be > 0. Production
	// default is 1024 — covers a few minutes of SEP-41 event volume
	// at network peak. With in-process dedup the steady-state
	// occupancy is much lower; this is mostly a tail-end safety net
	// for cold-start / restart bursts.
	BufferSize int

	// RecordTimeout caps how long a single Recorder.Record call may
	// block the worker. Default 2 seconds. A slow Postgres write
	// fails the record (logged) rather than holding up the queue.
	RecordTimeout time.Duration

	// Logger is used for warn/error lines from the worker. nil
	// falls through to slog.Default().
	Logger *slog.Logger
}

// NewAsyncSink constructs an AsyncSink. Returns the sink in
// stopped state — callers must call Start before Push will drain.
func NewAsyncSink(rec Recorder, opts AsyncSinkOptions) *AsyncSink {
	if opts.BufferSize <= 0 {
		opts.BufferSize = 1024
	}
	timeout := opts.RecordTimeout
	if timeout <= 0 {
		timeout = 2 * time.Second
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &AsyncSink{
		rec:     rec,
		logger:  logger,
		timeout: timeout,
		ch:      make(chan Hit, opts.BufferSize),
		done:    make(chan struct{}),
		seen:    make(map[string]struct{}),
	}
}

// Start launches the drain worker. Idempotent; calling twice is a
// no-op. Caller must Stop before the process exits to flush
// pending records.
func (s *AsyncSink) Start() {
	go s.run()
}

// Push enqueues a Hit. Non-blocking. Behaviour:
//   - After [AsyncSink.Stop] → dropped, DroppedCount incremented.
//   - Already-enqueued (ContractID, Kind, EventType, Symbol) →
//     silently skipped, SkippedCount incremented.
//   - Channel full → dropped, DroppedCount incremented.
//   - Otherwise → marked seen and enqueued.
//
// Implements [dispatcher.DiscoverySink] (structurally; circular
// import means dispatcher declares its own interface and this method
// satisfies it).
//
// The whole body runs under s.mu, including the non-blocking send.
// That is what makes Push safe against a concurrent Stop: Stop takes
// the same mutex to set `stopped` BEFORE closing the channel, so a
// Push either completes its send first or observes `stopped` and
// never sends. Without it, a Push racing shutdown could send on a
// closed channel and panic the dispatcher (AGT-lifecycle-race). Held
// across the send safely because the send is non-blocking and the
// drain worker never sends.
func (s *AsyncSink) Push(hit Hit) {
	key := seenKey(hit)

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.stopped {
		// Shutdown in progress; the worker is draining what it has.
		// Counted as a drop rather than silently vanishing.
		s.dropped++
		return
	}
	if _, ok := s.seen[key]; ok {
		s.skipped++
		return
	}
	s.seen[key] = struct{}{}

	select {
	case s.ch <- hit:
	default:
		s.dropped++
		// Roll back the seen-mark so a future Push for this key can
		// retry; otherwise a transient Postgres outage would leak the
		// entire stream of new contracts forever.
		delete(s.seen, key)
	}
}

// Stop closes the input channel and waits for the worker to finish
// draining. Pending records that fit within the worker's per-record
// timeout are flushed; any that error are logged. Idempotent.
func (s *AsyncSink) Stop() {
	s.stopOnce.Do(func() {
		// Mark stopped BEFORE closing so a concurrent Push can never
		// reach the send — see [AsyncSink.Push].
		s.mu.Lock()
		s.stopped = true
		s.mu.Unlock()
		close(s.ch)
		<-s.done
	})
}

// DroppedCount returns the number of Hits dropped because the
// channel was full. Operators alert when this counter rises
// monotonically — it indicates the worker can't keep up with peak
// event rate (typically a Postgres outage; in-process dedup means
// healthy steady-state should never drop).
func (s *AsyncSink) DroppedCount() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.dropped
}

// SkippedCount returns the number of Hits skipped because their
// (ContractID, EventType) had already been enqueued in this process.
// A high ratio of Skipped to (Skipped + Recorded) is expected and
// healthy — most events for already-discovered contracts are noise.
func (s *AsyncSink) SkippedCount() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.skipped
}

// FailedCount returns the number of Hits whose Recorder.Record write
// failed (recorder outage/timeout). Bridged to
// obs.DiscoveryRecordFailuresTotal for alerting — a sustained non-zero
// rate means discovery coverage is degrading under recorder pressure,
// which was invisible while the failure was only logged (C4-3).
func (s *AsyncSink) FailedCount() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.failed
}

// run drains the input channel until close. One Recorder.Record
// call per Hit; per-record timeout caps the worker's exposure to a
// slow recorder.
func (s *AsyncSink) run() {
	defer close(s.done)
	for hit := range s.ch {
		ctx, cancel := context.WithTimeout(context.Background(), s.timeout)
		if err := s.rec.Record(ctx, hit); err != nil {
			// Count the write failure (audit-2026-07-16 C4-3) — previously
			// this was a log-only path, so a recorder outage silently
			// stopped discovered_assets from growing. Exposed via
			// FailedCount() and bridged to obs.DiscoveryRecordFailuresTotal
			// by the indexer, mirroring dropped/skipped. Record's contract
			// is best-effort (the contract re-appears on a later event), so
			// this is a failure-RATE signal, not permanent-loss.
			s.mu.Lock()
			s.failed++
			// Roll back the seen-mark, exactly as the buffer-full path
			// does (DAT-09/DAT-11). Record IS best-effort — but only
			// because "the contract re-appears on a later event", and
			// the seen-set suppresses every later Push for this key. So
			// without this rollback a contract first sighted DURING a
			// recorder outage was dropped from discovery permanently,
			// for the lifetime of the process, with nothing but a
			// failure counter to show for it.
			delete(s.seen, seenKey(hit))
			s.mu.Unlock()
			s.logger.Warn("discovery: record failed",
				"err", err,
				"contract_id", hit.ContractID,
				"event_type", hit.EventType)
		}
		cancel()
	}
}
