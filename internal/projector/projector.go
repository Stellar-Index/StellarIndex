// Package projector tails the `soroban_events` raw-event landing
// zone (ADR-0029) and writes per-source classifier rows by
// invoking each protocol's existing Go decoder
// (`internal/sources/<protocol>/decode.go`). Per ADR-0032 the
// projector is the SINGLE write path for per-source tables;
// during Phase 3 it runs in parallel with the dispatcher's
// existing per-source sink (both write, ON CONFLICT DO NOTHING
// absorbs duplicates) so we can verify projection rate matches
// live ingest before Phase 4 makes the projector primary.
//
// Architecture (one component, many cursors):
//
//	soroban_events  (raw, authoritative)
//	     │
//	     ▼  StreamSorobanEvents from cursor.last_ledger
//	   Projector
//	     │
//	     ├─► aquarius.Decoder ──► persistTrade            (trades)
//	     ├─► blend.Decoder    ──► persistBlend*           (blend_*)
//	     ├─► phoenix.Decoder  ──► persistPhoenix*         (phoenix_*)
//	     ├─► ... per protocol
//	     ▼
//	   projector.cursor[source].last_ledger  (advances per cycle)
//
// Per-source cursors mean one stuck source (e.g. a decoder bug
// flooding decode_errors) doesn't block the others — each loops
// independently.
//
// Parallel-mode safety (Phase 3): the dispatcher's pre-existing
// per-source sink runs unchanged. Both writers race for the same
// (ledger, tx_hash, op_index, …) PK; ON CONFLICT DO NOTHING means
// whichever wins, the other no-ops. The projector's correctness
// signal is `projector_lag_ledgers` — if it stays low, the
// projector is keeping up; Phase 4 flips the dispatcher's
// per-source sink off and the projector becomes sole writer.
package projector

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/consumer"
	"github.com/Stellar-Index/StellarIndex/internal/dispatcher"
	"github.com/Stellar-Index/StellarIndex/internal/events"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/sources/sorobanevents"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// Interval is the catch-up cadence. The projector reads new
// soroban_events rows every Interval; between cycles the
// projector is idle. Right-sized to balance read-after-write
// latency (smaller is fresher) with Postgres scan overhead
// (smaller is more queries). 5s is a default that keeps r1's
// per-source tables ~5-10s behind raw; tunable per-deployment.
const Interval = 5 * time.Second

// BatchLimit caps how many ledgers the projector reads per source
// per cycle. Without a cap a catch-up after long outage would stream
// millions of rows in one transaction, blocking other work. Keep this
// small enough that dense protocol ranges, notably Aquarius reserve
// updates, finish inside PerSourceTimeout.
const BatchLimit = 1_000

// MinBatchLimit is the floor for the adaptive per-source window (see
// cycleOneSource): when a cycle exceeds PerSourceTimeout the window
// halves down to this floor. 25 dense mainnet ledgers decode + insert
// comfortably inside the timeout even for the heaviest sources
// (2026-07-10 incident: a maximally-dense aquarius rewards window at
// BatchLimit could NOT finish inside PerSourceTimeout, so the fixed
// window retried the identical range forever — a permanent stall the
// operator could only see as "lag stopped falling").
const MinBatchLimit = 25

// PerSourceTimeout caps one source's per-cycle work. A wedged
// downstream sink can't block other sources past this.
const PerSourceTimeout = 60 * time.Second

// SinkFunc is the per-event handler the projector calls after
// successful decode. `internal/pipeline/sink.go::HandleEvent` is the
// production wiring (it persists the decoded event to its per-source
// hypertable and RETURNS the underlying Insert error).
//
// The error return is load-bearing (audit-2026-07-16 C2-1/D1): a sink
// write can fail transiently (a Postgres deadlock / connection reset /
// statement-timeout) or permanently (a CHECK violation on a poison row).
// The projector consumes the error to decide whether to advance its
// cursor past the event's ledger:
//
//   - a TRANSIENT failure ([timescale.IsPermanentDataError] false) holds
//     the cursor at the last fully-committed ledger, so the next cycle
//     re-reads and retries the row. ON CONFLICT in the downstream Insert*
//     (DO NOTHING, or DO UPDATE since migration 0109) makes the retry
//     idempotent / corrective.
//   - a PERMANENT data fault ([timescale.IsPermanentDataError] true) is
//     logged loudly, counted, and SKIPPED (the cursor advances past it) —
//     blocking forever on a poison row is a worse outage than dropping it.
//
// Before this signature carried an error the projector could not see a
// sink failure at all: it advanced the cursor unconditionally on stream
// success, so a transient fault during a sole-writer (sep41) cycle
// permanently dropped that row (the loss C2-1 documents).
type SinkFunc func(ctx context.Context, ev consumer.Event) error

// Source describes one protocol's projection target. The
// projector keeps an independent cursor per source so one stuck
// decoder doesn't block the rest.
type Source struct {
	// Name is the cursor sub_source key + log label. Must be
	// unique within a Registry. Examples: "aquarius", "blend",
	// "phoenix", "soroswap-skim".
	Name string

	// Decoder is the protocol-specific event handler. Same
	// interface the dispatcher uses; the projector calls
	// Matches + Decode in the same order.
	Decoder dispatcher.Decoder

	// ContractIDs / Topic0Syms narrow the SQL pre-filter so the
	// projector doesn't stream irrelevant rows. Pass nil for
	// "match by Decoder.Matches alone" — coarser network read
	// but simpler config. Mirrors `StreamSorobanEvents`'s args.
	ContractIDs []string
	Topic0Syms  []string

	// ExcludeTopic0Syms drops events whose topic[0] symbol is in the
	// list at the SQL layer (topic_0_sym NOT IN …). For the DEX/lending
	// sources that dispatch by their own topic[0] symbols and have no
	// contract/topic prefilter, this excludes the CAP-67 classic-token
	// firehose (transfer/mint/burn/…) — which under the r1 archive's
	// uniform V4 meta is 99.999% of contract_events / soroban_events. A
	// caught-up source reads a tiny window so it never mattered, but a
	// far-behind source scanning a 10k-ledger catch-up window would pull
	// millions of firehose rows it then discards via Decoder.Matches,
	// blowing the cycle budget and wedging the source (the aquarius case).
	// Exclude-only and safe: these decoders never consume classic-token
	// topics, so no protocol event is dropped. Leave nil for sources that
	// DO consume those topics (sep41_*) or already prefilter by contract
	// (reflector/redstone).
	ExcludeTopic0Syms []string
}

// Registry is the set of sources the projector handles. Built
// once at startup; immutable while the projector runs.
type Registry struct {
	Sources []Source
}

// Projector reads soroban_events and routes decoded events to
// the sink for each registered source.
type Projector struct {
	store    *timescale.Store
	registry Registry
	sink     SinkFunc
	logger   *slog.Logger

	// chAddr, when non-empty, switches the per-source read from the Postgres
	// soroban_events landing zone to the ClickHouse Tier-1 lake's
	// contract_events (ADR-0034 #10 feed-switch — the dual-sink feeds CH
	// inline, so CH is authoritative for forward events and soroban_events can
	// be decommissioned). The per-source cursor (last_ledger) is
	// source-agnostic, so the switch is seamless. Empty = legacy
	// soroban_events read.
	chAddr string
}

// SetClickHouseSource switches the projector to read forward events from the
// ClickHouse lake at addr instead of Postgres soroban_events (ADR-0034 #10).
// Call before Run. Empty addr keeps the legacy soroban_events source.
func (p *Projector) SetClickHouseSource(addr string) { p.chAddr = addr }

// New constructs a Projector. Callers must call Run to start
// the loop.
func New(store *timescale.Store, registry Registry, sink SinkFunc, logger *slog.Logger) *Projector {
	if logger == nil {
		logger = slog.Default()
	}
	return &Projector{
		store:    store,
		registry: registry,
		sink:     sink,
		logger:   logger,
	}
}

// Run blocks until ctx is cancelled. Drives one goroutine per
// source; each independently tails its slice of soroban_events
// and advances its cursor.
func (p *Projector) Run(ctx context.Context) error {
	if p.store == nil {
		return errors.New("projector: nil store")
	}
	if p.sink == nil {
		return errors.New("projector: nil sink")
	}
	if len(p.registry.Sources) == 0 {
		p.logger.Warn("projector: empty registry; nothing to project")
		<-ctx.Done()
		return ctx.Err()
	}

	var wg sync.WaitGroup
	for _, src := range p.registry.Sources {
		wg.Add(1)
		go func(src Source) {
			defer wg.Done()
			p.runOneSource(ctx, src)
		}(src)
	}
	wg.Wait()
	return ctx.Err()
}

// processEventSafely runs one raw lake row through a source's decoder + sink
// under a per-row recover (X9, audit-2026-06-14). The dispatcher path recovers
// decoder panics in pipeline.ProcessLedger; the projector runs the SAME
// decoders on raw lake rows (including historical / upgraded-WASM shapes —
// "backfill sees every prior version") in a bare goroutine inside the LIVE
// indexer. Without this, a panic on one poison row crashes the whole indexer,
// and because the cursor doesn't advance past the bad row, restart re-reads it
// into a crash-loop.
//
// Returns:
//   - emitted:    the number of decoded outputs that were successfully
//     sinked (durably committed). On a mid-row sink failure it counts only
//     the outputs that committed BEFORE the failing one.
//   - decodeFail: true when the row is a DECODE failure — a returned decode
//     error OR a recovered panic. A deterministically broken row would only
//     re-fail on retry, so the caller advances the cursor regardless (the
//     failure is counted for visibility).
//   - sinkErr:    the FIRST sink (downstream write) error for this row, or
//     nil. Unlike a decode failure this is NOT necessarily deterministic —
//     the caller classifies it ([timescale.IsPermanentDataError]) to decide
//     whether to hold the cursor for retry (transient) or skip (permanent).
//     A recovered decode panic returns sinkErr=nil (nothing was written).
func processEventSafely(src Source, ev events.Event, sink func(consumer.Event) error, log *slog.Logger) (emitted int, decodeFail bool, sinkErr error) {
	defer func() {
		if rec := recover(); rec != nil {
			emitted, decodeFail, sinkErr = 0, true, nil
			log.Error("projector decode panicked; skipping row",
				"source", src.Name, "ledger", ev.Ledger, "tx", ev.TxHash,
				"op_index", ev.OperationIndex, "event_index", ev.EventIndex, "panic", rec)
		}
	}()
	if !src.Decoder.Matches(ev) {
		return 0, false, nil
	}
	outs, derr := src.Decoder.Decode(ev)
	if derr != nil {
		return 0, true, nil
	}
	for _, out := range outs {
		if err := sink(out); err != nil {
			// Stop at the first sink failure for this row. `emitted` counts
			// the outputs that DID commit; the caller classifies sinkErr
			// against ev.Ledger to gate the cursor.
			return emitted, false, err
		}
		emitted++
	}
	return emitted, false, nil
}

// runOneSource is the per-source catch-up loop. Reads from the
// projector cursor's last_ledger forward, batches up to
// BatchLimit rows per cycle, advances the cursor on success.
func (p *Projector) runOneSource(ctx context.Context, src Source) {
	t := time.NewTicker(Interval)
	defer t.Stop()
	// Adaptive window, owned by this goroutine (one per source): starts
	// at BatchLimit, halves on a deadline-exceeded cycle, doubles back
	// on success. See cycleOneSource.
	window := uint32(BatchLimit)
	// First cycle runs immediately so a fresh deploy starts
	// catching up without waiting Interval.
	p.cycleOneSource(ctx, src, &window)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			p.cycleOneSource(ctx, src, &window)
		}
	}
}

// cycleOneSource runs one read-decode-write cycle for one source.
// Failure handling:
//   - read / tip / cursor errors → log + leave the cursor untouched; the
//     next cycle retries the same rows.
//   - decode failures (decode error / recovered panic) → count + SKIP the
//     row (deterministic; a retry would re-fail) and let the cursor advance.
//   - TRANSIENT sink write failures (audit-2026-07-16 C2-1) → cap the cursor
//     at the last fully-committed ledger so the failing ledger is re-read
//     next cycle; the idempotent downstream Insert* absorbs the retry. NEVER
//     advances past an un-committed row — the anti-silent-loss property this
//     cycle now actually implements (the SinkFunc godoc's old claim).
//   - PERMANENT sink data faults (CHECK / numeric) → log LOUD + count + SKIP,
//     because a poison row must not wedge the source forever.
//
//nolint:gocognit,funlen // linear cycle (cursor read → tip → scan → cursor write) with a source branch (soroban_events vs CH); splitting into helpers would scatter the cycle's success/failure metric emissions and make the control flow harder to audit.
func (p *Projector) cycleOneSource(ctx context.Context, src Source, window *uint32) {
	start := time.Now()
	cycleCtx, cancel := context.WithTimeout(ctx, PerSourceTimeout)
	defer cancel()

	cursor, err := p.store.GetCursor(cycleCtx, "projector", src.Name)
	if err != nil && !errors.Is(err, timescale.ErrNotFound) {
		p.logger.Warn("projector: read cursor failed", "source", src.Name, "err", err)
		obs.ProjectorRunsTotal.WithLabelValues(src.Name, "error").Inc()
		return
	}
	fromLedger := uint32(0)
	if err == nil {
		// Resume one ledger AFTER the last fully-processed one.
		// soroban_events.ledger BETWEEN $1 AND $2 is inclusive on
		// both ends so adding 1 here avoids reprocessing the seam.
		fromLedger = cursor.LastLedger + 1
	}

	// Upper bound: live tip from ledgerstream. Without a tip we
	// scan to "wherever soroban_events extends," which during a
	// fresh deploy could be far ahead of where live writes have
	// committed. Better to track ledgerstream so the projector
	// never gets ahead of "what we promise is durable." In CH
	// feed-switch mode the bound is additionally clamped to the
	// lake's provably-complete watermark (see resolveTip).
	tip, err := p.resolveTip(cycleCtx, fromLedger)
	if err != nil {
		p.logger.Warn("projector: tip resolve failed", "source", src.Name, "err", err)
		obs.ProjectorRunsTotal.WithLabelValues(src.Name, "error").Inc()
		return
	}
	if tip < fromLedger {
		// Caught up — nothing at or beyond fromLedger. Must be `<`, not `<=`:
		// fromLedger = cursor.LastLedger+1 is the next UNPROCESSED ledger and
		// the [fromLedger, tip] scan is inclusive, so when tip == fromLedger
		// there is exactly one ledger (the tip) still to project. `<=` skipped
		// it — leaving the served tier permanently one ledger behind the
		// durable tip, and a permanent hole if ingest halted exactly there.
		// Found by audit A04-H1.
		obs.ProjectorRunsTotal.WithLabelValues(src.Name, "idle").Inc()
		obs.ProjectorLagLedgers.WithLabelValues(src.Name).Set(0)
		return
	}

	toLedger := tip
	if toLedger-fromLedger > *window {
		toLedger = fromLedger + *window
	}

	var (
		rowsScanned    int
		eventsEmitted  int
		decodeErrors   int
		lastSeenLedger uint32

		// Sink-durability tracking (audit-2026-07-16 C2-1). A TRANSIENT
		// sink write failure must NOT let the cursor advance past its
		// ledger, or that row is permanently lost for a sole-writer (sep41)
		// domain. firstTransientFailLedger is the LOWEST ledger with such a
		// failure this cycle; the cursor is then capped at
		// firstTransientFailLedger-1 (the last FULLY-committed ledger) so the
		// next cycle re-reads and retries [firstTransientFailLedger, …]. A
		// PERMANENT data fault (poison row) does NOT hold the cursor — it is
		// counted + skipped so it can't wedge the source forever.
		firstTransientFailLedger uint32
		sinkTransientFails       int
		sinkPermanentFails       int
	)
	// process runs the per-event decode + route, identical regardless of the
	// read source (soroban_events or CH contract_events). Decode failures
	// soft-fail (cursor still advances; the row is deterministically broken so
	// a retry would re-fail) and are counted for visibility. A SINK failure is
	// classified: transient → cap the cursor below ev.Ledger for retry;
	// permanent → count + skip.
	process := func(ev events.Event) {
		emitted, decodeFail, sinkErr := processEventSafely(src, ev,
			func(out consumer.Event) error { return p.sink(cycleCtx, out) }, p.logger)
		eventsEmitted += emitted
		if decodeFail {
			decodeErrors++
			return
		}
		if sinkErr == nil {
			return
		}
		if timescale.IsPermanentDataError(sinkErr) {
			// Poison row: retrying can never succeed, so skipping (letting
			// the cursor advance past it) is safer than stalling the source
			// forever. Log LOUD + count so it surfaces as an alert.
			sinkPermanentFails++
			p.logger.Error("projector: PERMANENT sink failure — skipping poison row (cursor advances past it)",
				"source", src.Name, "ledger", ev.Ledger, "tx", ev.TxHash,
				"op_index", ev.OperationIndex, "event_index", ev.EventIndex, "err", sinkErr)
			return
		}
		// Transient fault (deadlock / reset / statement-timeout / ctx /
		// unknown): hold the cursor below this ledger so the next cycle
		// re-reads and retries. Track the lowest such ledger.
		sinkTransientFails++
		if firstTransientFailLedger == 0 || ev.Ledger < firstTransientFailLedger {
			firstTransientFailLedger = ev.Ledger
		}
		p.logger.Warn("projector: transient sink failure — holding cursor for retry (NOT advancing past this ledger)",
			"source", src.Name, "ledger", ev.Ledger, "tx", ev.TxHash,
			"op_index", ev.OperationIndex, "event_index", ev.EventIndex, "err", sinkErr)
	}

	if p.chAddr != "" {
		// CH feed-switch (#10): read contract_events directly (already an
		// events.Event, no Reconstruct). No FINAL — small forward window +
		// idempotent downstream writes absorb any duplicate.
		err = clickhouse.StreamContractEventsFiltered(cycleCtx, p.chAddr, fromLedger, toLedger,
			src.ContractIDs, src.Topic0Syms, src.ExcludeTopic0Syms,
			false, // no FINAL: idempotent writes absorb dups
			true,  // withOpArgs: the projector routes every source, incl. OpArgs consumers (redstone); windows are BatchLimit-small
			func(ev events.Event) error {
				rowsScanned++
				if ev.Ledger > lastSeenLedger {
					lastSeenLedger = ev.Ledger
				}
				process(ev)
				return nil
			})
	} else {
		err = p.store.StreamSorobanEvents(cycleCtx, fromLedger, toLedger,
			src.ContractIDs, src.Topic0Syms, src.ExcludeTopic0Syms,
			func(row sorobanevents.Row) error {
				rowsScanned++
				if row.Ledger > lastSeenLedger {
					lastSeenLedger = row.Ledger
				}
				ev, rerr := sorobanevents.Reconstruct(row)
				if rerr != nil {
					// Skip a malformed row but keep the cursor advancing; the
					// row is unrecoverable so re-reading it next cycle would
					// just re-fail. Count it for visibility.
					decodeErrors++
					return nil //nolint:nilerr // intentional soft-fail; see comment.
				}
				process(ev)
				return nil
			})
	}
	if err != nil {
		// Adaptive shrink (2026-07-10 incident): a window too dense to
		// finish inside PerSourceTimeout would otherwise retry the
		// IDENTICAL range every cycle forever. Halve down to
		// MinBatchLimit so the retry converges; the success path below
		// doubles back toward BatchLimit once past the dense stretch.
		if next, shrunk := shrinkWindow(*window, err); shrunk {
			*window = next
			p.logger.Warn("projector: cycle exceeded deadline — shrinking window",
				"source", src.Name, "from", fromLedger, "to", toLedger, "next_window", *window)
		} else {
			p.logger.Warn("projector: stream failed", "source", src.Name, "err", err, "from", fromLedger, "to", toLedger)
		}
		obs.ProjectorRunsTotal.WithLabelValues(src.Name, "error").Inc()
		return
	}

	// Cursor watermark (audit-2026-07-16 C2-1): advance only to the highest
	// ledger for which EVERY event fully committed. With no transient sink
	// failure that is `toLedger` — a source silent in a range still moves the
	// cursor so we don't rescan empty stretches, and decode failures + skipped
	// poison rows don't hold it back. A transient sink failure caps the cursor
	// at firstTransientFailLedger-1 so the failing ledger (and everything
	// after it in this window) is re-read + retried next cycle; the idempotent
	// downstream Insert* absorbs the repeats. lastSeenLedger is only logged.
	commitTo := toLedger
	if firstTransientFailLedger != 0 {
		commitTo = firstTransientFailLedger - 1
	}
	if commitTo < fromLedger {
		// The window's FIRST ledger failed transiently — nothing new is
		// durably committed, so DON'T move the cursor; the next cycle retries
		// the identical range. This is a VISIBLE stall (rising lag + the
		// sink_retry metrics below), never a silent advance-past-loss.
		obs.ProjectorLagLedgers.WithLabelValues(src.Name).Set(float64(tip - fromLedger + 1))
		obs.ProjectorEventsDecoded.WithLabelValues(src.Name, "sink_retry").Add(float64(sinkTransientFails))
		if sinkPermanentFails > 0 {
			obs.ProjectorEventsDecoded.WithLabelValues(src.Name, "sink_permanent").Add(float64(sinkPermanentFails))
		}
		obs.ProjectorRunsTotal.WithLabelValues(src.Name, "sink_retry").Inc()
		p.logger.Warn("projector: no fully-committed progress (transient sink failure at the window's first ledger) — holding cursor for retry",
			"source", src.Name, "from", fromLedger, "to", toLedger,
			"first_transient_fail_ledger", firstTransientFailLedger,
			"transient_fails", sinkTransientFails, "permanent_fails", sinkPermanentFails)
		return
	}
	if err := p.store.UpsertCursor(cycleCtx, "projector", src.Name, commitTo); err != nil {
		p.logger.Warn("projector: cursor advance failed", "source", src.Name, "err", err)
		obs.ProjectorRunsTotal.WithLabelValues(src.Name, "error").Inc()
		return
	}

	// Window recovery: a successful cycle doubles back toward
	// BatchLimit so a one-off dense stretch doesn't permanently slow
	// the replay.
	*window = recoverWindow(*window)

	obs.ProjectorLagLedgers.WithLabelValues(src.Name).Set(float64(tip - commitTo))
	// "ok" counts only events that DURABLY committed — eventsEmitted excludes
	// any output whose sink write failed (audit-2026-07-16 C2-1 / C4-14: a
	// sink-lost event must never be reported as a successful projection).
	obs.ProjectorEventsDecoded.WithLabelValues(src.Name, "ok").Add(float64(eventsEmitted))
	if decodeErrors > 0 {
		obs.ProjectorEventsDecoded.WithLabelValues(src.Name, "decode_error").Add(float64(decodeErrors))
	}
	if sinkTransientFails > 0 {
		obs.ProjectorEventsDecoded.WithLabelValues(src.Name, "sink_retry").Add(float64(sinkTransientFails))
	}
	if sinkPermanentFails > 0 {
		obs.ProjectorEventsDecoded.WithLabelValues(src.Name, "sink_permanent").Add(float64(sinkPermanentFails))
	}
	// A partially-failed cycle made forward progress (commitTo >= fromLedger)
	// but still has a pending retry above commitTo — surface it as a distinct
	// run outcome so a genuinely-stuck source alerts rather than silently
	// stalling under an "ok" label.
	runOutcome := "ok"
	if sinkTransientFails > 0 {
		runOutcome = "sink_retry"
	}
	obs.ProjectorRunsTotal.WithLabelValues(src.Name, runOutcome).Inc()
	obs.ProjectorCycleDurationSeconds.WithLabelValues(src.Name).Observe(time.Since(start).Seconds())

	if eventsEmitted > 0 || decodeErrors > 0 || sinkTransientFails > 0 || sinkPermanentFails > 0 {
		p.logger.Info("projector cycle",
			"source", src.Name,
			"from", fromLedger, "to", toLedger, "committed_to", commitTo,
			"rows_scanned", rowsScanned,
			"events_emitted", eventsEmitted,
			"decode_errors", decodeErrors,
			"sink_transient_fails", sinkTransientFails,
			"sink_permanent_fails", sinkPermanentFails,
			"lag_ledgers", tip-commitTo,
			"elapsed", time.Since(start).Round(time.Millisecond),
		)
	}
}

// shrinkWindow halves the adaptive per-source window when a cycle
// failed on a deadline (floor MinBatchLimit). Returns (next, true)
// when a shrink should apply; (current, false) for non-deadline
// errors or when already at the floor.
func shrinkWindow(current uint32, err error) (uint32, bool) {
	if !errors.Is(err, context.DeadlineExceeded) || current <= MinBatchLimit {
		return current, false
	}
	next := current / 2
	if next < MinBatchLimit {
		next = MinBatchLimit
	}
	return next, true
}

// recoverWindow doubles the adaptive window back toward BatchLimit
// after a successful cycle.
func recoverWindow(current uint32) uint32 {
	if current >= BatchLimit {
		return BatchLimit
	}
	next := current * 2
	if next > BatchLimit {
		next = BatchLimit
	}
	return next
}

// resolveTip returns the upper scan bound for one cycle. The base
// bound is the live ledgerstream cursor's last_ledger — the same
// approach as the gap detector (gap_detector.go::resolveGapDetectorTip)
// — so the projector never gets ahead of durably-ingested ledgers.
//
// In CH feed-switch mode (chAddr set) the bound is additionally
// clamped to the lake's contiguous-completeness watermark for
// [from, …]: the live dual-sink can drop or partially write ledgers,
// so reading past the first hole would silently lose that ledger's
// events (the cursor advances to the bound unconditionally). Clamping
// to the watermark stalls the source AT a hole until the catch-up
// timer heals it, instead of skipping over it (ADR-0034 #10).
func (p *Projector) resolveTip(ctx context.Context, from uint32) (uint32, error) {
	c, err := p.store.GetCursor(ctx, "ledgerstream", "")
	if err != nil {
		if errors.Is(err, timescale.ErrNotFound) {
			return 0, nil
		}
		return 0, fmt.Errorf("ledgerstream cursor: %w", err)
	}
	tip := c.LastLedger
	if p.chAddr != "" {
		wm, werr := clickhouse.ContiguousWatermark(ctx, p.chAddr, from)
		if werr != nil {
			return 0, fmt.Errorf("ch watermark: %w", werr)
		}
		if wm < tip {
			tip = wm
		}
	}
	return tip, nil
}
