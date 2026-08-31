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

// WedgeCycles is how many CONSECUTIVE cycles a source must sit at the
// MinBatchLimit floor while a per-cycle deadline keeps it from committing
// forward progress before the projector flags it as WEDGED (obs.ProjectorWedged
// → 1). The adaptive window shrinks on a deadline, but at the floor there is
// nothing left to halve: a floor-sized range that stays over PerSourceTimeout
// retries the identical range forever (the 2026-07-10 / 2026-08-01 incidents).
// 5 was picked so the flag means "stuck", not "one slow cycle" — each floored
// deadline cycle can burn up to PerSourceTimeout, so 5 is minutes of provably
// non-advancing work, not a transient blip. The shrink logic itself is
// unchanged; this only makes the terminal stall observable/alertable.
const WedgeCycles = 5

// ReplayWindowRefreshInterval is how often the projector re-reads the
// operator-recorded projection dirty windows (migration 0125) to republish
// obs.ProjectorReplayWindowActive. One tiny SELECT for ALL sources per
// refresh (not per source per cycle), matched to the 30s evaluation
// interval of the `stellarindex.projector` alert group so the gauge is
// never more than one rule evaluation stale.
const ReplayWindowRefreshInterval = 30 * time.Second

// SinkFunc is the per-event handler the projector calls after
// successful decode. `internal/pipeline/sink.go::HandleEvent` is the
// production wiring (it persists the decoded event to its per-source
// hypertable and RETURNS the underlying Insert error).
//
// The error return is load-bearing (audit-2026-07-16 C2-1/D1): a sink
// write can fail transiently (a Postgres deadlock / connection reset /
// statement-timeout) or permanently (a CHECK violation, a negative
// SEP-41 amount, a Validate-rejected OracleUpdate). The projector
// classifies the error ([classifySinkFault]) to decide whether to
// advance its cursor past the event's ledger:
//
//   - a TRANSIENT failure ([dispositionRetry]) holds the cursor at the
//     last fully-committed ledger, so the next cycle re-reads and retries
//     the row. ON CONFLICT in the downstream Insert* (DO NOTHING, or DO
//     UPDATE since migration 0109) makes the retry idempotent /
//     corrective.
//   - a PERMANENT data fault ([dispositionSkip]) is logged loudly,
//     counted, and SKIPPED (the cursor advances past it) — blocking
//     forever on a poison row is a worse outage than dropping it.
//   - an UNCLASSIFIED failure ([dispositionUnclassified]) is retried like
//     a transient one but under a budget, then quarantined; see
//     [QuarantineAfterCycles].
//
// Before this signature carried an error the projector could not see a
// sink failure at all: it advanced the cursor unconditionally on stream
// success, so a transient fault during a sole-writer (sep41) cycle
// permanently dropped that row (the loss C2-1 documents).
type SinkFunc func(ctx context.Context, ev consumer.Event) error

// eventStore is the projector's slice of *timescale.Store: the per-source
// cursor read/write pair plus the soroban_events tail scan. Declared on the
// CONSUMER side (Go's "accept interfaces" idiom) so the cursor-durability
// state machine — which decides when the cursor may advance past a failing
// row, the property COR-11/COR-01 turn on — is exercisable in unit tests
// without a live Postgres. Production always passes a real *timescale.Store
// through [New].
type eventStore interface {
	GetCursor(ctx context.Context, source, sub string) (timescale.Cursor, error)
	UpsertCursor(ctx context.Context, source, sub string, lastLedger uint32) error
	StreamSorobanEvents(ctx context.Context, from, to uint32,
		contractIDs, topic0Syms, excludeTopic0Syms []string,
		fn func(row sorobanevents.Row) error) error
	// ProjectionDirtyWindows reads the operator-recorded rewind windows
	// (migration 0125) so the projector can publish
	// obs.ProjectorReplayWindowActive — the discriminator that tells an
	// INTENDED replay lag apart from a real one. Read-only here: the
	// windows are written by the ops tools and cleared by
	// compute-completeness; the projector never mutates them.
	ProjectionDirtyWindows(ctx context.Context) (map[string]timescale.ProjectionDirtyWindow, error)
}

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

	// NeedsStateWriteKeys opts the source into events.Event.StateWriteKeys
	// enrichment on the CH read path (batched ledger_entry_changes point
	// lookups — internal/storage/clickhouse/state_write_keys.go). Opt-in
	// per source like the completeness reconcile's NeedOpArgs: only a
	// decoder that reads the written contract-data keys pays for the
	// lookups. Today only redstone (exact accepted-feed subset attribution
	// for freshness-filtered write_prices batches).
	NeedsStateWriteKeys bool
}

// Registry is the set of sources the projector handles. Built
// once at startup; immutable while the projector runs.
type Registry struct {
	Sources []Source
}

// Projector reads soroban_events and routes decoded events to
// the sink for each registered source.
type Projector struct {
	store    eventStore
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

	// cursorMu guards lastCursor: the last cursor position each source
	// observed at the top of its cycle, published by cycleOneSource and
	// read by the single replay-window watcher. Keeping it in memory is
	// what makes the watcher ONE query per refresh instead of a cursor
	// read per source, and it is the position the watcher compares
	// against a recorded rewind window's to_ledger.
	cursorMu   sync.Mutex
	lastCursor map[string]uint32
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
	p := &Projector{
		registry: registry,
		sink:     sink,
		logger:   logger,
	}
	// Assign through the nil check: a nil *timescale.Store stored into the
	// eventStore interface is a NON-nil interface value, which would defeat
	// Run's "nil store" guard (the typed-nil trap).
	if store != nil {
		p.store = store
	}
	return p
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
	// One shared watcher for every source: publishes
	// obs.ProjectorReplayWindowActive so the lag alert can tell an
	// operator-initiated rewind apart from a real fall-behind.
	wg.Add(1)
	go func() {
		defer wg.Done()
		p.watchReplayWindows(ctx)
	}()
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

// watchReplayWindows republishes obs.ProjectorReplayWindowActive every
// [ReplayWindowRefreshInterval] until ctx is cancelled. Runs once for the
// whole projector (not per source): the underlying read returns every
// source's window in one query.
func (p *Projector) watchReplayWindows(ctx context.Context) {
	t := time.NewTicker(ReplayWindowRefreshInterval)
	defer t.Stop()
	// Publish immediately: this first pass is also what SEEDS the series
	// at 0 for every registered source, so the alert reads a real
	// "no replay in progress" zero from process start rather than "no
	// data" (an absent series is itself a silence). Deliberately owned by
	// the watcher rather than each source goroutine — two writers of the
	// same series at startup would race, and the loser could park the
	// flag at a stale 0 for a whole refresh interval.
	p.refreshReplayWindows(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			p.refreshReplayWindows(ctx)
		}
	}
}

// refreshReplayWindows sets obs.ProjectorReplayWindowActive for every
// registered source: 1 while that source's cursor is inside an
// operator-recorded projector-replay rewind ([replayWindowCovers] holds
// the exact bound), 0 otherwise.
//
// FAIL OPEN toward alerting (the deliberate asymmetry): a read error, an
// un-observed cursor, a window written by any tool other than
// projector-replay, or a cursor outside the recorded rewind all publish
// 0 — never 1. The gauge's ONLY job is to suppress a lag ticket, so every
// uncertainty must resolve to "do not suppress".
//
// The read carries its own deadline. Every other p.store call runs under
// cycleCtx; this one passed Run's ctx straight through, so a query that
// blocked — the table is one row per source, but statement_timeout is
// measured from command ARRIVAL and includes lock waits — parked this
// goroutine with the gauge holding whatever it last published. A stale 1
// keeps suppressing the lag ticket for a source nobody is replaying
// (wave-D RD-08).
//
// Not unbounded even before: the store is opened by
// [timescale.OpenBackground], which SETs statement_timeout on every
// connection and fails the connection outright if the SET does not take,
// so the freeze was already capped at that backstop (30m by default).
// This makes the bound local, explicit, and two orders of magnitude
// tighter.
//
// PerSourceTimeout (60s), NOT a budget matched to the refresh interval.
// Fail-open points toward NOISE, so a bound tight enough to trip on
// ordinary DB slowness would zero the gauge mid-replay and re-arm
// stellarindex_projector_lag_high for the whole catch-up — reinstating
// the multi-hour ticket storm #325 exists to remove. 60s is far above any
// healthy read of a one-row-per-source table and far below the backstop.
func (p *Projector) refreshReplayWindows(ctx context.Context) {
	readCtx, cancel := context.WithTimeout(ctx, PerSourceTimeout)
	defer cancel()

	windows, err := p.store.ProjectionDirtyWindows(readCtx)
	if err != nil {
		for _, src := range p.registry.Sources {
			obs.ProjectorReplayWindowActive.WithLabelValues(src.Name).Set(0)
		}
		p.logger.Warn("projector: read projection dirty windows failed; replay-window lag suppression disarmed",
			"err", err)
		return
	}
	for _, src := range p.registry.Sources {
		active := 0.0
		if w, ok := windows[src.Name]; ok {
			if cursor, seen := p.observedCursor(src.Name); seen && replayWindowCovers(w, cursor) {
				active = 1
			}
		}
		obs.ProjectorReplayWindowActive.WithLabelValues(src.Name).Set(active)
	}
}

// replayWindowCovers reports whether an OPERATOR REWIND ON RECORD explains
// this source's cursor position — the only state in which a replay's
// intended lag may excuse stellarindex_projector_lag_high.
//
// Three bounds, each closing a way the excuse could outlive its cause. A
// suppression is only ever as good as the proof it stays narrow:
//
//  1. PROVENANCE. Only a `projector-replay` window counts
//     ([timescale.ProjectionDirtyWindow.IsProjectorReplay]). The table's
//     other writer, `projected-rebuild -write`, never rewinds the live
//     cursor and its recorded range routinely COVERS that cursor's own
//     position: `-to` defaults to the live cursor (equal), and
//     `-allow-live-overlap` bypasses the one-writer guard so the range can
//     sit wholly above it (exercised on r1 2026-07-27). Either shape would
//     otherwise pin the flag at 1 while the source's projector is HELD — a
//     sink-retry hold, a poison hold, a wedge — which is precisely the
//     state the lag ticket exists to catch, and there would be no operator
//     rewind on record to explain the silence.
//
//  2. UPPER BOUND, EXCLUSIVE. The flag clears the instant the cursor
//     reaches the row's `to_ledger`; the remaining
//     catch-up from there is ordinary forward lag. Exclusive rather than
//     inclusive because the dirty row survives until compute-completeness
//     re-verifies the range (up to a day later), so a projector wedged
//     exactly AT to_ledger — replay finished, cursor stuck — must stay
//     alertable.
//
//     `to_ledger` is the row's bound, which is USUALLY the replay's own
//     pre-rewind position but need not be. The table holds one row per
//     source and its upsert takes the RANGE UNION (LEAST/GREATEST), so a
//     replay recorded while a projected-rebuild window is still pending
//     widens to the higher `to_ledger` and the flag expires there. That
//     is a recorded, ratified decision, documented with its operator
//     remedy in docs/operations/runbooks/projector-replay.md ("clear the
//     pending window with a compute-completeness run first if you want
//     the tighter bound"); this comment previously glossed the bound as
//     the pre-rewind position unconditionally, which is only true of the
//     un-widened row (wave-D RD-09).
//
//     The union itself is deliberate and must NOT be narrowed to tighten
//     this flag. It is what closed the 2026-07-31 carried-claim
//     invalidation gap (19,366 over-projected cctp rows), and
//     compute-completeness's forced re-reconcile floor — the table's
//     PRIMARY consumer — depends on it. Keeping only the newest writer's
//     range, or refusing to record while a window is pending, trades a
//     narrower alert suppression for a data-integrity regression on the
//     verifier path. The residue this leaves uncovered is one case:
//     lag high, still FALLING, inside the extra stretch — a
//     degraded-but-advancing projector. A wedged or falling-behind one
//     still tickets via stellarindex_projector_replay_stalled, which
//     arms precisely when this gauge reads 1.
//
//  3. LOWER BOUND. `projector-replay` parks the cursor at from_ledger-1
//     (internal/ops/ingest/projector.go: rewindTo = target-1), so a cursor
//     below that was not put there by this recorded rewind and has no
//     recorded excuse.
func replayWindowCovers(w timescale.ProjectionDirtyWindow, cursor uint32) bool {
	if !w.IsProjectorReplay() {
		return false
	}
	// uint64 so a from_ledger of 0 cannot underflow the -1.
	return uint64(cursor)+1 >= uint64(w.From) && cursor < w.To
}

// recordCursor publishes a source's cursor position for the replay-window
// watcher. Called at the top of every cycle, so a source whose cursor is
// HELD (a sink retry loop) keeps reporting the same position — which is
// exactly what the paired stalled-replay alert keys off.
func (p *Projector) recordCursor(source string, lastLedger uint32) {
	p.cursorMu.Lock()
	defer p.cursorMu.Unlock()
	if p.lastCursor == nil {
		p.lastCursor = make(map[string]uint32)
	}
	p.lastCursor[source] = lastLedger
}

// observedCursor returns the last cursor position recorded for source, and
// whether any cycle has recorded one yet this process life.
func (p *Projector) observedCursor(source string) (uint32, bool) {
	p.cursorMu.Lock()
	defer p.cursorMu.Unlock()
	v, ok := p.lastCursor[source]
	return v, ok
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

// wedgeTracker counts the CONSECUTIVE cycles a single source has spent at the
// adaptive-window floor (MinBatchLimit) while a per-cycle deadline keeps it from
// committing forward progress — the shrink-to-floor stall the adaptive window
// cannot escape on its own (a floor-sized range that stays over PerSourceTimeout
// retries the identical range every cycle forever). Owned by the per-source
// goroutine like `window` and the poisonTracker, so no locking is needed. At
// WedgeCycles consecutive floor-stalls it raises obs.ProjectorWedged; any
// advancing (or caught-up) cycle clears both the count and the gauge.
type wedgeTracker struct {
	floorStalls int
}

// floorStall records one cycle that ended at the window floor, under a deadline,
// without advancing the cursor. It raises the wedge gauge once the stall has
// persisted WedgeCycles consecutive cycles (and keeps it raised while it does).
func (wt *wedgeTracker) floorStall(source string) {
	wt.floorStalls++
	if wt.floorStalls >= WedgeCycles {
		obs.ProjectorWedged.WithLabelValues(source).Set(1)
	}
}

// advanced records a cycle that committed forward progress (or found the source
// caught up), clearing any accumulated stall and lowering the wedge gauge.
func (wt *wedgeTracker) advanced(source string) {
	wt.floorStalls = 0
	obs.ProjectorWedged.WithLabelValues(source).Set(0)
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
	// Per-row consecutive-failure counts for the poison-row escape hatch,
	// owned by this goroutine for the same reason as `window`.
	var tracker poisonTracker
	// Consecutive floor-stall count for the wedge gauge, owned by this
	// goroutine for the same reason. Seed the gauge at 0 up front so the
	// alert reads a real "healthy" zero from process start rather than
	// "no data" (an absent wedge series is itself a silence — the exact
	// ambiguity this signal exists to remove).
	var wedge wedgeTracker
	obs.ProjectorWedged.WithLabelValues(src.Name).Set(0)
	// First cycle runs immediately so a fresh deploy starts
	// catching up without waiting Interval.
	p.cycleOneSource(ctx, src, &window, &tracker, &wedge)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			p.cycleOneSource(ctx, src, &window, &tracker, &wedge)
		}
	}
}

// heldRow is one sink failure that HOLDS the cursor this cycle: the row's
// identity, why we are holding (its [sinkDisposition]), how many consecutive
// cycles it has now failed, and the error itself (for the log).
type heldRow struct {
	id          rowIdentity
	disposition sinkDisposition
	fails       int
	err         error
}

// quarantineCandidate returns the index of the ONE held row this cycle should
// give up on, or -1 for "keep holding everything".
//
// Only [dispositionUnclassified] rows are eligible — a positively-identified
// infra fault is never dropped, however long it lasts. `madeProgress` (some
// other event durably committed this cycle) is the sink-health proof that
// separates "this row is poison" from "the whole sink is broken": with it the
// budget is [QuarantineAfterCycles]; without it, the far longer
// [QuarantineAfterCyclesNoProgress], so a global fault produces a visible
// stall rather than a shedding storm.
//
// At most one row per cycle, always the lowest ledger, so a global fault that
// does eventually exhaust the long budget sheds at a bounded, loud rate.
func quarantineCandidate(held []heldRow, madeProgress bool) int {
	budget := QuarantineAfterCyclesNoProgress
	if madeProgress {
		budget = QuarantineAfterCycles
	}
	best := -1
	var bestLedger uint32
	for i := range held {
		if held[i].disposition != dispositionUnclassified || held[i].fails < budget {
			continue
		}
		if best < 0 || held[i].id.ledger < bestLedger {
			best, bestLedger = i, held[i].id.ledger
		}
	}
	return best
}

// lowestHeldLedger returns the lowest ledger still held for retry, and whether
// anything is held at all. The cursor may advance to (that ledger - 1).
func lowestHeldLedger(held []heldRow) (uint32, bool) {
	lowest := uint32(0)
	found := false
	for i := range held {
		if !found || held[i].id.ledger < lowest {
			lowest = held[i].id.ledger
			found = true
		}
	}
	return lowest, found
}

// cycleOneSource runs one read-decode-write cycle for one source.
// Failure handling:
//   - read / tip / cursor errors → log + leave the cursor untouched; the
//     next cycle retries the same rows.
//   - decode failures (decode error / recovered panic) → count + SKIP the
//     row (deterministic; a retry would re-fail) and let the cursor advance,
//     but mark the cycle runOutcome=decode_degraded (DATA-6 / NS-2) so a
//     decoder regression draining a whole class of rows is not reported as a
//     clean "ok" run and the per-source decode_error rate alert can page.
//   - TRANSIENT sink write failures (audit-2026-07-16 C2-1) → cap the cursor
//     at the last fully-committed ledger so the failing ledger is re-read
//     next cycle; the idempotent downstream Insert* absorbs the retry. NEVER
//     advances past an un-committed row — the anti-silent-loss property this
//     cycle now actually implements (the SinkFunc godoc's old claim).
//   - PERMANENT sink data faults (SQLSTATE 22/23, or a canonical value-shape
//     rejection raised before the statement ran) → log LOUD + count + SKIP,
//     because a poison row must not wedge the source forever.
//   - UNCLASSIFIED sink failures → held like a transient one, but only for a
//     bounded number of consecutive cycles; then quarantined (COR-11 /
//     COR-01, audit-2026-07-23). Under INV-4 each Soroban-derived domain has
//     exactly ONE writer, so a row nobody can classify — a store validation
//     error such as a negative SEP-41 transfer amount — used to halt the
//     entire domain forever from a single hostile or malformed on-chain
//     value. See [quarantineCandidate] for the give-up rule and
//     [QuarantineAfterCycles] for the budget.
//
// A quarantined row is NOT evidence-destroying: the raw event stays in the
// authoritative landing zone (soroban_events / the ClickHouse lake), the
// ERROR log carries its full identity (source, ledger, tx, op_index,
// event_index, error, consecutive-cycle count), and
// `stellarindex-ops projector-replay` re-drives the range once the underlying
// defect is fixed. What the cursor advance buys is that the OTHER rows of a
// sole-writer domain keep flowing meanwhile.
//
// cycleOneSource is intentionally a single linear cycle: read cursor → resolve
// durable tip → scan the window → classify each event's sink outcome (decode
// soft-fail / transient-hold / permanent-skip) → advance the cursor only to the
// last fully-committed ledger. The branch count is the durability state machine
// (audit-2026-07-16 C2-1); splitting it purely for the gocyclo metric would
// scatter that one narrative across helpers and obscure the cursor-watermark
// invariant, so it is suppressed rather than fragmented.
//
//nolint:gocognit,funlen // linear cycle (cursor read → tip → scan → cursor write) with a source branch (soroban_events vs CH); splitting into helpers would scatter the cycle's success/failure metric emissions and make the control flow harder to audit.
func (p *Projector) cycleOneSource(ctx context.Context, src Source, window *uint32, tracker *poisonTracker, wedge *wedgeTracker) { //nolint:gocyclo // essential, cohesive durability classification (C2-1)
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
		// Publish the position for the replay-window watcher (see
		// [refreshReplayWindows]). Recorded from the READ, not the
		// commit, so a held cursor keeps reporting its true position.
		p.recordCursor(src.Name, cursor.LastLedger)
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
		wedge.advanced(src.Name) // caught up to tip — definitively not wedged
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

		// Sink-durability tracking (audit-2026-07-16 C2-1). A sink write
		// failure that is NOT a positively-identified permanent data fault
		// must NOT let the cursor advance past its ledger, or that row is
		// permanently lost for a sole-writer (sep41) domain. `held` collects
		// those failures with their row identity; the cursor is then capped
		// at (lowest held ledger - 1) so the next cycle re-reads and retries
		// from there. A PERMANENT data fault (poison row) does NOT hold the
		// cursor — it is counted + skipped so it can't wedge the source
		// forever — and an UNCLASSIFIED failure stops holding once its retry
		// budget is exhausted (COR-11 / COR-01).
		held               []heldRow
		failedThisCycle    = make(map[rowIdentity]bool)
		sinkPermanentFails int
		sinkQuarantined    int
	)
	// process runs the per-event decode + route, identical regardless of the
	// read source (soroban_events or CH contract_events). Decode failures
	// soft-fail (cursor still advances; the row is deterministically broken so
	// a retry would re-fail) and are counted for visibility. A SINK failure is
	// classified ([classifySinkFault]): permanent → count + skip; transient or
	// unclassified → hold the cursor below ev.Ledger for retry, counting the
	// consecutive failing cycles for this exact row.
	// Adjacent-duplicate guard (2026-08-11, stake-buffer investigation):
	// the lake is an append log and the projector reads it WITHOUT FINAL
	// (see the feed-switch comment below), so re-ingested duplicate rows
	// reach this callback — one copy each, CONSECUTIVELY, because the
	// query's ORDER BY is the table's sort key. Stateless decoders +
	// keyed ON CONFLICT sinks absorb that; BUFFERED decoders (phoenix's
	// multi-event correlation) do NOT: a duplicate field-event re-opens
	// a just-completed group as a partial (orphan noise) and, worse,
	// interleaved with a genuine second leg it cross-assigns fields
	// between legs (the 616 bond/unbond corruption class). Skip exact
	// re-deliveries of the previous identity here — the ONE place every
	// event enters decode — mirroring reconcile.go's identical guard.
	// This also stops events_emitted over-counting duplicates (which
	// made the projector's emit counts structurally disagree with the
	// deduping completeness reconcile); rows_scanned deliberately still
	// counts raw rows — it is a scan metric.
	var (
		lastID     rowIdentity
		haveLastID bool
	)
	process := func(ev events.Event) {
		id := rowIdentity{
			ledger: ev.Ledger, txHash: ev.TxHash,
			opIndex: ev.OperationIndex, eventIndex: ev.EventIndex,
		}
		if haveLastID && id == lastID {
			return // exact-identity duplicate part; decode once
		}
		lastID, haveLastID = id, true
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
		// `id` was computed at closure entry for the duplicate guard.
		disposition := classifySinkFault(sinkErr)
		if disposition == dispositionSkip {
			// Poison row: retrying can never succeed, so skipping (letting
			// the cursor advance past it) is safer than stalling the source
			// forever. Log LOUD + count so it surfaces as an alert.
			sinkPermanentFails++
			tracker.forget(id)
			p.logger.Error("projector: PERMANENT sink failure — skipping poison row (cursor advances past it)",
				"source", src.Name, "ledger", ev.Ledger, "tx", ev.TxHash,
				"op_index", ev.OperationIndex, "event_index", ev.EventIndex, "err", sinkErr)
			return
		}
		// Transient (DB down / restarting / ctx) or unclassified (deadlock,
		// statement-timeout, a store validation error, anything new): hold the
		// cursor below this ledger so the next cycle re-reads and retries.
		failedThisCycle[id] = true
		fails := tracker.fail(id)
		held = append(held, heldRow{id: id, disposition: disposition, fails: fails, err: sinkErr})
		p.logger.Warn("projector: sink failure — holding cursor for retry (NOT advancing past this ledger)",
			"source", src.Name, "ledger", ev.Ledger, "tx", ev.TxHash,
			"op_index", ev.OperationIndex, "event_index", ev.EventIndex,
			"disposition", disposition.String(), "consecutive_cycles", fails, "err", sinkErr)
	}

	if p.chAddr != "" {
		// CH feed-switch (#10): read contract_events directly (already an
		// events.Event, no Reconstruct). No FINAL — small forward window +
		// idempotent downstream writes absorb any duplicate.
		err = clickhouse.StreamContractEventsFiltered(cycleCtx, p.chAddr, fromLedger, toLedger,
			src.ContractIDs, src.Topic0Syms, src.ExcludeTopic0Syms,
			false,                   // no FINAL: idempotent writes absorb dups
			true,                    // withOpArgs: the projector routes every source, incl. OpArgs consumers (redstone); windows are BatchLimit-small
			src.NeedsStateWriteKeys, // per-source: only redstone reads written contract-data keys
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
		// Wedge detection (post-shrink): a deadline that leaves the window at
		// the floor is the terminal shrink-to-floor stall — nothing left to
		// halve, the identical range retried forever. Non-deadline stream
		// errors (DB reset, etc.) are a different failure mode and leave the
		// count untouched; only a floored deadline counts toward the wedge.
		if errors.Is(err, context.DeadlineExceeded) && *window <= MinBatchLimit {
			wedge.floorStall(src.Name)
		}
		return
	}

	// Drop the failure history of every row that did NOT re-fail in the scan
	// just finished: the retry budget counts cycles in which the row was
	// re-read AND re-failed, so a row that healed (or that the window no
	// longer covers) starts from zero.
	tracker.retain(failedThisCycle)

	// Poison-row escape hatch (COR-11 / COR-01, audit-2026-07-23): give up on
	// at most ONE held row whose retry budget is exhausted, so a deterministic
	// failure nobody classified cannot hold a sole-writer domain's cursor
	// forever. The raw event survives in the lake; `projector-replay` re-drives
	// it once the underlying defect is fixed.
	if i := quarantineCandidate(held, eventsEmitted > 0); i >= 0 {
		h := held[i]
		sinkQuarantined++
		tracker.forget(h.id)
		held = append(held[:i], held[i+1:]...)
		p.logger.Error("projector: QUARANTINED un-processable row after exhausting the retry budget — cursor advances past it; re-drive with `stellarindex-ops projector-replay` once fixed",
			"source", src.Name, "ledger", h.id.ledger, "tx", h.id.txHash,
			"op_index", h.id.opIndex, "event_index", h.id.eventIndex,
			"consecutive_cycles", h.fails, "sink_healthy_this_cycle", eventsEmitted > 0,
			"err", h.err)
	}
	sinkTransientFails := len(held)

	// Adaptive shrink, SINK-side (2026-08-01 incident — the second half of
	// the 2026-07-10 fix): the stream-level shrink above only fires when the
	// CH scan itself times out. A window dense enough that the scan FINISHES
	// but the per-event sink writes exhaust PerSourceTimeout mid-batch ends
	// here instead — every remaining write (and the cursor upsert below)
	// fast-fails on the dead cycleCtx, the cursor holds, and the IDENTICAL
	// window retried forever (aquarius reserves at 63,488,687 wedged 3.5h
	// this way). If the cycle budget is spent and rows are held as transient,
	// halve the window with the same floor so the retry converges.
	if cycleCtx.Err() != nil && sinkTransientFails > 0 {
		if next, shrunk := shrinkWindow(*window, context.DeadlineExceeded); shrunk {
			*window = next
			p.logger.Warn("projector: cycle budget exhausted in sink writes — shrinking window",
				"source", src.Name, "from", fromLedger, "to", toLedger,
				"transient_fails", sinkTransientFails, "next_window", *window)
		}
	}

	// Cursor watermark (audit-2026-07-16 C2-1): advance only to the highest
	// ledger for which EVERY event fully committed. With nothing held that is
	// `toLedger` — a source silent in a range still moves the cursor so we
	// don't rescan empty stretches, and decode failures, skipped poison rows
	// and quarantined rows don't hold it back. A held sink failure caps the
	// cursor at (lowest held ledger - 1) so the failing ledger (and everything
	// after it in this window) is re-read + retried next cycle; the idempotent
	// downstream Insert* absorbs the repeats. lastSeenLedger is only logged.
	commitTo := toLedger
	firstHeldLedger, holding := lowestHeldLedger(held)
	if holding && firstHeldLedger <= fromLedger {
		// The window's FIRST ledger is still held — nothing new is durably
		// committed, so DON'T move the cursor; the next cycle retries the
		// identical range. This is a VISIBLE stall (rising lag + the
		// sink_retry metrics below), never a silent advance-past-loss.
		obs.ProjectorLagLedgers.WithLabelValues(src.Name).Set(float64(tip - fromLedger + 1))
		obs.ProjectorEventsDecoded.WithLabelValues(src.Name, "sink_retry").Add(float64(sinkTransientFails))
		if sinkPermanentFails > 0 {
			obs.ProjectorEventsDecoded.WithLabelValues(src.Name, "sink_permanent").Add(float64(sinkPermanentFails))
		}
		if sinkQuarantined > 0 {
			obs.ProjectorEventsDecoded.WithLabelValues(src.Name, "sink_quarantined").Add(float64(sinkQuarantined))
		}
		obs.ProjectorRunsTotal.WithLabelValues(src.Name, "sink_retry").Inc()
		p.logger.Warn("projector: no fully-committed progress (sink failure at the window's first ledger) — holding cursor for retry",
			"source", src.Name, "from", fromLedger, "to", toLedger,
			"first_held_ledger", firstHeldLedger,
			"transient_fails", sinkTransientFails, "permanent_fails", sinkPermanentFails,
			"quarantined", sinkQuarantined)
		// Wedge detection, sink side: the same terminal stall reached via the
		// sink-budget path (the 2026-08-01 aquarius-reserves incident) — the CH
		// scan finished but the per-event writes spent PerSourceTimeout, the
		// sink-side shrink above floored the window, and the cursor held. Gated
		// on a spent cycle budget so a plain transient sink outage (DB briefly
		// down, no deadline) — a different, self-announcing failure — does not
		// masquerade as a compressed-chunk wedge.
		if cycleCtx.Err() != nil && *window <= MinBatchLimit {
			wedge.floorStall(src.Name)
		}
		return
	}
	if holding {
		commitTo = firstHeldLedger - 1
	}
	if err := p.store.UpsertCursor(cycleCtx, "projector", src.Name, commitTo); err != nil {
		p.logger.Warn("projector: cursor advance failed", "source", src.Name, "err", err)
		obs.ProjectorRunsTotal.WithLabelValues(src.Name, "error").Inc()
		return
	}

	// Forward progress committed (commitTo >= fromLedger > the prior cursor):
	// whatever else happened this cycle, the source is advancing — clear any
	// accumulated wedge stall and lower the gauge.
	wedge.advanced(src.Name)

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
	if sinkQuarantined > 0 {
		obs.ProjectorEventsDecoded.WithLabelValues(src.Name, "sink_quarantined").Add(float64(sinkQuarantined))
	}
	// A partially-failed cycle made forward progress (commitTo >= fromLedger)
	// but still has a pending retry above commitTo — surface it as a distinct
	// run outcome so a genuinely-stuck source alerts rather than silently
	// stalling under an "ok" label.
	//
	// DATA-6 / NS-2 (audit-2026-08-14): a decode soft-fail (a returned decode
	// error or a recovered decoder panic) skips the row and advances the cursor
	// past it. That is correct for genuine poison DATA — and holding instead
	// would re-wedge a sole-writer source on a deterministic failure (COR-11) —
	// but a shipped decoder REGRESSION breaks a whole CLASS of valid events the
	// same way (the projector runs the SAME decoders as ingest; the phoenix
	// 5,161-orphaned-swap class), silently draining them from the served tier
	// while the cursor sails to tip. Before this the cycle still reported "ok",
	// so runs_total showed a clean run over dropped rows and no run-level signal
	// distinguished the loss. Mark a decode-dropping cycle "decode_degraded" so
	// it is NOT counted clean; the per-source decode_error RATE alert
	// (stellarindex_projector_decode_error_rate_high in projector.yml) is what
	// separates a sustained spike (a regression) from scattered poison rows and
	// pages. sink_retry keeps precedence: it means the cursor HELD (an
	// auto-recovering visible stall) and already alerts on its own.
	runOutcome := "ok"
	switch {
	case sinkTransientFails > 0:
		runOutcome = "sink_retry"
	case decodeErrors > 0:
		runOutcome = "decode_degraded"
	}
	obs.ProjectorRunsTotal.WithLabelValues(src.Name, runOutcome).Inc()
	obs.ProjectorCycleDurationSeconds.WithLabelValues(src.Name).Observe(time.Since(start).Seconds())

	if eventsEmitted > 0 || decodeErrors > 0 || sinkTransientFails > 0 || sinkPermanentFails > 0 || sinkQuarantined > 0 {
		p.logger.Info("projector cycle",
			"source", src.Name,
			"from", fromLedger, "to", toLedger, "committed_to", commitTo,
			"rows_scanned", rowsScanned,
			"events_emitted", eventsEmitted,
			"decode_errors", decodeErrors,
			"sink_transient_fails", sinkTransientFails,
			"sink_permanent_fails", sinkPermanentFails,
			"sink_quarantined", sinkQuarantined,
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
