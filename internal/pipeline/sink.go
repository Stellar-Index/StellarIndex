package pipeline

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"sync"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/consumer"
	"github.com/Stellar-Index/StellarIndex/internal/domain"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/sources/accounts"
	"github.com/Stellar-Index/StellarIndex/internal/sources/aquarius"
	"github.com/Stellar-Index/StellarIndex/internal/sources/band"
	"github.com/Stellar-Index/StellarIndex/internal/sources/blend"
	blend_backstop "github.com/Stellar-Index/StellarIndex/internal/sources/blend_backstop"
	blend_emitter "github.com/Stellar-Index/StellarIndex/internal/sources/blend_emitter"
	"github.com/Stellar-Index/StellarIndex/internal/sources/cctp"
	claimable_balances "github.com/Stellar-Index/StellarIndex/internal/sources/claimable_balances"
	"github.com/Stellar-Index/StellarIndex/internal/sources/comet"
	"github.com/Stellar-Index/StellarIndex/internal/sources/defindex"
	"github.com/Stellar-Index/StellarIndex/internal/sources/external"
	"github.com/Stellar-Index/StellarIndex/internal/sources/liquidity_pools"
	"github.com/Stellar-Index/StellarIndex/internal/sources/phoenix"
	"github.com/Stellar-Index/StellarIndex/internal/sources/redstone"
	"github.com/Stellar-Index/StellarIndex/internal/sources/reflector"
	"github.com/Stellar-Index/StellarIndex/internal/sources/rozo"
	sac_balances "github.com/Stellar-Index/StellarIndex/internal/sources/sac_balances"
	"github.com/Stellar-Index/StellarIndex/internal/sources/sdex"
	sep41_supply "github.com/Stellar-Index/StellarIndex/internal/sources/sep41_supply"
	sep41_transfers "github.com/Stellar-Index/StellarIndex/internal/sources/sep41_transfers"
	"github.com/Stellar-Index/StellarIndex/internal/sources/sorocredit"
	"github.com/Stellar-Index/StellarIndex/internal/sources/soroswap"
	soroswap_router "github.com/Stellar-Index/StellarIndex/internal/sources/soroswap_router"
	"github.com/Stellar-Index/StellarIndex/internal/sources/spectra"
	sushiswap_v3 "github.com/Stellar-Index/StellarIndex/internal/sources/sushiswap_v3"
	"github.com/Stellar-Index/StellarIndex/internal/sources/trustlines"
	"github.com/Stellar-Index/StellarIndex/internal/sources/upshift"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// SinkMode controls which event classes [PersistEvents] writes when
// draining the dispatcher's events channel. Introduced for ADR-0032
// Phase 4: once the projector becomes sole writer for Soroban-derived
// events, the dispatcher's events-goroutine must stop writing them
// (otherwise duplicate-PK errors flood + the writer-of-record is
// ambiguous). Soroban-derived sinks (trades, blend_*, phoenix_*,
// comet_*, soroswap_skim, sep41_*, cctp_events, rozo_events,
// reflector/redstone oracle_updates) ride the projector path;
// everything else (sdex trades, external CEX/FX, band oracle_updates,
// supply observers writing LedgerEntry observations) still rides
// the dispatcher path because those sources don't flow through
// soroban_events.
type SinkMode int

const (
	// SinkModeAll writes every consumer.Event the dispatcher emits —
	// nothing is skipped. Used when the projector is NOT running: the
	// dispatcher's events-goroutine is then the only writer, so it must
	// persist every class (including sep41). `stellarindex-ops backfill`
	// also uses this mode (the projector never runs there). When the
	// projector IS enabled the events-goroutine instead uses
	// [SinkModeSkipSoleWriter] (Phase-3) or [SinkModeSkipProjected]
	// (Phase-4) — see [SinkModeForProjector].
	SinkModeAll SinkMode = iota

	// SinkModeSkipProjected skips Soroban-derived events the
	// projector handles (see [IsProjectedEvent]). Phase 4+ the
	// dispatcher's events-goroutine uses this mode so the projector
	// owns Soroban-derived writes outright; the events-goroutine
	// continues handling sdex / external / band / supply observers.
	SinkModeSkipProjected

	// SinkModeSkipSoleWriter skips ONLY the events whose domain the
	// projector has EARNED sole-writer status for (see
	// [IsSoleWriterProjected]): the specs with [ProjectorSpec.SoleWriter]
	// set. It's the Phase-3 parallel mode for every
	// OTHER projected source (those still double-write for the
	// duplicate-absorbing ON CONFLICT soak) while the promoted
	// sole-writer domains are owned by the projector outright — so
	// they never double-write and, crucially, never depend on the
	// `PersistPerSource` flag's value (closing the config foot-gun
	// where a mis-set flag silently dropped sep41 rows). The
	// events-goroutine still writes sdex / external / band / supply
	// observers, exactly as in [SinkModeSkipProjected]. Its copies of
	// the double-written events count nothing: see [countsInProjector].
	SinkModeSkipSoleWriter
)

// SinkModeForProjector selects the dispatcher events-goroutine's sink
// mode from the projector config booleans. Extracted here (rather than
// inlined in cmd/stellarindex-indexer) so the foot-gun-closure
// invariant is unit-testable: for EVERY combination of these two
// booleans, a sole-writer source's event is written exactly once (see
// TestSinkModeForProjector_SoleWriterInvariant).
//
//   - projector disabled → SinkModeAll: the events-goroutine is the
//     ONLY writer, so it must persist every class (including sep41).
//   - projector enabled, persist_per_source=true → SinkModeSkipSoleWriter:
//     Phase-3 parallel for un-promoted sources, but the projector owns
//     the sole-writer domains outright — the events-goroutine
//     skips them so they are never double-written and never at risk of
//     the flag being flipped.
//   - projector enabled, persist_per_source=false → SinkModeSkipProjected:
//     Phase-4, projector is sole writer for ALL projected sources.
func SinkModeForProjector(projectorEnabled, persistPerSource bool) SinkMode {
	if !projectorEnabled {
		return SinkModeAll
	}
	if persistPerSource {
		return SinkModeSkipSoleWriter
	}
	return SinkModeSkipProjected
}

// skipInSink reports whether the dispatcher's events-goroutine must
// SKIP writing ev under mode because a running projector owns the
// write. The single source of truth for the two drain loops
// (persistWorker + drainBufferedEvents) so they can never disagree.
func skipInSink(ev consumer.Event, mode SinkMode) bool {
	switch mode {
	case SinkModeSkipProjected:
		return IsProjectedEvent(ev)
	case SinkModeSkipSoleWriter:
		return IsSoleWriterProjected(ev)
	default: // SinkModeAll
		return false
	}
}

// countsInProjector reports whether ev's per-source counts belong to the
// projector rather than to the dispatcher running under mode: in Phase-3
// parallel mode both writers persist every un-promoted projected event, so
// counting it on both sides doubled the `entries` column and
// stellarindex_source_events_total. The projector counts in every mode it
// runs in, so its tally reads the same in Phase 3 and Phase 4.
func countsInProjector(ev consumer.Event, mode SinkMode) bool {
	return mode == SinkModeSkipSoleWriter && IsProjectedEvent(ev)
}

// uncountedEntriesKey marks a ctx whose writes must not bump
// source_entry_counts; see [countsInProjector].
type uncountedEntriesKey struct{}

// dispatcherEventPersister wraps ep so a Phase-3 parallel write of a
// projected event still lands (the ON CONFLICT soak) but counts nothing:
// neither the received/last-seen metrics nor the `entries` tally.
func dispatcherEventPersister(ep eventPersister, mode SinkMode) eventPersister {
	return func(ctx context.Context, ev consumer.Event, countEvent bool) error {
		if countsInProjector(ev, mode) {
			return ep(context.WithValue(ctx, uncountedEntriesKey{}, true), ev, false)
		}
		return ep(ctx, ev, countEvent)
	}
}

// countReceived bumps the per-source received/last-seen metrics for one event.
func countReceived(source string) {
	obs.SourceEventsTotal.WithLabelValues(source).Inc()
	obs.SourceLastEventUnix.WithLabelValues(source).Set(float64(time.Now().Unix()))
}

// PersistEvents drains `in` and writes each event to its hypertable via the
// supplied store. Returns when ctx is canceled and the channel has been drained,
// or when the channel is closed.
//
// Cursor-vs-channel safety: callers advance their per-source cursor AFTER
// ProcessLedger enqueues events to `in` but BEFORE this sink writes them, so
// returning on cancellation without draining would drop up to cap(in) buffered
// events and `-resume` would skip them. The drain uses a fresh context bounded
// by [drainTimeout]; if it trips, the remaining events are logged at ERROR with
// their ledger range.
//
// `mode` (ADR-0032 Phase 4): see [SinkMode]. PersistEvents launches [PersistWorkers] concurrent drain goroutines sharing
// `in`, each with its own trade-batch buffer; one goroutine (one PG round trip
// in flight) backs ProcessLedger up far below the ledger rate. Each worker
// claims a pool connection per flush, so keep [PersistWorkers] well under
// PoolMaxOpenConns.
//
// Ordering within a source is NOT preserved across workers; safe because the
// trades PK is the full identity, so duplicate writes resolve via ON CONFLICT
// DO NOTHING.
//
// late (nil in the backfill, which refreshes its own chunks) observes committed
// trade writes.
func PersistEvents(ctx context.Context, logger *slog.Logger, store *timescale.Store, in <-chan consumer.Event, mode SinkMode, late *LateTradeRefresher) ShutdownLoss {
	lt := &lossTracker{}
	// Bounded async retry buffer for external (CEX/FX) trades that hit
	// an infrastructure fault (ADR-0041).
	// On-chain trades block-and-retry instead (cursor gating); external
	// trades — no cursor, vendor-refillable — buffer here and drop-oldest
	// under sustained overflow so they never block the pipeline. A nil
	// store (unit tests hitting only the default/unhandled path) skips
	// the buffer + its goroutine entirely.
	var extBuf *externalRetryBuffer
	var bufWG sync.WaitGroup
	var bufCancel context.CancelFunc
	if store != nil {
		extBuf = newExternalRetryBuffer(late.writer(store), logger, externalRetryBufferMaxDepth)
		// Run the retry buffer under a context DERIVED from ctx and tied to
		// worker lifetime, not ctx itself. extBuf.run only returns on
		// <-ctx.Done() (→ finalDrain). Under ctx, a BACKFILL — whose input
		// channel closes when the range completes but whose process ctx is
		// never canceled (no SIGTERM on a clean range-complete) — would leave
		// run() spinning forever, so bufWG.Wait() below hangs, the chunk never
		// logs complete, and the cursor + CAGG materialisation never run.
		// Cancelling this derived context once the workers have drained lets
		// run() exit via its fresh-context finalDrain (no buffered external
		// trades lost). Live ingest is unaffected: its workers only exit when
		// the source closes `in` (i.e. at shutdown), which is when ctx is
		// cancelled anyway.
		var bufCtx context.Context
		bufCtx, bufCancel = context.WithCancel(ctx)
		bufWG.Add(1)
		go func() {
			defer bufWG.Done()
			extBuf.run(bufCtx)
		}()
	}

	var wg sync.WaitGroup
	for i := 0; i < PersistWorkers; i++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			// store doubles as the trade writer AND — bound to the logger
			// by storeEventPersister — as the non-trade event persister.
			// Both seams exist so the shutdown-race tests can intercept a
			// write with a fake.
			persistWorker(ctx, logger, storeEventPersister(logger, store), late.writer(store), in, mode, workerID, extBuf, lt)
		}(i)
	}
	wg.Wait()
	// Workers have drained + block-retried their buffers; signal the external
	// buffer to finish its final drain and exit (see bufCtx above), then wait.
	if bufCancel != nil {
		bufCancel()
	}
	bufWG.Wait()
	return lt.snapshot()
}

// PersistWorkers is the count of concurrent drain goroutines run by
// PersistEvents. Sized to balance PG-pool capacity (25) and worker
// throughput. Measured on live r1: 4 workers gave ~5 ledgers/min vs
// the ~10 ledgers/min network rate. 8 workers lifts processing
// rate above the network rate so the cursor's last_updated stays
// fresh enough for the SLA-freshness threshold. Peak PG-conn use
// is still well under the 25-conn pool ceiling.
const PersistWorkers = 8

// ep and tw are the two write seams. In production both are the real
// store (PersistEvents passes it twice, once wrapped by
// [storeEventPersister]); a fake in the shutdown-race tests, which is the
// only reason they are parameters rather than a `*timescale.Store`.
//
//nolint:gocognit,contextcheck // batched-drain loop has natural fan-out: ctx.Done, ticker, channel — splitting hurts readability of the flush invariants. The shutdown flush intentionally uses a fresh context (parent is canceled); see flushShutdown.
func persistWorker(ctx context.Context, logger *slog.Logger, ep eventPersister, tw tradeWriter, in <-chan consumer.Event, mode SinkMode, workerID int, extBuf *externalRetryBuffer, lt *lossTracker) {
	tradeBuf := make([]canonical.Trade, 0, tradeBatchSize)
	// carried holds NON-trade events whose steady-state write was
	// cancelled mid-flight by the parent ctx — the non-trade twin of the
	// `flush` carry below. Such an event is already OFF the channel, so
	// nothing will ever redeliver it; abandoning it there would throw away
	// an already-cursored served-tier write while the worker's whole drain
	// budget still sits unused. Bounded in practice by one
	// entry: once ctx is cancelled shutdownSafeCtx hands out a FRESH
	// bounded context, so every later abandon is a genuine loss and is
	// reported rather than carried. Both arms that hand it to
	// [persistCarried] then RETURN, so there is no reset — nothing reads
	// it again.
	var carried []consumer.Event
	var drain drainDeadline
	ep = dispatcherEventPersister(ep, mode)
	flushTicker := time.NewTicker(tradeBatchFlushInterval)
	defer flushTicker.Stop()

	// flushWith writes the buffered batch through the resilient sink and
	// returns the trades the flush had to abandon because fctx was
	// cancelled mid-write (see flushTradeBatch).
	// buf routes external trades to the async retry buffer; passing nil
	// (shutdown paths) makes external trades block-and-retry within the
	// bounded shutdown context instead, since the buffer's background
	// retrier is winding down.
	flushWith := func(fctx context.Context, buf *externalRetryBuffer) []canonical.Trade {
		if len(tradeBuf) == 0 {
			return nil
		}
		batch := tradeBuf
		tradeBuf = make([]canonical.Trade, 0, tradeBatchSize)
		return flushTradeBatch(fctx, logger, tw, buf, batch, workerID)
	}
	// flush is the steady-state flush. It runs under the parent ctx (via
	// shutdownSafeCtx) so a stuck write can't hold shutdown hostage —
	// which means a shutdown that RACES an in-flight write cancels it.
	// The rows that write had to abandon are not lost: they go back into
	// tradeBuf, and the `<-ctx.Done()` arm's flushShutdown retries them
	// under the worker's bounded drain deadline — the same
	// carry-into-the-drain shape as the sorobanevents AsyncSink.
	// Without the carry they would fall straight into flushTradeBatch's
	// per-row isolation against the dead ctx and be logged as "abandoned —
	// re-derive" (up to tradeBatchSize rows) on every deploy that caught
	// a flush mid-flight. Only the parent-ctx case is carried: when
	// shutdownSafeCtx already handed out a fresh bounded ctx,
	// its deadline IS the drain budget, so an abandon there is reported
	// as the genuine loss it is.
	flush := func(fctx context.Context) {
		abandoned := flushWith(fctx, extBuf)
		if len(abandoned) == 0 {
			return
		}
		if fctx == ctx {
			tradeBuf = append(abandoned, tradeBuf...)
			return
		}
		reportAbandonedTrades(logger, lt, "steady-state flush under shutdown ctx", abandoned, fctx.Err())
	}

	// flushShutdown flushes this worker's in-memory tradeBuf on the
	// shutdown paths (parent ctx canceled OR channel closed) using a
	// FRESH context bounded by the worker's SHARED drain deadline.
	// The parent ctx is already canceled by the time those arms
	// fire, so passing it to BatchInsertTrades / persistTrade makes every
	// postgres call fail instantly and silently drops the buffered
	// trades. The fresh context (same pattern as drainBufferedEvents)
	// lets the final flush actually land; sharing ONE deadline with the
	// worker's other shutdown phases is what keeps the total
	// drain inside [ShutdownDeadline] instead of stacking a fresh
	// [drainTimeout] per phase.
	flushShutdown := func(deadline time.Time) {
		if len(tradeBuf) == 0 {
			return
		}
		fctx, cancel := context.WithDeadline(context.Background(), deadline)
		defer cancel()
		reportAbandonedTrades(logger, lt, "worker shutdown flush", flushWith(fctx, nil), fctx.Err())
	}

	for {
		select {
		case <-ctx.Done():
			// Shutdown requested. Before exiting, drain every event
			// THIS worker can pull right now, so the racy select — which can
			// pick this arm even while events sit buffered in `in` — cannot
			// drop in-flight work. The drain is NON-BLOCKING (the default arm
			// breaks out the instant `in` is momentarily empty), so a channel
			// the producer hasn't closed yet can't pin the worker: worker 0's
			// blocking drainBufferedEvents below still catches late arrivals
			// and the channel close. The parent ctx is already cancelled, so
			// trade-shaped events go to tradeBuf (flushed by flushShutdown with
			// a fresh ctx) and everything else drains under a fresh bounded ctx
			// — never the cancelled parent, which would fail every
			// insert instantly and silently drop the event.
			//
			// Every phase below shares ONE absolute deadline, latched
			// by whichever post-cancellation phase ran first (possibly a racy
			// select arm via shutdownSafeCtx), so the worker's whole drain is bounded by
			// [drainTimeout] rather than by drainTimeout × phases. That is what
			// guarantees worker 0 reaches drainBufferedEvents' deadline arm —
			// and logs the undrained ledger range — before main hard-exits at
			// [ShutdownDeadline].
			deadline := drain.get()
			// Carried events first: they were dequeued BEFORE anything
			// still sitting in `in`, and nothing else can redeliver them.
			persistCarried(carried, logger, ep, deadline, lt)
			shutdownCtx, shutdownCancel := context.WithDeadline(context.Background(), deadline)
			tradeBuf = drainInFlightNow(shutdownCtx, in, logger, ep, mode, tradeBuf, lt)
			shutdownCancel()
			flushShutdown(deadline)
			// Only the first worker handles the blocking shutdown drain
			// (catches events that arrive after our non-blocking sweep + the
			// channel close) to avoid duplicate drain work; the others exit.
			if workerID == 0 {
				drainBufferedEvents(in, logger, ep, tw, mode, deadline, lt)
			}
			return
		case <-flushTicker.C:
			// Go's select has no priority, so on
			// the very iteration ctx is cancelled this arm can still win the
			// race against `<-ctx.Done()` above. shutdownSafeCtx swaps in a
			// fresh bounded context when that happens, so the flush gets its
			// fair share of the drain budget instead of failing every insert
			// instantly against the already-dead parent.
			fctx, fcancel := shutdownSafeCtx(ctx, &drain)
			flush(fctx)
			fcancel()
		case ev, ok := <-in:
			if !ok {
				deadline := drain.get()
				persistCarried(carried, logger, ep, deadline, lt)
				flushShutdown(deadline)
				return
			}
			if skipInSink(ev, mode) {
				continue
			}
			if t, ok := tradeFromEvent(ev); ok {
				if !countsInProjector(ev, mode) {
					countReceived(t.Source)
				}
				tradeBuf = append(tradeBuf, t)
				if len(tradeBuf) >= tradeBatchSize {
					// See the flushTicker arm above.
					fctx, fcancel := shutdownSafeCtx(ctx, &drain)
					flush(fctx)
					fcancel()
				}
				continue
			}
			// Dispatcher drain for non-trade served-tier writes (oracle
			// updates, supply observations, blend / cctp / rozo rows):
			// same ADR-0041 failure policy as trades — see
			// persistEventResilient. See the flushTicker arm above.
			fctx, fcancel := shutdownSafeCtx(ctx, &drain)
			if err := persistEventResilient(fctx, logger, ep, ev); err != nil {
				// Same carry rule as `flush` above, for the same reason:
				// only the parent-ctx case is carried. When shutdownSafeCtx
				// already handed out a fresh bounded ctx its
				// deadline IS the drain budget, so an abandon there is the
				// genuine loss it is reported as.
				if fctx == ctx {
					carried = append(carried, ev)
				} else {
					reportAbandonedEvent(logger, lt, ev, err)
				}
			}
			fcancel()
		}
	}
}

// drainInFlightNow makes a NON-BLOCKING sweep over whatever is already
// buffered in `in` and returns tradeBuf with every trade-shaped event it
// found appended; non-trade events are persisted under ctx as it goes.
//
// [persistWorker]'s shutdown arm calls this before exiting,
// because the racy select can pick `<-ctx.Done()` while events are still
// sitting in the channel buffer. Non-blocking (the default arm returns
// the instant `in` is momentarily empty) so a channel the producer has
// not closed yet cannot pin the worker — worker 0's blocking
// [drainBufferedEvents] still catches late arrivals and the close.
//
// Extracted from that arm to keep [persistWorker] under the
// cognitive-complexity gate (the campaign's precedent is to lower real
// complexity rather than suppress the linter); behaviour is unchanged,
// including that a closed channel and a momentarily-empty one both just
// return.
func drainInFlightNow(ctx context.Context, in <-chan consumer.Event, logger *slog.Logger, ep eventPersister, mode SinkMode, tradeBuf []canonical.Trade, lt *lossTracker) []canonical.Trade {
	for {
		select {
		case ev, ok := <-in:
			if !ok {
				return tradeBuf
			}
			if skipInSink(ev, mode) {
				continue
			}
			if t, isTrade := tradeFromEvent(ev); isTrade {
				tradeBuf = append(tradeBuf, t)
				continue
			}
			if err := persistEventResilient(ctx, logger, ep, ev); err != nil {
				reportAbandonedEvent(logger, lt, ev, err)
			}
		default:
			return tradeBuf
		}
	}
}

// persistCarried is flushShutdown's non-trade half: it re-writes the
// events a steady-state persist had to abandon mid-flight, under a FRESH
// context bounded by the worker's SHARED drain deadline. Same rationale
// as flushShutdown — the parent ctx is already cancelled on both
// shutdown paths, so passing it through would fail every insert
// instantly — and the same reason for taking the absolute deadline
// rather than a fresh budget. Anything it still cannot land IS a genuine
// loss, so it is reported here rather than carried further.
//
// The caller resets its own slice afterwards; taking it by value keeps
// this a plain function rather than a closure over persistWorker's
// locals, which is what keeps that function under the complexity gate.
//
//nolint:contextcheck // intentional fresh context; see godoc above.
func persistCarried(carried []consumer.Event, logger *slog.Logger, ep eventPersister, deadline time.Time, lt *lossTracker) {
	if len(carried) == 0 {
		return
	}
	cctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()
	for _, ev := range carried {
		if err := persistEventResilient(cctx, logger, ep, ev); err != nil {
			reportAbandonedEvent(logger, lt, ev, err)
		}
	}
}

// shutdownSafeCtx returns ctx unchanged when it is still live. When ctx
// has ALREADY been cancelled it returns a FRESH context bounded by the
// worker's latched drain deadline instead: the racy select in
// [persistWorker]'s main loop can pick the flushTicker or `<-in` arm on
// the exact same iteration ctx.Done() fires, and passing the dead
// parent straight through to a flush/persist call would fail every
// write instantly — abandoning it — rather than giving it the same
// bounded shot at landing the worker's `<-ctx.Done()` arm would have
// given it. The caller must always call the returned CancelFunc (a
// no-op when ctx was live).
func shutdownSafeCtx(ctx context.Context, drain *drainDeadline) (context.Context, context.CancelFunc) {
	if ctx.Err() == nil {
		return ctx, func() {}
	}
	return context.WithDeadline(context.Background(), drain.get())
}

// drainDeadline latches ONE absolute post-cancellation deadline per
// worker. Whichever shutdown phase asks first fixes it — a racy
// select arm via [shutdownSafeCtx] or the `<-ctx.Done()` arm — and every
// later phase reuses it, so arms taken after cancellation spend the same
// [drainTimeout] budget instead of each starting a fresh one.
type drainDeadline struct{ at time.Time }

func (d *drainDeadline) get() time.Time {
	if d.at.IsZero() {
		d.at = time.Now().Add(drainTimeout)
	}
	return d.at
}

// tradeBatchSize caps the trades-per-batch in BatchInsertTrades.
// Sized to fit comfortably under PostgreSQL's max-bind-params limit
// (32767 parameters); each row has 12 placeholders, so 200 rows =
// 2400 placeholders — well under. Production throughput is roughly
// linear in this until either PG's planning cost or the events
// channel runs dry; 200 is the validated operating point.
const tradeBatchSize = 200

// tradeBatchFlushInterval is the upper bound on staleness for events
// stuck below the size threshold. With low-volume periods the buffer
// fills slowly; this caps how long a trade waits before landing
// regardless. 200ms keeps per-batch latency well under the SLA
// freshness threshold while still amortising the per-roundtrip cost.
const tradeBatchFlushInterval = 200 * time.Millisecond

// tradeFromEvent returns the underlying canonical.Trade for any
// event that targets the trades hypertable. Used by the PersistEvents
// batcher to route trade-shaped events down the batch path while
// leaving everything else (oracle updates, supply observations,
// log-only events) on the single-row HandleEvent path.
//
// MUST stay in lockstep with HandleEvent — every event type whose
// HandleEvent case calls persistTrade(...) must return its trade
// here, otherwise the event silently falls through to HandleEvent's
// per-event slow path (correctness-equivalent, but slow).
func tradeFromEvent(ev consumer.Event) (canonical.Trade, bool) {
	switch e := ev.(type) {
	case soroswap.TradeEvent:
		return e.Trade, true
	case aquarius.TradeEvent:
		return e.Trade, true
	case phoenix.TradeEvent:
		return e.Trade, true
	case comet.TradeEvent:
		return e.Trade, true
	case sushiswap_v3.TradeEvent:
		return e.Trade, true
	case sdex.TradeEvent:
		return e.Trade, true
	case external.TradeEvent:
		return e.Trade, true
	default:
		return canonical.Trade{}, false
	}
}

// IsProjectedEvent reports whether the ADR-0032 projector writes ev:
// its type is listed by a [SourceSpec] with a Projector. Phase 4+ the
// dispatcher's events goroutine skips these. Everything else (sdex,
// external CEX/FX, band, soroswap_router, supply observers) is written
// only by the events goroutine.
func IsProjectedEvent(ev consumer.Event) bool {
	return eventRoles[reflect.TypeOf(ev)].projected
}

// IsSoleWriterProjected reports whether the projector owns ev's write
// even in Phase-3 parallel mode ([ProjectorSpec.SoleWriter]), so the
// events goroutine skips it whatever PersistPerSource says. A subset of
// [IsProjectedEvent] by construction.
func IsSoleWriterProjected(ev consumer.Event) bool {
	return eventRoles[reflect.TypeOf(ev)].soleWriter
}

// drainBufferedEvents writes any remaining buffered events using a
// fresh shutdown context so postgres calls succeed past the parent
// context's cancellation. Bounded by the caller's shared drain
// `deadline` so a hung shutdown can't keep the binary alive
// indefinitely; on deadline, it surfaces the exact undrained ledger
// range at ERROR (recoverable from the CH lake per ADR-0034) rather
// than dropping it silently.
//
// Deliberately takes a DEADLINE, not a context — the whole reason this
// exists is to keep writing past the parent's cancellation, and the
// shared deadline requires the instant it gives up to be the same
// instant its caller's earlier shutdown phases were bounded by (a fresh
// [drainTimeout] here would push the ERROR report past main's
// [ShutdownDeadline], so it would never fire).
//
//nolint:contextcheck,gocognit // intentional fresh context + batched-drain fan-out; see godoc above.
func drainBufferedEvents(in <-chan consumer.Event, logger *slog.Logger, ep eventPersister, tw tradeWriter, mode SinkMode, deadline time.Time, lt *lossTracker) {
	drainCtx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()
	tradeBuf := make([]canonical.Trade, 0, tradeBatchSize)
	flushTrades := func() {
		if len(tradeBuf) == 0 {
			return
		}
		batch := tradeBuf
		tradeBuf = make([]canonical.Trade, 0, tradeBatchSize)
		// nil extBuf: at shutdown the async external buffer is winding
		// down, so every trade block-retries within the bounded drainCtx.
		// An infra fault here abandons after the
		// drainTimeout and logs the recoverable ledger range at ERROR.
		reportAbandonedTrades(logger, lt, "shutdown drain", flushTradeBatch(drainCtx, logger, tw, nil, batch, -1), drainCtx.Err())
	}
	for {
		select {
		case ev, ok := <-in:
			if !ok {
				flushTrades()
				return
			}
			if skipInSink(ev, mode) {
				continue
			}
			if t, ok := tradeFromEvent(ev); ok {
				tradeBuf = append(tradeBuf, t)
				if len(tradeBuf) >= tradeBatchSize {
					flushTrades()
				}
				continue
			}
			// Bounded by the shared drain deadline; an abandon here is a
			// genuine loss (there is no later pass to carry it to).
			if err := persistEventResilient(drainCtx, logger, ep, ev); err != nil {
				reportAbandonedEvent(logger, lt, ev, err)
			}
		case <-drainCtx.Done():
			flushTrades()
			// The bounded drain deadline tripped with events
			// possibly still buffered. drainFinalPass makes one last
			// best-effort NON-BLOCKING pass over whatever's immediately
			// available and reports exactly what's left undrained.
			drainFinalPass(in, logger, ep, tw, mode, lt)
			return
		}
	}
}

// ledgerSpan tracks the min/max ledger observed across the trades a shutdown
// drain could not persist, so the undrained-range ERROR can name the exact
// range an operator must re-derive (`ch-rebuild -sdex-gaps`).
//
// Extracted from drainFinalPass's inner loop purely to keep that function under
// the cognitive-complexity gate, lowering real complexity rather than
// suppressing the linter. min stays 0 until the first observation, so a drain
// that loses nothing reports 0/0.
type ledgerSpan struct{ min, max uint32 }

func (s *ledgerSpan) observe(l uint32) {
	if s.min == 0 || l < s.min {
		s.min = l
	}
	if l > s.max {
		s.max = l
	}
}

// drainFinalPass runs once the drain deadline has passed: a non-blocking sweep of what is
// already buffered in `in`, persisted under a fresh drainFinalPassBudget context because the
// parent is done. Every non-skipped event it picks up is counted and logged at ERROR with the
// trades' ledger span, so the residual can be re-derived from the lake (`ch-rebuild -sdex-gaps`).
// Standalone so the skip-before-count ordering is testable with a pre-filled `in`.
//
//nolint:contextcheck // intentional fresh context; see godoc above.
func drainFinalPass(in <-chan consumer.Event, logger *slog.Logger, ep eventPersister, tw tradeWriter, mode SinkMode, lt *lossTracker) {
	finalCtx, finalCancel := context.WithTimeout(context.Background(), drainFinalPassBudget)
	defer finalCancel()
	finalTrades := make([]canonical.Trade, 0, tradeBatchSize)
	var total, trades int
	var span ledgerSpan
drainRemainder:
	for {
		select {
		case ev, ok := <-in:
			if !ok {
				break drainRemainder
			}
			// Skip-check FIRST. A
			// projector-owned event (skipInSink) was never going to be
			// persisted by this drain even on a clean shutdown, so it
			// must not inflate undrained_events/undrained_trades or
			// widen the re-derive ledger range — that would send an
			// operator re-deriving a range the projector already owns
			// and durably wrote.
			if skipInSink(ev, mode) {
				continue
			}
			total++
			t, isTrade := tradeFromEvent(ev)
			if isTrade {
				trades++
				span.observe(t.Ledger)
				finalTrades = append(finalTrades, t)
				continue
			}
			if err := persistEventResilient(finalCtx, logger, ep, ev); err != nil {
				reportAbandonedEvent(logger, lt, ev, err)
			}
		default:
			break drainRemainder
		}
	}
	if len(finalTrades) > 0 {
		// nil extBuf: same shutdown block-and-retry posture as flushTrades.
		reportAbandonedTrades(logger, lt, "final pass", flushTradeBatch(finalCtx, logger, tw, nil, finalTrades, -1), finalCtx.Err())
	}
	if total > 0 {
		logger.Error("PersistEvents drain deadline exceeded — made a final best-effort persist pass; any residual is recoverable from the CH lake, re-derive this ledger range if the served tier is short",
			"undrained_events", total, "undrained_trades", trades,
			"ledger_from", span.min, "ledger_to", span.max)
	} else {
		logger.Warn("PersistEvents drain deadline exceeded — no events undrained",
			"buffered", len(in))
	}
}

// ShutdownDeadline is the process-level drain budget the indexer
// enforces after SIGINT/SIGTERM (cmd/stellarindex-indexer/main.go): once
// it expires main logs "drain timeout exceeded — hard exit" and returns,
// and the process dies with whatever the sink was still doing.
//
// Exported because the sink's own drain budgets are DERIVED from it.
// Independent budgets fail: a sink that gives itself 90s per drain
// phase across four stacked phases while main kills the process at 30s
// means worker 0's deadline arm — the one that logs the exact undrained
// ledger range at ERROR, the single artifact telling an operator what
// to re-derive — can never fire, and operators lose the loss report on
// every non-clean shutdown.
//
// Pinned against main's wiring by TestShutdownDeadline_MainUsesConstant
// and against the sink's own budgets by
// TestDrainBudget_FitsShutdownDeadline.
const ShutdownDeadline = 30 * time.Second

// Drain budgets of the indexer's other shutdown sinks. main wires each
// into its sink and runs the drains AFTER the [ShutdownDeadline] window,
// so the process's worst-case stop is their sum, not their max.
const (
	// CHLiveSinkStopBudget bounds clickhouse.LiveSink.Stop.
	CHLiveSinkStopBudget = 30 * time.Second
	// RawEventDrainBudget is the soroban-events sink's DrainGrace.
	RawEventDrainBudget = 20 * time.Second
	// DiscoveryDrainBudget bounds discovery.AsyncSink.Stop.
	DiscoveryDrainBudget = 10 * time.Second
	// stopTimeoutMargin covers the unbudgeted ctx-cancelled defers
	// (taggers, stats flusher) and process exit.
	stopTimeoutMargin = 30 * time.Second
)

// IndexerStopTimeout is the floor for the indexer unit's TimeoutStopSec:
// below it systemd SIGKILLs mid-drain and the buffered rows are lost.
// Pinned against both unit files by TestIndexerStopTimeout_UnitsCoverDrainBudgets.
const IndexerStopTimeout = ShutdownDeadline + CHLiveSinkStopBudget +
	RawEventDrainBudget + DiscoveryDrainBudget + stopTimeoutMargin

// drainFinalPassBudget is the tail reserved for the FINAL best-effort
// persist pass drainBufferedEvents makes after its deadline trips
// and for the external retry buffer's finalDrain. Both run
// AFTER the shared drain deadline has expired, so their budget must fit
// in the gap between [drainTimeout] and [ShutdownDeadline].
const drainFinalPassBudget = 5 * time.Second

// drainReportMargin is slack left at the end of the shutdown window for
// the undrained-range ERROR log to be emitted, flushed and shipped
// before main hard-exits. Nothing may be scheduled inside it.
const drainReportMargin = 5 * time.Second

// drainTimeout caps how long PersistEvents will spend writing
// already-buffered events on shutdown. Derived from [ShutdownDeadline]
// so the whole sequence — drain, final best-effort pass, ERROR report —
// completes INSIDE the process's shutdown window by construction.
//
// 20s is ample for the work itself: a 256-deep buffer at typical
// 1ms-per-insert latency drains in ~250 ms, so this tolerates a ~80x
// slowdown (e.g. postgres saturated by a concurrent VACUUM) before
// giving up. If the deadline trips anyway (e.g. postgres genuinely
// down), drainBufferedEvents logs the exact undrained ledger range at
// ERROR rather than dropping silently — the raw ops are in the CH lake
// (ADR-0034), so the range is recoverable via `stellarindex-ops
// ch-rebuild -sdex-gaps` and the completeness timer.
//
// It is a budget for the WHOLE post-cancellation drain of one worker,
// not per phase: the worker computes one absolute deadline when it sees
// shutdown and every phase shares it (see persistWorker). Stacking
// independent per-phase timeouts would let the sink's total exceed
// main's deadline.
const drainTimeout = ShutdownDeadline - drainFinalPassBudget - drainReportMargin

// HandleEvent dispatches one event to its hypertable insert and RETURNS the
// underlying Insert error (nil on success). A panic is recovered and returned as
// a generic (non-classified) error, which the projector treats as transient;
// that is acceptable because the sole-writer sep41 insert path returns errors
// rather than panicking, and decoder panics are caught upstream in the
// projector's processEventSafely.
//
// The error return is load-bearing for the ADR-0032 projector, which gates its
// cursor on it: a transiently failed write must NOT let the cursor advance past
// that ledger, or a sole-writer (sep41) row is permanently lost. The
// dispatcher's drain reaches it through persistEventResilient.
//
// A trade the store permanently rejects is returned as a *[TradeDroppedError],
// never nil: nil means the row landed. The projector skips that output, counts
// it outcome="sink_permanent" and keeps sinking the row's other outputs;
// `stellarindex-ops projected-rebuild` counts it as a permanent drop and still
// checkpoints the window, because no re-run can land it (every OTHER insert
// failure holds the window).
//
// Exported for the projector (`internal/projector`) and the ops re-derive tools
// (`stellarindex-ops projected-rebuild` / `ch-rebuild`).
func HandleEvent(ctx context.Context, logger *slog.Logger, store *timescale.Store, ev consumer.Event) error {
	return handleEvent(ctx, logger, store, ev, true)
}

// ErrSinkPanic is the sentinel every recovered sink panic wraps, so both
// classifiers on the durability edge — this package's [classifyFault] and
// the projector's classifySinkFault — read it as permanent for that event:
// a tight retry loop over a panicking decode is strictly worse than
// isolating it.
var ErrSinkPanic = errors.New("pipeline: panic in event sink")

// handleEvent is [HandleEvent]'s body with one extra knob: countEvent.
// The per-source received/last-seen counters must be bumped ONCE per
// event, but the sink re-invokes this function on an
// infrastructure retry — without the knob a 10-minute Postgres outage
// would inflate stellarindex_source_events_total by one count per
// backoff attempt for the blocked event and drag its last-event
// timestamp forward, corrupting exactly the freshness/liveness signals
// an operator reads during that outage. Retries pass false.
func handleEvent(ctx context.Context, logger *slog.Logger, store *timescale.Store, ev consumer.Event, countEvent bool) (retErr error) { //nolint:gocyclo,funlen // dispatch table; one case per consumer.Event implementation. Splitting would reduce clarity.
	defer func() {
		if r := recover(); r != nil {
			logger.Error("panic in event sink — recovered",
				"panic", fmt.Sprintf("%v", r),
				"kind", ev.EventKind(),
				"source", ev.Source())
			obs.SourceInsertErrorsTotal.WithLabelValues(ev.Source(), "panic").Inc()
			retErr = fmt.Errorf("%w for %s/%s: %v", ErrSinkPanic, ev.Source(), ev.EventKind(), r)
		}
	}()

	source := eventSource(ev)
	if source == "_unknown" {
		logger.Warn("event with empty source", "kind", ev.EventKind())
	}
	if countEvent {
		countReceived(source)
	}

	// Every arm RETURNS its persist result so the projector can gate its
	// cursor on a sink failure. Trade-shaped events go through
	// persistTrade's ADR-0041 block-and-retry (infra faults BLOCK the
	// caller, data faults drop and return a *TradeDroppedError) and RETURN
	// its abandon error like every other arm, so a bounded-ctx caller (the
	// projector's per-source cycle) cursor-gates trades too. Swallowing the
	// error (return nil) would let the projector advance the cursor past a
	// trade abandoned when its 60s cycle ctx expired during a Postgres
	// outage, and that trade would never self-heal.
	switch e := ev.(type) {
	case soroswap.TradeEvent:
		return persistTrade(ctx, logger, store, e.Trade)
	case soroswap.SkimEvent:
		return persistSoroswapSkim(ctx, logger, store, e)
	case soroswap.LiquidityEvent:
		return persistSoroswapLiquidity(ctx, logger, store, e)
	case aquarius.TradeEvent:
		return persistTrade(ctx, logger, store, e.Trade)
	case aquarius.ReservesEvent:
		return persistAquariusReserves(ctx, logger, store, e)
	case aquarius.LiquidityEvent:
		return persistAquariusLiquidity(ctx, logger, store, e)
	case aquarius.RewardsEvent:
		return persistAquariusRewards(ctx, logger, store, e)
	case aquarius.AdminEvent:
		return persistAquariusAdmin(ctx, logger, store, e)
	case aquarius.FeeEvent:
		return persistAquariusFee(ctx, logger, store, e)
	case aquarius.KillEvent:
		return persistAquariusKill(ctx, logger, store, e)
	case phoenix.TradeEvent:
		return persistTrade(ctx, logger, store, e.Trade)
	case phoenix.LiquidityEvent:
		return persistPhoenixLiquidity(ctx, logger, store, e)
	case phoenix.InitializeEvent:
		return persistPhoenixInitialize(ctx, logger, store, e)
	case phoenix.AdminEvent:
		return persistPhoenixAdmin(ctx, logger, store, e)
	case phoenix.StakeEvent:
		return persistPhoenixStake(ctx, logger, store, e)
	case comet.TradeEvent:
		return persistTrade(ctx, logger, store, e.Trade)
	case sushiswap_v3.TradeEvent:
		return persistTrade(ctx, logger, store, e.Trade)
	case sushiswap_v3.PositionEvent:
		return persistSushiswapV3Position(ctx, logger, store, e)
	case comet.LiquidityEvent:
		return persistCometLiquidity(ctx, logger, store, e)
	case upshift.Event:
		return persistUpshiftVaultEvent(ctx, logger, store, e)
	case spectra.Event:
		return persistSpectraEvent(ctx, logger, store, e)
	case sdex.TradeEvent:
		return persistTrade(ctx, logger, store, e.Trade)
	case reflector.UpdateEvent:
		return persistOracle(ctx, logger, store, e.Update)
	case redstone.UpdateEvent:
		return persistOracle(ctx, logger, store, e.Update)
	case band.UpdateEvent:
		return persistOracle(ctx, logger, store, e.Update)
	case soroswap_router.Event:
		// Persist to the soroswap_router_swaps hypertable (migration
		// 0049), one row per router invocation, so the gap-detector
		// target on the hypertable measures honest coverage (a
		// log-only source has no row to count). The `entries` bump for
		// /v1/diagnostics/ingestion follows the landed insert, as in
		// every persist helper: the sink re-invokes this function on an
		// infra retry, so a bump ahead of the insert would count one
		// entry per attempt (and one for a row the store then rejected).
		row := timescale.SoroswapRouterSwap{
			Ledger:          e.Swap.Ledger,
			LedgerCloseTime: e.Swap.ClosedAt,
			TxHash:          e.Swap.TxHash,
			OpIndex:         uint32(e.Swap.OpIndex),
			ContractID:      e.Swap.ContractID,
			FunctionName:    e.Swap.Function,
			OpSource:        e.Swap.OpSource,
			TxSource:        e.Swap.TxSource,
			Recipient:       e.Swap.Recipient,
			Path:            e.Swap.Path,
			AmountIn:        e.Swap.AmountIn.String(),
			AmountOut:       e.Swap.AmountOut.String(),
			CallSig:         e.Swap.CallSig(),
			// Tree-position columns (migration 0101):
			// where in the tx's auth tree the router was invoked.
			CallPath:  e.Swap.CallPath,
			CallDepth: e.Swap.CallDepth,
			CallKind:  e.Swap.CallKind,
		}
		if !e.Swap.DeadlineTs.IsZero() {
			row.DeadlineTS = &e.Swap.DeadlineTs
		}
		if err := store.InsertSoroswapRouterSwap(ctx, row); err != nil {
			// Count the persist failure like every sibling case in this
			// switch: a Warn without a SourceInsertErrorsTotal bump leaves
			// a dropped swap invisible to metrics/alerts, and silent to
			// any caller that discards the returned err. Counter only —
			// the case still returns err.
			obs.SourceInsertErrorsTotal.WithLabelValues(soroswap_router.SourceName, "soroswap_router_swap").Inc()
			logger.Warn("soroswap-router persist failed",
				"source", soroswap_router.SourceName,
				"tx_hash", e.Swap.TxHash, "ledger", e.Swap.Ledger,
				"err", err)
			return err
		}
		bumpEntryCount(ctx, logger, store, soroswap_router.SourceName)
		return nil
	case defindex.Event:
		// Strategy-layer flow (vault → strategy capital movement).
		// Persists to defindex_flows with layer='strategy' (migration
		// 0050). `actor` here is the vault contract C-strkey
		// (the strategy contract's `from` field); end-user attribution
		// lives at the vault layer (case defindex.VaultEvent below).
		strategyRow := timescale.DefindexFlow{
			Ledger:          e.Flow.Ledger,
			LedgerCloseTime: e.Flow.ClosedAt,
			TxHash:          e.Flow.TxHash,
			OpIndex:         uint32(e.Flow.OpIndex),
			EventIndex:      e.Flow.EventIndex,
			ContractID:      e.Flow.ContractID,
			Layer:           timescale.DefindexLayerStrategy,
			Direction:       timescale.DefindexDirection(e.Flow.Direction),
			Actor:           e.Flow.From,
			Amount:          e.Flow.Amount.String(),
		}
		if err := store.InsertDefindexFlow(ctx, strategyRow); err != nil {
			// Count the persist failure — see the soroswap-router case
			// above for why. Counter only; the case still returns err.
			obs.SourceInsertErrorsTotal.WithLabelValues(defindex.SourceName, "defindex_flow_strategy").Inc()
			logger.Warn("defindex strategy persist failed",
				"source", defindex.SourceName,
				"tx_hash", e.Flow.TxHash, "ledger", e.Flow.Ledger,
				"err", err)
			return err
		}
		bumpEntryCount(ctx, logger, store, defindex.SourceName)
		return nil
	case defindex.VaultEvent:
		// Vault-wrapper layer (user-facing deposit/withdraw).
		// Persists to defindex_flows with layer='vault'. `actor` here
		// is the end-user G-strkey (or routing C-strkey if the user
		// came via an aggregator).
		amounts := make([]string, 0, len(e.Flow.Amounts))
		for _, a := range e.Flow.Amounts {
			amounts = append(amounts, a.String())
		}
		vaultRow := timescale.DefindexFlow{
			Ledger:          e.Flow.Ledger,
			LedgerCloseTime: e.Flow.ClosedAt,
			TxHash:          e.Flow.TxHash,
			OpIndex:         uint32(e.Flow.OpIndex),
			EventIndex:      e.Flow.EventIndex,
			ContractID:      e.Flow.ContractID,
			Layer:           timescale.DefindexLayerVault,
			Direction:       timescale.DefindexDirection(e.Flow.Direction),
			Actor:           e.Flow.User,
			AmountsVec:      amounts,
			DfTokens:        e.Flow.DfTokens.String(),
		}
		if err := store.InsertDefindexFlow(ctx, vaultRow); err != nil {
			// Count the persist failure — see the soroswap-router case
			// above for why. Counter only; the case still returns err.
			obs.SourceInsertErrorsTotal.WithLabelValues(defindex.SourceName, "defindex_flow_vault").Inc()
			logger.Warn("defindex vault persist failed",
				"source", defindex.SourceName,
				"tx_hash", e.Flow.TxHash, "ledger", e.Flow.Ledger,
				"err", err)
			return err
		}
		bumpEntryCount(ctx, logger, store, defindex.SourceName)
		return nil
	case defindex.DFeesEvent:
		// Vault-layer per-asset protocol-fee distribution (`dfees`).
		// One event per distributed_fees Vec entry, one row per
		// event — fee_index carries the Vec position. dfees fires in the
		// SAME op as the vault deposit/withdraw flow above and fans out
		// per token, a shape defindex_flows' one-row-per-flow schema
		// doesn't carry — hence its own defindex_fees table (migration
		// 0146; correlate by tx_hash + op_index).
		feeRow := timescale.DefindexFee{
			Ledger:          e.Fee.Ledger,
			LedgerCloseTime: e.Fee.ClosedAt,
			TxHash:          e.Fee.TxHash,
			OpIndex:         uint32(e.Fee.OpIndex),
			EventIndex:      e.Fee.EventIndex,
			FeeIndex:        uint32(e.Fee.FeeIndex),
			ContractID:      e.Fee.Vault,
			Token:           e.Fee.Token,
			Amount:          e.Fee.Amount.String(),
		}
		if err := store.InsertDefindexFee(ctx, feeRow); err != nil {
			// Count the persist failure — see the soroswap-router case
			// above for why. Counter only; the case still returns err.
			obs.SourceInsertErrorsTotal.WithLabelValues(defindex.SourceName, "defindex_fees").Inc()
			logger.Warn("defindex dfees persist failed",
				"source", defindex.SourceName,
				"tx_hash", e.Fee.TxHash, "ledger", e.Fee.Ledger,
				"err", err)
			return err
		}
		bumpEntryCount(ctx, logger, store, defindex.SourceName)
		return nil
	case defindex.AdminEvent:
		return persistDefindexAdmin(ctx, logger, store, e.Admin)
	case external.TradeEvent:
		return persistTrade(ctx, logger, store, e.Trade)
	case external.UpdateEvent:
		return persistOracle(ctx, logger, store, e.Update)
	case blend.NewAuctionEvent:
		return persistBlendNewAuction(ctx, logger, store, e)
	case blend.FillAuctionEvent:
		return persistBlendFillAuction(ctx, logger, store, e)
	case blend.DeleteAuctionEvent:
		return persistBlendDeleteAuction(ctx, logger, store, e)
	case blend.PositionEvent:
		return persistBlendPositionEvent(ctx, logger, store, e)
	case blend.EmissionEvent:
		return persistBlendEmissionEvent(ctx, logger, store, e)
	case blend.AdminEvent:
		return persistBlendAdminEvent(ctx, logger, store, e)
	case blend_backstop.Event:
		return persistBlendBackstopEvent(ctx, logger, store, e)
	case blend_emitter.DistributeEvent:
		return persistBlendEmitterDistribute(ctx, logger, store, e)
	case blend_emitter.DropEvent:
		return persistBlendEmitterDrop(ctx, logger, store, e)
	case blend_emitter.SwapConfigEvent:
		return persistBlendEmitterSwapConfig(ctx, logger, store, e)
	case cctp.Event:
		return persistCCTPEvent(ctx, logger, store, e)
	case rozo.Event:
		return persistRozoEvent(ctx, logger, store, e)
	case sorocredit.Event:
		return persistSoroCreditEvent(ctx, logger, store, e)
	case accounts.Observation:
		return persistAccountObservation(ctx, logger, store, e)
	case trustlines.Observation:
		return persistTrustlineObservation(ctx, logger, store, e)
	case claimable_balances.Observation:
		return persistClaimableObservation(ctx, logger, store, e)
	case liquidity_pools.Observation:
		return persistLPReserveObservation(ctx, logger, store, e)
	case sac_balances.Observation:
		return persistSACBalanceObservation(ctx, logger, store, e)
	case sep41_supply.Event:
		return persistSEP41SupplyEvent(ctx, logger, store, e)
	case sep41_transfers.Event:
		return persistSEP41TransferEvent(ctx, logger, store, e)
	default:
		// A source emitted an event type the sink doesn't know how
		// to persist. Usually means a new source was registered in
		// BuildDispatcher but the type-switch wasn't updated in
		// lock-step. Count + log — silent drops would otherwise
		// look like "metrics say we're ingesting but the tables
		// stay empty" from the operator's POV. Return nil (not an
		// error): the lockstep AST tests guarantee every projected
		// type has an arm, so this is unreachable for the projector;
		// returning an error would make it retry a wiring bug forever.
		obs.SourceInsertErrorsTotal.WithLabelValues(source, "unhandled").Inc()
		logger.Warn("unhandled event kind",
			"kind", ev.EventKind(),
			"source", source)
		return nil
	}
}

// eventSource returns the metrics/log attribution label for ev,
// substituting "_unknown" for a source a decoder left empty (a label
// must never be blank — it makes the row invisible on every per-source
// dashboard).
func eventSource(ev consumer.Event) string {
	if s := ev.Source(); s != "" {
		return s
	}
	return "_unknown"
}

// persistEventResilient writes ONE non-trade served-tier event with the same
// ADR-0041 failure policy as the trade path: an infrastructure fault BLOCKS the
// drain goroutine and retries with capped backoff (backpressure that gates the
// enqueue-advanced cursor), while a permanent data fault is counted and skipped
// so one poison row can't wedge the pipeline.
//
// Callers must not `_ = HandleEvent(...)`: this path carries writes NOBODY else
// makes (band oracle_updates, external.UpdateEvent, supply LedgerEntry
// observations, soroswap_router swaps), which a Postgres fault would silently
// drop while the cursor advanced.
//
// Retries reuse TradeInsertRetriesTotal so the `trade_insert_backpressure` alert
// fires here too. Genuine drops show on SourceInsertErrorsTotal{kind="dropped"}.
//
// Return contract. Unlike [persistTrade], a permanent-fault drop is NOT a
// *[TradeDroppedError]: the callers (dispatcher drain carry-or-report arms) read
// ANY non-nil return as a shutdown abandon, so a drop would be double-reported.
//   - nil: the write landed, OR it hit a permanent data fault (counted + logged
//     HERE and dropped).
//   - the ctx error: the retry was abandoned because ctx was cancelled or its
//     drain deadline expired. NOT counted or logged here: only the caller knows
//     whether the event still has a bounded drain pass ahead of it
//     (persistWorker's `<-in` arm) or is a genuine loss
//     ([reportAbandonedEvent]).
func persistEventResilient(ctx context.Context, logger *slog.Logger, ep eventPersister, ev consumer.Event) error {
	attempt := 0
	err := retryInfra(ctx, logger, "handle_event", func(c context.Context) error {
		attempt++
		// Only the first attempt counts the event on the per-source
		// received/last-seen metrics; see handleEvent's countEvent godoc.
		return ep(c, ev, attempt == 1)
	})
	if err == nil {
		return nil
	}
	if isCtxErr(err) {
		return err
	}
	obs.SourceInsertErrorsTotal.WithLabelValues(eventSource(ev), "dropped").Inc()
	logger.Error("served-tier event dropped — permanent data fault, row isolated so the pipeline keeps moving",
		"kind", ev.EventKind(), "source", eventSource(ev), "err", err)
	return nil
}

// reportAbandonedEvent counts and logs a non-trade served-tier event no
// drain pass could land — the non-trade sibling of
// [reportAbandonedTrades]. Call it exactly where the event has nowhere
// left to go, never where it is about to be retried: consumer.Event
// carries no ledger (kind + source is the whole identity the interface
// exposes), so the re-derive hint is the source's own gap detector /
// the completeness verdict rather than a ledger range.
func reportAbandonedEvent(logger *slog.Logger, lt *lossTracker, ev consumer.Event, err error) {
	lt.event()
	obs.SourceInsertErrorsTotal.WithLabelValues(eventSource(ev), "dropped").Inc()
	// The per-source `dropped` counter above feeds a RATE alert (≥ 0.1/s
	// for 5 min) that a handful of rows lost at shutdown never trips;
	// this one is the shutdown-loss signal in its own right.
	obs.SinkUndrainedRowsTotal.WithLabelValues(obs.SinkPersistEvents, "event").Inc()
	logger.Error("served-tier event abandoned on shutdown — re-derive this source's tail (per-source gap detector / completeness verdict will show it)",
		"kind", ev.EventKind(), "source", eventSource(ev), "err", err)
}

// ShutdownLoss is what one [PersistEvents] run abandoned: rows no drain
// pass landed. A caller that checkpoints a resume cursor (backfill) must
// not move it past them.
type ShutdownLoss struct {
	// Rows is the number of abandoned rows.
	Rows int
	// MinLedger is the lowest ledger among abandoned trades; 0 when none.
	MinLedger uint32
	// LedgerUnknown is set when an abandoned row was a non-trade event,
	// which carries no ledger, so no safe cursor cap can be derived.
	LedgerUnknown bool
}

// lossTracker accumulates one PersistEvents run's [ShutdownLoss] across
// its workers. Nil-safe so the drain helpers' direct unit tests can pass nil.
type lossTracker struct {
	mu   sync.Mutex
	loss ShutdownLoss
}

func (l *lossTracker) trades(abandoned []canonical.Trade) {
	if l == nil || len(abandoned) == 0 {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.loss.Rows += len(abandoned)
	for _, t := range abandoned {
		switch {
		case t.Ledger == 0: // off-chain trade: no ledger to cap on
			l.loss.LedgerUnknown = true
		case l.loss.MinLedger == 0 || t.Ledger < l.loss.MinLedger:
			l.loss.MinLedger = t.Ledger
		}
	}
}

func (l *lossTracker) event() {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.loss.Rows++
	l.loss.LedgerUnknown = true
}

func (l *lossTracker) snapshot() ShutdownLoss {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.loss
}

// eventPersister writes ONE non-trade served-tier event. It is always
// [handleEvent] bound to the real store in production (see
// [storeEventPersister]); the shutdown-race tests bind a fake, which is
// the only reason the sink's drain path takes this rather than a
// concrete *timescale.Store — the same seam rationale as [tradeWriter]
// on the trade half.
type eventPersister func(ctx context.Context, ev consumer.Event, countEvent bool) error

// storeEventPersister binds [handleEvent] to a store and logger. A nil
// store is legal and unchanged in behaviour: the unit tests that only
// exercise the default/unhandled and Validate-rejects paths pass one.
func storeEventPersister(logger *slog.Logger, store *timescale.Store) eventPersister {
	return func(ctx context.Context, ev consumer.Event, countEvent bool) error {
		return handleEvent(ctx, logger, store, ev, countEvent)
	}
}

// TradeDroppedError reports that a trade was PERMANENTLY dropped: the store
// rejected it as a deterministic data fault ([isPermanentDataFault] — SQLSTATE
// class 22/23 or a canonical validation sentinel), so it was counted, logged
// and skipped rather than retried. The row is NOT in the served tier.
//
// It exists so "landed" and "dropped" do not share a return value. If
// [persistTrade] returned nil for both, the projector — which binds
// [HandleEvent] as its per-event sink and counts every nil as a durable commit
// — would publish the dropped trade under
// stellarindex_projector_events_decoded_total{outcome="ok"}, the one label
// that promises it was written.
//
// Unwrap exposes the store's error, and that is load-bearing in both
// directions: the projector classifies with timescale.IsPermanentDataError /
// the canonical sentinels and SKIPS (outcome="sink_permanent", cursor
// advances — an opaque error would read as unclassified and HOLD a sole-writer
// cursor on a row that can never land), and this package's own
// [classifyFault] keeps reading it as [faultData] (an opaque error would
// block-and-retry forever inside [persistEventResilient]).
type TradeDroppedError struct {
	Source  string
	Ledger  uint32
	TxHash  string
	OpIndex uint32
	// Cause is the store's permanent-fault error, verbatim.
	Cause error
}

func (e *TradeDroppedError) Error() string {
	return fmt.Sprintf("pipeline: trade permanently dropped (%s ledger %d tx %s op %d): %v",
		e.Source, e.Ledger, e.TxHash, e.OpIndex, e.Cause)
}

// Unwrap returns the store's permanent-fault error so errors.Is / errors.As
// classification sees through the wrapper.
func (e *TradeDroppedError) Unwrap() error { return e.Cause }

// newTradeDroppedError wraps cause with the dropped trade's identity.
func newTradeDroppedError(t canonical.Trade, cause error) *TradeDroppedError {
	return &TradeDroppedError{Source: t.Source, Ledger: t.Ledger, TxHash: t.TxHash, OpIndex: t.OpIndex, Cause: cause}
}

// persistTrade writes one trade with ADR-0041 infrastructure resilience: an
// infra fault (connection refused/reset, PG restarting) is RETRIED with
// backpressure, blocking the caller so an on-chain cursor can't advance past an
// un-landed trade, while a data fault (constraint / numeric / validation) is
// permanent for that row and is dropped + counted so retrying can't wedge the
// pipeline. It also serves the projector's per-event sink (HandleEvent).
//
// Takes the narrow [tradeWriter] interface so the retry path is testable.
//
// Return contract (a BOUNDED-ctx caller cursor-gates on it):
//   - nil ONLY when the trade landed.
//   - a *[TradeDroppedError] on a permanent data fault. REPORTED, not folded into
//     nil: the projector counts every nil as a durable commit, so nil would
//     publish a dropped trade as outcome="ok". The projector classifies the
//     unwrapped error permanent and SKIPS it (cursor advances), labelled
//     sink_permanent. A drop is never a ctx error, so [isCtxErr] callers are
//     unaffected.
//   - the ctx error when the retry is abandoned (shutdown, or the projector's
//     60s cycle timeout), so the projector HOLDS the cursor and re-derives.
//     persistTradeRouted / retryOnChainBatchBlocking deliberately ignore it.
//
// NOT the instrumentation point for the unit-ratio sentinel or usd_volume
// coverage: the live path goes through [flushTradeBatch] and bypasses this.
func persistTrade(ctx context.Context, logger *slog.Logger, w tradeWriter, t canonical.Trade) error {
	if err := retryInfra(ctx, logger, "insert_trade", func(c context.Context) error {
		return w.InsertTrade(c, t)
	}); err != nil {
		if isCtxErr(err) {
			// Shutdown / cycle-timeout mid-retry — the raw op is durable in
			// the CH lake (ADR-0034), so this row is re-derivable; surface it
			// loudly AND return it so a bounded-ctx caller gates the cursor.
			// Its own kind: kind="trade" means a row is GONE, and the
			// any-rate persist_drop tripwire keys on it.
			obs.SourceInsertErrorsTotal.WithLabelValues(t.Source, obs.InsertErrorKindTradeAbandoned).Inc()
			logger.Error("insert trade abandoned on shutdown — recoverable from the CH lake (ADR-0034); re-derive",
				"source", t.Source, "ledger", t.Ledger, "tx_hash", t.TxHash, "op_index", t.OpIndex, "err", err)
			return err
		}
		// Permanent data fault — deterministic for this row, so DROP it (no
		// retry) and REPORT the drop. The typed error unwraps to a permanent
		// fault, which the projector skips rather than holds, so a poison row
		// still cannot loop it (nil here would read as "landed").
		obs.SourceInsertErrorsTotal.WithLabelValues(t.Source, obs.InsertErrorKindTradeDropped).Inc()
		logger.Error("insert trade failed (permanent data fault — row skipped)",
			"source", t.Source,
			"ledger", t.Ledger,
			"tx_hash", t.TxHash,
			"op_index", t.OpIndex,
			"err", err,
		)
		return newTradeDroppedError(t, err)
	}
	logger.Debug("trade ingested",
		"source", t.Source,
		"ledger", t.Ledger,
		"pair", t.Pair.String(),
	)
	return nil
}

// recordOracleMetricIfMapped emits the paired staleness gauges
// ([obs.RecordOracleUpdate]) for every mapped oracle asset, but skips
// [canonical.AssetOracleRaw] rows. Capture-totality persists every
// oracle-published symbol verbatim, including ones on no allow-list,
// so an unmapped ("raw:<symbol>") row would otherwise mint its own
// unbounded asset label value — exactly the "passthrough every asset"
// cardinality risk OracleLastUpdateUnix's doc warns about. Raw rows
// are record-layer only and are never compared by the interpretation
// layer, so they have nothing to alert on here.
func recordOracleMetricIfMapped(source string, asset canonical.Asset, updatedAtUnix float64) {
	if !asset.IsMapped() {
		return
	}
	obs.RecordOracleUpdate(source, asset.String(), updatedAtUnix)
}

func persistOracle(ctx context.Context, logger *slog.Logger, store *timescale.Store, u canonical.OracleUpdate) error {
	if err := store.InsertOracleUpdate(ctx, u); err != nil {
		obs.SourceInsertErrorsTotal.WithLabelValues(u.Source, "oracle").Inc()
		logger.Error("insert oracle update failed",
			"source", u.Source,
			"ledger", u.Ledger,
			"tx_hash", u.TxHash,
			"op_index", u.OpIndex,
			"asset", u.Asset.String(),
			"err", err,
		)
		return err
	}
	recordOracleMetricIfMapped(u.Source, u.Asset, float64(u.Timestamp.Unix()))
	logger.Debug("oracle update ingested",
		"source", u.Source,
		"ledger", u.Ledger,
		"asset", u.Asset.String(),
		"price", u.Price.String(),
		"decimals", u.Decimals,
	)
	return nil
}

func persistBlendNewAuction(ctx context.Context, logger *slog.Logger, store *timescale.Store, e blend.NewAuctionEvent) error {
	if err := store.InsertBlendNewAuction(ctx, e); err != nil {
		obs.SourceInsertErrorsTotal.WithLabelValues(blend.SourceName, "blend_auction").Inc()
		logger.Error("insert blend new_auction failed",
			"pool", e.Pool, "user", e.User, "auction_type", e.AuctionType,
			"ledger", e.Ledger, "tx_hash", e.TxHash, "err", err)
		return err
	}
	bumpEntryCount(ctx, logger, store, blend.SourceName)
	logger.Info("blend new_auction ingested",
		"pool", e.Pool, "user", e.User, "auction_type", e.AuctionType,
		"percent", e.Percent, "ledger", e.Ledger)
	return nil
}

func persistBlendFillAuction(ctx context.Context, logger *slog.Logger, store *timescale.Store, e blend.FillAuctionEvent) error {
	if err := store.InsertBlendFillAuction(ctx, e); err != nil {
		obs.SourceInsertErrorsTotal.WithLabelValues(blend.SourceName, "blend_auction").Inc()
		logger.Error("insert blend fill_auction failed",
			"pool", e.Pool, "user", e.User, "filler", e.Filler,
			"ledger", e.Ledger, "tx_hash", e.TxHash, "err", err)
		return err
	}
	bumpEntryCount(ctx, logger, store, blend.SourceName)
	logger.Info("blend fill_auction ingested",
		"pool", e.Pool, "user", e.User, "filler", e.Filler,
		"fill_percent", e.FillPercent, "ledger", e.Ledger)
	return nil
}

func persistBlendDeleteAuction(ctx context.Context, logger *slog.Logger, store *timescale.Store, e blend.DeleteAuctionEvent) error {
	if err := store.InsertBlendDeleteAuction(ctx, e); err != nil {
		obs.SourceInsertErrorsTotal.WithLabelValues(blend.SourceName, "blend_auction").Inc()
		logger.Error("insert blend delete_auction failed",
			"pool", e.Pool, "user", e.User, "auction_type", e.AuctionType,
			"ledger", e.Ledger, "err", err)
		return err
	}
	bumpEntryCount(ctx, logger, store, blend.SourceName)
	logger.Info("blend delete_auction ingested",
		"pool", e.Pool, "user", e.User, "auction_type", e.AuctionType,
		"ledger", e.Ledger)
	return nil
}

func persistCometLiquidity(ctx context.Context, logger *slog.Logger, store *timescale.Store, e comet.LiquidityEvent) error {
	if err := store.InsertCometLiquidity(ctx, timescale.CometLiquidityEvent{
		ContractID:      e.ContractID,
		Ledger:          e.Ledger,
		LedgerCloseTime: e.ObservedAt,
		TxHash:          e.TxHash,
		OpIndex:         e.OpIndex,
		EventIndex:      e.EventIndex,
		Kind:            timescale.CometLiquidityKind(e.Kind),
		Caller:          e.Caller,
		Token:           e.Token,
		Amount:          e.Amount,
		PoolAmountIn:    e.PoolAmountIn,
	}); err != nil {
		obs.SourceInsertErrorsTotal.WithLabelValues(comet.SourceName, "comet_liquidity").Inc()
		logger.Error("insert Comet liquidity event failed",
			"contract_id", e.ContractID, "kind", e.Kind,
			"ledger", e.Ledger, "tx_hash", e.TxHash, "err", err)
		return err
	}
	bumpEntryCount(ctx, logger, store, comet.SourceName)
	logger.Debug("Comet liquidity event ingested",
		"source", comet.SourceName, "kind", e.Kind,
		"contract_id", e.ContractID, "ledger", e.Ledger,
		"token", e.Token, "amount", e.Amount.String())
	return nil
}

// persistUpshiftVaultEvent lands one decoded Upshift vault event into
// upshift_vault_events (migration 0157) — a deposit, a withdrawal, a
// movement of the vault's own share token, or a change in the capital
// the vault has deployed into strategies. Upshift publishes no price,
// so these rows never reach the trades hypertable or VWAP.
func persistUpshiftVaultEvent(ctx context.Context, logger *slog.Logger, store *timescale.Store, e upshift.Event) error {
	if err := store.InsertUpshiftVaultEvent(ctx, timescale.UpshiftVaultEvent{
		ContractID:      e.ContractID,
		Ledger:          e.Ledger,
		LedgerCloseTime: e.ObservedAt,
		TxHash:          e.TxHash,
		OpIndex:         e.OpIndex,
		EventIndex:      e.EventIndex,
		Kind:            timescale.UpshiftVaultEventKind(e.Kind),
		Caller:          e.Caller,
		Receiver:        e.Receiver,
		Owner:           e.Owner,
		Assets:          e.Assets,
		Shares:          e.Shares,
		OldAmount:       e.OldAmount,
		NewAmount:       e.NewAmount,
	}); err != nil {
		obs.SourceInsertErrorsTotal.WithLabelValues(upshift.SourceName, "upshift_vault_events").Inc()
		logger.Error("insert Upshift vault event failed",
			"contract_id", e.ContractID, "kind", e.Kind,
			"ledger", e.Ledger, "tx_hash", e.TxHash, "err", err)
		return err
	}
	bumpEntryCount(ctx, logger, store, upshift.SourceName)
	logger.Debug("Upshift vault event ingested",
		"source", upshift.SourceName, "kind", e.Kind,
		"contract_id", e.ContractID, "ledger", e.Ledger)
	return nil
}

// persistAquariusReserves lands one update_reserves observation (the
// pool's POST-STATE reserve vector) into aquarius_reserves — one row
// per token position (migration 0089). The first real Aquarius TVL /
// liquidity-depth signal; Aquarius has no published price so these
// rows never reach VWAP.
func persistAquariusReserves(ctx context.Context, logger *slog.Logger, store *timescale.Store, e aquarius.ReservesEvent) error {
	// reserves_sync is a DISTINCT reserve-sync signal (not the
	// update_reserves post-state); route it to its own table. An empty
	// Kind is treated as update_reserves.
	if e.Kind == aquarius.EventReservesSync {
		if err := store.InsertAquariusReservesSync(ctx, timescale.AquariusReservesSyncEvent{
			ContractID:      e.ContractID,
			Ledger:          e.Ledger,
			LedgerCloseTime: e.ObservedAt,
			TxHash:          e.TxHash,
			OpIndex:         e.OpIndex,
			EventIndex:      e.EventIndex,
			Reserves:        e.Reserves,
		}); err != nil {
			obs.SourceInsertErrorsTotal.WithLabelValues(aquarius.SourceName, "aquarius_reserves_sync").Inc()
			logger.Error("insert Aquarius reserves_sync failed",
				"contract_id", e.ContractID, "ledger", e.Ledger,
				"tx_hash", e.TxHash, "tokens", len(e.Reserves), "err", err)
			return err
		}
		bumpEntryCount(ctx, logger, store, aquarius.SourceName)
		logger.Debug("Aquarius reserves_sync ingested",
			"source", aquarius.SourceName, "contract_id", e.ContractID,
			"ledger", e.Ledger, "tokens", len(e.Reserves))
		return nil
	}
	if err := store.InsertAquariusReserves(ctx, timescale.AquariusReservesEvent{
		ContractID:      e.ContractID,
		Ledger:          e.Ledger,
		LedgerCloseTime: e.ObservedAt,
		TxHash:          e.TxHash,
		OpIndex:         e.OpIndex,
		EventIndex:      e.EventIndex,
		Reserves:        e.Reserves,
	}); err != nil {
		obs.SourceInsertErrorsTotal.WithLabelValues(aquarius.SourceName, "aquarius_reserves").Inc()
		logger.Error("insert Aquarius reserves failed",
			"contract_id", e.ContractID, "ledger", e.Ledger,
			"tx_hash", e.TxHash, "tokens", len(e.Reserves), "err", err)
		return err
	}
	bumpEntryCount(ctx, logger, store, aquarius.SourceName)
	logger.Debug("Aquarius reserves ingested",
		"source", aquarius.SourceName, "contract_id", e.ContractID,
		"ledger", e.Ledger, "tokens", len(e.Reserves))
	return nil
}

func persistAquariusFee(ctx context.Context, logger *slog.Logger, store *timescale.Store, e aquarius.FeeEvent) error {
	row := timescale.AquariusProtocolFeeEvent{
		ContractID:      e.ContractID,
		Ledger:          e.Ledger,
		LedgerCloseTime: e.ObservedAt,
		TxHash:          e.TxHash,
		OpIndex:         e.OpIndex,
		EventIndex:      e.EventIndex,
		Kind:            e.Kind,
	}
	if e.Kind == aquarius.EventSetProtocolFee {
		row.Fee0New, row.Fee0Old = e.Fee0New, e.Fee0Old
		row.Fee1New, row.Fee1Old = e.Fee1New, e.Fee1Old
		row.HasOldFee = e.HasOldFee
	} else {
		row.Recipient = e.Recipient
		row.Token = e.Token
		row.Amount = e.Amount.String()
	}
	if err := store.InsertAquariusProtocolFee(ctx, row); err != nil {
		obs.SourceInsertErrorsTotal.WithLabelValues(aquarius.SourceName, "aquarius_protocol_fee").Inc()
		logger.Error("insert Aquarius protocol fee failed",
			"contract_id", e.ContractID, "ledger", e.Ledger,
			"tx_hash", e.TxHash, "kind", e.Kind, "err", err)
		return err
	}
	bumpEntryCount(ctx, logger, store, aquarius.SourceName)
	logger.Debug("Aquarius protocol fee ingested",
		"source", aquarius.SourceName, "contract_id", e.ContractID,
		"ledger", e.Ledger, "kind", e.Kind)
	return nil
}

func persistAquariusKill(ctx context.Context, logger *slog.Logger, store *timescale.Store, e aquarius.KillEvent) error {
	if err := store.InsertAquariusKillSwitch(ctx, timescale.AquariusKillSwitchEvent{
		ContractID:      e.ContractID,
		Ledger:          e.Ledger,
		LedgerCloseTime: e.ObservedAt,
		TxHash:          e.TxHash,
		OpIndex:         e.OpIndex,
		EventIndex:      e.EventIndex,
		Action:          e.Action,
	}); err != nil {
		obs.SourceInsertErrorsTotal.WithLabelValues(aquarius.SourceName, "aquarius_kill_switches").Inc()
		logger.Error("insert Aquarius kill switch failed",
			"contract_id", e.ContractID, "ledger", e.Ledger,
			"tx_hash", e.TxHash, "action", e.Action, "err", err)
		return err
	}
	bumpEntryCount(ctx, logger, store, aquarius.SourceName)
	logger.Debug("Aquarius kill switch ingested",
		"source", aquarius.SourceName, "contract_id", e.ContractID,
		"ledger", e.Ledger, "action", e.Action)
	return nil
}

// persistAquariusLiquidity lands one deposit_liquidity /
// withdraw_liquidity observation into aquarius_liquidity — one row per
// token position, shares on the token_index=0 row (migration 0089).
func persistAquariusLiquidity(ctx context.Context, logger *slog.Logger, store *timescale.Store, e aquarius.LiquidityEvent) error {
	if err := store.InsertAquariusLiquidity(ctx, timescale.AquariusLiquidityEvent{
		ContractID:      e.ContractID,
		Ledger:          e.Ledger,
		LedgerCloseTime: e.ObservedAt,
		TxHash:          e.TxHash,
		OpIndex:         e.OpIndex,
		EventIndex:      e.EventIndex,
		Action:          timescale.AquariusLiquidityAction(e.Action),
		Tokens:          e.Tokens,
		Amounts:         e.Amounts,
		Shares:          e.Shares,
	}); err != nil {
		obs.SourceInsertErrorsTotal.WithLabelValues(aquarius.SourceName, "aquarius_liquidity").Inc()
		logger.Error("insert Aquarius liquidity failed",
			"contract_id", e.ContractID, "action", e.Action,
			"ledger", e.Ledger, "tx_hash", e.TxHash, "err", err)
		return err
	}
	bumpEntryCount(ctx, logger, store, aquarius.SourceName)
	logger.Debug("Aquarius liquidity ingested",
		"source", aquarius.SourceName, "action", e.Action,
		"contract_id", e.ContractID, "ledger", e.Ledger,
		"tokens", len(e.Tokens))
	return nil
}

// persistAquariusRewards lands one rewards-gauge event (any of the
// twelve kinds — migration 0099) into
// aquarius_rewards_events.
func persistAquariusRewards(ctx context.Context, logger *slog.Logger, store *timescale.Store, e aquarius.RewardsEvent) error {
	row := timescale.AquariusRewardsEvent{
		ContractID:      e.ContractID,
		Ledger:          e.Ledger,
		LedgerCloseTime: e.ObservedAt,
		TxHash:          e.TxHash,
		OpIndex:         e.OpIndex,
		EventIndex:      e.EventIndex,
		Kind:            timescale.AquariusRewardsKind(e.Kind),
		UserAddress:     e.UserAddress,
		Attributes:      e.Attributes,
	}
	if e.Amount != nil {
		row.Amount = e.Amount
	}
	if err := store.InsertAquariusRewardsEvent(ctx, row); err != nil {
		obs.SourceInsertErrorsTotal.WithLabelValues(aquarius.SourceName, "aquarius_rewards_events").Inc()
		logger.Error("insert Aquarius rewards event failed",
			"contract_id", e.ContractID, "kind", e.Kind,
			"ledger", e.Ledger, "tx_hash", e.TxHash, "err", err)
		return err
	}
	bumpEntryCount(ctx, logger, store, aquarius.SourceName)
	logger.Debug("Aquarius rewards event ingested",
		"source", aquarius.SourceName, "kind", e.Kind,
		"contract_id", e.ContractID, "ledger", e.Ledger)
	return nil
}

// persistAquariusAdmin lands one governance/upgrade admin event (any
// of the eight kinds — migration 0100) into
// aquarius_admin.
func persistAquariusAdmin(ctx context.Context, logger *slog.Logger, store *timescale.Store, e aquarius.AdminEvent) error {
	row := timescale.AquariusAdminEvent{
		ContractID:      e.ContractID,
		Ledger:          e.Ledger,
		LedgerCloseTime: e.ObservedAt,
		TxHash:          e.TxHash,
		OpIndex:         e.OpIndex,
		EventIndex:      e.EventIndex,
		Kind:            timescale.AquariusAdminKind(e.Kind),
		Admin:           e.Admin,
		Target:          e.Target,
		Attributes:      e.Attributes,
	}
	if err := store.InsertAquariusAdminEvent(ctx, row); err != nil {
		obs.SourceInsertErrorsTotal.WithLabelValues(aquarius.SourceName, "aquarius_admin").Inc()
		logger.Error("insert Aquarius admin event failed",
			"contract_id", e.ContractID, "kind", e.Kind,
			"ledger", e.Ledger, "tx_hash", e.TxHash, "err", err)
		return err
	}
	bumpEntryCount(ctx, logger, store, aquarius.SourceName)
	logger.Debug("Aquarius admin event ingested",
		"source", aquarius.SourceName, "kind", e.Kind,
		"contract_id", e.ContractID, "ledger", e.Ledger)
	return nil
}

// persistBlendPositionEvent routes one money-market position event
// (supply / withdraw / supply_collateral / withdraw_collateral /
// borrow / repay / flash_loan) to the blend_positions hypertable.
func persistBlendPositionEvent(ctx context.Context, logger *slog.Logger, store *timescale.Store, e blend.PositionEvent) error {
	if err := store.InsertBlendPositionEvent(ctx, domain.BlendPositionEvent(e)); err != nil {
		obs.SourceInsertErrorsTotal.WithLabelValues(blend.SourceName, "blend_position").Inc()
		logger.Error("insert blend position event failed",
			"pool", e.Pool, "kind", e.Kind, "user", e.User, "asset", e.Asset,
			"ledger", e.Ledger, "tx_hash", e.TxHash, "err", err)
		return err
	}
	bumpEntryCount(ctx, logger, store, blend.SourceName)
	logger.Debug("blend position event ingested",
		"pool", e.Pool, "kind", e.Kind, "user", e.User, "asset", e.Asset,
		"token_amount", e.TokenAmount.String(), "ledger", e.Ledger)
	return nil
}

// persistBlendEmissionEvent routes one emission / credit-risk event
// (gulp / claim / reserve_emission_update / gulp_emissions /
// bad_debt / defaulted_debt) to the blend_emissions hypertable.
func persistBlendEmissionEvent(ctx context.Context, logger *slog.Logger, store *timescale.Store, e blend.EmissionEvent) error {
	if err := store.InsertBlendEmissionEvent(ctx, domain.BlendEmissionEvent(e)); err != nil {
		obs.SourceInsertErrorsTotal.WithLabelValues(blend.SourceName, "blend_emission").Inc()
		logger.Error("insert blend emission event failed",
			"pool", e.Pool, "kind", e.Kind,
			"ledger", e.Ledger, "tx_hash", e.TxHash, "err", err)
		return err
	}
	bumpEntryCount(ctx, logger, store, blend.SourceName)
	logger.Debug("blend emission event ingested",
		"pool", e.Pool, "kind", e.Kind, "ledger", e.Ledger)
	return nil
}

// persistBlendAdminEvent routes one admin / pool-config / pool-
// factory lifecycle event (set_admin / update_pool /
// queue_set_reserve / cancel_set_reserve / set_reserve / set_status
// / deploy) to the blend_admin hypertable. The deploy event from
// the pool-factory drives runtime pool enumeration.
func persistBlendAdminEvent(ctx context.Context, logger *slog.Logger, store *timescale.Store, e blend.AdminEvent) error {
	if err := store.InsertBlendAdminEvent(ctx, domain.BlendAdminEvent(e)); err != nil {
		obs.SourceInsertErrorsTotal.WithLabelValues(blend.SourceName, "blend_admin").Inc()
		logger.Error("insert blend admin event failed",
			"contract_id", e.ContractID, "kind", e.Kind,
			"ledger", e.Ledger, "tx_hash", e.TxHash, "err", err)
		return err
	}
	bumpEntryCount(ctx, logger, store, blend.SourceName)
	logger.Info("blend admin event ingested",
		"contract_id", e.ContractID, "kind", e.Kind,
		"admin", e.Admin, "target", e.Target, "asset", e.Asset,
		"ledger", e.Ledger)
	return nil
}

func persistBlendBackstopEvent(ctx context.Context, logger *slog.Logger, store *timescale.Store, e blend_backstop.Event) error {
	if err := store.InsertBlendBackstopEvent(ctx, timescale.BlendBackstopEvent{
		ContractID:  e.ContractID,
		Ledger:      e.Ledger,
		TxHash:      e.TxHash,
		OpIndex:     uint32(e.OpIndex),
		EventIndex:  uint32(e.EventIndex),
		ObservedAt:  e.ObservedAt,
		EventType:   timescale.BlendBackstopEventType(e.EventType),
		Pool:        e.Pool,
		UserAddress: e.UserAddress,
		Amount:      e.Amount,
		Amount2:     e.Amount2,
		Attributes:  e.Attributes,
	}); err != nil {
		obs.SourceInsertErrorsTotal.WithLabelValues(blend_backstop.SourceName, "blend_backstop_event").Inc()
		logger.Error("insert Blend backstop event failed",
			"contract_id", e.ContractID, "event_type", e.EventType,
			"ledger", e.Ledger, "tx_hash", e.TxHash, "err", err)
		return err
	}
	bumpEntryCount(ctx, logger, store, blend_backstop.SourceName)
	logger.Debug("Blend backstop event ingested",
		"source", blend_backstop.SourceName, "event_type", e.EventType,
		"contract_id", e.ContractID, "ledger", e.Ledger, "tx_hash", e.TxHash)
	return nil
}

// persistBlendEmitterDistribute writes one `distribute` row (one
// BLND emission to a backstop) via Store.InsertBlendEmitterDistribute.
func persistBlendEmitterDistribute(ctx context.Context, logger *slog.Logger, store *timescale.Store, e blend_emitter.DistributeEvent) error {
	if err := store.InsertBlendEmitterDistribute(ctx, timescale.BlendEmitterDistributeEvent{
		ContractID:      e.ContractID,
		Ledger:          e.Ledger,
		LedgerCloseTime: e.ObservedAt,
		TxHash:          e.TxHash,
		OpIndex:         e.OpIndex,
		EventIndex:      e.EventIndex,
		BackstopID:      e.BackstopID,
		Amount:          e.Amount,
	}); err != nil {
		obs.SourceInsertErrorsTotal.WithLabelValues(blend_emitter.SourceName, "blend_emitter_distribute").Inc()
		logger.Error("insert Blend Emitter distribute failed",
			"contract_id", e.ContractID, "backstop_id", e.BackstopID,
			"ledger", e.Ledger, "tx_hash", e.TxHash, "err", err)
		return err
	}
	bumpEntryCount(ctx, logger, store, blend_emitter.SourceName)
	logger.Debug("Blend Emitter distribute ingested",
		"source", blend_emitter.SourceName, "backstop_id", e.BackstopID,
		"ledger", e.Ledger, "amount", e.Amount.String())
	return nil
}

// persistBlendEmitterDrop writes one `drop` event, fanned out to one
// row per recipient by Store.InsertBlendEmitterDrop (single
// transaction — see its godoc for why a partial fan-out can't
// happen).
func persistBlendEmitterDrop(ctx context.Context, logger *slog.Logger, store *timescale.Store, e blend_emitter.DropEvent) error {
	recipients := make([]timescale.BlendEmitterRecipient, len(e.Recipients))
	for i, r := range e.Recipients {
		recipients[i] = timescale.BlendEmitterRecipient{Address: r.Address, Amount: r.Amount}
	}
	if err := store.InsertBlendEmitterDrop(ctx, timescale.BlendEmitterDropEvent{
		ContractID:      e.ContractID,
		Ledger:          e.Ledger,
		LedgerCloseTime: e.ObservedAt,
		TxHash:          e.TxHash,
		OpIndex:         e.OpIndex,
		EventIndex:      e.EventIndex,
		Recipients:      recipients,
	}); err != nil {
		obs.SourceInsertErrorsTotal.WithLabelValues(blend_emitter.SourceName, "blend_emitter_drop").Inc()
		logger.Error("insert Blend Emitter drop failed",
			"contract_id", e.ContractID, "recipients", len(e.Recipients),
			"ledger", e.Ledger, "tx_hash", e.TxHash, "err", err)
		return err
	}
	bumpEntryCount(ctx, logger, store, blend_emitter.SourceName)
	logger.Info("Blend Emitter drop ingested",
		"source", blend_emitter.SourceName, "recipients", len(e.Recipients),
		"ledger", e.Ledger, "tx_hash", e.TxHash)
	return nil
}

// blendEmitterUnlockTime converts a Soroban-emitted raw UnlockTime to a
// UTC time, or the zero time if the raw value would overflow the int64
// cast (see canonical.UnboundedUnixSeconds) — a queued (q_swap)
// UnlockTime is legitimately days beyond its own ledger close time, so
// a close-time-windowed guard like SafeUnixSeconds would wrongly clamp
// a real value (see blend_emitter/README's timelock lifecycle).
func blendEmitterUnlockTime(raw uint64) time.Time {
	t, ok := canonical.UnboundedUnixSeconds(raw)
	if !ok {
		return time.Time{}
	}
	return t
}

// persistBlendEmitterSwapConfig writes one `q_swap` / `swap` row via
// Store.InsertBlendEmitterSwapConfig.
func persistBlendEmitterSwapConfig(ctx context.Context, logger *slog.Logger, store *timescale.Store, e blend_emitter.SwapConfigEvent) error {
	unlockTime := blendEmitterUnlockTime(e.UnlockTime)
	if err := store.InsertBlendEmitterSwapConfig(ctx, timescale.BlendEmitterSwapConfigEvent{
		ContractID:       e.ContractID,
		Ledger:           e.Ledger,
		LedgerCloseTime:  e.ObservedAt,
		TxHash:           e.TxHash,
		OpIndex:          e.OpIndex,
		EventIndex:       e.EventIndex,
		Kind:             timescale.BlendEmitterKind(e.Kind),
		NewBackstop:      e.NewBackstop,
		NewBackstopToken: e.NewBackstopToken,
		UnlockTime:       unlockTime,
	}); err != nil {
		obs.SourceInsertErrorsTotal.WithLabelValues(blend_emitter.SourceName, "blend_emitter_swap_config").Inc()
		logger.Error("insert Blend Emitter swap config failed",
			"contract_id", e.ContractID, "kind", e.Kind,
			"ledger", e.Ledger, "tx_hash", e.TxHash, "err", err)
		return err
	}
	bumpEntryCount(ctx, logger, store, blend_emitter.SourceName)
	logger.Info("Blend Emitter swap config ingested",
		"source", blend_emitter.SourceName, "kind", e.Kind,
		"new_backstop", e.NewBackstop, "ledger", e.Ledger)
	return nil
}

func persistCCTPEvent(ctx context.Context, logger *slog.Logger, store *timescale.Store, e cctp.Event) error {
	if err := store.InsertCCTPEvent(ctx, timescale.CCTPEvent{
		ContractID:         e.ContractID,
		Ledger:             e.Ledger,
		TxHash:             e.TxHash,
		OpIndex:            uint32(e.OpIndex),
		EventIndex:         e.EventIndex,
		ObservedAt:         e.ObservedAt,
		EventType:          timescale.CCTPEventType(e.EventType),
		Amount:             e.Amount,
		Fee:                e.Fee,
		Token:              e.Token,
		CounterpartyDomain: e.CounterpartyDomain,
		Attributes:         e.Attributes,
	}); err != nil {
		obs.SourceInsertErrorsTotal.WithLabelValues(cctp.SourceName, "cctp_event").Inc()
		logger.Error("insert CCTP event failed",
			"contract_id", e.ContractID, "event_type", e.EventType,
			"ledger", e.Ledger, "tx_hash", e.TxHash, "err", err)
		return err
	}
	bumpEntryCount(ctx, logger, store, cctp.SourceName)
	logger.Debug("CCTP event ingested",
		"source", cctp.SourceName, "event_type", e.EventType,
		"contract_id", e.ContractID, "ledger", e.Ledger, "tx_hash", e.TxHash)
	return nil
}

func persistRozoEvent(ctx context.Context, logger *slog.Logger, store *timescale.Store, e rozo.Event) error {
	if err := store.InsertRozoEvent(ctx, timescale.RozoEvent{
		ContractID:  e.ContractID,
		Ledger:      e.Ledger,
		TxHash:      e.TxHash,
		OpIndex:     uint32(e.OpIndex),
		EventIndex:  e.EventIndex,
		ObservedAt:  e.ObservedAt,
		EventType:   timescale.RozoEventType(e.EventType),
		Amount:      e.Amount,
		Destination: e.Destination,
		From:        e.From,
		Memo:        e.Memo,
		Token:       e.Token,
	}); err != nil {
		obs.SourceInsertErrorsTotal.WithLabelValues(rozo.SourceName, "rozo_event").Inc()
		logger.Error("insert Rozo event failed",
			"contract_id", e.ContractID, "event_type", e.EventType,
			"ledger", e.Ledger, "tx_hash", e.TxHash, "err", err)
		return err
	}
	bumpEntryCount(ctx, logger, store, rozo.SourceName)
	logger.Debug("Rozo event ingested",
		"source", rozo.SourceName, "event_type", e.EventType,
		"contract_id", e.ContractID, "ledger", e.Ledger, "tx_hash", e.TxHash)
	return nil
}

// persistSoroCreditEvent routes one sorocredit event to its served-tier
// table by EventType — the single Go event type fans out to four tables
// (credit_positions / credit_statements / credit_settlements /
// credit_events). It converts the source event into the timescale row
// struct here (storage keeps its no-upward-import boundary). NOTE: the
// on-wire "Liquidation" event is a SCHEDULED settlement, written to
// credit_settlements — NOT a distress/liquidation signal (see
// internal/sources/sorocredit).
func persistSoroCreditEvent(ctx context.Context, logger *slog.Logger, store *timescale.Store, e sorocredit.Event) error {
	var err error
	switch e.EventType {
	case sorocredit.TypeNewCollateralContract:
		err = store.InsertCreditPosition(ctx, timescale.CreditPosition{
			CollateralContract: e.CollateralContract,
			PositionUUID:       e.PositionUUID,
			PositionName:       e.PositionName,
			Owner:              e.Owner,
			Ledger:             e.Ledger,
			LedgerCloseTime:    e.ObservedAt,
			TxHash:             e.TxHash,
			OpIndex:            e.OpIndex,
			EventIndex:         e.EventIndex,
		})
	case sorocredit.TypeStatement:
		var stmtTime time.Time
		if e.StatementTime != nil {
			stmtTime = *e.StatementTime
		}
		err = store.InsertCreditStatement(ctx, timescale.CreditStatement{
			StatementUUID:      e.StatementUUID,
			PositionUUID:       e.PositionUUID,
			CollateralContract: e.CollateralContract,
			Amount:             e.Amount,
			StatementTime:      stmtTime,
			Ledger:             e.Ledger,
			LedgerCloseTime:    e.ObservedAt,
			TxHash:             e.TxHash,
			OpIndex:            e.OpIndex,
			EventIndex:         e.EventIndex,
		})
	case sorocredit.TypeSettlement:
		err = store.InsertCreditSettlement(ctx, timescale.CreditSettlement{
			CollateralContract: e.CollateralContract,
			PositionUUID:       e.PositionUUID,
			StatementUUID:      e.StatementUUID,
			SettlerAccount:     e.Account,
			DebtAsset:          e.Asset,
			SettledAmount:      e.Amount,
			Attributes:         e.Attributes,
			Ledger:             e.Ledger,
			LedgerCloseTime:    e.ObservedAt,
			TxHash:             e.TxHash,
			OpIndex:            e.OpIndex,
			EventIndex:         e.EventIndex,
		})
	case sorocredit.TypeWithdrawal, sorocredit.TypeBeaconUpdated,
		sorocredit.TypeSupportedAssetAdded, sorocredit.TypeCollateralHashUpdated,
		sorocredit.TypeTreasuryUpdated:
		err = store.InsertCreditEvent(ctx, timescale.CreditEvent{
			EventType:          string(e.EventType),
			CollateralContract: e.CollateralContract,
			Asset:              e.Asset,
			Account:            e.Account,
			Amount:             e.Amount,
			Attributes:         e.Attributes,
			Ledger:             e.Ledger,
			LedgerCloseTime:    e.ObservedAt,
			TxHash:             e.TxHash,
			OpIndex:            e.OpIndex,
			EventIndex:         e.EventIndex,
		})
	default:
		obs.SourceInsertErrorsTotal.WithLabelValues(sorocredit.SourceName, "unhandled_type").Inc()
		logger.Warn("unhandled sorocredit EventType", "event_type", e.EventType, "ledger", e.Ledger)
		return err
	}
	if err != nil {
		obs.SourceInsertErrorsTotal.WithLabelValues(sorocredit.SourceName, "sorocredit_"+string(e.EventType)).Inc()
		logger.Error("insert sorocredit event failed",
			"event_type", e.EventType, "collateral_contract", e.CollateralContract,
			"ledger", e.Ledger, "tx_hash", e.TxHash, "err", err)
		return err
	}
	bumpEntryCount(ctx, logger, store, sorocredit.SourceName)
	logger.Debug("sorocredit event ingested",
		"source", sorocredit.SourceName, "event_type", e.EventType,
		"collateral_contract", e.CollateralContract, "ledger", e.Ledger)
	return nil
}

// bumpEntryCount is the shared 'entries' counter increment used by
// every sink whose decoded events don't ride the trades + oracle_updates
// per-insert bump path (those tables have their counter bump inlined
// in the INSERT, counting only rows that landed). Surfaces
// source-attributed protocol activity on /v1/diagnostics/ingestion's
// `entries` column. It is a bare +1 per persisted event, so a Phase-3
// parallel write must not reach it: [dispatcherEventPersister] marks
// that ctx. Errors are logged at Warn — a failed bump doesn't fail
// the underlying decode/persist; the operator's periodic
// `stellarindex-ops seed-entry-counts` reconciles drift.
func bumpEntryCount(ctx context.Context, logger *slog.Logger, store *timescale.Store, source string) {
	if uncounted, _ := ctx.Value(uncountedEntriesKey{}).(bool); uncounted {
		return
	}
	if err := store.BumpSourceEntryCount(ctx, source, 1); err != nil {
		logger.Warn("bump source entry count failed",
			"source", source, "err", err)
	}
}

func persistAccountObservation(ctx context.Context, logger *slog.Logger, store *timescale.Store, o accounts.Observation) error {
	if err := store.InsertAccountObservation(ctx, domain.AccountObservation(o)); err != nil {
		obs.SourceInsertErrorsTotal.WithLabelValues(accounts.SourceName, "account_observation").Inc()
		logger.Error("insert account observation failed",
			"account_id", o.AccountID, "ledger", o.Ledger,
			"is_removal", o.IsRemoval, "err", err)
		return err
	}
	bumpEntryCount(ctx, logger, store, accounts.SourceName)
	logger.Debug("account observation ingested",
		"account_id", o.AccountID, "ledger", o.Ledger,
		"balance_stroops", o.Balance.String(),
		"home_domain", o.HomeDomain, "is_removal", o.IsRemoval)
	return nil
}

func persistTrustlineObservation(ctx context.Context, logger *slog.Logger, store *timescale.Store, o trustlines.Observation) error {
	if err := store.InsertTrustlineObservation(ctx, timescale.TrustlineObservation{
		AccountID:      o.AccountID,
		AssetKey:       o.AssetKey,
		Ledger:         o.Ledger,
		ObservedAt:     o.ObservedAt,
		Balance:        o.Balance,
		IsRemoval:      o.IsRemoval,
		IntraLedgerSeq: o.IntraLedgerSeq,
	}); err != nil {
		obs.SourceInsertErrorsTotal.WithLabelValues(trustlines.SourceName, "trustline_observation").Inc()
		logger.Error("insert trustline observation failed",
			"account_id", o.AccountID, "asset_key", o.AssetKey, "ledger", o.Ledger,
			"is_removal", o.IsRemoval, "err", err)
		return err
	}
	bumpEntryCount(ctx, logger, store, trustlines.SourceName)
	logger.Debug("trustline observation ingested",
		"account_id", o.AccountID, "asset_key", o.AssetKey, "ledger", o.Ledger,
		"balance_stroops", o.Balance.String(), "is_removal", o.IsRemoval)
	return nil
}

func persistClaimableObservation(ctx context.Context, logger *slog.Logger, store *timescale.Store, o claimable_balances.Observation) error {
	if err := store.InsertClaimableObservation(ctx, timescale.ClaimableObservation{
		ClaimableID:    o.ClaimableID,
		AssetKey:       o.AssetKey,
		Ledger:         o.Ledger,
		ObservedAt:     o.ObservedAt,
		Balance:        o.Balance,
		IsRemoval:      o.IsRemoval,
		IntraLedgerSeq: o.IntraLedgerSeq,
	}); err != nil {
		obs.SourceInsertErrorsTotal.WithLabelValues(claimable_balances.SourceName, "claimable_observation").Inc()
		logger.Error("insert claimable observation failed",
			"claimable_id", o.ClaimableID, "asset_key", o.AssetKey, "ledger", o.Ledger,
			"err", err)
		return err
	}
	bumpEntryCount(ctx, logger, store, claimable_balances.SourceName)
	logger.Debug("claimable observation ingested",
		"claimable_id", o.ClaimableID, "asset_key", o.AssetKey, "ledger", o.Ledger,
		"balance_stroops", o.Balance.String())
	return nil
}

func persistLPReserveObservation(ctx context.Context, logger *slog.Logger, store *timescale.Store, o liquidity_pools.Observation) error {
	if err := store.InsertLPReserveObservation(ctx, timescale.LPReserveObservation{
		PoolID:         o.PoolID,
		AssetKey:       o.AssetKey,
		Ledger:         o.Ledger,
		ObservedAt:     o.ObservedAt,
		Balance:        o.Balance,
		IsRemoval:      o.IsRemoval,
		IntraLedgerSeq: o.IntraLedgerSeq,
	}); err != nil {
		obs.SourceInsertErrorsTotal.WithLabelValues(liquidity_pools.SourceName, "lp_reserve_observation").Inc()
		logger.Error("insert LP-reserve observation failed",
			"pool_id", o.PoolID, "asset_key", o.AssetKey, "ledger", o.Ledger,
			"err", err)
		return err
	}
	bumpEntryCount(ctx, logger, store, liquidity_pools.SourceName)
	logger.Debug("LP-reserve observation ingested",
		"pool_id", o.PoolID, "asset_key", o.AssetKey, "ledger", o.Ledger,
		"balance_stroops", o.Balance.String())
	return nil
}

func persistSACBalanceObservation(ctx context.Context, logger *slog.Logger, store *timescale.Store, o sac_balances.Observation) error {
	if err := store.InsertSACBalanceObservation(ctx, timescale.SACBalanceObservation{
		ContractID:     o.ContractID,
		AssetKey:       o.AssetKey,
		Holder:         o.Holder,
		Ledger:         o.Ledger,
		ObservedAt:     o.ObservedAt,
		Balance:        o.Balance,
		IsRemoval:      o.IsRemoval,
		IntraLedgerSeq: o.IntraLedgerSeq,
	}); err != nil {
		obs.SourceInsertErrorsTotal.WithLabelValues(sac_balances.SourceName, "sac_balance_observation").Inc()
		logger.Error("insert SAC balance observation failed",
			"contract_id", o.ContractID, "holder", o.Holder, "asset_key", o.AssetKey,
			"ledger", o.Ledger, "err", err)
		return err
	}
	bumpEntryCount(ctx, logger, store, sac_balances.SourceName)
	logger.Debug("SAC balance observation ingested",
		"contract_id", o.ContractID, "holder", o.Holder, "asset_key", o.AssetKey,
		"ledger", o.Ledger, "balance_stroops", o.Balance.String(),
		"is_removal", o.IsRemoval)
	return nil
}

func persistSoroswapSkim(ctx context.Context, logger *slog.Logger, store *timescale.Store, e soroswap.SkimEvent) error {
	txHash, err := timescale.DecodeSoroswapTxHash(e.TxHash)
	if err != nil {
		obs.SourceInsertErrorsTotal.WithLabelValues(soroswap.SourceName, "soroswap_skim_event").Inc()
		logger.Error("decode soroswap skim tx_hash failed",
			"contract_id", e.ContractID, "ledger", e.Ledger, "tx_hash", e.TxHash, "err", err)
		return err
	}
	if err := store.InsertSoroswapSkimEvent(ctx, timescale.SoroswapSkimEvent{
		ContractID:      e.ContractID,
		Ledger:          e.Ledger,
		LedgerCloseTime: e.ObservedAt,
		TxHash:          txHash,
		OpIndex:         int16(e.OpIndex),
		EventIndex:      int16(e.EventIndex),
		To:              e.To,
		Amount0:         e.Amount0.String(),
		Amount1:         e.Amount1.String(),
	}); err != nil {
		obs.SourceInsertErrorsTotal.WithLabelValues(soroswap.SourceName, "soroswap_skim_event").Inc()
		logger.Error("insert soroswap skim event failed",
			"contract_id", e.ContractID, "ledger", e.Ledger, "tx_hash", e.TxHash, "err", err)
		return err
	}
	bumpEntryCount(ctx, logger, store, soroswap.SourceName)
	logger.Debug("soroswap skim event ingested",
		"contract_id", e.ContractID, "ledger", e.Ledger,
		"amount_0", e.Amount0.String(), "amount_1", e.Amount1.String(),
		"to", e.To)
	return nil
}

func persistSoroswapLiquidity(ctx context.Context, logger *slog.Logger, store *timescale.Store, e soroswap.LiquidityEvent) error {
	txHash, err := timescale.DecodeSoroswapTxHash(e.TxHash)
	if err != nil {
		obs.SourceInsertErrorsTotal.WithLabelValues(soroswap.SourceName, "soroswap_liquidity").Inc()
		logger.Error("decode soroswap liquidity tx_hash failed",
			"contract_id", e.ContractID, "ledger", e.Ledger, "tx_hash", e.TxHash, "err", err)
		return err
	}
	if err := store.InsertSoroswapLiquidity(ctx, timescale.SoroswapLiquidityEvent{
		Pair:            e.ContractID,
		Ledger:          e.Ledger,
		LedgerCloseTime: e.ObservedAt,
		TxHash:          txHash,
		OpIndex:         int16(e.OpIndex),
		EventIndex:      int16(e.EventIndex),
		Action:          e.Action,
		Provider:        e.To,
		Token0:          e.Token0,
		Token1:          e.Token1,
		Amount0:         e.Amount0.String(),
		Amount1:         e.Amount1.String(),
		Liquidity:       e.Liquidity.String(),
		NewReserve0:     e.NewReserve0.String(),
		NewReserve1:     e.NewReserve1.String(),
	}); err != nil {
		obs.SourceInsertErrorsTotal.WithLabelValues(soroswap.SourceName, "soroswap_liquidity").Inc()
		logger.Error("insert soroswap liquidity event failed",
			"contract_id", e.ContractID, "ledger", e.Ledger, "tx_hash", e.TxHash, "err", err)
		return err
	}
	bumpEntryCount(ctx, logger, store, soroswap.SourceName)
	logger.Debug("soroswap liquidity event ingested",
		"contract_id", e.ContractID, "ledger", e.Ledger, "action", e.Action,
		"amount_0", e.Amount0.String(), "amount_1", e.Amount1.String(),
		"liquidity", e.Liquidity.String(), "provider", e.To)
	return nil
}

func persistSushiswapV3Position(ctx context.Context, logger *slog.Logger, store *timescale.Store, e sushiswap_v3.PositionEvent) error {
	const table = "sushiswap_v3_position_events"
	txHash, err := timescale.DecodeSoroswapTxHash(e.TxHash)
	if err != nil {
		obs.SourceInsertErrorsTotal.WithLabelValues(sushiswap_v3.SourceName, table).Inc()
		logger.Error("decode sushiswap_v3 position tx_hash failed",
			"contract_id", e.ContractID, "ledger", e.Ledger, "tx_hash", e.TxHash, "err", err)
		return err
	}
	row := timescale.SushiswapV3PositionEvent{
		Pool:            e.ContractID,
		Ledger:          e.Ledger,
		LedgerCloseTime: e.ObservedAt,
		TxHash:          txHash,
		OpIndex:         int16(e.OpIndex),
		EventIndex:      int16(e.EventIndex),
		Action:          e.Action,
		Owner:           e.Owner,
		Sender:          e.Sender,
		Recipient:       e.Recipient,
		Token0:          e.Token0,
		Token1:          e.Token1,
		TickLower:       e.TickLower,
		TickUpper:       e.TickUpper,
		Amount0:         e.Amount0.String(),
		Amount1:         e.Amount1.String(),
	}
	if e.Action != sushiswap_v3.EventCollect {
		row.Liquidity = e.Liquidity.String()
	}
	if err := store.InsertSushiswapV3PositionEvent(ctx, row); err != nil {
		obs.SourceInsertErrorsTotal.WithLabelValues(sushiswap_v3.SourceName, table).Inc()
		logger.Error("insert sushiswap_v3 position event failed",
			"contract_id", e.ContractID, "ledger", e.Ledger, "tx_hash", e.TxHash, "err", err)
		return err
	}
	bumpEntryCount(ctx, logger, store, sushiswap_v3.SourceName)
	logger.Debug("sushiswap_v3 position event ingested",
		"contract_id", e.ContractID, "ledger", e.Ledger, "action", e.Action,
		"amount_0", row.Amount0, "amount_1", row.Amount1, "owner", e.Owner)
	return nil
}

func persistPhoenixLiquidity(ctx context.Context, logger *slog.Logger, store *timescale.Store, e phoenix.LiquidityEvent) error {
	c := e.Change
	sharesStr := ""
	// Withdraw rows have shares_amount populated; provide rows don't.
	if c.Action == phoenix.EventActionWithdrawLiquidity {
		sharesStr = c.SharesAmount.String()
	}
	if err := store.InsertPhoenixLiquidityChange(ctx, timescale.PhoenixLiquidityChange{
		Pool:         c.Pool,
		Ledger:       c.Ledger,
		ObservedAt:   c.ClosedAt,
		TxHash:       c.TxHash,
		OpIndex:      uint32(c.OpIndex),
		EventIndex:   uint32(c.EventIndex), //nolint:gosec // EventIndex is non-negative by Soroban spec.
		Action:       timescale.PhoenixLiquidityAction(c.Action),
		Sender:       c.Sender,
		TokenA:       c.TokenA,
		TokenB:       c.TokenB,
		AmountA:      c.AmountA.String(),
		AmountB:      c.AmountB.String(),
		SharesAmount: sharesStr,
	}); err != nil {
		obs.SourceInsertErrorsTotal.WithLabelValues(phoenix.SourceName, "phoenix_liquidity").Inc()
		logger.Error("insert phoenix liquidity failed",
			"pool", c.Pool, "action", c.Action,
			"ledger", c.Ledger, "tx_hash", c.TxHash, "err", err)
		return err
	}
	bumpEntryCount(ctx, logger, store, phoenix.SourceName)
	logger.Debug("phoenix liquidity ingested",
		"pool", c.Pool, "action", c.Action,
		"sender", c.Sender, "ledger", c.Ledger)
	return nil
}

func persistPhoenixInitialize(ctx context.Context, logger *slog.Logger, store *timescale.Store, e phoenix.InitializeEvent) error {
	if err := store.InsertPhoenixInitialize(ctx, timescale.PhoenixInitializeEvent{
		Pool:            e.Pool,
		Ledger:          e.Ledger,
		LedgerCloseTime: e.ObservedAt,
		TxHash:          e.TxHash,
		OpIndex:         e.OpIndex,
		EventIndex:      e.EventIndex,
		TokenSlot:       e.TokenSlot,
		Token:           e.Token,
	}); err != nil {
		obs.SourceInsertErrorsTotal.WithLabelValues(phoenix.SourceName, "phoenix_initialize").Inc()
		logger.Error("insert Phoenix initialize failed",
			"pool", e.Pool, "ledger", e.Ledger, "tx_hash", e.TxHash,
			"token_slot", e.TokenSlot, "err", err)
		return err
	}
	bumpEntryCount(ctx, logger, store, phoenix.SourceName)
	logger.Debug("Phoenix initialize ingested",
		"source", phoenix.SourceName, "pool", e.Pool, "ledger", e.Ledger,
		"token_slot", e.TokenSlot, "token", e.Token)
	return nil
}

func persistPhoenixAdmin(ctx context.Context, logger *slog.Logger, store *timescale.Store, e phoenix.AdminEvent) error {
	// Only the min-trading settings carry a value; the zero Amount of every
	// other action must stay NULL, not "0".
	value := ""
	if e.AdminAction == phoenix.AdminActionBlendSetMinTradingA || e.AdminAction == phoenix.AdminActionBlendSetMinTradingB {
		value = e.Value.String()
	}
	if err := store.InsertPhoenixAdmin(ctx, timescale.PhoenixAdminEvent{
		Pool:            e.Pool,
		Ledger:          e.Ledger,
		LedgerCloseTime: e.ObservedAt,
		TxHash:          e.TxHash,
		OpIndex:         e.OpIndex,
		EventIndex:      e.EventIndex,
		AdminAction:     e.AdminAction,
		Admin:           e.Admin,
		Value:           value,
	}); err != nil {
		obs.SourceInsertErrorsTotal.WithLabelValues(phoenix.SourceName, "phoenix_admin_events").Inc()
		logger.Error("insert Phoenix admin failed",
			"pool", e.Pool, "ledger", e.Ledger, "tx_hash", e.TxHash,
			"admin_action", e.AdminAction, "err", err)
		return err
	}
	bumpEntryCount(ctx, logger, store, phoenix.SourceName)
	logger.Debug("Phoenix admin ingested",
		"source", phoenix.SourceName, "pool", e.Pool, "ledger", e.Ledger,
		"admin_action", e.AdminAction, "admin", e.Admin)
	return nil
}

func persistDefindexAdmin(ctx context.Context, logger *slog.Logger, store *timescale.Store, a defindex.VaultAdmin) error {
	var amount string
	if a.Amount != nil {
		amount = a.Amount.String()
	}
	if err := store.InsertDefindexAdminEvent(ctx, timescale.DefindexAdminEvent{
		Ledger:          a.Ledger,
		LedgerCloseTime: a.ClosedAt,
		TxHash:          a.TxHash,
		OpIndex:         uint32(a.OpIndex),
		EventIndex:      a.EventIndex,
		ContractID:      a.Vault,
		EventKind:       a.Kind,
		Caller:          a.Caller,
		Strategy:        a.Strategy,
		NewAddress:      a.NewAddress,
		Amount:          amount,
	}); err != nil {
		obs.SourceInsertErrorsTotal.WithLabelValues(defindex.SourceName, "defindex_admin_events").Inc()
		logger.Warn("defindex admin persist failed",
			"source", defindex.SourceName,
			"tx_hash", a.TxHash, "ledger", a.Ledger, "kind", a.Kind,
			"err", err)
		return err
	}
	bumpEntryCount(ctx, logger, store, defindex.SourceName)
	return nil
}

func persistPhoenixStake(ctx context.Context, logger *slog.Logger, store *timescale.Store, e phoenix.StakeEvent) error {
	c := e.Change
	amountStr := ""
	// bond/unbond always carry an amount; withdraw_rewards /
	// distribute_rewards don't emit one on the event itself (see
	// phoenix/events.go doc) — leave the column NULL rather than
	// writing the zero-value "0" (canonical.Amount{}.String() == "0").
	if c.Action == phoenix.EventActionBond || c.Action == phoenix.EventActionUnbond {
		amountStr = c.Amount.String()
	}
	if err := store.InsertPhoenixStakeEvent(ctx, timescale.PhoenixStakeEvent{
		StakeContract: c.Contract,
		Ledger:        c.Ledger,
		ObservedAt:    c.ClosedAt,
		TxHash:        c.TxHash,
		OpIndex:       uint32(c.OpIndex),
		EventIndex:    uint32(c.EventIndex), //nolint:gosec // EventIndex is non-negative by Soroban spec.
		Action:        timescale.PhoenixStakeAction(c.Action),
		User:          c.User,
		LPToken:       c.LPToken,
		Amount:        amountStr,
	}); err != nil {
		obs.SourceInsertErrorsTotal.WithLabelValues(phoenix.SourceName, "phoenix_stake").Inc()
		logger.Error("insert phoenix stake event failed",
			"stake_contract", c.Contract, "action", c.Action,
			"ledger", c.Ledger, "tx_hash", c.TxHash, "err", err)
		return err
	}
	bumpEntryCount(ctx, logger, store, phoenix.SourceName)
	logger.Debug("phoenix stake event ingested",
		"stake_contract", c.Contract, "action", c.Action,
		"user", c.User, "ledger", c.Ledger)
	return nil
}

func persistSEP41SupplyEvent(ctx context.Context, logger *slog.Logger, store *timescale.Store, e sep41_supply.Event) error {
	if err := store.InsertSEP41SupplyEvent(ctx, SEP41SupplyRowOf(e)); err != nil {
		obs.SourceInsertErrorsTotal.WithLabelValues(sep41_supply.SourceName, "sep41_supply_event").Inc()
		logger.Error("insert SEP-41 supply event failed",
			"contract_id", e.ContractID, "kind", e.Kind, "ledger", e.Ledger,
			"tx_hash", e.TxHash, "err", err)
		return err
	}
	bumpEntryCount(ctx, logger, store, sep41_supply.SourceName)
	logger.Debug("SEP-41 supply event ingested",
		"contract_id", e.ContractID, "kind", e.Kind, "ledger", e.Ledger,
		"amount", e.Amount.String(), "counterparty", e.Counterparty)
	return nil
}

// persistSEP41TransferEvent routes one sep41_transfers audit-trail
// event (transfer / approve / set_admin / set_authorized) to the
// sep41_transfers hypertable. It enables per-account net-position queries — the Stellar moat
// feature CG/CMC structurally cannot offer.
func persistSEP41TransferEvent(ctx context.Context, logger *slog.Logger, store *timescale.Store, e sep41_transfers.Event) error {
	if err := store.InsertSEP41Transfer(ctx, SEP41TransferRowOf(e)); err != nil {
		obs.SourceInsertErrorsTotal.WithLabelValues(sep41_transfers.SourceName, "sep41_transfers_event").Inc()
		logger.Error("insert SEP-41 transfer event failed",
			"contract_id", e.ContractID, "kind", e.Kind, "ledger", e.Ledger,
			"tx_hash", e.TxHash, "err", err)
		return err
	}
	bumpEntryCount(ctx, logger, store, sep41_transfers.SourceName)
	logger.Debug("SEP-41 transfer event ingested",
		"contract_id", e.ContractID, "kind", e.Kind, "ledger", e.Ledger,
		"from", e.FromAddr, "to", e.ToAddr)
	return nil
}

// SEP41TransferRowOf converts a decoded sep41_transfers event to its
// storage row — exported so batch writers (ch-rebuild) share the
// exact mapping the live sink uses.
func SEP41TransferRowOf(e sep41_transfers.Event) timescale.SEP41TransferRow {
	return timescale.SEP41TransferRow{
		ContractID:      e.ContractID,
		Ledger:          e.Ledger,
		TxHash:          e.TxHash,
		OpIndex:         e.OpIndex,
		EventIndex:      e.EventIndex,
		ObservedAt:      e.ObservedAt,
		Kind:            timescale.SEP41TransferKind(e.Kind),
		FromAddr:        e.FromAddr,
		ToAddr:          e.ToAddr,
		Amount:          e.Amount,
		LiveUntilLedger: e.LiveUntilLedger,
		Authorized:      e.Authorized,
	}
}

// SEP41SupplyRowOf is the sep41_supply sibling of SEP41TransferRowOf.
func SEP41SupplyRowOf(e sep41_supply.Event) timescale.SEP41SupplyEvent {
	return timescale.SEP41SupplyEvent{
		ContractID:   e.ContractID,
		Ledger:       e.Ledger,
		TxHash:       e.TxHash,
		OpIndex:      e.OpIndex,
		EventIndex:   e.EventIndex,
		ObservedAt:   e.ObservedAt,
		Kind:         timescale.SEP41EventKind(e.Kind),
		Amount:       e.Amount,
		Counterparty: e.Counterparty,
	}
}
