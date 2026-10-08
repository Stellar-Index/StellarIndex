package discovery

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/worker"
)

// AsyncSink is a non-blocking [Recorder] adapter for the dispatcher hot path:
// Push enqueues, one worker records. A repeat (ContractID, Kind, EventType,
// Symbol) key is not re-enqueued but folded into a pending delta that Stop
// flushes with the true [Hit.Count], so event_count tracks real volume. A full
// buffer drops the hit (counted in DroppedCount): stalling dispatch is worse
// than losing one sighting of a busy contract.
type AsyncSink struct {
	rec          Recorder
	logger       *slog.Logger
	timeout      time.Duration
	drainTimeout time.Duration

	// drainCtx parents every Record call; Stop cancels it once
	// drainTimeout has elapsed so the shutdown drain has an absolute
	// deadline instead of RecordTimeout per buffered hit.
	drainCtx    context.Context
	drainCancel context.CancelFunc

	ch        chan Hit
	startOnce sync.Once
	stopOnce  sync.Once
	done      chan struct{}

	mu      sync.Mutex
	stopped bool
	dropped uint64
	skipped uint64
	failed  uint64
	seen    map[string]struct{}
	// pending accumulates the count + latest ledger/observed-at for
	// keys already in `seen` whose repeat Pushes were skipped rather
	// than enqueued. Flushed to the Recorder on Stop.
	pending map[string]*pendingDelta
}

// pendingDelta is one dedup key's unrecorded observation count and its latest
// hit, so the flush carries the true last-seen ledger.
type pendingDelta struct {
	hit   Hit
	count int64
}

// seenKey is the dedup key. Push's mark and both rollbacks must agree on it
// byte-for-byte, or the rollback silently stops working. Kind and Symbol are
// empty on a plain SEP-41 hit, so its key equals the EventType-only one.
func seenKey(hit Hit) string {
	return hit.ContractID + "\x00" + string(hit.Kind) + "\x00" + string(hit.EventType) + "\x00" + hit.Symbol
}

// AsyncSinkOptions configures a [NewAsyncSink].
type AsyncSinkOptions struct {
	// BufferSize is the channel depth; must be > 0. Production uses 1024, a
	// safety net for restart bursts given dedup.
	BufferSize int

	// RecordTimeout caps how long a single Recorder.Record call may
	// block the worker. Default 2 seconds. A slow Postgres write
	// fails the record (logged) rather than holding up the queue.
	RecordTimeout time.Duration

	// DrainTimeout bounds the whole drain in [AsyncSink.Stop] (default 10s);
	// hits still buffered then are abandoned and counted as dropped.
	DrainTimeout time.Duration

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
	drainTimeout := opts.DrainTimeout
	if drainTimeout <= 0 {
		drainTimeout = 10 * time.Second
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	drainCtx, drainCancel := context.WithCancel(context.Background()) //nolint:gosec // G118 false positive: drainCancel is stored and called from Stop
	return &AsyncSink{
		rec:          rec,
		logger:       logger,
		timeout:      timeout,
		drainTimeout: drainTimeout,
		drainCtx:     drainCtx,
		drainCancel:  drainCancel,
		ch:           make(chan Hit, opts.BufferSize),
		done:         make(chan struct{}),
		seen:         make(map[string]struct{}),
		pending:      make(map[string]*pendingDelta),
	}
}

// Start launches the drain worker. Idempotent; calling twice is a
// no-op. Caller must Stop before the process exits to flush
// pending records.
func (s *AsyncSink) Start() {
	s.startOnce.Do(func() { go s.run() })
}

// Push enqueues a Hit without blocking: dropped after Stop or when the buffer
// is full, folded into the key's pending delta if already enqueued. It
// satisfies dispatcher.DiscoverySink structurally (import cycle). The whole
// body, send included, holds s.mu so a concurrent Stop cannot close the
// channel under it; safe because the send is non-blocking.
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
		if p, ok := s.pending[key]; ok {
			p.count++
			if hit.Ledger >= p.hit.Ledger {
				p.hit = hit
			}
		} else {
			s.pending[key] = &pendingDelta{hit: hit, count: 1}
		}
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

// Stop closes the input and waits at most DrainTimeout for the drain; the
// indexer's systemd stop timeout (pipeline.IndexerStopTimeout) is sized from
// that bound. Idempotent.
func (s *AsyncSink) Stop() {
	s.stopOnce.Do(func() {
		timer := time.AfterFunc(s.drainTimeout, s.drainCancel)
		defer timer.Stop()
		defer s.drainCancel()
		// Mark stopped BEFORE closing so a concurrent Push can never
		// reach the send — see [AsyncSink.Push].
		s.mu.Lock()
		s.stopped = true
		s.mu.Unlock()
		close(s.ch)
		<-s.done
	})
}

// DroppedCount returns hits dropped because the buffer was full or the sink
// stopped; a rising value usually means a Postgres outage.
func (s *AsyncSink) DroppedCount() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.dropped
}

// SkippedCount returns hits folded into an already-enqueued key; a high
// ratio is normal.
func (s *AsyncSink) SkippedCount() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.skipped
}

// FailedCount returns failed Recorder writes, bridged to
// obs.DiscoveryRecordFailuresTotal so a recorder outage alerts.
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
	// An unrecovered panic in ANY goroutine kills the whole process this
	// sink is linked into. Registered after close(s.done) so it unwinds
	// first and done still closes: a contained panic must not leave Stop
	// blocked forever on a worker that is already gone.
	defer worker.Recover(s.logger, "discovery-async-sink-drain")
	abandoned := 0
	defer func() {
		if abandoned > 0 {
			s.logger.Warn("discovery: drain deadline reached — buffered hits abandoned",
				"abandoned", abandoned, "drain_timeout", s.drainTimeout.String())
		}
	}()
	for hit := range s.ch {
		if s.drainCtx.Err() != nil {
			s.mu.Lock()
			s.dropped++
			s.mu.Unlock()
			abandoned++
			continue
		}
		ctx, cancel := context.WithTimeout(s.drainCtx, s.timeout)
		if err := s.rec.Record(ctx, hit); err != nil {
			// Counted, not just logged, so a recorder outage that stops
			// discovered_assets growing is visible.
			s.mu.Lock()
			s.failed++
			// Roll back the seen-mark: the seen-set suppresses every later
			// Push, so a contract first sighted during an outage would
			// otherwise never be recorded.
			delete(s.seen, seenKey(hit))
			s.mu.Unlock()
			s.logger.Warn("discovery: record failed",
				"err", err,
				"contract_id", hit.ContractID,
				"event_type", hit.EventType)
		}
		cancel()
	}
	s.flushPending()
}

// flushPending records every pending delta once, after the drain. Failures are
// logged and counted, not retried; the process is shutting down.
func (s *AsyncSink) flushPending() {
	s.mu.Lock()
	batch := s.pending
	s.pending = make(map[string]*pendingDelta)
	s.mu.Unlock()

	for _, p := range batch {
		hit := p.hit
		hit.Count = p.count
		ctx, cancel := context.WithTimeout(s.drainCtx, s.timeout)
		err := s.rec.Record(ctx, hit)
		cancel()
		if err != nil {
			s.mu.Lock()
			s.failed++
			s.mu.Unlock()
			s.logger.Warn("discovery: flush pending delta failed",
				"err", err,
				"contract_id", hit.ContractID,
				"count", p.count)
		}
	}
}
