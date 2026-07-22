package pipeline

import (
	"context"
	"fmt"
	"log/slog"
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
	"github.com/Stellar-Index/StellarIndex/internal/sources/trustlines"
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
	// [IsSoleWriterProjected]) — currently just the sep41 domain
	// (F-1316 / TASK #16b). It's the Phase-3 parallel mode for every
	// OTHER projected source (those still double-write for the
	// duplicate-absorbing ON CONFLICT soak) while the promoted
	// sole-writer domains are owned by the projector outright — so
	// they never double-write and, crucially, never depend on the
	// `PersistPerSource` flag's value (closing the config foot-gun
	// where a mis-set flag silently dropped sep41 rows). The
	// events-goroutine still writes sdex / external / band / supply
	// observers, exactly as in [SinkModeSkipProjected].
	SinkModeSkipSoleWriter
)

// SinkModeForProjector selects the dispatcher events-goroutine's sink
// mode from the projector config booleans. Extracted here (rather than
// inlined in cmd/stellarindex-indexer) so the foot-gun-closure
// invariant is unit-testable: for EVERY combination of these two
// booleans, a sep41 event is written exactly once (see
// TestSinkModeForProjector_Sep41SoleWriterInvariant).
//
//   - projector disabled → SinkModeAll: the events-goroutine is the
//     ONLY writer, so it must persist every class (including sep41).
//   - projector enabled, persist_per_source=true → SinkModeSkipSoleWriter:
//     Phase-3 parallel for un-promoted sources, but the projector owns
//     the sole-writer domains (sep41) outright — the events-goroutine
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

// PersistEvents drains `in` and writes each event to its hypertable
// via the supplied store. Returns when ctx is canceled and the
// channel has been drained, or when the channel is closed.
//
// One goroutine drains; per-event work is sequential. Throughput is
// bounded by InsertTrade / InsertOracleUpdate latency. If that ever
// becomes the bottleneck, the right fix is per-pair sharding inside
// the store, not parallel sinks here — sequential ordering keeps the
// trades hypertable's per-(source, pair, ts) uniqueness sane.
//
// Cursor-vs-channel safety: callers (the indexer's pipeline + the
// backfill subcommand) advance their per-source cursor AFTER
// ProcessLedger enqueues events to `in`, but BEFORE this sink
// writes them to postgres. If we returned on ctx cancellation
// without draining, up to cap(in) buffered events would be silently
// dropped while the cursor's "I processed up to ledger N" claim
// stayed advanced — the trades for those ledgers would be missing
// even though `-resume` would skip them on restart. The drain
// below uses a fresh context (parent ctx is already canceled, so
// postgres calls would fail) bounded to 30s so a stuck shutdown
// can't hang the binary forever; if the deadline trips, the
// remaining buffered events are dropped and logged.
//
// The `mode` parameter is new with ADR-0032 Phase 4: see the
// [SinkMode] godoc for why the dispatcher's events-goroutine
// skips Soroban-derived events once the projector is sole writer.
//
// PersistEvents launches [PersistWorkers] concurrent drain
// goroutines, each maintaining its own trade-batch buffer + PG
// connection. Live-r1 incident 2026-06-01: the single-goroutine
// drain capped throughput at ~5 trades/sec (single PG roundtrip in
// flight at any time) even with batched INSERTs; the indexer's
// ProcessLedger goroutine was blocked on `events <- ev` waiting for
// drain progress, so the cursor advanced ~1 ledger/min vs the ~10/min
// network rate.
//
// Go's channel semantics let multiple receivers safely share one
// channel — each receive consumes one element atomically. The
// PostgreSQL pool (PoolMaxOpenConns = 25) carries the concurrent
// writes; each goroutine claims a connection per flush, releases
// it after, so a small worker pool of 4 fits comfortably under the
// pool ceiling alongside the aggregator + api binaries on the same
// host.
//
// Per-event ordering within a source is NOT preserved across workers
// (a later event can flush before an earlier one). The trades
// hypertable's PK includes the full identity (source, ledger,
// tx_hash, op_index, ts) so identical writes race-but-resolve via
// ON CONFLICT DO NOTHING. There is no ordering constraint at the
// storage layer that a parallel drain breaks.
//
// `mode` semantics unchanged from the single-goroutine version.
func PersistEvents(ctx context.Context, logger *slog.Logger, store *timescale.Store, in <-chan consumer.Event, mode SinkMode) {
	// Bounded async retry buffer for external (CEX/FX) trades that hit
	// an infrastructure fault (ADR-0041 / 2026-07-06 Postgres outage).
	// On-chain trades block-and-retry instead (cursor gating); external
	// trades — no cursor, vendor-refillable — buffer here and drop-oldest
	// under sustained overflow so they never block the pipeline. A nil
	// store (unit tests hitting only the default/unhandled path) skips
	// the buffer + its goroutine entirely.
	var extBuf *externalRetryBuffer
	var bufWG sync.WaitGroup
	if store != nil {
		extBuf = newExternalRetryBuffer(store, logger, externalRetryBufferMaxDepth)
		bufWG.Add(1)
		go func() {
			defer bufWG.Done()
			extBuf.run(ctx)
		}()
	}

	var wg sync.WaitGroup
	for i := 0; i < PersistWorkers; i++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			persistWorker(ctx, logger, store, in, mode, workerID, extBuf)
		}(i)
	}
	wg.Wait()
	// Workers have drained + block-retried their buffers; let the
	// external buffer's background retrier finish its final drain.
	bufWG.Wait()
}

// PersistWorkers is the count of concurrent drain goroutines run by
// PersistEvents. Sized to balance PG-pool capacity (25) and worker
// throughput. Live r1 2026-06-01: 4 workers gave ~5 ledgers/min vs
// the ~10 ledgers/min network rate. 8 workers lifts processing
// rate above the network rate so the cursor's last_updated stays
// fresh enough for the SLA-freshness threshold. Peak PG-conn use
// is still well under the 25-conn pool ceiling.
const PersistWorkers = 8

//nolint:gocognit,contextcheck // batched-drain loop has natural fan-out: ctx.Done, ticker, channel — splitting hurts readability of the flush invariants. The shutdown flush intentionally uses a fresh context (parent is canceled); see flushShutdown.
func persistWorker(ctx context.Context, logger *slog.Logger, store *timescale.Store, in <-chan consumer.Event, mode SinkMode, workerID int, extBuf *externalRetryBuffer) {
	tradeBuf := make([]canonical.Trade, 0, tradeBatchSize)
	flushTicker := time.NewTicker(tradeBatchFlushInterval)
	defer flushTicker.Stop()

	// flushWith writes the buffered batch through the resilient sink.
	// buf routes external trades to the async retry buffer; passing nil
	// (shutdown paths) makes external trades block-and-retry within the
	// bounded shutdown context instead, since the buffer's background
	// retrier is winding down (2026-07-06 outage fix).
	flushWith := func(fctx context.Context, buf *externalRetryBuffer) {
		if len(tradeBuf) == 0 {
			return
		}
		batch := tradeBuf
		tradeBuf = make([]canonical.Trade, 0, tradeBatchSize)
		flushTradeBatch(fctx, logger, store, buf, batch, workerID)
	}
	flush := func(fctx context.Context) { flushWith(fctx, extBuf) }

	// flushShutdown flushes this worker's in-memory tradeBuf on the
	// shutdown paths (parent ctx canceled OR channel closed) using a
	// FRESH bounded context. F-1318: the parent ctx is already
	// canceled by the time those arms fire, so passing it to
	// BatchInsertTrades / persistTrade makes every postgres call fail
	// instantly and silently drops the buffered trades. The fresh
	// context (same pattern as drainBufferedEvents) lets the final
	// flush actually land, bounded by drainTimeout so a hung shutdown
	// can't pin the binary forever.
	flushShutdown := func() {
		if len(tradeBuf) == 0 {
			return
		}
		fctx, cancel := context.WithTimeout(context.Background(), drainTimeout)
		defer cancel()
		flushWith(fctx, nil)
	}

	for {
		select {
		case <-ctx.Done():
			// C2-17: shutdown requested. Before exiting, drain every event
			// THIS worker can pull right now, so the racy select — which can
			// pick this arm even while events sit buffered in `in` — cannot
			// drop in-flight work. The drain is NON-BLOCKING (the default arm
			// breaks out the instant `in` is momentarily empty), so a channel
			// the producer hasn't closed yet can't pin the worker: worker 0's
			// blocking drainBufferedEvents below still catches late arrivals
			// and the channel close. The parent ctx is already cancelled, so
			// trade-shaped events go to tradeBuf (flushed by flushShutdown with
			// a fresh ctx) and everything else drains under a fresh bounded ctx
			// (F-1318) — never the cancelled parent, which would fail every
			// insert instantly and silently drop the event.
			shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), drainTimeout)
		drainInFlight:
			for {
				select {
				case ev, ok := <-in:
					if !ok {
						break drainInFlight
					}
					if skipInSink(ev, mode) {
						continue
					}
					if t, ok := tradeFromEvent(ev); ok {
						tradeBuf = append(tradeBuf, t)
						continue
					}
					_ = HandleEvent(shutdownCtx, logger, store, ev)
				default:
					break drainInFlight
				}
			}
			shutdownCancel()
			flushShutdown()
			// Only the first worker handles the blocking shutdown drain
			// (catches events that arrive after our non-blocking sweep + the
			// channel close) to avoid duplicate drain work; the others exit.
			if workerID == 0 {
				drainBufferedEvents(in, logger, store, mode)
			}
			return
		case <-flushTicker.C:
			flush(ctx)
		case ev, ok := <-in:
			if !ok {
				flushShutdown()
				return
			}
			if skipInSink(ev, mode) {
				continue
			}
			if t, ok := tradeFromEvent(ev); ok {
				obs.SourceEventsTotal.WithLabelValues(t.Source).Inc()
				obs.SourceLastEventUnix.WithLabelValues(t.Source).Set(float64(time.Now().Unix()))
				tradeBuf = append(tradeBuf, t)
				if len(tradeBuf) >= tradeBatchSize {
					flush(ctx)
				}
				continue
			}
			// Dispatcher drain: log-and-continue. HandleEvent already logs +
			// counts the failure internally; these events either double-write
			// (the projector owns the sole-writer domains) or ride the
			// block-and-retry trade path, so the drain doesn't gate on the err.
			_ = HandleEvent(ctx, logger, store, ev)
		}
	}
}

// tradeBatchSize caps the trades-per-batch in BatchInsertTrades.
// Sized to fit comfortably under PostgreSQL's max-bind-params limit
// (32767 parameters); each row has 12 placeholders, so 200 rows =
// 2400 placeholders — well under. Production throughput is roughly
// linear in this until either PG's planning cost or the events
// channel runs dry; 200 is the operating point validated post the
// r1 2026-06-01 incident.
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
// per-event slow path (correctness-equivalent, performance bug).
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
	case sdex.TradeEvent:
		return e.Trade, true
	case external.TradeEvent:
		return e.Trade, true
	default:
		return canonical.Trade{}, false
	}
}

// IsProjectedEvent reports whether the ADR-0032 projector handles
// `ev`. Phase 4+ the dispatcher's events-goroutine skips these so
// the projector owns the write outright. Non-projected events
// (sdex, external CEX/FX, band, supply observers) continue through
// the events-goroutine because they don't flow through
// soroban_events.
//
// MUST stay in lockstep with `internal/projector/registry.go`
// `buildSource` — every consumer.Event a registered source can emit
// must return true here. Guarded by lockstep_ast_test.go
// (TestLockstep_RegistrySourcesFullyWired), which AST-walks this
// switch, the registry cases, and every projected source package's
// consumer.Event implementations — a missed wiring edit fails CI
// instead of silently dropping rows (F-1316 class). A prior version
// of this comment cited an "ADR-0030 lint guard" that never existed.
func IsProjectedEvent(ev consumer.Event) bool {
	switch ev.(type) {
	case soroswap.TradeEvent, soroswap.SkimEvent,
		aquarius.TradeEvent, aquarius.ReservesEvent, aquarius.LiquidityEvent,
		aquarius.RewardsEvent, aquarius.AdminEvent,
		phoenix.TradeEvent, phoenix.LiquidityEvent, phoenix.StakeEvent,
		comet.TradeEvent, comet.LiquidityEvent,
		reflector.UpdateEvent, redstone.UpdateEvent,
		blend.NewAuctionEvent, blend.FillAuctionEvent, blend.DeleteAuctionEvent,
		blend.PositionEvent, blend.EmissionEvent, blend.AdminEvent,
		blend_backstop.Event,
		blend_emitter.DistributeEvent, blend_emitter.DropEvent, blend_emitter.SwapConfigEvent,
		cctp.Event, rozo.Event,
		sorocredit.Event,
		defindex.Event, defindex.VaultEvent,
		sep41_supply.Event, sep41_transfers.Event:
		return true
	default:
		// sdex.TradeEvent, external.TradeEvent, external.UpdateEvent,
		// band.UpdateEvent, soroswap_router.Event (log-only), supply
		// observers (accounts / trustlines / claimable_balances /
		// liquidity_pools / sac_balances) — all out of scope for the
		// projector per ADR-0032.
		return false
	}
}

// IsSoleWriterProjected reports whether the projector has EARNED
// sole-writer status for ev's domain — i.e. the domain is fully
// re-derived and its projection is verified by the default ADR-0033
// reconcile catalogue, so the projector owns the write outright even
// in Phase-3 parallel mode and the dispatcher's events-goroutine must
// skip it REGARDLESS of the `PersistPerSource` flag. This closes the
// F-1316 config foot-gun: the sep41 domain's write path no longer
// depends on a flag whose zero-value (false) once silently dropped
// all sep41 rows.
//
// The set is deliberately narrow — a strict SUBSET of
// [IsProjectedEvent] (guarded by TestSoleWriter_SubsetOfProjected).
// A source is added here only after its full-history re-derive lands
// AND it enters the compute-completeness catalogue
// (cmd/stellarindex-ops/reconciliation_catalogue.go), so an
// undetected projector regression can't silently lose rows the
// dispatcher used to double-write. Today that is only the sep41
// domain (TASK #16b, 2026-07-06 re-derive + f457f2a4 catalogue
// promotion); every other projected source stays in Phase-3 parallel
// (double-write) until it too is promoted.
func IsSoleWriterProjected(ev consumer.Event) bool {
	switch ev.(type) {
	case sep41_supply.Event, sep41_transfers.Event:
		return true
	default:
		return false
	}
}

// drainBufferedEvents writes any remaining buffered events using a
// fresh shutdown context so postgres calls succeed past the parent
// context's cancellation. Bounded by [drainTimeout] so a hung
// shutdown can't keep the binary alive indefinitely; on deadline, it
// surfaces the exact undrained ledger range at ERROR (recoverable from
// the CH lake per ADR-0034) rather than dropping it silently.
//
// Deliberately does not take a context parameter — the whole reason
// this exists is to keep writing past the parent's cancellation.
//
//nolint:contextcheck,gocognit // intentional fresh context + batched-drain fan-out; see godoc above.
func drainBufferedEvents(in <-chan consumer.Event, logger *slog.Logger, store *timescale.Store, mode SinkMode) {
	drainCtx, cancel := context.WithTimeout(context.Background(), drainTimeout)
	defer cancel()
	tradeBuf := make([]canonical.Trade, 0, tradeBatchSize)
	flushTrades := func() {
		if len(tradeBuf) == 0 {
			return
		}
		batch := tradeBuf
		tradeBuf = make([]canonical.Trade, 0, tradeBatchSize)
		// nil extBuf: at shutdown the async external buffer is winding
		// down, so every trade block-retries within the bounded drainCtx
		// (2026-07-06 outage fix). An infra fault here abandons after the
		// drainTimeout and logs the recoverable ledger range at ERROR.
		flushTradeBatch(drainCtx, logger, store, nil, batch, -1)
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
			_ = HandleEvent(drainCtx, logger, store, ev) // shutdown drain: log-and-continue (HandleEvent logs internally)
		case <-drainCtx.Done():
			flushTrades()
			// C2-17 + G15-08: the bounded drain deadline tripped with events
			// possibly still buffered. Make a FINAL best-effort pass to
			// PERSIST the immediately-available remainder under a FRESH short
			// context (the parent drainCtx is done; passing it would fail every
			// insert instantly). Non-blocking (default arm) so the producer
			// having stopped means we drain the fixed buffered set and exit,
			// never block on a fresh arrival. Anything this STILL can't land is
			// recoverable — under ADR-0034 every raw op is durably in the
			// ClickHouse lake — so it's surfaced at ERROR with its exact ledger
			// span for `stellarindex-ops ch-rebuild -sdex-gaps` + the
			// completeness timer, instead of becoming a silent served-tier gap.
			//
			// Count EVERY undrained event, not just trade-shaped ones (G15-08):
			// oracle updates, supply observations, blend / cctp / rozo rows are
			// served-tier writes too. Trade ledger bounds come from trade-shaped
			// events (only they carry a Ledger we can range on) so the re-derive
			// hint stays actionable.
			finalCtx, finalCancel := context.WithTimeout(context.Background(), drainTimeout)
			finalTrades := make([]canonical.Trade, 0, tradeBatchSize)
			var total, trades int
			var minL, maxL uint32
		drainRemainder:
			for {
				select {
				case ev, ok := <-in:
					if !ok {
						break drainRemainder
					}
					total++
					t, isTrade := tradeFromEvent(ev)
					if isTrade {
						trades++
						if minL == 0 || t.Ledger < minL {
							minL = t.Ledger
						}
						if t.Ledger > maxL {
							maxL = t.Ledger
						}
					}
					if skipInSink(ev, mode) {
						continue
					}
					if isTrade {
						finalTrades = append(finalTrades, t)
						continue
					}
					_ = HandleEvent(finalCtx, logger, store, ev)
				default:
					break drainRemainder
				}
			}
			if len(finalTrades) > 0 {
				// nil extBuf: same shutdown block-and-retry posture as flushTrades.
				flushTradeBatch(finalCtx, logger, store, nil, finalTrades, -1)
			}
			finalCancel()
			if total > 0 {
				logger.Error("PersistEvents drain deadline exceeded — made a final best-effort persist pass; any residual is recoverable from the CH lake, re-derive this ledger range if the served tier is short",
					"undrained_events", total, "undrained_trades", trades,
					"ledger_from", minL, "ledger_to", maxL)
			} else {
				logger.Warn("PersistEvents drain deadline exceeded — no events undrained",
					"buffered", len(in))
			}
			return
		}
	}
}

// drainTimeout caps how long PersistEvents will spend writing
// already-buffered events on shutdown. 90s is comfortable headroom:
// a 256-deep buffer at typical 1ms-per-insert latency drains in
// ~250 ms; 90s tolerates a 300x slowdown (e.g. postgres saturated by
// a concurrent VACUUM) before giving up. If the deadline trips anyway
// (e.g. postgres genuinely down), drainBufferedEvents logs the exact
// undrained ledger range at ERROR rather than dropping silently — the
// raw ops are in the CH lake (ADR-0034), so the range is recoverable
// via `stellarindex-ops ch-rebuild -sdex-gaps` and the completeness timer.
const drainTimeout = 90 * time.Second

// HandleEvent dispatches one event to its hypertable insert and RETURNS
// the underlying Insert error (nil on success). Panic-recovers so a
// single malformed Amount can't take the whole sink down — a recovered
// panic is surfaced as the returned error too.
//
// The error return is load-bearing for the ADR-0032 projector
// (audit-2026-07-16 C2-1): the projector invokes HandleEvent as its
// per-event sink and gates its cursor on the result — a write that fails
// transiently must NOT let the cursor advance past that ledger, or a
// sole-writer (sep41) row is permanently lost. The dispatcher's
// events-goroutine drain and the ops re-derive tools still call
// HandleEvent for log-and-continue and DISCARD the error (their events
// either double-write or ride the block-and-retry trade path), so every
// caller compiles against the one signature.
//
// A recovered panic is returned as a generic (non-classified) error: the
// projector treats it as transient (retry-and-alert) per its safe-side
// default. This is acceptable because the sole-writer sep41 insert path
// validates + returns errors rather than panicking, and decoder panics
// are caught upstream in the projector's own processEventSafely recover.
//
// Exported for use by the projector (`internal/projector`) and the ops
// re-derive tools (`stellarindex-ops projected-rebuild` / `ch-rebuild`).
func HandleEvent(ctx context.Context, logger *slog.Logger, store *timescale.Store, ev consumer.Event) (retErr error) { //nolint:gocyclo,funlen // dispatch table; one case per consumer.Event implementation. Splitting would reduce clarity.
	defer func() {
		if r := recover(); r != nil {
			logger.Error("panic in event sink — recovered",
				"panic", fmt.Sprintf("%v", r),
				"kind", ev.EventKind(),
				"source", ev.Source())
			obs.SourceInsertErrorsTotal.WithLabelValues(ev.Source(), "panic").Inc()
			retErr = fmt.Errorf("pipeline: panic in event sink for %s/%s: %v", ev.Source(), ev.EventKind(), r)
		}
	}()

	source := ev.Source()
	if source == "" {
		logger.Warn("event with empty source", "kind", ev.EventKind())
		source = "_unknown"
	}
	obs.SourceEventsTotal.WithLabelValues(source).Inc()
	obs.SourceLastEventUnix.WithLabelValues(source).Set(float64(time.Now().Unix()))

	// Every arm RETURNS its persist result so the projector can gate its
	// cursor on a sink failure (audit-2026-07-16 C2-1). Trade-shaped events
	// are the one exception: persistTrade owns ADR-0041 block-and-retry
	// (infra faults BLOCK the caller, data faults skip) so it never silently
	// loses a write — those arms return nil and let that policy stand rather
	// than double-gating it here.
	switch e := ev.(type) {
	case soroswap.TradeEvent:
		persistTrade(ctx, logger, store, e.Trade)
		return nil
	case soroswap.SkimEvent:
		return persistSoroswapSkim(ctx, logger, store, e)
	case aquarius.TradeEvent:
		persistTrade(ctx, logger, store, e.Trade)
		return nil
	case aquarius.ReservesEvent:
		return persistAquariusReserves(ctx, logger, store, e)
	case aquarius.LiquidityEvent:
		return persistAquariusLiquidity(ctx, logger, store, e)
	case aquarius.RewardsEvent:
		return persistAquariusRewards(ctx, logger, store, e)
	case aquarius.AdminEvent:
		return persistAquariusAdmin(ctx, logger, store, e)
	case phoenix.TradeEvent:
		persistTrade(ctx, logger, store, e.Trade)
		return nil
	case phoenix.LiquidityEvent:
		return persistPhoenixLiquidity(ctx, logger, store, e)
	case phoenix.StakeEvent:
		return persistPhoenixStake(ctx, logger, store, e)
	case comet.TradeEvent:
		persistTrade(ctx, logger, store, e.Trade)
		return nil
	case comet.LiquidityEvent:
		return persistCometLiquidity(ctx, logger, store, e)
	case sdex.TradeEvent:
		persistTrade(ctx, logger, store, e.Trade)
		return nil
	case reflector.UpdateEvent:
		return persistOracle(ctx, logger, store, e.Update)
	case redstone.UpdateEvent:
		return persistOracle(ctx, logger, store, e.Update)
	case band.UpdateEvent:
		return persistOracle(ctx, logger, store, e.Update)
	case soroswap_router.Event:
		// Persist to the soroswap_router_swaps hypertable (migration
		// 0049). Pre-Phase-B this was log-only — the source had no
		// per-source gap-detector signal because there was no row
		// to count. Now we write a row per router invocation; the
		// gap-detector target on the hypertable measures honest
		// coverage. Counter bump still surfaces activity on the
		// `entries` column of /v1/diagnostics/ingestion.
		bumpEntryCount(ctx, logger, store, soroswap_router.SourceName)
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
			// ROADMAP #11 tree-position columns (migration 0101):
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
			// switch (audit-2026-07-16 C4-3): before this, the router /
			// defindex cases were the ONLY persist paths that logged a
			// Warn without bumping SourceInsertErrorsTotal, so a dropped
			// swap was invisible to metrics/alerts. Callers that discard
			// this returned err (the dispatcher drain's `_ = HandleEvent`)
			// then dropped the row silently. Counter only — control flow
			// (return err) is unchanged.
			obs.SourceInsertErrorsTotal.WithLabelValues(soroswap_router.SourceName, "soroswap_router_swap").Inc()
			logger.Warn("soroswap-router persist failed",
				"source", soroswap_router.SourceName,
				"tx_hash", e.Swap.TxHash, "ledger", e.Swap.Ledger,
				"err", err)
			return err
		}
		return nil
	case defindex.Event:
		// Strategy-layer flow (vault → strategy capital movement).
		// Persists to defindex_flows with layer='strategy' (migration
		// 0050). `actor` here is the vault contract C-strkey
		// (the strategy contract's `from` field); end-user attribution
		// lives at the vault layer (case defindex.VaultEvent below).
		bumpEntryCount(ctx, logger, store, defindex.SourceName)
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
			// Count the persist failure (audit-2026-07-16 C4-3) — see the
			// soroswap-router case above for why. Counter only; return err
			// (control flow) unchanged.
			obs.SourceInsertErrorsTotal.WithLabelValues(defindex.SourceName, "defindex_flow_strategy").Inc()
			logger.Warn("defindex strategy persist failed",
				"source", defindex.SourceName,
				"tx_hash", e.Flow.TxHash, "ledger", e.Flow.Ledger,
				"err", err)
			return err
		}
		return nil
	case defindex.VaultEvent:
		// Vault-wrapper layer (user-facing deposit/withdraw).
		// Persists to defindex_flows with layer='vault'. `actor` here
		// is the end-user G-strkey (or routing C-strkey if the user
		// came via an aggregator).
		bumpEntryCount(ctx, logger, store, defindex.SourceName)
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
			// Count the persist failure (audit-2026-07-16 C4-3) — see the
			// soroswap-router case above for why. Counter only; return err
			// (control flow) unchanged.
			obs.SourceInsertErrorsTotal.WithLabelValues(defindex.SourceName, "defindex_flow_vault").Inc()
			logger.Warn("defindex vault persist failed",
				"source", defindex.SourceName,
				"tx_hash", e.Flow.TxHash, "ledger", e.Flow.Ledger,
				"err", err)
			return err
		}
		return nil
	case external.TradeEvent:
		persistTrade(ctx, logger, store, e.Trade)
		return nil
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

// persistTrade writes one trade with infrastructure-resilience
// (ADR-0041 / 2026-07-06 Postgres-outage fix). An infra fault
// (connection refused/reset, PG restarting) is RETRIED with
// backpressure — blocking the caller so an on-chain cursor can't
// advance past an un-landed trade — rather than dropped. A data fault
// (constraint / numeric / validation) is error-and-skipped exactly as
// before: permanent for that row, so retrying would just wedge the
// pipeline. Also used by the projector's per-event sink (HandleEvent),
// which gains the same cursor-gating retry.
//
// Takes the narrow [tradeWriter] interface (satisfied by
// *timescale.Store) so the retry path is unit-testable with a fake.
//
// NOT the instrumentation point for the unit-ratio sentinel
// (stellarindex_dex_trade_unit_ratio_total, 2026-07-07 Phoenix
// incident): the dispatcher's primary live path routes trades through
// [flushTradeBatch] → w.BatchInsertTrades, bypassing this function
// entirely on the success case, so a check here would silently miss
// the majority of on-chain trades. See
// timescale.isDexUnitRatioTrade's godoc for the actual choke point.
func persistTrade(ctx context.Context, logger *slog.Logger, w tradeWriter, t canonical.Trade) {
	// usd_volume coverage is counted inside InsertTrade /
	// BatchInsertTrades (see timescale.usdPopulatedLabel), NOT here.
	// This function is only one of the paths trades take — the
	// dispatcher's primary batch path and every external connector
	// bypass it — so a counter here measured a small, unrepresentative
	// slice. Counting at the storage choke point also removes the
	// second full resolution this used to run per trade.
	if err := retryInfra(ctx, logger, "insert_trade", func(c context.Context) error {
		return w.InsertTrade(c, t)
	}); err != nil {
		obs.SourceInsertErrorsTotal.WithLabelValues(t.Source, "trade").Inc()
		if isCtxErr(err) {
			// Shutdown mid-retry — the raw op is durable in the CH lake
			// (ADR-0034), so this row is re-derivable; surface it loudly
			// instead of dropping silently.
			logger.Error("insert trade abandoned on shutdown — recoverable from the CH lake (ADR-0034); re-derive",
				"source", t.Source, "ledger", t.Ledger, "tx_hash", t.TxHash, "op_index", t.OpIndex, "err", err)
			return
		}
		logger.Error("insert trade failed",
			"source", t.Source,
			"ledger", t.Ledger,
			"tx_hash", t.TxHash,
			"op_index", t.OpIndex,
			"err", err,
		)
		return
	}
	logger.Debug("trade ingested",
		"source", t.Source,
		"ledger", t.Ledger,
		"pair", t.Pair.String(),
	)
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
	obs.OracleLastUpdateUnix.WithLabelValues(u.Source, u.Asset.String()).
		Set(float64(u.Timestamp.Unix()))
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

// persistAquariusReserves lands one update_reserves observation (the
// pool's POST-STATE reserve vector) into aquarius_reserves — one row
// per token position (migration 0089). The first real Aquarius TVL /
// liquidity-depth signal; Aquarius has no published price so these
// rows never reach VWAP.
func persistAquariusReserves(ctx context.Context, logger *slog.Logger, store *timescale.Store, e aquarius.ReservesEvent) error {
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
// twelve kinds — migration 0099, ROADMAP #89) into
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
// of the eight kinds — migration 0100, ROADMAP #89) into
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

// persistBlendEmitterSwapConfig writes one `q_swap` / `swap` row via
// Store.InsertBlendEmitterSwapConfig.
func persistBlendEmitterSwapConfig(ctx context.Context, logger *slog.Logger, store *timescale.Store, e blend_emitter.SwapConfigEvent) error {
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
		UnlockTime:       time.Unix(int64(e.UnlockTime), 0).UTC(), //nolint:gosec // UnlockTime is a Soroban-emitted Unix timestamp, in range.
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
		sorocredit.TypeSupportedAssetAdded, sorocredit.TypeCollateralHashUpdated:
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
// in the INSERT). Surfaces source-attributed protocol activity on
// /v1/diagnostics/ingestion's `entries` column for the broader set
// of sources (blend lending, soroswap-router + defindex log-only
// sinks). Errors are logged at Warn — a failed bump doesn't fail
// the underlying decode/persist; the operator's periodic
// `stellarindex-ops seed-entry-counts` reconciles drift.
func bumpEntryCount(ctx context.Context, logger *slog.Logger, store *timescale.Store, source string) {
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
// sep41_transfers hypertable. F-0021 closure (audit-2026-05-26):
// unlocks per-account net-position queries — the Stellar moat
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
