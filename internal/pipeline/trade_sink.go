package pipeline

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/StellarIndex/stellar-index/internal/canonical"
	"github.com/StellarIndex/stellar-index/internal/obs"
	"github.com/StellarIndex/stellar-index/internal/sources/external"
	"github.com/StellarIndex/stellar-index/internal/storage/timescale"
)

// tradeWriter is the narrow subset of [*timescale.Store] the resilient
// trade-sink helpers depend on. Declared as an interface so the retry /
// buffer logic is unit-testable with a fake that fails on demand,
// without a real Postgres (see trade_sink_test.go). *timescale.Store
// satisfies it via its exported methods.
type tradeWriter interface {
	BatchInsertTrades(ctx context.Context, trades []canonical.Trade) error
	InsertTrade(ctx context.Context, t canonical.Trade) error
	WouldPopulateUSDVolume(ctx context.Context, t canonical.Trade) bool
}

// Backpressure / retry tunables for the 2026-07-06 Postgres-outage fix.
//
// During a Postgres outage the trade sink used to DROP writes while the
// ledger cursor kept advancing (a ~205-ledger sdex hole healed from the
// lake, plus unrecoverable CEX drops). The sink now RETRIES an
// infrastructure-classified failure (see [timescale.IsInfraError]) with
// capped exponential backoff. For on-chain trades the retry BLOCKS the
// drain goroutine, which fills the events channel and stalls
// `ProcessLedger`'s `events <- ev` send — so the ledger cursor cannot
// advance past ledgers whose trades haven't durably landed (ADR-0041's
// enqueue-advance cursor is thereby held behind the un-landed writes,
// with nothing dropped).
const (
	// infraRetryInitialBackoff is the first sleep before re-attempting
	// an infra-failed insert.
	infraRetryInitialBackoff = 100 * time.Millisecond
	// infraRetryMaxBackoff caps the exponential backoff — a 17-minute
	// outage retries every ~5s once ramped, which paces the load on a
	// recovering Postgres without abandoning the write.
	infraRetryMaxBackoff = 5 * time.Second
	// infraRetryLogEvery limits log volume during a sustained outage:
	// log the first attempt then every Nth after.
	infraRetryLogEvery = 20
)

// isCtxErr reports whether err is a context cancellation / deadline —
// i.e. shutdown, not a retryable infra fault. Callers stop retrying and
// surface the abandoned work (recoverable from the CH lake) on this.
func isCtxErr(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

// retryInfra runs do and, while it returns an INFRASTRUCTURE-classified
// error ([timescale.IsInfraError]), retries with capped exponential
// backoff until do succeeds, returns a non-infra error, or ctx is done.
//
// Return contract:
//   - nil                         — do eventually succeeded.
//   - a non-infra (data) error    — surfaced immediately for the caller
//     to error-and-skip; NEVER retried (a constraint / numeric fault is
//     permanent for that row).
//   - ctx.Err()                   — the context was cancelled mid-retry
//     (shutdown); the caller surfaces the abandoned, lake-recoverable
//     work.
//
// Blocking is the point: while this loops, the calling drain goroutine
// is not consuming the events channel, which is the backpressure that
// gates the on-chain ledger cursor (2026-07-06 outage).
func retryInfra(ctx context.Context, logger *slog.Logger, op string, do func(context.Context) error) error {
	backoff := infraRetryInitialBackoff
	attempts := 0
	for {
		err := do(ctx)
		switch {
		case err == nil:
			if attempts > 0 {
				obs.TradeInsertRetriesTotal.WithLabelValues("recovered").Inc()
				logger.Info("trade sink recovered after infra retry", "op", op, "retries", attempts)
			}
			return nil
		case isCtxErr(err):
			obs.TradeInsertRetriesTotal.WithLabelValues("abandoned").Inc()
			return err
		case !timescale.IsInfraError(err):
			return err // data fault — caller error-and-skips
		}

		attempts++
		obs.TradeInsertRetriesTotal.WithLabelValues("retry").Inc()
		if attempts == 1 || attempts%infraRetryLogEvery == 0 {
			logger.Warn("infrastructure fault on trade insert — retrying with backpressure (on-chain cursor will NOT advance until this lands)",
				"op", op, "attempt", attempts, "backoff", backoff.String(), "err", err)
		}
		select {
		case <-ctx.Done():
			obs.TradeInsertRetriesTotal.WithLabelValues("abandoned").Inc()
			return ctx.Err()
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, infraRetryMaxBackoff)
	}
}

// flushTradeBatch writes one buffered trade batch with the resilient
// failure policy (ADR-0041 / 2026-07-06 outage). It replaces the old
// "batch failed → per-row → drop on error" path, which silently lost
// writes during a Postgres outage while the cursor advanced.
//
// Behaviour by failure class:
//   - success                    — return.
//   - non-infra (data fault or    — isolate per-row (the pre-existing
//     lock contention)             belt-and-braces fallback), so one bad
//     row can't sink the batch and contention resolves via single-row
//     lock order.
//   - infrastructure fault        — split by lake-recoverability:
//     ON-CHAIN trades (sdex + Soroban DEXes) block-and-retry so the
//     cursor stalls until they land (nothing dropped); EXTERNAL CEX/FX
//     trades (no cursor, vendor-refillable) go to the bounded async
//     retry buffer, which drops-oldest under sustained overflow.
//
// extBuf may be nil (shutdown drain / projector / backfill paths that
// carry no external trades) — then every trade block-and-retries within
// the caller's bounded context.
func flushTradeBatch(ctx context.Context, logger *slog.Logger, w tradeWriter, extBuf *externalRetryBuffer, batch []canonical.Trade, workerID int) {
	if len(batch) == 0 {
		return
	}
	err := w.BatchInsertTrades(ctx, batch)
	if err == nil {
		return
	}

	if !timescale.IsInfraError(err) {
		// Data fault OR row-lock contention (deadlock / serialization).
		// Isolate per-row: one bad row must not sink the batch, and
		// contention clears via single-row lock acquisition. This is the
		// 2026-07-05 batch-sort commit's belt-and-braces fallback, now
		// with per-row infra-resilience inside persistTrade.
		logger.Warn("batch trade insert failed (non-infra); isolating per-row",
			"worker", workerID, "batch_size", len(batch), "err", err)
		for _, t := range batch {
			persistTrade(ctx, logger, w, t)
		}
		return
	}

	// Infrastructure fault: Postgres is unreachable / restarting. Route
	// each trade by whether it is recoverable from the CH lake.
	logger.Warn("batch trade insert failed (infrastructure) — routing to backpressure retry",
		"worker", workerID, "batch_size", len(batch), "err", err)
	onchain := batch[:0:0] // fresh backing array on first append; never aliases batch
	for _, t := range batch {
		if extBuf != nil && !external.IsOnChain(t.Source) {
			extBuf.enqueue(t) // bounded async retry, drop-oldest on overflow
			continue
		}
		onchain = append(onchain, t)
	}
	if len(onchain) > 0 {
		retryOnChainBatchBlocking(ctx, logger, w, onchain)
	}
}

// retryOnChainBatchBlocking block-retries an on-chain trade batch until
// it lands (cursor gating) or the context is cancelled. On shutdown the
// abandoned ledger range is logged at ERROR — the raw ops are durable in
// the CH lake (ADR-0034), so the range is re-derivable.
func retryOnChainBatchBlocking(ctx context.Context, logger *slog.Logger, w tradeWriter, batch []canonical.Trade) {
	err := retryInfra(ctx, logger, "onchain_batch", func(c context.Context) error {
		return w.BatchInsertTrades(c, batch)
	})
	switch {
	case err == nil:
		return
	case isCtxErr(err):
		lo, hi := ledgerRange(batch)
		logger.Error("on-chain trade batch abandoned on shutdown — recoverable from the CH lake (ADR-0034); re-derive this ledger range",
			"batch_size", len(batch), "ledger_from", lo, "ledger_to", hi, "err", err)
	default:
		// A non-infra fault surfaced from a retry attempt (a bad row that
		// only errors once the DB is reachable) → isolate per-row.
		logger.Warn("on-chain trade batch hit a non-infra fault after retry; isolating per-row",
			"batch_size", len(batch), "err", err)
		for _, t := range batch {
			persistTrade(ctx, logger, w, t)
		}
	}
}

// ledgerRange returns the min/max ledger across a trade batch, for the
// re-derive hint in abandon logs. lo==0 means the batch was empty.
func ledgerRange(batch []canonical.Trade) (lo, hi uint32) {
	for _, t := range batch {
		if lo == 0 || t.Ledger < lo {
			lo = t.Ledger
		}
		if t.Ledger > hi {
			hi = t.Ledger
		}
	}
	return lo, hi
}

// externalRetryBufferMaxDepth bounds the in-memory retry buffer for
// external (CEX/FX) trades. ~50k trades is a few minutes of a busy CEX
// feed — enough to ride out a short Postgres blip without loss, bounded
// so a long outage can't grow memory without limit. Overflow drops the
// OLDEST (freshest prices are the ones worth keeping) and is counted.
const externalRetryBufferMaxDepth = 50_000

// externalRetryInterval is how often the background goroutine re-attempts
// the buffered external trades.
const externalRetryInterval = 2 * time.Second

// externalRetryBuffer is the bounded, drop-oldest, async retry queue for
// external CEX/FX trades that hit an infrastructure fault (ADR-0041 /
// 2026-07-06 outage). External trades have no ledger cursor and are
// vendor-refillable, so — unlike on-chain trades — they must NOT block
// the pipeline: they are buffered here and retried by [run]; if the
// bound is exceeded the oldest are dropped with a loud metric.
type externalRetryBuffer struct {
	mu       sync.Mutex
	ring     []canonical.Trade
	maxDepth int
	w        tradeWriter
	logger   *slog.Logger
}

func newExternalRetryBuffer(w tradeWriter, logger *slog.Logger, maxDepth int) *externalRetryBuffer {
	if maxDepth <= 0 {
		maxDepth = externalRetryBufferMaxDepth
	}
	return &externalRetryBuffer{maxDepth: maxDepth, w: w, logger: logger}
}

// enqueue adds one external trade to the tail, dropping the oldest if
// the buffer is full.
func (b *externalRetryBuffer) enqueue(t canonical.Trade) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.ring = append(b.ring, t)
	b.trimOldestLocked()
}

// requeueFrontLocked re-inserts a failed drain batch at the FRONT (it is
// older than anything enqueued meanwhile), then trims to the bound.
// Caller holds b.mu.
func (b *externalRetryBuffer) requeueFrontLocked(older []canonical.Trade) {
	b.ring = append(older, b.ring...)
	b.trimOldestLocked()
}

// trimOldestLocked drops from the FRONT (oldest) until the ring fits
// maxDepth, counting every dropped trade so genuine loss is never
// silent. Caller holds b.mu.
func (b *externalRetryBuffer) trimOldestLocked() {
	if over := len(b.ring) - b.maxDepth; over > 0 {
		for _, dropped := range b.ring[:over] {
			obs.SourceInsertErrorsTotal.WithLabelValues(dropped.Source, "dropped").Inc()
		}
		b.logger.Warn("external trade retry buffer full — dropped oldest (vendor-refillable per ADR-0041)",
			"dropped", over, "max_depth", b.maxDepth)
		// Copy to a fresh slice so the dropped head can be GC'd.
		b.ring = append([]canonical.Trade(nil), b.ring[over:]...)
	}
	obs.TradeInsertBufferDepth.Set(float64(len(b.ring)))
}

// run drives the background retrier until ctx is cancelled, then does a
// final bounded drain of whatever remains.
func (b *externalRetryBuffer) run(ctx context.Context) {
	ticker := time.NewTicker(externalRetryInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			b.finalDrain()
			return
		case <-ticker.C:
			b.drainOnce(ctx)
		}
	}
}

// drainOnce attempts the whole current ring in one batch. The ring is
// emptied for the attempt (new arrivals accumulate into a fresh ring),
// so a concurrent enqueue never evicts an in-flight entry. On infra
// failure or shutdown the batch is re-queued at the front; on a data
// fault the batch is isolated per-row so one bad row can't wedge it.
func (b *externalRetryBuffer) drainOnce(ctx context.Context) {
	b.mu.Lock()
	if len(b.ring) == 0 {
		b.mu.Unlock()
		return
	}
	batch := b.ring
	b.ring = nil
	obs.TradeInsertBufferDepth.Set(0)
	b.mu.Unlock()

	err := b.w.BatchInsertTrades(ctx, batch)
	switch {
	case err == nil:
		obs.TradeInsertRetriesTotal.WithLabelValues("recovered").Inc()
		return
	case timescale.IsInfraError(err) || isCtxErr(err):
		if timescale.IsInfraError(err) {
			obs.TradeInsertRetriesTotal.WithLabelValues("retry").Inc()
		}
		b.mu.Lock()
		b.requeueFrontLocked(batch)
		b.mu.Unlock()
		return
	default:
		// Data fault in the batch → isolate per-row. Landed rows are
		// gone; the offending rows are dropped loudly (external is
		// vendor-refillable — better a counted drop than a wedged buffer).
		for _, t := range batch {
			if e := b.w.InsertTrade(ctx, t); e != nil {
				obs.SourceInsertErrorsTotal.WithLabelValues(t.Source, "dropped").Inc()
				b.logger.Warn("external trade dropped during per-row isolation (data fault; vendor-refillable)",
					"source", t.Source, "err", e)
			}
		}
	}
}

// finalDrain makes one last bounded attempt to land the buffer at
// shutdown, using a fresh context (the parent is already cancelled).
//
//nolint:contextcheck // intentional fresh context: the parent ctx is cancelled at shutdown, so threading it would fail every insert instantly (same pattern as drainBufferedEvents / flushShutdown).
func (b *externalRetryBuffer) finalDrain() {
	b.mu.Lock()
	pending := len(b.ring)
	b.mu.Unlock()
	if pending == 0 {
		return
	}
	fctx, cancel := context.WithTimeout(context.Background(), drainTimeout)
	defer cancel()
	b.drainOnce(fctx)

	b.mu.Lock()
	remaining := len(b.ring)
	b.mu.Unlock()
	if remaining > 0 {
		b.logger.Warn("external trade retry buffer not fully drained at shutdown — remaining entries are vendor-refillable (ADR-0041)",
			"remaining", remaining)
	}
}
