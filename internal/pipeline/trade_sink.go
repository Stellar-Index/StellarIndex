package pipeline

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/sources/external"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
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
// infrastructure-classified failure (see [faultClass]) with
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

// faultClass is the ADR-0041 classification of a write failure on the
// ingest sink's durability edge. It is the single source of truth for
// "retry or drop?" across all three sink paths — the trade batch path,
// the external retry buffer, and the dispatcher's non-trade drain — so
// they can never disagree about what a given error means.
//
// The rule is deliberately ASYMMETRIC (REL-08, audit-2026-07-23): a
// DROP requires POSITIVE proof that the write can never succeed; a
// block-and-retry is the DEFAULT. The pre-REL-08 code had it the other
// way round — it retried only what [timescale.IsInfraError] positively
// recognised and dropped everything else — so a genuine infrastructure
// fault whose SQLSTATE that predicate does not enumerate (53100
// disk_full, 53200 out_of_memory, class 58 system/I-O errors, XX000
// internal_error) was handled as a permanent per-row data fault: the
// trade was dropped while the enqueue-advanced cursor (ADR-0041
// Decision 1) sailed past its ledger, which is precisely the silent
// served-tier gap ADR-0041 exists to prevent.
//
// Under the asymmetric rule an unrecognised fault blocks-and-retries
// instead: the drain goroutine stalls, the events channel fills,
// ProcessLedger's `events <- ev` send blocks, and the cursor cannot
// advance past writes that have not landed. Nothing is lost and the
// stall is loud — every attempt bumps
// TradeInsertRetriesTotal{outcome="retry"}, which the
// `trade_insert_backpressure` alert fires on.
type faultClass int

const (
	// faultInfra — an infrastructure fault (DB unreachable, restarting,
	// out of connections/disk/memory) OR any error we cannot positively
	// classify. Policy: block-and-retry with capped backoff until it
	// lands or the context is cancelled. Never dropped.
	faultInfra faultClass = iota

	// faultData — a POSITIVELY identified permanent fault for this row:
	// a pq class-22 data exception / class-23 constraint violation
	// ([timescale.IsPermanentDataError]), a canonical validation
	// sentinel the store rejected before touching Postgres, or a
	// recovered sink panic. Retrying can never succeed, so the offending
	// row is isolated, counted and skipped — one bad row must not wedge
	// the pipeline.
	faultData

	// faultShutdown — the context was cancelled / its deadline expired
	// mid-write. Not a fault of the data or the database: the caller
	// abandons and surfaces the un-landed ledger range at ERROR (raw ops
	// are durable in the CH lake per ADR-0034, so the range is
	// re-derivable).
	faultShutdown
)

// classifyFault maps a non-nil write error to its [faultClass].
// Callers must check `err == nil` first.
//
// Note that [timescale.IsInfraError] does not appear here: it is a
// POSITIVE infra predicate and faultInfra is already the default, so
// every error it recognises classifies as faultInfra either way (pinned
// by TestClassifyFault_StoragePredicatesAgree). Keeping the default open
// is the whole point — a fault the storage layer has not yet learned to
// name must still be retried rather than dropped.
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

// isPermanentDataFault reports whether err is POSITIVELY known to be
// permanent for the row being written — the only justification for
// dropping it.
//
// Two families qualify beyond [timescale.IsPermanentDataError] (pq
// class 22/23), and both are deterministic for the value, not the
// database:
//
//   - a canonical validation sentinel: the store's pre-flight
//     Validate() rejects a malformed Trade / OracleUpdate / Amount /
//     Asset / strkey before any SQL runs, so the same value fails
//     identically forever. Without this arm the asymmetric default
//     would block-and-retry a poison row until shutdown.
//   - a recovered sink panic ([errSinkPanic]): deterministic for the
//     event that produced it. HandleEvent's exported contract still
//     hands the projector a generic error there (its own safe-side
//     default is retry-and-alert); this sink treats it as permanent
//     because a tight retry loop over a panicking decode is strictly
//     worse than isolating the event.
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
		errSinkPanic,
	} {
		if errors.Is(err, sentinel) {
			return true
		}
	}
	return false
}

// retryInfra runs do and, while it returns a [faultInfra]-classified
// error, retries with capped exponential backoff until do succeeds,
// returns a permanent data fault, or ctx is done.
//
// Return contract:
//   - nil                         — do eventually succeeded.
//   - a [faultData] error         — surfaced immediately for the caller
//     to error-and-skip; NEVER retried (a constraint / validation fault
//     is permanent for that row).
//   - ctx.Err()                   — the context was cancelled mid-retry
//     (shutdown); the caller surfaces the abandoned, lake-recoverable
//     work.
//
// REL-08 (audit-2026-07-23): the retry predicate is [classifyFault],
// not [timescale.IsInfraError] — an error nobody has positively
// classified is retried, not dropped. See the [faultClass] godoc.
//
// Used for single-row trade inserts AND (since REL-08) for the
// dispatcher drain's non-trade served-tier writes, hence the `op`
// label in the logs.
//
// Blocking is the point: while this loops, the calling drain goroutine
// is not consuming the events channel, which is the backpressure that
// gates the on-chain ledger cursor (2026-07-06 outage).
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

// flushTradeBatch writes one buffered trade batch with the resilient
// failure policy (ADR-0041 / 2026-07-06 outage). It replaces the old
// "batch failed → per-row → drop on error" path, which silently lost
// writes during a Postgres outage while the cursor advanced.
//
// Behaviour by failure class:
//   - success                    — return.
//   - anything not positively     — isolate per-row (the pre-existing
//     recognised as infra           belt-and-braces fallback), so one bad
//     row can't sink the batch and lock contention resolves via
//     single-row lock order. Each row is then re-classified on its own
//     ([persistTradeRouted]): a permanent data fault is counted and
//     skipped, everything else follows the infra policy below.
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
		// Not the positively-recognised infra signature: a per-row data
		// fault, row-lock contention (deadlock / serialization), or a
		// fault nobody has classified yet. Isolate per-row — one bad row
		// must not sink the batch, and contention clears via single-row
		// lock acquisition. This is the 2026-07-05 batch-sort commit's
		// belt-and-braces fallback; per-row resilience now lives in
		// persistTradeRouted (REL-08: it keeps external trades OFF the
		// blocking path, which plain persistTrade would have put them on
		// once an unclassified fault started retrying instead of dropping).
		logger.Warn("batch trade insert failed (non-infra); isolating per-row",
			"worker", workerID, "batch_size", len(batch), "err", err)
		for _, t := range batch {
			persistTradeRouted(ctx, logger, w, extBuf, t)
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

// persistTradeRouted persists ONE trade with the ADR-0041 policy while
// preserving the on-chain / external split that [flushTradeBatch]'s
// infra path applies to a whole batch: on-chain trades block-and-retry
// (cursor gating), external CEX/FX trades hand off to the bounded async
// buffer so they never block the pipeline. A permanent data fault is
// counted and skipped for both.
//
// extBuf == nil (shutdown drain / projector / backfill paths) makes
// every trade block-and-retry inside the caller's bounded context, as
// before.
//
// REL-08: the batch path's per-row isolation pass used to call
// persistTrade directly, which was safe only while an unrecognised
// fault returned immediately. Now that such a fault blocks-and-retries,
// routing external rows here is what keeps "external never blocks the
// pipeline" (ADR-0041 acceptance caveat) true.
func persistTradeRouted(ctx context.Context, logger *slog.Logger, w tradeWriter, extBuf *externalRetryBuffer, t canonical.Trade) {
	if extBuf == nil || external.IsOnChain(t.Source) {
		// Batch path runs under an indefinite ctx and owns its own shutdown
		// drain, so persistTrade's abandon error (returned only on ctx-cancel)
		// is intentionally ignored here.
		_ = persistTrade(ctx, logger, w, t)
		return
	}
	err := w.InsertTrade(ctx, t)
	if err == nil {
		return
	}
	if classifyFault(err) == faultData {
		obs.SourceInsertErrorsTotal.WithLabelValues(t.Source, "trade").Inc()
		logger.Error("insert external trade failed (permanent data fault) — row skipped",
			"source", t.Source, "ledger", t.Ledger, "tx_hash", t.TxHash,
			"op_index", t.OpIndex, "err", err)
		return
	}
	extBuf.enqueue(t) // bounded async retry, drop-oldest on overflow
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
			// Indefinite-ctx batch path; abandon error handled by the drain.
			_ = persistTrade(ctx, logger, w, t)
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
//
// REL-08 (audit-2026-07-23): the per-row isolation pass used to treat
// EVERY per-row error as a permanent drop, so a Postgres infra fault
// that surfaced mid-pass (the DB went away between the batch attempt
// and row k, or the batch error was an unclassified infra fault in the
// first place) permanently discarded rows that were merely un-landed.
// It now drops only [faultData] rows and re-queues the rest — the
// bounded ring plus drop-oldest still caps memory, and a genuine
// overflow drop stays counted on
// SourceInsertErrorsTotal{kind="dropped"}.
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

	// Permanent data fault somewhere in the batch → isolate per-row so
	// one bad row can't wedge the buffer. Rows that fail permanently are
	// dropped loudly (external is vendor-refillable); a row that hits an
	// infra fault or shutdown mid-pass is NOT lost — it and the rest of
	// the un-attempted tail go back to the front of the ring for the
	// next tick.
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

// finalDrain makes one last bounded attempt to land the buffer at
// shutdown, using a fresh context (the parent is already cancelled).
//
// Bounded by [drainFinalPassBudget], not [drainTimeout] (CON-10): this
// runs AFTER PersistEvents' workers have finished their own drain, i.e.
// at the very end of the process's [ShutdownDeadline] window, so it gets
// the reserved tail. External trades are the vendor-refillable class —
// whatever doesn't land is warned about with its depth and refetched
// from the venue.
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
	b.mu.Unlock()
	if remaining > 0 {
		b.logger.Warn("external trade retry buffer not fully drained at shutdown — remaining entries are vendor-refillable (ADR-0041)",
			"remaining", remaining)
	}
}
