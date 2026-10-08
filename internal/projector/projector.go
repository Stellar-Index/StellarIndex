// Package projector tails the `soroban_events` landing zone (ADR-0029) and
// writes per-source rows through each protocol's own decoder; per ADR-0032 it
// is the single write path for projected tables.
//
//	soroban_events  (raw, authoritative)
//	     │
//	     ▼  StreamSorobanEvents from cursor.last_ledger
//	   Projector
//	     │
//	     ├─► aquarius.Decoder ──► persistTrade            (trades)
//	     ├─► blend.Decoder    ──► persistBlend*           (blend_*)
//	     ├─► ... per protocol
//	     ▼
//	   projector.cursor[source].last_ledger  (advances per cycle)
//
// Each source has its own cursor so one stuck decoder cannot block the
// others. Until per-source promotion the dispatcher may write the same rows;
// both race on the same PK and ON CONFLICT makes the loser a no-op.
package projector

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"sync"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/consumer"
	"github.com/Stellar-Index/StellarIndex/internal/dispatcher"
	"github.com/Stellar-Index/StellarIndex/internal/events"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/sources/sorobanevents"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
	"github.com/Stellar-Index/StellarIndex/internal/worker"
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

// reconstructErrLogEvery throttles the per-row malformed-row warning within a
// cycle: a systematically broken landing-zone shape fails every row it touches.
const reconstructErrLogEvery = 20

// MinBatchLimit is the floor for the adaptive per-source window (see
// cycleOneSource): when a cycle exceeds PerSourceTimeout the window
// halves down to this floor. 25 dense mainnet ledgers decode + insert
// comfortably inside the timeout even for the heaviest sources
// (a maximally-dense aquarius rewards window at BatchLimit did NOT
// finish inside PerSourceTimeout, so a fixed window would retry the
// identical range forever — a permanent stall the operator could only
// see as "lag stopped falling").
const MinBatchLimit = 25

// PerSourceTimeout caps one source's per-cycle work. A wedged
// downstream sink can't block other sources past this.
const PerSourceTimeout = 60 * time.Second

// cursorCommitTimeout bounds the cycle's closing cursor CAS, which runs
// outside the per-cycle budget so work already committed is never discarded.
const cursorCommitTimeout = 10 * time.Second

// WedgeCycles is how many CONSECUTIVE cycles a source must sit at the
// MinBatchLimit floor while a per-cycle deadline keeps it from committing
// forward progress before the projector flags it as WEDGED (obs.ProjectorWedged
// → 1). The adaptive window shrinks on a deadline, but at the floor there is
// nothing left to halve: a floor-sized range that stays over PerSourceTimeout
// retries the identical range forever.
// 5 was picked so the flag means "stuck", not "one slow cycle" — each floored
// deadline cycle can burn up to PerSourceTimeout, so 5 is minutes of provably
// non-advancing work, not a transient blip. This leaves the shrink logic
// alone; it only makes the terminal stall observable/alertable.
const WedgeCycles = 5

// MaxCycleBudgetMultiple caps how far a floor-stalled source's per-cycle
// budget escalates (PerSourceTimeout << n, n <= 3 → 8×, 8 minutes). A floor
// range that cannot finish in that is not a slow range, it needs an operator.
const MaxCycleBudgetMultiple = 8

// ReplayWindowRefreshInterval is how often the projector re-reads the
// operator-recorded projection dirty windows (migration 0125) to republish
// obs.ProjectorReplayWindowActive. One tiny SELECT for ALL sources per
// refresh (not per source per cycle), matched to the 30s evaluation
// interval of the `stellarindex.projector` alert group so the gauge is
// never more than one rule evaluation stale.
const ReplayWindowRefreshInterval = 30 * time.Second

// SinkFunc persists one decoded event (production: pipeline/sink.go
// HandleEvent) and returns the insert error, which [classifySinkFault] turns
// into a cursor decision:
//
//   - transient ([dispositionRetry]): hold the cursor; the next cycle retries,
//     and ON CONFLICT in the Insert* makes that idempotent.
//   - permanent data fault ([dispositionSkip]): log, count and skip, since a
//     poison row blocking forever is the worse outage, but only once another
//     event in the cycle committed and at most [PermanentSkipPerCycle] per
//     cycle, so a global fault stalls instead of draining the backlog.
//   - unclassified ([dispositionUnclassified]): retried under a budget, then
//     quarantined; see [QuarantineAfterCycles].
//
// Without the error a transient fault in a sole-writer cycle would drop the
// row for good.
type SinkFunc func(ctx context.Context, ev consumer.Event) error

// eventStore is the projector's slice of *timescale.Store: the per-source
// cursor read/write pair plus the soroban_events tail scan. Declared on the
// CONSUMER side (Go's "accept interfaces" idiom) so the cursor-durability
// state machine — which decides when the cursor may advance past a failing
// row — is exercisable in unit tests
// without a live Postgres. Production always passes a real *timescale.Store
// through [New].
type eventStore interface {
	GetCursor(ctx context.Context, source, sub string) (timescale.Cursor, error)
	// AdvanceCursorFrom is the projector's ONLY cursor write: a
	// compare-and-swap against the position the cycle read.
	// The unconditional never-regress upsert is deliberately not in this
	// interface — a cycle's commit is derived from a read that may be
	// PerSourceTimeout old, and an upsert would let it overwrite a
	// projector-replay rewind that landed in between.
	AdvanceCursorFrom(ctx context.Context, source, sub string, expected timescale.CursorRead, newLast uint32) (bool, error)
	StreamSorobanEvents(ctx context.Context, from, to uint32,
		contractIDs, topic0Syms, excludeTopic0Syms []string,
		fn func(row sorobanevents.Row) error) error
	// FirstSorobanEventLedger is StreamSorobanEvents' first matching ledger
	// in [from, to] (false when none) — the never-run source's seed.
	FirstSorobanEventLedger(ctx context.Context, from, to uint32,
		contractIDs, topic0Syms, excludeTopic0Syms []string) (uint32, bool, error)
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
	// Read through PrefilterContractIDs, never directly.
	ContractIDs []string
	Topic0Syms  []string

	// ContractIDsFunc, when set, supersedes ContractIDs and is re-read
	// every cycle: for a gate that grows in-stream (sushiswap_v3's
	// factory-seeded pools) a boot-time list filters a new pool's events
	// out before Matches ever sees them.
	ContractIDsFunc func() []string

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

	// Genesis is the first ledger at which this source can have an event
	// (its protocol's verified first-event / factory-genesis ledger), or 0
	// when none is declared. A source with NO cursor row starts here rather
	// than at ledger 0 / the lake floor, so a newly enabled source does not
	// crawl tens of millions of ledgers that cannot hold its events at the
	// BatchLimit-per-cycle ceiling. It must never be ABOVE the true first
	// event — that would skip it — so only exact or rounded-down values.
	Genesis uint32
}

// PrefilterContractIDs returns the contract-id prefilter for one read.
func (s Source) PrefilterContractIDs() []string {
	if s.ContractIDsFunc != nil {
		return s.ContractIDsFunc()
	}
	return s.ContractIDs
}

// prefilterWidened returns the contracts s's live gate now admits that
// snapshot — the prefilter a read just ran with — excluded. Empty when
// the gate is static or the read was unfiltered.
func prefilterWidened(s Source, snapshot []string) []string {
	if s.ContractIDsFunc == nil || len(snapshot) == 0 {
		return nil
	}
	had := make(map[string]struct{}, len(snapshot))
	for _, c := range snapshot {
		had[c] = struct{}{}
	}
	var added []string
	for _, c := range s.ContractIDsFunc() {
		if _, ok := had[c]; !ok {
			added = append(added, c)
		}
	}
	return added
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

	// lakeEvents, when non-nil, switches the per-source read from the Postgres
	// soroban_events landing zone to the ClickHouse Tier-1 lake's
	// contract_events (ADR-0041 feed-switch — the dual-sink feeds CH
	// inline, so CH is authoritative for forward events and soroban_events can
	// be decommissioned). The per-source cursor (last_ledger) is
	// source-agnostic, so the switch is seamless. Nil = legacy
	// soroban_events read.
	lakeEvents eventSource

	// rawSettled, in soroban_events mode, blocks until every raw row pushed
	// before the call is committed (the in-process raw sink's Sync). The
	// ledgerstream cursor advances on enqueue, not on commit, so it is the
	// soroban_events analogue of the lake's contiguous watermark.
	rawSettled func(context.Context) error

	// cursorMu guards lastCursor: the last cursor position each source
	// observed at the top of its cycle, published by cycleOneSource and
	// read by the single replay-window watcher. Keeping it in memory is
	// what makes the watcher ONE query per refresh instead of a cursor
	// read per source, and it is the position the watcher compares
	// against a recorded rewind window's to_ledger.
	cursorMu   sync.Mutex
	lastCursor map[string]uint32

	// seedMu guards seeds: the start ledger [Projector.seedFromLedger] found
	// for each cursor-less source, reused until the source's first commit
	// writes a cursor so the seek runs once, not once per cycle.
	seedMu sync.Mutex
	seeds  map[string]uint32

	// decoderStatsMu guards decoderStats: the last-observed values of a
	// source's decoder-owned loss counters (Decoder.EvictedOrphans /
	// UnknownContractDrops), read via the same duck-typed interfaces
	// dispatcher.Stats() uses. The projector builds its OWN decoder
	// instance per source (registry.go buildSource) — a separate
	// instance from the live indexer's dispatcher, with independent
	// buffer state — so without this the projector's half of the loss
	// signal (the half that actually governs what gets WRITTEN, per
	// ADR-0032) has no observability at all.
	decoderStatsMu sync.Mutex
	decoderStats   map[string]decoderLossCounters
}

// decoderLossCounters is the last snapshot of a decoder's cumulative
// loss counters, used to compute the per-cycle delta emitted as
// ProjectorEventsDecoded outcomes.
type decoderLossCounters struct {
	evictedOrphans       int
	unknownContractDrops int
	nonDirectionalSwaps  int
}

// emitDecoderLossDeltas reads src.Decoder's duck-typed loss counters (the
// same interfaces dispatcher.Stats() reads) and emits the delta since the
// last call for this source as ProjectorEventsDecoded outcomes. A decoder
// that implements none of them is a no-op.
func (p *Projector) emitDecoderLossDeltas(src Source) {
	orphanReporter, hasOrphans := src.Decoder.(interface{ EvictedOrphans() int })
	dropReporter, hasDrops := src.Decoder.(interface{ UnknownContractDrops() int })
	nonDirReporter, hasNonDir := src.Decoder.(interface{ SkippedNonDirectional() int })
	if !hasOrphans && !hasDrops && !hasNonDir {
		return
	}
	var cur decoderLossCounters
	if hasOrphans {
		cur.evictedOrphans = orphanReporter.EvictedOrphans()
	}
	if hasDrops {
		cur.unknownContractDrops = dropReporter.UnknownContractDrops()
	}
	if hasNonDir {
		cur.nonDirectionalSwaps = nonDirReporter.SkippedNonDirectional()
	}

	p.decoderStatsMu.Lock()
	if p.decoderStats == nil {
		p.decoderStats = make(map[string]decoderLossCounters)
	}
	prev := p.decoderStats[src.Name]
	p.decoderStats[src.Name] = cur
	p.decoderStatsMu.Unlock()

	if d := cur.evictedOrphans - prev.evictedOrphans; d > 0 {
		obs.ProjectorEventsDecoded.WithLabelValues(src.Name, "orphan_evicted").Add(float64(d))
	}
	if d := cur.unknownContractDrops - prev.unknownContractDrops; d > 0 {
		obs.ProjectorEventsDecoded.WithLabelValues(src.Name, "unknown_contract_drop").Add(float64(d))
	}
	// Non-directional swaps are an expected non-trade class (ADR-0033), not
	// lost data — its own outcome label, kept out of orphan/unknown-drop.
	if d := cur.nonDirectionalSwaps - prev.nonDirectionalSwaps; d > 0 {
		obs.ProjectorEventsDecoded.WithLabelValues(src.Name, "non_directional_skip").Add(float64(d))
	}
}

// SetClickHouseSource switches the projector to read forward events from the
// ClickHouse lake at addr instead of Postgres soroban_events (ADR-0041 feed-switch).
// Call before Run. Empty addr keeps the legacy soroban_events source.
func (p *Projector) SetClickHouseSource(addr string) {
	if addr == "" {
		p.lakeEvents = nil
		return
	}
	p.lakeEvents = chEventSource{addr: addr}
}

// SetRawEventBarrier installs the soroban_events completeness barrier
// resolveTip waits on in soroban_events mode. Call before Run. Without it
// the soroban_events read trusts the ledgerstream cursor, which is only
// sound when nothing buffers the raw writes (tests writing rows directly).
func (p *Projector) SetRawEventBarrier(settled func(context.Context) error) { p.rawSettled = settled }

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

// replayWindowWorkerName is the worker_panics_total label for the shared
// replay-window watcher Run starts.
const replayWindowWorkerName = "projector-replay-windows"

// sourceWorkerName is the worker_panics_total label for one source's goroutine.
func sourceWorkerName(source string) string { return "projector-source-" + source }

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
	// Each goroutine recovers its own panic: the indexer's guard around Run
	// cannot reach them, and an unrecovered one kills the process before its
	// sinks drain. A panicked source stops; its siblings keep projecting.
	//
	// One shared watcher for every source: publishes
	// obs.ProjectorReplayWindowActive so the lag alert can tell an
	// operator-initiated rewind apart from a real fall-behind.
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer worker.Recover(p.logger, replayWindowWorkerName)
		p.watchReplayWindows(ctx)
	}()
	for _, src := range p.registry.Sources {
		wg.Add(1)
		go func(src Source) {
			defer wg.Done()
			defer worker.Recover(p.logger, sourceWorkerName(src.Name))
			p.runOneSource(ctx, src)
		}(src)
	}
	wg.Wait()
	if ctx.Err() == nil {
		// Every loop exits only on ctx.Done, so reaching here uncancelled means
		// each goroutine was stopped by a recovered panic — not a clean exit.
		return errors.New("projector: every goroutine stopped on a recovered panic before cancellation")
	}
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

// refreshReplayWindows sets obs.ProjectorReplayWindowActive per source: 1
// while its cursor is inside a recorded projector-replay rewind
// ([replayWindowCovers]), else 0. It fails open: any read error or doubt
// publishes 0, because the gauge only suppresses a lag ticket.
//
// The read has its own PerSourceTimeout (60s) deadline so a blocked query
// cannot leave a stale 1 suppressing tickets until the 30m statement_timeout
// backstop. It is not tighter because a timeout zeroes the gauge mid-replay
// and re-arms stellarindex_projector_lag_high for the whole catch-up.
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

// replayWindowCovers reports whether a recorded operator rewind explains this
// source's cursor, the only case where replay lag may excuse
// stellarindex_projector_lag_high. Each bound stops the excuse outliving its
// cause:
//
//  1. Provenance: only a `projector-replay` window counts. A
//     `projected-rebuild -write` range can cover the live cursor without
//     rewinding it and would silence the alert while the projector is held.
//  2. Upper bound, exclusive: the dirty row lives until compute-completeness
//     re-verifies it, so a projector wedged exactly at to_ledger must stay
//     alertable. The upsert takes the range union, so a replay overlapping a
//     pending rebuild window expires at the wider bound; do not narrow the
//     union, compute-completeness relies on it to invalidate carried claims.
//     A wedged projector still tickets via
//     stellarindex_projector_replay_stalled.
//  3. Lower bound: projector-replay parks the cursor at from_ledger-1, so a
//     cursor below that has no recorded excuse.
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

// processEventSafely runs one lake row through a source's decoder and sink
// under a per-row recover ([dispatcher.DecodeRow]). The projector runs inside
// the live indexer over every historical WASM shape, so an unrecovered panic
// on one poison row would crash-loop the indexer on restart.
//
//   - emitted: outputs durably sinked, up to any failing one.
//   - decodeFail: a decode error or recovered panic; the caller advances past
//     it since a retry would fail the same way.
//   - sinkErr: a *[rowSinkFaults] of every write fault, or nil. A permanent
//     fault ([dispositionSkip]) drops that output and continues, because the
//     caller skips the row and unoffered siblings would be lost (soroswap and
//     phoenix emit several outputs per row). The first retryable or
//     unclassified fault stops the row; the caller re-reads it next cycle.
func processEventSafely(src Source, ev events.Event, sink func(consumer.Event) error, log *slog.Logger) (emitted int, decodeFail bool, sinkErr error) {
	outs, matched, derr := dispatcher.DecodeRow(src.Name, src.Decoder, ev, log)
	if !matched {
		return 0, false, nil
	}
	if derr != nil {
		return 0, true, nil //nolint:nilerr // reported via decodeFail: the caller counts and skips the row
	}
	var faults rowSinkFaults
	for _, out := range outs {
		err := sink(out)
		if err == nil {
			emitted++
			continue
		}
		if classifySinkFault(err) == dispositionSkip {
			// This OUTPUT can never land; its siblings still can. Record the
			// drop and keep going — see the godoc.
			faults.dropped = append(faults.dropped, err)
			continue
		}
		// Retryable or unclassified: stop here. `emitted` counts the outputs
		// that DID commit; the caller holds the cursor below this output's
		// ledger (heldLedger, ≤ ev.Ledger), so the row — including the outputs
		// after this one — is re-read next cycle.
		faults.held = err
		faults.heldLedger = outputLedger(out, ev.Ledger)
		break
	}
	return emitted, false, faults.asError()
}

// rowSinkFaults is the sink-fault record for ONE lake row: what
// [processEventSafely] returns as its sinkErr. The two halves are kept apart
// because the caller reacts to them differently — and must never infer one
// from the other.
type rowSinkFaults struct {
	// dropped holds one error per output the sink PERMANENTLY rejected
	// ([dispositionSkip]), in decode order. Each is counted
	// outcome="sink_permanent" — a per-OUTPUT count, like outcome="ok" — and
	// none of them holds the cursor.
	dropped []error
	// held is the first retryable or unclassified fault, which stopped the
	// row; nil when the row ran to its last output. It holds the cursor.
	held error
	// heldLedger is the ledger the held output belongs to: a buffered decoder
	// can emit an earlier ledger's output while scanning a later event, and the
	// cursor must stay below the OUTPUT's ledger for the retry to re-read it.
	heldLedger uint32
}

// outputLedger returns the output's own ledger when it declares one (the
// phoenix sweep rescue), else the ledger of the lake row it was decoded from.
func outputLedger(out consumer.Event, rowLedger uint32) uint32 {
	if c, ok := out.(interface{ EventLedger() uint32 }); ok {
		if l := c.EventLedger(); l != 0 && l < rowLedger {
			return l
		}
	}
	return rowLedger
}

// asError returns f as an error, or a true nil when the row had no sink fault
// (never a typed-nil *rowSinkFaults inside a non-nil interface).
func (f *rowSinkFaults) asError() error {
	if len(f.dropped) == 0 && f.held == nil {
		return nil
	}
	return f
}

func (f *rowSinkFaults) Error() string {
	return errors.Join(f.Unwrap()...).Error()
}

// Unwrap exposes every fault of the row — the drops in decode order, then the
// fault that stopped it — so errors.Is / errors.As see through the record.
func (f *rowSinkFaults) Unwrap() []error {
	all := make([]error, 0, len(f.dropped)+1)
	all = append(all, f.dropped...)
	if f.held != nil {
		all = append(all, f.held)
	}
	return all
}

// rowFaultsOf recovers the per-row record from a [processEventSafely] sinkErr.
// An error of any other shape is classified as a single fault, so a future
// return path keeps both guarantees: a permanent fault cannot hold the
// cursor, and nothing else can be skipped.
func rowFaultsOf(sinkErr error) *rowSinkFaults {
	var faults *rowSinkFaults
	if errors.As(sinkErr, &faults) {
		return faults
	}
	if classifySinkFault(sinkErr) == dispositionSkip {
		return &rowSinkFaults{dropped: []error{sinkErr}}
	}
	return &rowSinkFaults{held: sinkErr}
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

// budget is this source's per-cycle deadline: PerSourceTimeout doubled for each
// consecutive floor-stall, capped at MaxCycleBudgetMultiple. The window cannot
// shrink below the floor, so a longer deadline is the only lever left to let
// the identical range finish; an advancing cycle resets it.
func (wt *wedgeTracker) budget() time.Duration {
	mult := 1
	for i := 0; i < wt.floorStalls && mult < MaxCycleBudgetMultiple; i++ {
		mult *= 2
	}
	return PerSourceTimeout * time.Duration(mult)
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
	// This source's own ClickHouse connection (CH feed-switch mode), owned
	// here for the same reason: no dial per cycle, and a slow watermark scan
	// on one source never queues another source behind a shared pool.
	lake := p.newSourceLake()
	defer lake.close()
	// First cycle runs immediately so a fresh deploy starts
	// catching up without waiting Interval.
	p.cycleOneSource(ctx, src, &window, &tracker, &wedge, lake)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			p.cycleOneSource(ctx, src, &window, &tracker, &wedge, lake)
		}
	}
}

// lakeReader is the ClickHouse read surface a CH-mode cycle needs;
// *clickhouse.WatermarkReader satisfies it.
type lakeReader interface {
	ContiguousWatermark(ctx context.Context, from, to uint32) (uint32, error)
	LakeMinLedger(ctx context.Context) (uint32, error)
	Close() error
}

// eventSource is the projector's read of the ClickHouse lake: the per-cycle
// contract_events scan, a cursor-less source's first-event seek, and the
// per-source watermark connection. Production uses [chEventSource]; tests
// inject an in-memory fake.
type eventSource interface {
	StreamEvents(ctx context.Context, from, to uint32, contractIDs, topic0Syms, excludeTopic0Syms []string,
		withStateWriteKeys bool, fn func(events.Event) error) error
	FirstEventLedger(ctx context.Context, from, to uint32,
		contractIDs, topic0Syms, excludeTopic0Syms []string) (uint32, bool, error)
	OpenLake(ctx context.Context) (lakeReader, error)
}

// chEventSource is the [eventSource] over the ClickHouse lake at addr.
type chEventSource struct{ addr string }

// StreamEvents reads contract_events directly (already an events.Event, no
// Reconstruct). No FINAL: the forward window is BatchLimit-small and
// downstream writes are idempotent, so a duplicate is absorbed.
func (s chEventSource) StreamEvents(ctx context.Context, from, to uint32, contractIDs, topic0Syms, excludeTopic0Syms []string,
	withStateWriteKeys bool, fn func(events.Event) error,
) error {
	return clickhouse.StreamContractEventsFiltered(ctx, s.addr, from, to,
		contractIDs, topic0Syms, excludeTopic0Syms,
		false,              // no FINAL: idempotent writes absorb dups
		true,               // withOpArgs: the projector routes every source, incl. OpArgs consumers (redstone)
		withStateWriteKeys, // per-source: only redstone reads written contract-data keys
		fn)
}

func (s chEventSource) FirstEventLedger(ctx context.Context, from, to uint32,
	contractIDs, topic0Syms, excludeTopic0Syms []string,
) (uint32, bool, error) {
	return clickhouse.FirstContractEventLedgerFiltered(ctx, s.addr, from, to, contractIDs, topic0Syms, excludeTopic0Syms)
}

func (s chEventSource) OpenLake(ctx context.Context) (lakeReader, error) {
	r, err := clickhouse.NewWatermarkReader(ctx, s.addr)
	if err != nil {
		return nil, err
	}
	return r, nil
}

// newSourceLake returns one source goroutine's lake connection, opened on
// first use. Only CH feed-switch mode ever opens it.
func (p *Projector) newSourceLake() *sourceLake {
	return &sourceLake{open: func(ctx context.Context) (lakeReader, error) {
		if p.lakeEvents == nil {
			return nil, errors.New("projector: no ClickHouse lake configured")
		}
		return p.lakeEvents.OpenLake(ctx)
	}}
}

// sourceLake is one source goroutine's lazily-opened lake connection. A
// failed open is not cached, so a ClickHouse outage at start self-heals on a
// later cycle instead of latching. Not safe for concurrent use: exactly one
// goroutine (runOneSource) owns it.
type sourceLake struct {
	open func(ctx context.Context) (lakeReader, error)
	r    lakeReader
}

func (s *sourceLake) reader(ctx context.Context) (lakeReader, error) {
	if s.r == nil {
		r, err := s.open(ctx)
		if err != nil {
			return nil, err
		}
		s.r = r
	}
	return s.r, nil
}

func (s *sourceLake) close() {
	if s.r != nil {
		_ = s.r.Close()
		s.r = nil
	}
}

// heldRow is one sink failure that HOLDS the cursor this cycle: the row's
// identity, why we are holding (its [sinkDisposition]), how many consecutive
// cycles it has now failed, and the error itself (for the log).
type heldRow struct {
	id rowIdentity
	// holdLedger is the ledger the cursor must stay below when it differs from
	// id.ledger (a carried output from an earlier ledger); 0 means id.ledger.
	holdLedger  uint32
	disposition sinkDisposition
	fails       int
	err         error
}

// heldAt is the ledger this row holds the cursor below.
func (r heldRow) heldAt() uint32 {
	if r.holdLedger != 0 {
		return r.holdLedger
	}
	return r.id.ledger
}

// quarantineCandidate returns the index of the one held row this cycle gives
// up on, or -1. Only [dispositionUnclassified] rows qualify; an identified
// infra fault is never dropped. madeProgress (another event committed this
// cycle) separates a poison row from a broken sink: with it the budget is
// [QuarantineAfterCycles], without it [QuarantineAfterCyclesNoProgress], so a
// global fault stalls visibly instead of shedding. At most one row per cycle,
// lowest ledger first.
func quarantineCandidate(held []heldRow, madeProgress bool) int {
	budget := QuarantineAfterCyclesNoProgress
	if madeProgress {
		budget = QuarantineAfterCycles
	}
	return shedCandidate(held, dispositionUnclassified, budget)
}

// permanentSkipCandidate returns the index of the next [dispositionSkip] row
// this cycle may shed, or -1. A class-22/23 rejection is row-local only while
// the sink accepts other writes; a migration adding a constraint every live
// row violates raises the same SQLSTATE globally. So with madeProgress the
// row is shed at once, and without it only after
// [QuarantineAfterCyclesNoProgress] (about an hour, well past the lag
// alerts). [PermanentSkipPerCycle] bounds the rate. Lowest ledger first keeps
// the watermark monotonic.
func permanentSkipCandidate(poisoned []heldRow, madeProgress bool) int {
	budget := QuarantineAfterCyclesNoProgress
	if madeProgress {
		budget = 1
	}
	return shedCandidate(poisoned, dispositionSkip, budget)
}

// shedCandidate returns the index of the lowest-ledger row of `rows` whose
// disposition is `want` and which has failed at least `budget` consecutive
// cycles, or -1 when none qualifies. One row per call is the whole point: both
// give-up arms shed at a bounded, loud rate rather than draining a backlog.
func shedCandidate(rows []heldRow, want sinkDisposition, budget int) int {
	best := -1
	var bestLedger uint32
	for i := range rows {
		if rows[i].disposition != want || rows[i].fails < budget {
			continue
		}
		if best < 0 || rows[i].heldAt() < bestLedger {
			best, bestLedger = i, rows[i].heldAt()
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
		if !found || held[i].heldAt() < lowest {
			lowest = held[i].heldAt()
			found = true
		}
	}
	return lowest, found
}

// commitCursor compare-and-swaps the cursor from what this cycle read to
// commitTo and reports whether the cycle counts as progress. An
// unconditional write would overwrite a projector-replay rewind landing
// mid-cycle, so the replay would report success and re-project nothing.
//
// Losing the CAS keeps this cycle's idempotent sink writes and abandons only
// the position; the next cycle starts from the rewind. It counts as "error"
// because the one way to lose it repeatedly, two projectors on one cursor,
// should ticket. The write gets its own deadline: on the cycle's expired
// context the watermark would be lost and the window re-projected forever.
func (p *Projector) commitCursor(ctx context.Context, source string, read timescale.CursorRead, commitTo uint32) bool {
	commitCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cursorCommitTimeout)
	defer cancel()
	advanced, err := p.store.AdvanceCursorFrom(commitCtx, "projector", source, read, commitTo)
	if err != nil {
		p.logger.Warn("projector: cursor advance failed", "source", source, "err", err)
		obs.ProjectorRunsTotal.WithLabelValues(source, "error").Inc()
		return false
	}
	if !advanced {
		p.logger.Warn("projector: cursor moved during this cycle (a projector-replay rewind, or a second projector on this cursor) — abandoning this cycle's advance; the next cycle re-reads the cursor",
			"source", source, "read_exists", read.Exists, "read_cursor", read.LastLedger, "abandoned_commit_to", commitTo)
		obs.ProjectorRunsTotal.WithLabelValues(source, "error").Inc()
		return false
	}
	return true
}

// cycleOneSource runs one read-decode-write cycle for one source:
//
//   - read, tip or cursor error: leave the cursor; the next cycle retries.
//   - decode failure: skip the row (a retry would re-fail) but mark the run
//     decode_degraded so a decoder regression is not reported as "ok".
//   - transient sink fault: cap the cursor at the last fully committed ledger;
//     never advance past an uncommitted row.
//   - permanent sink fault (SQLSTATE 22/23 or a value-shape rejection): skip,
//     under the [permanentSkipCandidate] rules, holding the rest.
//   - unclassified sink fault: hold for a bounded number of cycles, then
//     quarantine ([quarantineCandidate]); a sole-writer domain such as sep41
//     would otherwise halt on one malformed on-chain value.
//
// Quarantine destroys no evidence: the raw event stays in the lake, the
// ERROR log carries its identity, and projector-replay re-drives the range
// once the defect is fixed. Kept as one linear cycle so the cursor-watermark
// invariant reads in one place.
//
//nolint:gocognit,funlen // linear cycle (cursor read → tip → scan → cursor write) with a source branch (soroban_events vs CH); splitting into helpers would scatter the cycle's success/failure metric emissions and make the control flow harder to audit.
func (p *Projector) cycleOneSource(ctx context.Context, src Source, window *uint32, tracker *poisonTracker, wedge *wedgeTracker, lake *sourceLake) { //nolint:gocyclo // essential, cohesive durability classification
	start := time.Now()
	cycleCtx, cancel := context.WithTimeout(ctx, wedge.budget())
	defer cancel()

	cursor, err := p.store.GetCursor(cycleCtx, "projector", src.Name)
	if err != nil && !errors.Is(err, timescale.ErrNotFound) {
		p.logger.Warn("projector: read cursor failed", "source", src.Name, "err", err)
		obs.ProjectorRunsTotal.WithLabelValues(src.Name, "error").Inc()
		p.publishLagFromLastRead(cycleCtx, src.Name)
		return
	}
	var fromLedger uint32
	// read is what this cycle's commit is conditional on (see commitCursor):
	// everything below derives from it, so the advance may only land if the
	// row still says the same thing when the cycle ends.
	var read timescale.CursorRead
	if err == nil {
		read = timescale.CursorRead{Exists: true, LastLedger: cursor.LastLedger}
		// Resume one ledger AFTER the last fully-processed one.
		// soroban_events.ledger BETWEEN $1 AND $2 is inclusive on
		// both ends so adding 1 here avoids reprocessing the seam.
		fromLedger = cursor.LastLedger + 1
		// Publish the position for the replay-window watcher (see
		// [refreshReplayWindows]). Recorded from the READ, not the
		// commit, so a held cursor keeps reporting its true position.
		p.recordCursor(src.Name, cursor.LastLedger)
	} else {
		fromLedger = p.seedFromLedger(cycleCtx, src, lake)
	}

	// Upper bound: live tip from ledgerstream. Without a tip we
	// scan to "wherever soroban_events extends," which during a
	// fresh deploy could be far ahead of where live writes have
	// committed. Better to track ledgerstream so the projector
	// never gets ahead of "what we promise is durable." In CH
	// feed-switch mode the bound is additionally clamped to the
	// lake's provably-complete watermark (see resolveTip).
	tip, durableTip, err := p.resolveTip(cycleCtx, lake, fromLedger, saturatingAdd(fromLedger, *window))
	if err != nil {
		p.logger.Warn("projector: tip resolve failed", "source", src.Name, "err", err)
		obs.ProjectorRunsTotal.WithLabelValues(src.Name, "error").Inc()
		if durableTip > 0 {
			publishLag(src.Name, fromLedger, durableTip)
		}
		return
	}
	// Published before the scan so every later exit (stream error, gate
	// re-read, failed commit) reports the true lag instead of a stale gauge;
	// a committing cycle overwrites it with its post-commit value.
	publishLag(src.Name, fromLedger, durableTip)
	if tip < fromLedger {
		// Caught up — nothing at or beyond fromLedger. Must be `<`, not `<=`:
		// fromLedger = cursor.LastLedger+1 is the next UNPROCESSED ledger and
		// the [fromLedger, tip] scan is inclusive, so when tip == fromLedger
		// there is exactly one ledger (the tip) still to project. `<=` would skip
		// it — leaving the served tier permanently one ledger behind the
		// durable tip, and a permanent hole if ingest halted exactly there.
		p.recordNothingToScan(src.Name, fromLedger, tip, durableTip)
		wedge.advanced(src.Name) // nothing scannable — not the window-floor wedge
		return
	}

	toLedger := tip
	if toLedger-fromLedger > *window {
		toLedger = fromLedger + *window
	}

	var (
		rowsScanned       int
		eventsEmitted     int
		decodeErrors      int
		reconstructErrors int
		lastSeenLedger    uint32

		// Sink-durability tracking. A sink write
		// failure that is NOT a positively-identified permanent data fault
		// must NOT let the cursor advance past its ledger, or that row is
		// permanently lost for a sole-writer (sep41) domain. `held` collects
		// those failures with their row identity; the cursor is then capped
		// at (lowest held ledger - 1) so the next cycle re-reads and retries
		// from there. A PERMANENT data fault (poison row) lands in `poisoned`
		// instead: it holds the cursor too, but only until this cycle's shed
		// cap releases it ([PermanentSkipPerCycle]) — bounded so a poison
		// row can't wedge the source forever and rate-limited so a GLOBAL
		// class-22/23 fault can't shed a whole backlog at once. An
		// UNCLASSIFIED failure stops holding once its retry budget is exhausted.
		held               []heldRow
		poisoned           []heldRow
		failedThisCycle    = make(map[rowIdentity]bool)
		sinkPermanentFails int
		sinkI128Overflows  int
		sinkQuarantined    int
	)
	// process runs the per-event decode + route, identical regardless of the
	// read source (soroban_events or CH contract_events). Decode failures
	// soft-fail (cursor still advances; the row is deterministically broken so
	// a retry would re-fail) and are counted for visibility. A SINK failure is
	// classified ([classifySinkFault]): permanent → count + skip; transient or
	// unclassified → hold the cursor below ev.Ledger for retry, counting the
	// consecutive failing cycles for this exact row.
	// Adjacent-duplicate guard:
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
		faults := rowFaultsOf(sinkErr)
		// One failing cycle per ROW, charged once: a row's poison outputs and
		// its held fault share one identity, and both arms below read the
		// count, so charging it twice would halve every budget.
		failedThisCycle[id] = true
		fails := tracker.fail(id)
		// Poison OUTPUTS: retrying can never succeed, so they are logged LOUD
		// and counted EACH — the row's other outputs were still offered to the sink.
		for _, dropErr := range faults.dropped {
			// An i128 overflow is counted (and alerted) apart from the rest:
			// it is not a verdict about an on-chain value but proof that an
			// int64 has been introduced on one of OUR amount paths, which
			// ADR-0003 promises a SEV-1 for. Own `outcome`, so the promise
			// has a rule to hang on; the outcomes still partition, so nothing
			// is double-counted.
			if isI128Overflow(dropErr) {
				sinkI128Overflows++
				p.logger.Error("projector: PERMANENT sink failure — i128 OVERFLOW (SEV-1, ADR-0003): an int64 has reached an amount path; every value that path touched is suspect, not just this row",
					"source", src.Name, "ledger", ev.Ledger, "tx", ev.TxHash,
					"op_index", ev.OperationIndex, "event_index", ev.EventIndex, "err", dropErr)
				continue
			}
			sinkPermanentFails++
			p.logger.Error("projector: PERMANENT sink failure — poison output (the cursor advances past its row only under this cycle's shed cap)",
				"source", src.Name, "ledger", ev.Ledger, "tx", ev.TxHash,
				"op_index", ev.OperationIndex, "event_index", ev.EventIndex, "err", dropErr)
		}
		if faults.held != nil {
			sinkErr = faults.held
			disposition := classifySinkFault(sinkErr)
			// Transient (DB down / restarting / ctx) or unclassified (deadlock,
			// statement-timeout, a store validation error, anything new): hold
			// the cursor below this ledger so the next cycle re-reads and
			// retries. The whole ROW is retried, poison outputs included, so it
			// is not also a shed candidate — the cursor is already held for it,
			// and shedding would drop the retry budget this row is counting.
			held = append(held, heldRow{id: id, holdLedger: faults.heldLedger, disposition: disposition, fails: fails, err: sinkErr})
			if fails == 1 || fails%heldRowLogEvery == 0 {
				p.logger.Warn("projector: sink failure — holding cursor for retry (NOT advancing past this ledger)",
					"source", src.Name, "ledger", ev.Ledger, "tx", ev.TxHash,
					"op_index", ev.OperationIndex, "event_index", ev.EventIndex,
					"disposition", disposition.String(), "consecutive_cycles", fails, "err", sinkErr)
			}
			return
		}
		// Every fault of this row is permanent, so the cursor MAY advance past
		// it — but not unconditionally, and not with the rest of the window in
		// the same pass. A class-22/23 verdict is row-local only when the sink
		// is otherwise working, and this arm cannot tell that apart from a
		// migration that rejects every row it reads. So the row becomes a shed
		// CANDIDATE and [permanentSkipCandidate] releases at most
		// [PermanentSkipPerCycle] of them once the scan is done; the rest
		// hold the cursor.
		if len(faults.dropped) > 0 {
			poisoned = append(poisoned, heldRow{
				id: id, disposition: dispositionSkip, fails: fails,
				err: errors.Join(faults.dropped...),
			})
		}
	}

	prefilter := src.PrefilterContractIDs()
	if p.lakeEvents != nil {
		// CH feed-switch (ADR-0041): read contract_events (see chEventSource).
		err = p.lakeEvents.StreamEvents(cycleCtx, fromLedger, toLedger,
			prefilter, src.Topic0Syms, src.ExcludeTopic0Syms, src.NeedsStateWriteKeys,
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
			prefilter, src.Topic0Syms, src.ExcludeTopic0Syms,
			func(row sorobanevents.Row) error {
				rowsScanned++
				if row.Ledger > lastSeenLedger {
					lastSeenLedger = row.Ledger
				}
				ev, rerr := sorobanevents.Reconstruct(row)
				if rerr != nil {
					// Skip a malformed row but keep the cursor advancing; the
					// row is unrecoverable so re-reading it next cycle would
					// just re-fail. Count it, and log its identity so the
					// dropped population can be found without re-scanning.
					reconstructErrors++
					if reconstructErrors == 1 || reconstructErrors%reconstructErrLogEvery == 0 {
						p.logger.Warn("projector: malformed landing-zone row — skipped (cursor advances past it)",
							"source", src.Name, "ledger", row.Ledger, "tx", hex.EncodeToString(row.TxHash),
							"op_index", row.OpIndex, "event_index", row.EventIndex,
							"contract", row.ContractID, "failures_this_cycle", reconstructErrors, "err", rerr)
					}
					return nil //nolint:nilerr // intentional soft-fail; see comment.
				}
				process(ev)
				return nil
			})
	}
	if err != nil {
		// Adaptive shrink: a window too dense to
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

	if added := prefilterWidened(src, prefilter); len(added) > 0 {
		p.holdForWidenedGate(src.Name, fromLedger, toLedger, added)
		return
	}

	// Poison-row shed cap: release at most [PermanentSkipPerCycle] of the
	// rows the sink PERMANENTLY rejected, lowest ledger first, and keep
	// holding the rest. Shedding every poison row of the window inline, on
	// cycle one, is correct for a scattered bad row and catastrophic for the
	// same SQLSTATE arriving globally (a migration whose NOT NULL / CHECK
	// every row violates): the cursor would advance past the entire backlog in
	// one pass and the only counter it bumps (outcome="sink_permanent") says
	// nothing about how much was in flight.
	// Capped, the same fault is a visible stall that bleeds one logged row per
	// cycle while the lag alert climbs.
	//
	// The cap bounds the RATE; `eventsEmitted > 0` — the same sink-health
	// proof the quarantine arm below takes — bounds the FACT. With no other
	// event of this cycle durably committed there is no evidence the sink is
	// healthy, so the verdict waits out [QuarantineAfterCyclesNoProgress]
	// before anything is shed.
	for shed := 0; shed < PermanentSkipPerCycle; shed++ {
		i := permanentSkipCandidate(poisoned, eventsEmitted > 0)
		if i < 0 {
			break
		}
		r := poisoned[i]
		tracker.forget(r.id)
		poisoned = append(poisoned[:i], poisoned[i+1:]...)
		p.logger.Error("projector: SKIPPING poison row — cursor advances past it; re-drive with `stellarindex-ops projector-replay` once the underlying defect is fixed",
			"source", src.Name, "ledger", r.id.ledger, "tx", r.id.txHash,
			"op_index", r.id.opIndex, "event_index", r.id.eventIndex,
			"poison_rows_still_held", len(poisoned), "consecutive_cycles", r.fails,
			"sink_healthy_this_cycle", eventsEmitted > 0, "err", r.err)
	}
	sinkPoisonHeld := len(poisoned)

	// Poison-row escape hatch: give up on
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

	// Adaptive shrink, SINK-side: the stream-level shrink above only fires
	// when the CH scan itself times out. A window dense enough that the scan
	// FINISHES but the per-event sink writes exhaust PerSourceTimeout
	// mid-batch ends here instead — every remaining write fast-fails on the
	// dead cycleCtx, the cursor holds below the first failed row, and without
	// a shrink the IDENTICAL window would retry forever (aquarius reserves at
	// 63,488,687 wedged 3.5h this way). If the cycle budget is spent and rows
	// are held as transient, halve the window with the same floor so the
	// retry converges.
	if cycleCtx.Err() != nil && sinkTransientFails > 0 {
		if next, shrunk := shrinkWindow(*window, context.DeadlineExceeded); shrunk {
			*window = next
			p.logger.Warn("projector: cycle budget exhausted in sink writes — shrinking window",
				"source", src.Name, "from", fromLedger, "to", toLedger,
				"transient_fails", sinkTransientFails, "next_window", *window)
		}
	}

	// Cursor watermark: advance only to the highest
	// ledger for which EVERY event fully committed. With nothing held that is
	// `toLedger` — a source silent in a range still moves the cursor so we
	// don't rescan empty stretches, and decode failures, skipped poison rows
	// and quarantined rows don't hold it back. A held sink failure caps the
	// cursor at (lowest held ledger - 1) so the failing ledger (and everything
	// after it in this window) is re-read + retried next cycle; the idempotent
	// downstream Insert* absorbs the repeats. lastSeenLedger is only logged.
	commitTo := toLedger
	firstHeldLedger, holding := lowestHeldLedger(held)
	// A poison row the shed cap did not release holds the cursor exactly like
	// a retryable fault: until this source's next cycles have bled it off one
	// at a time, advancing past it would be the unbounded silent loss the cap
	// exists to stop.
	if poisonLedger, poisonHolding := lowestHeldLedger(poisoned); poisonHolding &&
		(!holding || poisonLedger < firstHeldLedger) {
		firstHeldLedger, holding = poisonLedger, true
	}
	if holding && firstHeldLedger <= fromLedger {
		// The window's FIRST ledger is still held — nothing new is durably
		// committed, so DON'T move the cursor; the next cycle retries the
		// identical range. This is a VISIBLE stall (rising lag + the
		// sink_retry metrics below), never a silent advance-past-loss.
		obs.ProjectorEventsDecoded.WithLabelValues(src.Name, "sink_retry").Add(float64(sinkTransientFails))
		if sinkPermanentFails > 0 {
			obs.ProjectorEventsDecoded.WithLabelValues(src.Name, "sink_permanent").Add(float64(sinkPermanentFails))
		}
		if sinkI128Overflows > 0 {
			obs.ProjectorEventsDecoded.WithLabelValues(src.Name, "sink_i128_overflow").Add(float64(sinkI128Overflows))
		}
		if sinkQuarantined > 0 {
			obs.ProjectorEventsDecoded.WithLabelValues(src.Name, "sink_quarantined").Add(float64(sinkQuarantined))
		}
		obs.ProjectorRunsTotal.WithLabelValues(src.Name, "sink_retry").Inc()
		p.logger.Warn("projector: no fully-committed progress (sink failure at the window's first ledger) — holding cursor for retry",
			"source", src.Name, "from", fromLedger, "to", toLedger,
			"first_held_ledger", firstHeldLedger,
			"transient_fails", sinkTransientFails, "permanent_fails", sinkPermanentFails,
			"i128_overflows", sinkI128Overflows,
			"poison_rows_held", sinkPoisonHeld, "quarantined", sinkQuarantined)
		// Wedge detection, sink side: the same terminal stall reached via the
		// sink-budget path (the aquarius-reserves wedge above) — the CH
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
	if !p.commitCursor(ctx, src.Name, read, commitTo) {
		return
	}

	// Forward progress committed (commitTo >= fromLedger > the prior cursor):
	// whatever else happened this cycle, the source is advancing — clear any
	// accumulated wedge stall and lower the gauge.
	wedge.advanced(src.Name)

	// Window recovery: a successful cycle doubles back toward
	// BatchLimit so a one-off dense stretch doesn't permanently slow
	// the replay. A cycle that spent its budget is no such evidence: keep the
	// sink-side shrink above.
	if cycleCtx.Err() == nil {
		*window = recoverWindow(*window)
	}

	obs.ProjectorLagLedgers.WithLabelValues(src.Name).Set(float64(durableTip - commitTo))
	// "ok" counts only events that DURABLY committed — eventsEmitted excludes
	// any output whose sink write failed (a
	// sink-lost event must never be reported as a successful projection).
	obs.ProjectorEventsDecoded.WithLabelValues(src.Name, "ok").Add(float64(eventsEmitted))
	if decodeErrors > 0 {
		obs.ProjectorEventsDecoded.WithLabelValues(src.Name, "decode_error").Add(float64(decodeErrors))
	}
	if reconstructErrors > 0 {
		obs.ProjectorEventsDecoded.WithLabelValues(src.Name, "reconstruct_error").Add(float64(reconstructErrors))
	}
	if sinkTransientFails > 0 {
		obs.ProjectorEventsDecoded.WithLabelValues(src.Name, "sink_retry").Add(float64(sinkTransientFails))
	}
	if sinkPermanentFails > 0 {
		obs.ProjectorEventsDecoded.WithLabelValues(src.Name, "sink_permanent").Add(float64(sinkPermanentFails))
	}
	if sinkI128Overflows > 0 {
		obs.ProjectorEventsDecoded.WithLabelValues(src.Name, "sink_i128_overflow").Add(float64(sinkI128Overflows))
	}
	if sinkQuarantined > 0 {
		obs.ProjectorEventsDecoded.WithLabelValues(src.Name, "sink_quarantined").Add(float64(sinkQuarantined))
	}
	p.emitDecoderLossDeltas(src)
	// A partially-failed cycle made forward progress (commitTo >= fromLedger)
	// but still has a pending retry above commitTo — surface it as a distinct
	// run outcome so a genuinely-stuck source alerts rather than silently
	// stalling under an "ok" label.
	//
	// A decode soft-fail (a returned decode
	// error or a recovered decoder panic) skips the row and advances the cursor
	// past it. That is correct for genuine poison DATA — and holding instead
	// would wedge a sole-writer source on a deterministic failure —
	// but a shipped decoder REGRESSION breaks a whole CLASS of valid events the
	// same way (the projector runs the SAME decoders as ingest; the phoenix
	// 5,161-orphaned-swap class), silently draining them from the served tier
	// while the cursor sails to tip. Reported "ok", such a cycle would make
	// runs_total show a clean run over dropped rows, and no run-level signal would
	// distinguish the loss. Mark a decode-dropping cycle "decode_degraded" so
	// it is NOT counted clean; the per-source decode_error RATE alert
	// (stellarindex_projector_decode_error_rate_high in projector.yml) is what
	// separates a sustained spike (a regression) from scattered poison rows and
	// pages. sink_retry keeps precedence: it means the cursor HELD (an
	// auto-recovering visible stall) and already alerts on its own.
	//
	// A poison row the shed cap held back counts the same way: the cursor was
	// capped below it and the next cycle re-reads it, which is exactly what
	// "sink_retry" already means at the run level — an auto-recovering visible
	// stall. Reporting that cycle "ok" would hide the one signal a global
	// class-22/23 fault produces before its rows start bleeding off.
	runOutcome := "ok"
	switch {
	case sinkTransientFails > 0 || sinkPoisonHeld > 0:
		runOutcome = "sink_retry"
	case decodeErrors > 0 || reconstructErrors > 0:
		runOutcome = "decode_degraded"
	}
	obs.ProjectorRunsTotal.WithLabelValues(src.Name, runOutcome).Inc()
	obs.ProjectorCycleDurationSeconds.WithLabelValues(src.Name).Observe(time.Since(start).Seconds())

	if eventsEmitted > 0 || decodeErrors > 0 || reconstructErrors > 0 || sinkTransientFails > 0 || sinkPermanentFails > 0 ||
		sinkI128Overflows > 0 ||
		sinkPoisonHeld > 0 || sinkQuarantined > 0 {
		p.logger.Info("projector cycle",
			"source", src.Name,
			"from", fromLedger, "to", toLedger, "committed_to", commitTo,
			"rows_scanned", rowsScanned,
			"events_emitted", eventsEmitted,
			"decode_errors", decodeErrors,
			"reconstruct_errors", reconstructErrors,
			"sink_transient_fails", sinkTransientFails,
			"sink_permanent_fails", sinkPermanentFails,
			"sink_i128_overflows", sinkI128Overflows,
			"sink_poison_rows_held", sinkPoisonHeld,
			"sink_quarantined", sinkQuarantined,
			"lag_ledgers", durableTip-commitTo,
			"last_seen_ledger", lastSeenLedger,
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

// recordNothingToScan emits the metrics for a cycle whose scan range is empty
// (scanTip < fromLedger). When the lake watermark clamped the bound below
// ledgers ledgerstream already holds, the source is HELD at a lake hole, not
// caught up: it is counted as watermark_held and its lag is the real distance
// to the durable tip, so a watermark that stops advancing pages via lag.
func (p *Projector) recordNothingToScan(source string, fromLedger, scanTip, durableTip uint32) {
	publishLag(source, fromLedger, durableTip)
	if durableTip < fromLedger {
		obs.ProjectorRunsTotal.WithLabelValues(source, "idle").Inc()
		return
	}
	obs.ProjectorRunsTotal.WithLabelValues(source, "watermark_held").Inc()
	p.logger.Warn("projector: held at the lake's contiguous watermark — ledgers past it are not yet provably complete",
		"source", source, "from", fromLedger, "watermark", scanTip, "durable_tip", durableTip)
}

// publishLag sets the source's lag gauge to the ledgers in
// [fromLedger, durableTip] — everything the durable tip holds past the
// cursor — or 0 once the cursor has reached it.
func publishLag(source string, fromLedger, durableTip uint32) {
	lag := 0.0
	if durableTip >= fromLedger {
		lag = float64(durableTip - fromLedger + 1)
	}
	obs.ProjectorLagLedgers.WithLabelValues(source).Set(lag)
}

// publishLagFromLastRead keeps the lag gauge live on a cycle whose cursor
// read failed. Only this projector advances the cursor, and never without a
// read, so the last read position is still where the source stands. With no
// prior read or no durable tip the gauge is left alone and the error outcome
// in ProjectorRunsTotal is the signal.
func (p *Projector) publishLagFromLastRead(ctx context.Context, source string) {
	last, ok := p.observedCursor(source)
	if !ok {
		return
	}
	c, err := p.store.GetCursor(ctx, "ledgerstream", "")
	if err != nil {
		return
	}
	publishLag(source, last+1, c.LastLedger)
}

// seedFromLedger returns the first ledger a source with no cursor scans. Every
// ledger below the seed is proven to hold nothing the source's stream would
// return, so starting there loses no event — where ledger 0 made a new source
// crawl the whole pre-history at BatchLimit per Interval. A failed seek falls
// back to 0, the slow but lossless crawl.
func (p *Projector) seedFromLedger(ctx context.Context, src Source, lake *sourceLake) uint32 {
	p.seedMu.Lock()
	seed, ok := p.seeds[src.Name]
	p.seedMu.Unlock()
	if ok {
		return seed
	}
	seed, settled, err := p.findSeed(ctx, src, lake)
	if err != nil {
		p.logger.Warn("projector: first-event seek failed; scanning from ledger 0", "source", src.Name, "err", err)
		seed, settled = 0, true
	}
	if !settled {
		return seed
	}
	p.seedMu.Lock()
	if p.seeds == nil {
		p.seeds = make(map[string]uint32)
	}
	p.seeds[src.Name] = seed
	p.seedMu.Unlock()
	p.logger.Info("projector: no cursor; seeded start ledger", "source", src.Name, "from", seed)
	return seed
}

// findSeed seeks the source's first matching event in [floor, tip], using the
// same tip bound a cycle scans to, so the range it proves empty is exactly
// what a scan from the floor would have read. With no match it returns tip:
// the one-ledger scan [tip, tip] then writes the cursor. settled is false
// while nothing is durable at or above the floor; the caller seeks again.
//
// The floor is freshSourceStart: the source's Genesis, raised in CH
// feed-switch mode to the lake's own first present ledger, so the seek
// never scans ledgers before the source existed or the lake begins.
func (p *Projector) findSeed(ctx context.Context, src Source, lake *sourceLake) (seed uint32, settled bool, err error) {
	floor, err := p.freshSourceStart(ctx, src, lake)
	if err != nil {
		return 0, false, err
	}
	tip, _, err := p.resolveTip(ctx, lake, floor, math.MaxUint32)
	if err != nil {
		return 0, false, err
	}
	if tip < floor {
		return floor, false, nil
	}
	var first uint32
	var found bool
	if p.lakeEvents != nil {
		first, found, err = p.lakeEvents.FirstEventLedger(ctx, floor, tip,
			src.ContractIDs, src.Topic0Syms, src.ExcludeTopic0Syms)
	} else {
		first, found, err = p.store.FirstSorobanEventLedger(ctx, floor, tip,
			src.ContractIDs, src.Topic0Syms, src.ExcludeTopic0Syms)
	}
	if err != nil {
		return 0, false, err
	}
	if !found {
		return tip, true, nil
	}
	return first, true, nil
}

// holdForWidenedGate ends a cycle whose read admitted new contracts into
// the source's gate mid-window (a factory announced a pool): that pool's
// events in [from, to] were filtered out by the prefilter the read started
// with. The cursor stays put so the next cycle re-reads the window with
// the widened prefilter; the idempotent downstream Insert* absorbs the
// rows already written. Converges in one extra read, since the re-read
// re-seeds contracts the gate already holds.
func (p *Projector) holdForWidenedGate(source string, from, to uint32, added []string) {
	obs.ProjectorRunsTotal.WithLabelValues(source, "gate_widened").Inc()
	p.logger.Info("projector: contract gate widened mid-window — re-reading the window with the new prefilter",
		"source", source, "from", from, "to", to, "added", len(added), "first_added", added[0])
}

// resolveTip returns the upper scan bound for one cycle. The base
// bound is the live ledgerstream cursor's last_ledger — the same
// approach as the gap detector (gap_detector.go::resolveGapDetectorTip)
// — so the projector never gets ahead of durably-ingested ledgers.
//
// In CH feed-switch mode (lakeEvents set) the bound is additionally
// clamped to the lake's contiguous-completeness watermark for
// [from, …]: the live dual-sink can drop or partially write ledgers,
// so reading past the first hole would silently lose that ledger's
// events (the cursor advances to the bound unconditionally). Clamping
// to the watermark stalls the source AT a hole until the catch-up
// timer heals it, instead of skipping over it (ADR-0041 feed-switch).
//
// In soroban_events mode the same hazard has a different shape: the raw
// sink commits asynchronously after the cursor advances, so the tip's own
// rows may still be buffered. The bound is only returned once the raw-event
// barrier ([Projector.SetRawEventBarrier]) has seen every row pushed before
// the cursor read commit; a barrier that cannot settle fails the cycle.
//
// scanLimit bounds the CH watermark query: a cycle reads at most one batch
// window past from, so scanning the lake to its tip every cycle is wasted work
// for a lagging source.
//
// It also returns the unclamped ledgerstream tip: lag is always measured
// against that, so a stalled watermark shows as rising lag rather than as
// a caught-up source. A watermark error still returns durableTip, so the
// failed cycle can publish lag.
func (p *Projector) resolveTip(ctx context.Context, lake *sourceLake, from, scanLimit uint32) (scanTip, durableTip uint32, err error) {
	c, err := p.store.GetCursor(ctx, "ledgerstream", "")
	if err != nil {
		if errors.Is(err, timescale.ErrNotFound) {
			return 0, 0, nil
		}
		return 0, 0, fmt.Errorf("ledgerstream cursor: %w", err)
	}
	durableTip = c.LastLedger
	scanTip = durableTip
	if p.lakeEvents != nil {
		reader, rerr := lake.reader(ctx)
		if rerr != nil {
			return 0, durableTip, fmt.Errorf("ch watermark conn: %w", rerr)
		}
		wm, werr := reader.ContiguousWatermark(ctx, from, scanLimit)
		if werr != nil {
			return 0, durableTip, fmt.Errorf("ch watermark: %w", werr)
		}
		if wm < scanTip {
			scanTip = wm
		}
	} else if p.rawSettled != nil {
		if serr := p.rawSettled(ctx); serr != nil {
			return 0, durableTip, fmt.Errorf("soroban_events barrier: %w", serr)
		}
	}
	return scanTip, durableTip, nil
}

func saturatingAdd(a, b uint32) uint32 {
	if a > math.MaxUint32-b {
		return math.MaxUint32
	}
	return a + b
}

// freshSourceStart is the first ledger a source with no cursor row scans:
// its declared [Source.Genesis], raised in CH feed-switch mode to the lake's
// first ledger (as ch-cap67-movements' resolveStart does) — a floor below the
// lake's start is a boundary hole the watermark would stall on forever.
func (p *Projector) freshSourceStart(ctx context.Context, src Source, lake *sourceLake) (uint32, error) {
	if p.lakeEvents == nil {
		return src.Genesis, nil
	}
	floor, err := freshSourceFloor(ctx, lake)
	if err != nil {
		return 0, err
	}
	return max(src.Genesis, floor), nil
}

// freshSourceFloor is the lake's first present ledger, and never 0 even on
// an empty lake.
func freshSourceFloor(ctx context.Context, lake *sourceLake) (uint32, error) {
	r, err := lake.reader(ctx)
	if err != nil {
		return 0, err
	}
	lakeMin, err := r.LakeMinLedger(ctx)
	if err != nil {
		return 0, err
	}
	return max(1, lakeMin), nil
}
