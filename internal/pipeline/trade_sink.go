package pipeline

import (
	"context"
	"errors"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/sources/external"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// tradeWriter is the narrow subset of *timescale.Store the resilient helpers need, so retry/buffer logic is testable with a fake.
type tradeWriter interface {
	BatchInsertTrades(ctx context.Context, trades []canonical.Trade) error
	InsertTrade(ctx context.Context, t canonical.Trade) error
	WouldPopulateUSDVolume(ctx context.Context, t canonical.Trade) bool
}

// Backpressure tunables for a Postgres outage: infra failures retry with capped backoff, blocking the drain goroutine,
// so the events channel fills and the ledger cursor cannot pass trades that have not landed (ADR-0041).
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

// faultClass is the ADR-0041 retry-or-drop classification shared by the trade batch, external buffer and non-trade drain paths.
// Asymmetric on purpose: a drop needs POSITIVE proof the write can never succeed; block-and-retry is the default,
// since dropping an unrecognised infra fault (disk_full, out_of_memory) would let the cursor sail past a lost row.
type faultClass int

const (
	// faultInfra is an infrastructure fault or any error we cannot positively classify: block-and-retry with capped backoff until it lands
	// or ctx is cancelled, never dropped.
	faultInfra faultClass = iota

	// faultData: positively permanent for this row (class 22/23, a canonical validation sentinel, or a recovered sink panic); isolate, count, skip.
	faultData

	// faultShutdown: ctx cancelled mid-write; the caller abandons and reports the range, which the CH lake can re-derive (ADR-0034).
	faultShutdown
)

// classifyFault maps a non-nil write error to its faultClass.
// timescale.IsInfraError is deliberately unused: faultInfra is the open default, so faults the storage layer cannot name yet are still retried.
func classifyFault(err error) faultClass {
	switch {
	case isCtxErr(err):
		return faultShutdown
	case isPermanentDataFault(err):
		return faultData
	default:
		return faultInfra
	}
}

// isPermanentDataFault reports whether err is positively permanent for the row, the only justification for dropping it:
// timescale.IsPermanentDataError, a canonical validation sentinel (pre-flight Validate failures are deterministic),
// or a recovered sink panic (ErrSinkPanic).
func isPermanentDataFault(err error) bool {
	if timescale.IsPermanentDataError(err) {
		return true
	}
	for _, sentinel := range []error{
		canonical.ErrInvalidTrade,
		canonical.ErrInvalidOracle,
		canonical.ErrInvalidAmount,
		canonical.ErrInvalidAsset,
		canonical.ErrInvalidStrkey,
		ErrSinkPanic,
	} {
		if errors.Is(err, sentinel) {
			return true
		}
	}
	return false
}

// retryInfra runs do, retrying faultInfra errors with capped backoff until it succeeds, returns a faultData error, or ctx is done (then ctx.Err()).
// Blocking is the point: the stalled drain goroutine is the backpressure that gates the cursor. Used for trades and non-trade writes (hence op).
func retryInfra(ctx context.Context, logger *slog.Logger, op string, do func(context.Context) error) error {
	backoff := infraRetryInitialBackoff
	attempts := 0
	for {
		err := do(ctx)
		if err == nil {
			if attempts > 0 {
				obs.TradeInsertRetriesTotal.WithLabelValues("recovered").Inc()
				logger.Info("sink recovered after infra retry", "op", op, "retries", attempts)
			}
			return nil
		}
		switch classifyFault(err) {
		case faultShutdown:
			obs.TradeInsertRetriesTotal.WithLabelValues("abandoned").Inc()
			return err
		case faultData:
			return err // permanent for this row — caller error-and-skips
		case faultInfra:
		}

		attempts++
		obs.TradeInsertRetriesTotal.WithLabelValues("retry").Inc()
		if attempts == 1 || attempts%infraRetryLogEvery == 0 {
			logger.Warn("infrastructure fault on sink write — retrying with backpressure (on-chain cursor will NOT advance until this lands)",
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

// flushTradeBatch writes one buffered batch with the ADR-0041 policy: non-infra failure isolates per-row; infra failure blocks-and-retries on-chain trades,
// while external trades go to extBuf (nil: everything block-retries). Returns trades un-landed because ctx was cancelled,
// which the caller must carry into its shutdown drain; per-row isolation against a dead ctx would misreport them as lost.
func flushTradeBatch(ctx context.Context, logger *slog.Logger, w tradeWriter, extBuf *externalRetryBuffer, batch []canonical.Trade, workerID int) []canonical.Trade {
	if len(batch) == 0 {
		return nil
	}
	err := w.BatchInsertTrades(ctx, batch)
	if err == nil {
		return nil
	}
	if isCtxErr(err) {
		// ctx died under the write. Not a fault of the data or the
		// database, and a per-row pass against a dead ctx can only fail
		// every row instantly — hand the whole batch back to the caller.
		return batch
	}

	if !timescale.IsInfraError(err) {
		// Not a recognised infra signature (data fault, lock contention, or unclassified): isolate per-row so one bad row cannot sink the batch.
		// Per-row resilience lives in persistTradeRouted, which keeps external trades off the blocking path.
		logger.Warn("batch trade insert failed (non-infra); isolating per-row",
			"worker", workerID, "batch_size", len(batch), "err", err)
		var abandoned []canonical.Trade
		for _, t := range batch {
			if e := persistTradeRouted(ctx, logger, w, extBuf, t); e != nil && isCtxErr(e) {
				abandoned = append(abandoned, t)
			}
		}
		return abandoned
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
		return retryOnChainBatchBlocking(ctx, logger, w, onchain)
	}
	return nil
}

// reportAbandonedTrades logs at ERROR, with the ledger range, trades a bounded shutdown flush could not land, so an operator can re-derive them from the lake.
// Not for a steady-state flush the parent ctx interrupted: persistWorker carries those into its drain.
func reportAbandonedTrades(logger *slog.Logger, lt *lossTracker, phase string, abandoned []canonical.Trade, err error) {
	if len(abandoned) == 0 {
		return
	}
	lt.trades(abandoned)
	lo, hi := ledgerRange(abandoned)
	// Counted by ROW, not by batch: the alert's value is the size of the
	// served-tier gap the cursor has already advanced past.
	obs.SinkUndrainedRowsTotal.WithLabelValues(obs.SinkPersistEvents, "trade").Add(float64(len(abandoned)))
	logger.Error("trade batch abandoned on shutdown — recoverable from the CH lake (ADR-0034); re-derive this ledger range",
		"phase", phase, "batch_size", len(abandoned), "ledger_from", lo, "ledger_to", hi, "err", err)
}

// persistTradeRouted persists ONE trade with the ADR-0041 policy: on-chain trades block-and-retry, external trades go to the async buffer (nil extBuf: block-retry), so external never blocks the pipeline.
// Returns the abandon error for a block-retried trade, a *TradeDroppedError for a permanent drop on either arm, nil if landed or buffered.
func persistTradeRouted(ctx context.Context, logger *slog.Logger, w tradeWriter, extBuf *externalRetryBuffer, t canonical.Trade) error {
	if extBuf == nil || external.IsOnChain(t.Source) {
		return persistTrade(ctx, logger, w, t)
	}
	err := w.InsertTrade(ctx, t)
	if err == nil {
		return nil
	}
	if classifyFault(err) == faultData {
		obs.SourceInsertErrorsTotal.WithLabelValues(t.Source, obs.InsertErrorKindTradeDropped).Inc()
		logger.Error("insert external trade failed (permanent data fault) — row skipped",
			"source", t.Source, "ledger", t.Ledger, "tx_hash", t.TxHash,
			"op_index", t.OpIndex, "err", err)
		return newTradeDroppedError(t, err)
	}
	extBuf.enqueue(t) // bounded async retry, drop-oldest on overflow
	return nil
}

// retryOnChainBatchBlocking block-retries an on-chain batch until it lands or ctx is cancelled, returning the rows still un-landed (nil if all landed or dropped).
func retryOnChainBatchBlocking(ctx context.Context, logger *slog.Logger, w tradeWriter, batch []canonical.Trade) []canonical.Trade {
	err := retryInfra(ctx, logger, "onchain_batch", func(c context.Context) error {
		return w.BatchInsertTrades(c, batch)
	})
	switch {
	case err == nil:
		return nil
	case isCtxErr(err):
		return batch
	default:
		// A non-infra fault surfaced from a retry attempt (a bad row that
		// only errors once the DB is reachable) → isolate per-row.
		logger.Warn("on-chain trade batch hit a non-infra fault after retry; isolating per-row",
			"batch_size", len(batch), "err", err)
		var abandoned []canonical.Trade
		for _, t := range batch {
			if e := persistTrade(ctx, logger, w, t); e != nil && isCtxErr(e) {
				abandoned = append(abandoned, t)
			}
		}
		return abandoned
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

// externalRetryBufferMaxDepth bounds the external-trade retry buffer (~minutes of a busy CEX feed); overflow drops the oldest and is counted.
const externalRetryBufferMaxDepth = 50_000

// externalRetryInterval is how often the background goroutine re-attempts
// the buffered external trades.
const externalRetryInterval = 2 * time.Second

// externalRetryBuffer is the bounded, drop-oldest, async retry queue for external trades hit by an infra fault (ADR-0041).
// They have no cursor and are vendor-refillable, so they must not block the pipeline.
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

// drainOnce attempts the whole ring as one batch; the ring is emptied meanwhile so a concurrent enqueue never evicts an in-flight entry.
// On infra failure or shutdown the batch is re-queued at the front; per-row isolation drops only faultData rows and re-queues the rest,
// since an infra fault mid-pass must not discard merely un-landed rows.
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
	if err == nil {
		obs.TradeInsertRetriesTotal.WithLabelValues("recovered").Inc()
		return
	}
	if class := classifyFault(err); class != faultData {
		if class == faultInfra {
			obs.TradeInsertRetriesTotal.WithLabelValues("retry").Inc()
		}
		b.mu.Lock()
		b.requeueFrontLocked(batch)
		b.mu.Unlock()
		return
	}

	// Permanent data fault in the batch: isolate per-row and drop failing rows loudly (vendor-refillable);
	// rows hitting an infra fault or shutdown mid-pass, plus the untried tail, go back to the ring front.
	for i, t := range batch {
		e := b.w.InsertTrade(ctx, t)
		if e == nil {
			continue
		}
		if classifyFault(e) == faultData {
			obs.SourceInsertErrorsTotal.WithLabelValues(t.Source, "dropped").Inc()
			b.logger.Warn("external trade dropped during per-row isolation (permanent data fault; vendor-refillable)",
				"source", t.Source, "ledger", t.Ledger, "tx_hash", t.TxHash, "err", e)
			continue
		}
		obs.TradeInsertRetriesTotal.WithLabelValues("retry").Inc()
		b.logger.Warn("external trade isolation pass hit an infrastructure fault — re-queuing the remainder for the next retry tick",
			"source", t.Source, "requeued", len(batch)-i, "err", e)
		b.mu.Lock()
		b.requeueFrontLocked(batch[i:])
		b.mu.Unlock()
		return
	}
}

// finalDrain makes one last attempt at shutdown under a fresh ctx bounded by drainFinalPassBudget (it runs at the end of ShutdownDeadline).
// Whatever does not land is counted on SinkUndrainedRowsTotal and logged at ERROR with depth and venues for refetch.
//
//nolint:contextcheck // intentional fresh context: the parent ctx is cancelled at shutdown, so threading it would fail every insert instantly (same pattern as drainBufferedEvents / flushShutdown).
func (b *externalRetryBuffer) finalDrain() {
	b.mu.Lock()
	pending := len(b.ring)
	b.mu.Unlock()
	if pending == 0 {
		return
	}
	fctx, cancel := context.WithTimeout(context.Background(), drainFinalPassBudget)
	defer cancel()
	b.drainOnce(fctx)

	b.mu.Lock()
	remaining := len(b.ring)
	venues := ringVenues(b.ring)
	b.mu.Unlock()
	if remaining == 0 {
		return
	}
	// Counted by ROW like reportAbandonedTrades: these rows have nowhere
	// left to go once this pass ends, and the alert's value is the size of
	// the served-tier gap whatever its recovery path.
	obs.SinkUndrainedRowsTotal.WithLabelValues(obs.SinkPersistEvents, "trade").Add(float64(remaining))
	b.logger.Error("external trade retry buffer not fully drained at shutdown — remaining entries are vendor-refillable (ADR-0041); record the venues and window",
		"remaining", remaining, "venues", venues)
}

// ringVenues lists the distinct trade sources in ring, sorted, for the
// shutdown-loss report. Caller holds b.mu.
func ringVenues(ring []canonical.Trade) []string {
	seen := make(map[string]struct{}, 4)
	for _, t := range ring {
		seen[t.Source] = struct{}{}
	}
	venues := make([]string, 0, len(seen))
	for v := range seen {
		venues = append(venues, v)
	}
	sort.Strings(venues)
	return venues
}
