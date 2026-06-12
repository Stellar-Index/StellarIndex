package clickhouse

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"
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
	// MaxBufferLedgers caps how many ledgers the underlying Sink holds in
	// memory before it starts BOUNDED-DROPPING incoming extracts during a
	// sustained ClickHouse outage (G12-01; default 4096). The channel
	// (BufferSize) bounds the worker's INBOX; this bounds the Sink's per-table
	// row slices, which the channel cap does NOT (a failing flush keeps them
	// intact while the worker keeps appending). Combined, the live path's CH
	// backlog is bounded at ~BufferSize + MaxBufferLedgers ledgers; beyond that
	// the ch-live-catchup gap-scan timer heals the gap.
	MaxBufferLedgers int
	// Logger for worker warn/error lines; nil → slog.Default().
	Logger *slog.Logger
}

// LiveSink is the real-time fan-out adapter (ADR-0034): the indexer pushes each
// ledger's structural [LedgerExtract] and a single worker goroutine batches them
// into ClickHouse on a short interval, keeping the lake within ~seconds of the
// chain (vs the ~10-min ch-live-catchup timer, which remains as the completeness
// backstop for anything this best-effort sink drops under pressure).
//
// Safety: PushLedger is NON-BLOCKING — on a full buffer it DROPS the whole
// LedgerExtract (DroppedCount++) rather than back-pressuring into the live
// ingest loop. A slow or down ClickHouse therefore can never stall Postgres
// ingest / pricing freshness. Only the lake's real-time edge degrades under CH
// pressure.
//
// Completeness: a drop (or a mid-flush write error) leaves a HOLE in the lake.
// The ch-live-catchup timer heals holes — but ONLY if it gap-scans below
// CH_max, not just extends the tip (a tip-only [CH_max+1,tip] catch-up can never
// re-fill a hole the sink already wrote past). The real-time projector
// (ADR-0034 #10) does NOT trust the lake to be hole-free: it reads
// contract_events only up to ContiguousWatermark — the highest ledger with no
// hole below it — so an unhealed drop stalls the projector at the hole rather
// than silently losing the dropped ledger's events.
type LiveSink struct {
	sink    *Sink
	logger  *slog.Logger
	timeout time.Duration
	flush   time.Duration

	ch       chan LedgerExtract
	stopOnce sync.Once
	stopping chan struct{}
	done     chan struct{}

	mu       sync.Mutex
	written  uint64 // ledgers DURABLY flushed to CH (post-Flush, not enqueue).
	buffered uint64 // ledgers accepted into the in-memory buffer (pre-flush).
	dropped  uint64 // ledgers dropped: full channel (PushLedger) or full buffer (Add).
	errored  uint64 // failed Add / Flush operations (a sustained-outage signal).
}

// NewLiveSink opens a CH connection (flushEvery high — the worker controls flush
// cadence via FlushInterval, not per-Add) and returns a stopped LiveSink; call
// Start before PushLedger, and Stop before exit to drain.
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
	// flushEvery large so Add buffers; the worker's ticker drives flushes at
	// FlushInterval (real-time at the low live rate; batched under burst).
	sink, err := Open(ctx, addr, 1000)
	if err != nil {
		return nil, err
	}
	// G12-01: cap the underlying Sink's in-memory buffers so a sustained CH
	// outage can't grow the heap unbounded on the shared r1 host.
	sink.SetMaxBufferLedgers(opts.MaxBufferLedgers)
	// The decode-at-ingest supply path writes stellar.supply_flows in every
	// flush; ensure it exists before the worker starts (idempotent).
	if err := EnsureSupplyFlowsTable(ctx, addr); err != nil {
		_ = sink.Close(ctx)
		return nil, err
	}
	return &LiveSink{
		sink:     sink,
		logger:   logger,
		timeout:  opts.WriteTimeout,
		flush:    opts.FlushInterval,
		ch:       make(chan LedgerExtract, opts.BufferSize),
		stopping: make(chan struct{}),
		done:     make(chan struct{}),
	}, nil
}

// Start launches the drain worker. Call once.
func (l *LiveSink) Start() { go l.run() }

// PushLedger enqueues a ledger's structural extract for the worker. NON-BLOCKING:
// drops (DroppedCount++) if the buffer is full or shutdown is in progress, so it
// never stalls the caller (the live ingest loop).
func (l *LiveSink) PushLedger(ext LedgerExtract) {
	select {
	case l.ch <- ext:
	default:
		l.bump(&l.dropped)
	}
}

func (l *LiveSink) run() {
	defer close(l.done)
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
	ctx, cancel := context.WithTimeout(context.Background(), l.timeout)
	defer cancel()
	if err := l.sink.Add(ctx, ext); err != nil {
		// G12-01: a full buffer is a bounded DROP (heals via ch-live-catchup),
		// not a write ERROR. Distinguish so the metric/log don't conflate a
		// healthy back-pressure drop with a genuine CH write failure.
		if errors.Is(err, ErrBufferFull) {
			l.bump(&l.dropped)
			l.logger.Warn("clickhouse live-sink: buffer full — extract dropped",
				"ledger", ext.Ledger.LedgerSeq, "cap_ledgers", l.sink.BufferedLedgers())
			return
		}
		l.bump(&l.errored)
		l.logger.Warn("clickhouse live-sink: add failed", "ledger", ext.Ledger.LedgerSeq, "err", err)
		return
	}
	// G12-02: count buffer-enqueue as `buffered`, NOT `written`. `written` is
	// reserved for ledgers DURABLY flushed to CH (doFlush), so the metric
	// can't claim durability the lake doesn't have during a CH stall.
	l.bump(&l.buffered)
}

func (l *LiveSink) doFlush() {
	ctx, cancel := context.WithTimeout(context.Background(), l.timeout)
	defer cancel()
	// Snapshot how many ledgers are about to be flushed so a success can
	// credit `written` accurately (Flush clears the buffer on success).
	pending := uint64(l.sink.BufferedLedgers())
	if err := l.sink.Flush(ctx); err != nil {
		l.bump(&l.errored)
		l.logger.Warn("clickhouse live-sink: flush failed", "err", err, "buffered_ledgers", pending)
		return
	}
	l.add64(&l.written, pending)
}

// Stop signals shutdown, drains the buffer + final flush, closes the CH conn.
// Idempotent.
func (l *LiveSink) Stop() {
	l.stopOnce.Do(func() {
		close(l.stopping)
		<-l.done
		ctx, cancel := context.WithTimeout(context.Background(), l.timeout)
		defer cancel()
		_ = l.sink.Close(ctx)
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

// WrittenCount / BufferedCount / DroppedCount / ErroredCount expose worker
// counters for metrics + the shutdown log line. WrittenCount is ledgers
// DURABLY flushed to CH (post-Flush); BufferedCount is ledgers accepted into
// the in-memory buffer (pre-flush) — the two diverge by the unflushed backlog,
// which during a CH stall is the early-warning signal.
func (l *LiveSink) WrittenCount() uint64  { l.mu.Lock(); defer l.mu.Unlock(); return l.written }
func (l *LiveSink) BufferedCount() uint64 { l.mu.Lock(); defer l.mu.Unlock(); return l.buffered }
func (l *LiveSink) DroppedCount() uint64  { l.mu.Lock(); defer l.mu.Unlock(); return l.dropped }
func (l *LiveSink) ErroredCount() uint64  { l.mu.Lock(); defer l.mu.Unlock(); return l.errored }
