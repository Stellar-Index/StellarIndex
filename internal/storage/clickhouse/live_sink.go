package clickhouse

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/worker"
)

// LiveSinkOptions configures a [NewLiveSink].
type LiveSinkOptions struct {
	// BufferSize is the channel depth (default 512). At the live ledger rate
	// (~1 every 5s) this is hours of headroom; it only matters during a CH
	// write hiccup, where a full buffer DROPS (see PushLedger) rather than
	// blocking — the live ingest hot path must never stall on ClickHouse.
	BufferSize int
	// FlushInterval bounds how long a partial batch waits before the worker
	// flushes it to ClickHouse (default 2s). This is the freshness floor: CH
	// lags the chain by at most ~FlushInterval + the indexer's own lag.
	FlushInterval time.Duration
	// WriteTimeout caps one flush (default 30s).
	WriteTimeout time.Duration
	// StopTimeout bounds the whole of [LiveSink.Stop] — the in-flight
	// flush, the drain, the final flush and Close together (default
	// WriteTimeout). Ledgers still unflushed when it expires are logged
	// and left to ch-live-catchup.
	StopTimeout time.Duration
	// MaxBufferLedgers caps how many ledgers the Sink holds in memory before it drops incoming
	// extracts during a sustained ClickHouse outage (default 4096). BufferSize bounds only the
	// worker's INBOX; the Sink's per-table row slices are not bounded by it (a failing flush keeps
	// them while the worker keeps appending). Combined, the backlog is ~BufferSize +
	// MaxBufferLedgers ledgers; beyond that the ch-live-catchup gap-scan timer heals the gap.
	MaxBufferLedgers int
	// Logger for worker warn/error lines; nil → slog.Default().
	Logger *slog.Logger
}

// LiveSink is the real-time fan-out adapter (ADR-0034) batching pushed ledger extracts into ClickHouse; PushLedger is NON-BLOCKING and DROPS on a full buffer, leaving a lake HOLE that ch-live-catchup must heal below CH_max.
type LiveSink struct {
	sink        *Sink
	logger      *slog.Logger
	timeout     time.Duration
	stopTimeout time.Duration
	flush       time.Duration

	// base parents every CH operation; Stop cancels it when stopTimeout
	// elapses, which is what makes the shutdown drain bounded.
	base       context.Context
	baseCancel context.CancelFunc

	ch       chan LedgerExtract
	stopOnce sync.Once
	stopping chan struct{}
	done     chan struct{}

	mu       sync.Mutex
	stopped  bool   // set under mu before stopping closes; PushLedger drops after it.
	written  uint64 // ledgers DURABLY flushed to CH (post-Flush, not enqueue).
	buffered uint64 // ledgers accepted into the in-memory buffer (pre-flush).
	dropped  uint64 // ledgers dropped: full channel (PushLedger) or full buffer (Add).
	errored  uint64 // failed Add / Flush operations (a sustained-outage signal).
}

// NewLiveSink opens a CH connection and returns a stopped LiveSink; call Start before PushLedger and Stop before exit to drain.
func NewLiveSink(ctx context.Context, addr string, opts LiveSinkOptions) (*LiveSink, error) {
	if opts.BufferSize <= 0 {
		opts.BufferSize = 512
	}
	if opts.FlushInterval <= 0 {
		opts.FlushInterval = 2 * time.Second
	}
	if opts.WriteTimeout <= 0 {
		opts.WriteTimeout = 30 * time.Second
	}
	if opts.MaxBufferLedgers <= 0 {
		opts.MaxBufferLedgers = 4096
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	// flushEvery is large so Add buffers; the worker's ticker drives flushes. Every flush writes
	// stellar.supply_flows, so ensure it exists (idempotent) before Open's schema check, which
	// refuses a lake missing any table Flush writes to.
	if err := EnsureSupplyFlowsTable(ctx, addr); err != nil {
		return nil, err
	}
	sink, err := Open(ctx, addr, 1000)
	if err != nil {
		return nil, err
	}
	// Cap the underlying Sink's in-memory buffers so a sustained CH
	// outage can't grow the heap unbounded on the shared r1 host.
	sink.SetMaxBufferLedgers(opts.MaxBufferLedgers)
	return newLiveSink(sink, logger, opts), nil
}

// newLiveSink wires a LiveSink around an already-open Sink. opts must
// already carry NewLiveSink's defaults for BufferSize/FlushInterval/WriteTimeout.
func newLiveSink(sink *Sink, logger *slog.Logger, opts LiveSinkOptions) *LiveSink {
	stopTimeout := opts.StopTimeout
	if stopTimeout <= 0 {
		stopTimeout = opts.WriteTimeout
	}
	base, baseCancel := context.WithCancel(context.Background()) //nolint:gosec // G118 false positive: baseCancel is stored and called from Stop
	l := &LiveSink{
		sink:        sink,
		logger:      logger,
		timeout:     opts.WriteTimeout,
		stopTimeout: stopTimeout,
		flush:       opts.FlushInterval,
		base:        base,
		baseCancel:  baseCancel,
		ch:          make(chan LedgerExtract, opts.BufferSize),
		stopping:    make(chan struct{}),
		done:        make(chan struct{}),
	}
	// Credit `written` wherever a flush succeeds, not only in doFlush: once
	// the buffer passes flushEvery (a recovering CH outage), Add flushes inline.
	sink.onFlushed = func(n int) { l.add64(&l.written, uint64(n)) }
	return l
}

// Start launches the drain worker. Call once.
func (l *LiveSink) Start() { go l.run() }

// PushLedger enqueues a ledger's extract for the worker, NON-BLOCKING: it drops (DroppedCount++) on a full buffer or shutdown, and a drop stalls the lake watermark until the catch-up timer heals it.
func (l *LiveSink) PushLedger(ext LedgerExtract) {
	l.mu.Lock()
	stopped, sent := l.stopped, false
	if !stopped {
		select {
		case l.ch <- ext:
			sent = true
		default:
		}
	}
	if !sent {
		l.dropped++
	}
	l.mu.Unlock()
	switch {
	case sent:
	case stopped:
		l.logger.Warn("clickhouse live-sink: push after Stop — ledger DROPPED from the live lake write",
			"ledger", ext.Ledger.LedgerSeq)
	default:
		l.logger.Warn("clickhouse live-sink: buffer full — ledger DROPPED from the live lake write",
			"ledger", ext.Ledger.LedgerSeq, "buffer", cap(l.ch))
	}
}

func (l *LiveSink) run() {
	defer close(l.done)
	// An unrecovered panic in ANY goroutine kills the whole process this sink
	// is linked into. Registered after close(l.done) so it unwinds first and
	// done still closes: a contained panic must not leave Stop() blocked
	// forever on a worker that is already gone.
	defer worker.Recover(l.logger, "clickhouse-live-sink-drain")
	ticker := time.NewTicker(l.flush)
	defer ticker.Stop()
	for {
		select {
		case ext := <-l.ch:
			l.add(ext)
		case <-ticker.C:
			l.doFlush()
		case <-l.stopping:
			// Drain whatever's buffered, then a final flush, then exit.
			for {
				select {
				case ext := <-l.ch:
					l.add(ext)
				default:
					l.doFlush()
					return
				}
			}
		}
	}
}

func (l *LiveSink) add(ext LedgerExtract) {
	ctx, cancel := context.WithTimeout(l.base, l.timeout)
	defer cancel()
	if err := l.sink.Add(ctx, ext); err != nil {
		// A full buffer is a bounded DROP (heals via ch-live-catchup),
		// not a write ERROR. Distinguish so the metric/log don't conflate a
		// healthy back-pressure drop with a genuine CH write failure.
		if errors.Is(err, ErrBufferFull) {
			l.bump(&l.dropped)
			l.logger.Warn("clickhouse live-sink: buffer full — extract dropped",
				"ledger", ext.Ledger.LedgerSeq, "cap_ledgers", l.sink.BufferedLedgers())
			return
		}
		// Any other error is Add's inline flush failing: ext is buffered
		// and goes out with the next successful flush.
		l.bump(&l.errored)
		l.logger.Warn("clickhouse live-sink: inline flush failed", "ledger", ext.Ledger.LedgerSeq, "err", err)
	}
	// Count buffer-enqueue as `buffered`, NOT `written`. `written` is
	// credited only by a successful Flush (Sink.onFlushed), so the metric
	// can't claim durability the lake doesn't have during a CH stall.
	l.bump(&l.buffered)
}

func (l *LiveSink) doFlush() {
	ctx, cancel := context.WithTimeout(l.base, l.timeout)
	defer cancel()
	if err := l.sink.Flush(ctx); err != nil {
		l.bump(&l.errored)
		l.logger.Warn("clickhouse live-sink: flush failed", "err", err, "buffered_ledgers", l.sink.BufferedLedgers())
	}
}

// Stop signals shutdown, drains the buffer, flushes and closes the CH conn within StopTimeout (sizes the indexer's systemd stop timeout, pipeline.IndexerStopTimeout); idempotent.
func (l *LiveSink) Stop() {
	l.stopOnce.Do(func() {
		timer := time.AfterFunc(l.stopTimeout, l.baseCancel)
		defer timer.Stop()
		defer l.baseCancel()
		l.mu.Lock()
		l.stopped = true
		l.mu.Unlock()
		close(l.stopping)
		<-l.done
		ctx, cancel := context.WithTimeout(l.base, l.timeout)
		defer cancel()
		// Close retries the flush, so a failure here means rows never
		// reached the lake; ch-live-catchup re-derives them.
		pending := l.sink.BufferedLedgers()
		if err := l.sink.Close(ctx); err != nil {
			l.bump(&l.errored)
			l.logger.Warn("clickhouse live-sink: shutdown flush or close failed — unflushed ledgers left to ch-live-catchup",
				"err", err, "unflushed_ledgers", pending, "stop_timeout", l.stopTimeout.String())
		}
	})
}

func (l *LiveSink) bump(p *uint64) {
	l.mu.Lock()
	*p++
	l.mu.Unlock()
}

func (l *LiveSink) add64(p *uint64, n uint64) {
	l.mu.Lock()
	*p += n
	l.mu.Unlock()
}

// WrittenCount / BufferedCount / DroppedCount / ErroredCount expose worker counters; WrittenCount is DURABLY flushed ledgers, BufferedCount pre-flush, and their gap is the backlog signal during a CH stall.
func (l *LiveSink) WrittenCount() uint64  { l.mu.Lock(); defer l.mu.Unlock(); return l.written }
func (l *LiveSink) BufferedCount() uint64 { l.mu.Lock(); defer l.mu.Unlock(); return l.buffered }
func (l *LiveSink) DroppedCount() uint64  { l.mu.Lock(); defer l.mu.Unlock(); return l.dropped }
func (l *LiveSink) ErroredCount() uint64  { l.mu.Lock(); defer l.mu.Unlock(); return l.errored }
