// Package projector tails the `soroban_events` landing zone (ADR-0029) and writes per-source rows
// through each protocol's own decoder; per ADR-0032 it is the single write path for projected
// tables.
//
// Each source has its own cursor so one stuck decoder cannot block the others. Until per-source
// promotion the dispatcher may write the same rows; both race on the same PK and ON CONFLICT
// makes the loser a no-op.
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

// Interval is the catch-up cadence, trading freshness against Postgres scan overhead.
const Interval = 5 * time.Second

// BatchLimit caps how many ledgers the projector reads per source per cycle; without it a catch-up
// after a long outage would stream millions of rows in one transaction. It must stay small enough
// that dense ranges, notably Aquarius reserve updates, finish inside PerSourceTimeout.
const BatchLimit = 1_000

// reconstructErrLogEvery throttles the per-row malformed-row warning, since a broken shape fails every row.
const reconstructErrLogEvery = 20

// MinBatchLimit is the floor of the adaptive per-source window (see cycleOneSource), which halves
// when a cycle exceeds PerSourceTimeout. A fixed window would retry an over-dense range forever.
const MinBatchLimit = 25

// PerSourceTimeout caps one source's per-cycle work so a wedged sink cannot block other sources.
const PerSourceTimeout = 60 * time.Second

// cursorCommitTimeout bounds the closing cursor CAS, which runs outside the cycle budget so
// committed work is never discarded.
const cursorCommitTimeout = 10 * time.Second

// WedgeCycles is how many CONSECUTIVE cycles a source must sit at the MinBatchLimit floor, blocked
// by the cycle deadline, before obs.ProjectorWedged is set. At the floor nothing is left to halve,
// so the flag means "stuck", not "one slow cycle".
const WedgeCycles = 5

// MaxCycleBudgetMultiple caps how far a floor-stalled source's cycle budget escalates
// (PerSourceTimeout << n, n <= 3). A floor range that cannot finish in that needs an operator.
const MaxCycleBudgetMultiple = 8

// ReplayWindowRefreshInterval is how often the dirty windows are re-read to republish
// obs.ProjectorReplayWindowActive; it matches the alert group's evaluation interval so the gauge is
// never more than one evaluation stale.
const ReplayWindowRefreshInterval = 30 * time.Second

// SinkFunc persists one decoded event and returns the insert error, which [classifySinkFault] turns
// into a cursor decision. Without the error a transient fault in a sole-writer cycle would drop the
// row for good.
//
//   - transient ([dispositionRetry]): hold the cursor; ON CONFLICT makes the retry idempotent.
//   - permanent data fault ([dispositionSkip]): log, count and skip, since a poison row blocking
//     forever is the worse outage, but only once another event in the cycle committed and at most
//     [PermanentSkipPerCycle] per cycle, so a global fault stalls instead of draining the backlog.
//   - unclassified ([dispositionUnclassified]): retried under a budget, then quarantined; see
//     [QuarantineAfterCycles].
type SinkFunc func(ctx context.Context, ev consumer.Event) error

// eventStore is the projector's slice of *timescale.Store, declared consumer-side so the
// cursor-durability state machine is unit-testable without Postgres.
type eventStore interface {
	GetCursor(ctx context.Context, source, sub string) (timescale.Cursor, error)
	// AdvanceCursorFrom is the ONLY cursor write: a compare-and-swap against the position the cycle
	// read. An unconditional upsert would overwrite a projector-replay rewind landed since that read.
	AdvanceCursorFrom(ctx context.Context, source, sub string, expected timescale.CursorRead, newLast uint32) (bool, error)
	StreamSorobanEvents(ctx context.Context, from, to uint32,
		contractIDs, topic0Syms, excludeTopic0Syms []string,
		fn func(row sorobanevents.Row) error) error
	// FirstSorobanEventLedger is StreamSorobanEvents' first matching ledger
	// in [from, to] (false when none) — the never-run source's seed.
	FirstSorobanEventLedger(ctx context.Context, from, to uint32,
		contractIDs, topic0Syms, excludeTopic0Syms []string) (uint32, bool, error)
	// ProjectionDirtyWindows reads the recorded rewind windows so an intended replay lag can be told
	// from a real one. Read-only: ops tools write the windows and compute-completeness clears them.
	ProjectionDirtyWindows(ctx context.Context) (map[string]timescale.ProjectionDirtyWindow, error)
}

// Source describes one protocol's projection target, with its own cursor so one stuck decoder
// does not block the rest.
type Source struct {
	// Name is the cursor sub_source key and log label, unique within a Registry.
	Name string

	// Decoder is the protocol's event handler, called Matches then Decode as the dispatcher does.
	Decoder dispatcher.Decoder

	// ContractIDs / Topic0Syms narrow the SQL pre-filter so irrelevant rows are not streamed. Nil
	// means match by Decoder.Matches alone (a coarser read). Read ContractIDs through
	// PrefilterContractIDs, never directly.
	ContractIDs []string
	Topic0Syms  []string

	// ContractIDsFunc, when set, supersedes ContractIDs and is re-read every cycle: for a gate that
	// grows in-stream (factory-seeded pools) a boot-time list would filter a new pool out.
	ContractIDsFunc func() []string

	// ExcludeTopic0Syms drops events by topic[0] symbol in SQL. Sources with no contract/topic
	// prefilter use it to exclude the CAP-67 classic-token firehose (transfer/mint/burn), which a
	// far-behind source would otherwise pull by the million and discard in Decoder.Matches, blowing
	// the cycle budget. Leave nil for sources that consume those topics (sep41_*) or prefilter by
	// contract.
	ExcludeTopic0Syms []string

	// NeedsStateWriteKeys opts into events.Event.StateWriteKeys enrichment on the CH read path
	// (batched ledger_entry_changes lookups); only decoders that read written contract-data keys
	// should pay for it.
	NeedsStateWriteKeys bool

	// Genesis is the first ledger a source can have an event (0 when undeclared); a cursor-less
	// source starts here, not at the lake floor, to avoid crawling empty ledgers. It must never be
	// ABOVE the true first event, which would skip it.
	Genesis uint32
}

// PrefilterContractIDs returns the contract-id prefilter for one read.
func (s Source) PrefilterContractIDs() []string {
	if s.ContractIDsFunc != nil {
		return s.ContractIDsFunc()
	}
	return s.ContractIDs
}

// prefilterWidened returns the contracts s's live gate now admits that the read's snapshot excluded.
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

// Registry is the set of sources the projector handles, immutable once running.
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

	// lakeEvents, when non-nil, reads from the ClickHouse lake's contract_events instead of the
	// Postgres soroban_events landing zone; the per-source cursor is agnostic to which.
	lakeEvents eventSource

	// rawSettled, in soroban_events mode, blocks until every raw row pushed before the call is
	// committed. The ledgerstream cursor advances on enqueue, not commit, so this stands in for the
	// lake's contiguous watermark.
	rawSettled func(context.Context) error

	// cursorMu guards lastCursor: each source's cursor at the top of its cycle, kept in memory so the
	// replay-window watcher costs one query per refresh, not a cursor read per source.
	cursorMu   sync.Mutex
	lastCursor map[string]uint32

	// seedMu guards seeds: each cursor-less source's start ledger, reused until its first commit so
	// the seek runs once.
	seedMu sync.Mutex
	seeds  map[string]uint32

	// decoderStatsMu guards decoderStats: the last-seen decoder loss counters. The projector builds
	// its OWN decoder per source with independent buffer state, and that instance governs what is
	// WRITTEN, so its loss signal needs its own observability.
	decoderStatsMu sync.Mutex
	decoderStats   map[string]decoderLossCounters
}

// decoderLossCounters is a decoder's last cumulative loss snapshot, for the per-cycle delta.
type decoderLossCounters struct {
	evictedOrphans       int
	unknownContractDrops int
	nonDirectionalSwaps  int
}

// emitDecoderLossDeltas emits src.Decoder's loss counters (duck-typed, as dispatcher.Stats() reads
// them) as ProjectorEventsDecoded deltas since the last call; a decoder with none is a no-op.
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
	// Assign through the nil check: a nil *timescale.Store in the eventStore interface is non-nil
	// and would defeat Run's nil-store guard (typed-nil trap).
	if store != nil {
		p.store = store
	}
	return p
}

// replayWindowWorkerName is the worker_panics_total label for the shared replay-window watcher.
const replayWindowWorkerName = "projector-replay-windows"

// sourceWorkerName is the worker_panics_total label for one source's goroutine.
func sourceWorkerName(source string) string { return "projector-source-" + source }

// Run blocks until ctx is cancelled, driving one goroutine per source.
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
	// Each goroutine recovers its own panic (the indexer's guard cannot reach them, and an
	// unrecovered one kills the process before sinks drain); a panicked source stops, siblings continue.
	// One shared watcher tells the lag alert an operator rewind from a real fall-behind.
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
		// Loops exit only on ctx.Done, so uncancelled here means every goroutine died on a recovered panic.
		return errors.New("projector: every goroutine stopped on a recovered panic before cancellation")
	}
	return ctx.Err()
}

// watchReplayWindows republishes obs.ProjectorReplayWindowActive every [ReplayWindowRefreshInterval].
// It runs once per projector because one query returns every source's window.
func (p *Projector) watchReplayWindows(ctx context.Context) {
	t := time.NewTicker(ReplayWindowRefreshInterval)
	defer t.Stop()
	// Publish immediately to seed the series at 0, so the alert reads a real "no replay" rather than
	// absent data (a silence). Only the watcher writes it; two startup writers could race.
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

// refreshReplayWindows sets obs.ProjectorReplayWindowActive per source: 1 while its cursor is inside
// a recorded rewind ([replayWindowCovers]), else 0. It fails open (a read error publishes 0) because
// the gauge only suppresses a lag ticket.
//
// The read has a PerSourceTimeout deadline so a blocked query cannot leave a stale 1 suppressing
// tickets until the 30m statement_timeout; it is not tighter because a timeout zeroes the gauge
// mid-replay and re-arms stellarindex_projector_lag_high.
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

// replayWindowCovers reports whether a recorded operator rewind explains this source's cursor, the
// only case where replay lag may excuse stellarindex_projector_lag_high. Each bound stops the
// excuse outliving its cause:
//
//  1. Provenance: only a `projector-replay` window counts; a `projected-rebuild -write` range can
//     cover the live cursor without rewinding it and would silence the alert.
//  2. Upper bound, exclusive: the dirty row lives until compute-completeness re-verifies it, so a
//     projector wedged exactly at to_ledger stays alertable. The upsert takes the range union; do
//     not narrow it, compute-completeness relies on it to invalidate carried claims.
//  3. Lower bound: projector-replay parks the cursor at from_ledger-1, so a lower cursor has no
//     recorded excuse.
func replayWindowCovers(w timescale.ProjectionDirtyWindow, cursor uint32) bool {
	if !w.IsProjectorReplay() {
		return false
	}
	// uint64 so a from_ledger of 0 cannot underflow the -1.
	return uint64(cursor)+1 >= uint64(w.From) && cursor < w.To
}

// recordCursor publishes a source's cursor for the replay-window watcher at the top of every cycle,
// so a HELD cursor keeps reporting the same position, which the stalled-replay alert keys off.
func (p *Projector) recordCursor(source string, lastLedger uint32) {
	p.cursorMu.Lock()
	defer p.cursorMu.Unlock()
	if p.lastCursor == nil {
		p.lastCursor = make(map[string]uint32)
	}
	p.lastCursor[source] = lastLedger
}

// observedCursor returns the last recorded cursor for source, and whether one exists yet.
func (p *Projector) observedCursor(source string) (uint32, bool) {
	p.cursorMu.Lock()
	defer p.cursorMu.Unlock()
	v, ok := p.lastCursor[source]
	return v, ok
}

// processEventSafely runs one lake row through a source's decoder and sink under a per-row recover
// ([dispatcher.DecodeRow]): the live indexer sees every historical WASM shape, and one poison-row
// panic would crash-loop it on restart.
//
//   - emitted: outputs durably sinked, up to any failing one.
//   - decodeFail: a decode error or recovered panic; the caller advances past it since a retry
//     would fail the same way.
//   - sinkErr: a *[rowSinkFaults] or nil. A permanent fault ([dispositionSkip]) drops that output
//     and continues, because the caller skips the row and unoffered siblings (soroswap and phoenix
//     emit several per row) would be lost. The first retryable or unclassified fault stops the
//     row; the caller re-reads it next cycle.
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
			// This OUTPUT can never land; its siblings still can, so record the drop and go on.
			faults.dropped = append(faults.dropped, err)
			continue
		}
		// Retryable or unclassified: stop here. The caller holds the cursor below this output's ledger
		// so the row, including later outputs, is re-read next cycle.
		faults.held = err
		faults.heldLedger = outputLedger(out, ev.Ledger)
		break
	}
	return emitted, false, faults.asError()
}

// rowSinkFaults is the sink-fault record for ONE lake row ([processEventSafely]'s sinkErr). The
// halves stay apart because the caller reacts to them differently.
type rowSinkFaults struct {
	// dropped holds one error per output the sink PERMANENTLY rejected ([dispositionSkip]); each
	// counts as outcome="sink_permanent" per OUTPUT and none holds the cursor.
	dropped []error
	// held is the first retryable or unclassified fault, which stopped the row and holds the cursor.
	held error
	// heldLedger is the held output's own ledger: a buffered decoder can emit an earlier ledger's
	// output while scanning a later event, and the cursor must stay below the OUTPUT's ledger.
	heldLedger uint32
}

// outputLedger returns the output's own ledger when it declares one (phoenix sweep rescue), else the row's.
func outputLedger(out consumer.Event, rowLedger uint32) uint32 {
	if c, ok := out.(interface{ EventLedger() uint32 }); ok {
		if l := c.EventLedger(); l != 0 && l < rowLedger {
			return l
		}
	}
	return rowLedger
}

// asError returns f as an error, or a true nil (never a typed-nil inside an interface) when clean.
func (f *rowSinkFaults) asError() error {
	if len(f.dropped) == 0 && f.held == nil {
		return nil
	}
	return f
}

func (f *rowSinkFaults) Error() string {
	return errors.Join(f.Unwrap()...).Error()
}

// Unwrap exposes every fault of the row (drops in decode order, then the one that stopped it) to
// errors.Is / errors.As.
func (f *rowSinkFaults) Unwrap() []error {
	all := make([]error, 0, len(f.dropped)+1)
	all = append(all, f.dropped...)
	if f.held != nil {
		all = append(all, f.held)
	}
	return all
}

// rowFaultsOf recovers the per-row record from a sinkErr. Any other error shape is classified as a
// single fault, so a permanent fault still cannot hold the cursor and nothing else can be skipped.
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

// wedgeTracker counts the CONSECUTIVE cycles a source sat at the window floor (MinBatchLimit) with
// the cycle deadline blocking progress, a stall the shrinking window cannot escape. It is
// goroutine-owned, so needs no locking. At WedgeCycles it raises obs.ProjectorWedged; any advancing
// or caught-up cycle clears it.
type wedgeTracker struct {
	floorStalls int
}

// budget is the per-cycle deadline: PerSourceTimeout doubled per consecutive floor-stall, capped at
// MaxCycleBudgetMultiple. The window cannot shrink below the floor, so a longer deadline is the
// only lever left; an advancing cycle resets it.
func (wt *wedgeTracker) budget() time.Duration {
	mult := 1
	for i := 0; i < wt.floorStalls && mult < MaxCycleBudgetMultiple; i++ {
		mult *= 2
	}
	return PerSourceTimeout * time.Duration(mult)
}

// floorStall records one cycle that ended at the floor under a deadline without advancing, and
// raises the wedge gauge once that has persisted WedgeCycles cycles.
func (wt *wedgeTracker) floorStall(source string) {
	wt.floorStalls++
	if wt.floorStalls >= WedgeCycles {
		obs.ProjectorWedged.WithLabelValues(source).Set(1)
	}
}

// advanced records forward progress (or caught-up), clearing the stall and the wedge gauge.
func (wt *wedgeTracker) advanced(source string) {
	wt.floorStalls = 0
	obs.ProjectorWedged.WithLabelValues(source).Set(0)
}

// runOneSource is the per-source catch-up loop: read forward from the cursor, up to BatchLimit
// rows per cycle, advancing the cursor on success.
func (p *Projector) runOneSource(ctx context.Context, src Source) {
	t := time.NewTicker(Interval)
	defer t.Stop()
	// Per-source state below is goroutine-owned, so needs no locking. The adaptive window starts at
	// BatchLimit, halves on a deadline-exceeded cycle and doubles back on success.
	window := uint32(BatchLimit)
	var tracker poisonTracker
	// Seed the wedge gauge at 0 so the alert reads a real "healthy" zero, not absent data (a silence).
	var wedge wedgeTracker
	obs.ProjectorWedged.WithLabelValues(src.Name).Set(0)
	// Own ClickHouse connection per source: no dial per cycle, and a slow watermark scan never
	// queues another source behind a shared pool.
	lake := p.newSourceLake()
	defer lake.close()
	// First cycle runs immediately so a fresh deploy does not wait Interval.
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

// lakeReader is the ClickHouse read surface a CH-mode cycle needs.
type lakeReader interface {
	ContiguousWatermark(ctx context.Context, from, to uint32) (uint32, error)
	LakeMinLedger(ctx context.Context) (uint32, error)
	Close() error
}

// eventSource is the projector's read of the ClickHouse lake; tests inject an in-memory fake for
// [chEventSource].
type eventSource interface {
	StreamEvents(ctx context.Context, from, to uint32, contractIDs, topic0Syms, excludeTopic0Syms []string,
		withStateWriteKeys bool, fn func(events.Event) error) error
	FirstEventLedger(ctx context.Context, from, to uint32,
		contractIDs, topic0Syms, excludeTopic0Syms []string) (uint32, bool, error)
	OpenLake(ctx context.Context) (lakeReader, error)
}

// chEventSource is the [eventSource] over the ClickHouse lake at addr.
type chEventSource struct{ addr string }

// StreamEvents reads contract_events directly. No FINAL: the forward window is BatchLimit-small and
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

// newSourceLake returns one source goroutine's lake connection, opened on first use.
func (p *Projector) newSourceLake() *sourceLake {
	return &sourceLake{open: func(ctx context.Context) (lakeReader, error) {
		if p.lakeEvents == nil {
			return nil, errors.New("projector: no ClickHouse lake configured")
		}
		return p.lakeEvents.OpenLake(ctx)
	}}
}

// sourceLake is one source goroutine's lazily-opened lake connection. A failed open is not cached,
// so a ClickHouse outage at start self-heals. Not safe for concurrent use.
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

// heldRow is one sink failure that HOLDS the cursor this cycle, with its [sinkDisposition] and
// consecutive-failure count.
type heldRow struct {
	id rowIdentity
	// holdLedger is the ledger the cursor must stay below when it differs from id.ledger (a carried
	// output from an earlier ledger); 0 means id.ledger.
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

// quarantineCandidate returns the index of the one held row to give up on, or -1. Only
// [dispositionUnclassified] rows qualify; an identified infra fault is never dropped. madeProgress
// separates a poison row from a broken sink: the budget is [QuarantineAfterCycles] with it and
// [QuarantineAfterCyclesNoProgress] without, so a global fault stalls visibly instead of shedding.
// At most one row per cycle, lowest ledger first.
func quarantineCandidate(held []heldRow, madeProgress bool) int {
	budget := QuarantineAfterCyclesNoProgress
	if madeProgress {
		budget = QuarantineAfterCycles
	}
	return shedCandidate(held, dispositionUnclassified, budget)
}

// permanentSkipCandidate returns the index of the next [dispositionSkip] row to shed, or -1. A
// class-22/23 rejection is row-local only while the sink accepts other writes (a migration adding
// a constraint every row violates raises the same SQLSTATE globally), so with madeProgress the row
// is shed at once, and without it only after [QuarantineAfterCyclesNoProgress]. Lowest ledger
// first keeps the watermark monotonic.
func permanentSkipCandidate(poisoned []heldRow, madeProgress bool) int {
	budget := QuarantineAfterCyclesNoProgress
	if madeProgress {
		budget = 1
	}
	return shedCandidate(poisoned, dispositionSkip, budget)
}

// shedCandidate returns the lowest-ledger row with disposition `want` that failed at least `budget`
// consecutive cycles, or -1. One row per call keeps both give-up arms bounded and loud.
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

// lowestHeldLedger returns the lowest ledger still held for retry; the cursor may reach it minus 1.
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

// commitCursor compare-and-swaps the cursor from what this cycle read to commitTo. An unconditional
// write would overwrite a projector-replay rewind landing mid-cycle, so the replay would report
// success and re-project nothing.
//
// Losing the CAS abandons only the position (sink writes are idempotent); it counts as "error"
// because repeated losses (two projectors on one cursor) should ticket. The write has its own
// deadline: on the cycle's expired context the watermark would be lost and the window re-projected
// forever.
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
//   - decode failure: skip the row (a retry would re-fail) but mark the run decode_degraded so a
//     decoder regression is not reported as "ok".
//   - transient sink fault: cap the cursor at the last fully committed ledger.
//   - permanent sink fault (SQLSTATE 22/23 or a value-shape rejection): skip under the
//     [permanentSkipCandidate] rules, holding the rest.
//   - unclassified sink fault: hold a bounded number of cycles, then quarantine
//     ([quarantineCandidate]); a sole-writer domain such as sep41 would otherwise halt on one
//     malformed on-chain value.
//
// Quarantine destroys no evidence: the raw event stays in the lake and projector-replay re-drives
// the range once the defect is fixed. It is one linear cycle so the cursor-watermark invariant reads
// in one place.
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
	// read is what the commit is conditional on (see commitCursor): everything below derives from it.
	var read timescale.CursorRead
	if err == nil {
		read = timescale.CursorRead{Exists: true, LastLedger: cursor.LastLedger}
		// +1 because the BETWEEN scan is inclusive on both ends. Recorded from the READ, not the
		// commit, so a held cursor keeps reporting its true position to the replay-window watcher.
		fromLedger = cursor.LastLedger + 1
		p.recordCursor(src.Name, cursor.LastLedger)
	} else {
		fromLedger = p.seedFromLedger(cycleCtx, src, lake)
	}

	// Upper bound: the live ledgerstream tip, since scanning to wherever soroban_events extends could
	// run ahead of what live writes committed. In CH mode it is also clamped to the lake's provably
	// complete watermark (see resolveTip).
	tip, durableTip, err := p.resolveTip(cycleCtx, lake, fromLedger, saturatingAdd(fromLedger, *window))
	if err != nil {
		p.logger.Warn("projector: tip resolve failed", "source", src.Name, "err", err)
		obs.ProjectorRunsTotal.WithLabelValues(src.Name, "error").Inc()
		if durableTip > 0 {
			publishLag(src.Name, fromLedger, durableTip)
		}
		return
	}
	// Published before the scan so every later exit (stream error, failed commit) reports the true
	// lag, not a stale gauge; a committing cycle overwrites it with its post-commit value.
	publishLag(src.Name, fromLedger, durableTip)
	if tip < fromLedger {
		// Must be `<`, not `<=`: fromLedger is the next UNPROCESSED ledger and the scan is inclusive,
		// so tip == fromLedger still has one ledger to project.
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

		// A sink failure not positively identified as a permanent data fault must not let the cursor
		// pass its ledger, or the row is lost for a sole-writer (sep41) domain: `held` caps the
		// cursor at (lowest held ledger - 1). A PERMANENT fault lands in `poisoned` and holds the
		// cursor only until the shed cap ([PermanentSkipPerCycle]) releases it, so a poison row
		// cannot wedge the source and a GLOBAL class-22/23 fault cannot shed a whole backlog.
		held               []heldRow
		poisoned           []heldRow
		failedThisCycle    = make(map[rowIdentity]bool)
		sinkPermanentFails int
		sinkI128Overflows  int
		sinkQuarantined    int
	)
	// process runs the per-event decode + route for either read source. Decode failures soft-fail
	// (the row is deterministically broken, so the cursor still advances). A SINK failure is
	// classified ([classifySinkFault]): permanent is counted and skipped; transient or unclassified
	// holds the cursor below ev.Ledger and counts consecutive failing cycles for this exact row.
	//
	// Adjacent-duplicate guard: the lake is read WITHOUT FINAL, so re-ingested duplicates arrive
	// CONSECUTIVELY. Keyed ON CONFLICT sinks absorb that, but BUFFERED decoders (phoenix's
	// multi-event correlation) do not: a duplicate field-event re-opens a completed group and
	// cross-assigns fields between legs. rows_scanned still counts raw rows.
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
		// Charged once per ROW: poison outputs and the held fault share one identity, so charging
		// twice would halve every budget.
		failedThisCycle[id] = true
		fails := tracker.fail(id)
		// Poison OUTPUTS cannot succeed on retry, so each is logged loud and counted.
		for _, dropErr := range faults.dropped {
			// An i128 overflow proves an int64 reached one of OUR amount paths (ADR-0003 SEV-1), not a
			// bad on-chain value; its own outcome gives the alert a rule without double-counting.
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
			// Transient or unclassified: hold the cursor below this ledger. The whole ROW is retried,
			// so it is not also a shed candidate; shedding would drop the budget it is counting.
			held = append(held, heldRow{id: id, holdLedger: faults.heldLedger, disposition: disposition, fails: fails, err: sinkErr})
			if fails == 1 || fails%heldRowLogEvery == 0 {
				p.logger.Warn("projector: sink failure — holding cursor for retry (NOT advancing past this ledger)",
					"source", src.Name, "ledger", ev.Ledger, "tx", ev.TxHash,
					"op_index", ev.OperationIndex, "event_index", ev.EventIndex,
					"disposition", disposition.String(), "consecutive_cycles", fails, "err", sinkErr)
			}
			return
		}
		// All faults are permanent, but a class-22/23 verdict is row-local only while the sink
		// works, and this arm cannot tell that from a migration rejecting every row. So the row is
		// only a shed CANDIDATE; [permanentSkipCandidate] releases at most [PermanentSkipPerCycle].
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
					// Skipped with the cursor advancing, since re-reading an unrecoverable row re-fails.
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
		// A window too dense for PerSourceTimeout would retry the IDENTICAL range forever, so halve
		// toward MinBatchLimit; the success path doubles back toward BatchLimit.
		if next, shrunk := shrinkWindow(*window, err); shrunk {
			*window = next
			p.logger.Warn("projector: cycle exceeded deadline — shrinking window",
				"source", src.Name, "from", fromLedger, "to", toLedger, "next_window", *window)
		} else {
			p.logger.Warn("projector: stream failed", "source", src.Name, "err", err, "from", fromLedger, "to", toLedger)
		}
		obs.ProjectorRunsTotal.WithLabelValues(src.Name, "error").Inc()
		// A deadline at the window floor is the terminal stall; non-deadline errors leave the count alone.
		if errors.Is(err, context.DeadlineExceeded) && *window <= MinBatchLimit {
			wedge.floorStall(src.Name)
		}
		return
	}

	// The budget counts cycles in which a row was re-read AND re-failed, so a healed row restarts at zero.
	tracker.retain(failedThisCycle)

	if added := prefilterWidened(src, prefilter); len(added) > 0 {
		p.holdForWidenedGate(src.Name, fromLedger, toLedger, added)
		return
	}

	// Shed cap: release at most [PermanentSkipPerCycle] permanently rejected rows, lowest ledger
	// first. Shedding all inline would pass a whole backlog when the same SQLSTATE arrives globally
	// (a migration whose CHECK every row violates); capped, it is a visible stall bleeding one
	// logged row per cycle. `eventsEmitted > 0` is the sink-health proof; without it the verdict
	// waits out [QuarantineAfterCyclesNoProgress].
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

	// Give up on at most ONE held row with an exhausted retry budget, so an unclassified
	// deterministic failure cannot hold a sole-writer domain's cursor forever.
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

	// Sink-side shrink: if the scan finishes but sink writes exhaust PerSourceTimeout, the rest
	// fast-fail on the dead cycleCtx and the IDENTICAL window would retry forever.
	if cycleCtx.Err() != nil && sinkTransientFails > 0 {
		if next, shrunk := shrinkWindow(*window, context.DeadlineExceeded); shrunk {
			*window = next
			p.logger.Warn("projector: cycle budget exhausted in sink writes — shrinking window",
				"source", src.Name, "from", fromLedger, "to", toLedger,
				"transient_fails", sinkTransientFails, "next_window", *window)
		}
	}

	// Cursor watermark: advance only to the highest ledger for which EVERY event fully committed.
	// With nothing held that is `toLedger`, so empty stretches are not rescanned. A held failure caps
	// it at (lowest held ledger - 1); idempotent Insert* absorbs the re-read.
	commitTo := toLedger
	firstHeldLedger, holding := lowestHeldLedger(held)
	// A poison row the shed cap did not release holds the cursor like a retryable fault; advancing
	// past it would be the unbounded silent loss the cap exists to stop.
	if poisonLedger, poisonHolding := lowestHeldLedger(poisoned); poisonHolding &&
		(!holding || poisonLedger < firstHeldLedger) {
		firstHeldLedger, holding = poisonLedger, true
	}
	if holding && firstHeldLedger <= fromLedger {
		// Nothing new is durably committed: leave the cursor, a VISIBLE stall (rising lag plus
		// sink_retry), never a silent advance past loss.
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
		// Gated on a spent cycle budget so a plain transient sink outage, which announces itself,
		// does not masquerade as a wedge.
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

	wedge.advanced(src.Name)

	// A cycle that spent its budget is no evidence the dense stretch has passed, so the sink-side
	// shrink stands.
	if cycleCtx.Err() == nil {
		*window = recoverWindow(*window)
	}

	obs.ProjectorLagLedgers.WithLabelValues(src.Name).Set(float64(durableTip - commitTo))
	// eventsEmitted excludes outputs whose sink write failed, so a sink-lost event is never "ok".
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
	// A distinct outcome makes a source with a pending retry above commitTo alert instead of
	// stalling under "ok". A decoder REGRESSION drops a whole class of valid events while the cursor
	// sails to tip, so a decode-dropping cycle is "decode_degraded" and the decode_error RATE alert
	// separates a sustained spike from scattered poison rows. sink_retry takes precedence, including
	// for poison rows the shed cap held back.
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

// shrinkWindow halves the adaptive window when a cycle failed on a deadline, down to
// MinBatchLimit. It returns (current, false) for other errors or at the floor.
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

// recoverWindow doubles the adaptive window back toward BatchLimit after a successful cycle.
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

// recordNothingToScan emits metrics for a cycle with an empty scan range (scanTip < fromLedger).
// If the lake watermark clamped the bound below ledgers ledgerstream already holds, the source is
// HELD at a lake hole, not caught up: it counts as watermark_held with lag measured to the durable
// tip, so a stalled watermark pages via lag.
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

// publishLag sets the lag gauge to the ledgers in [fromLedger, durableTip], or 0 once the cursor
// has reached the tip.
func publishLag(source string, fromLedger, durableTip uint32) {
	lag := 0.0
	if durableTip >= fromLedger {
		lag = float64(durableTip - fromLedger + 1)
	}
	obs.ProjectorLagLedgers.WithLabelValues(source).Set(lag)
}

// publishLagFromLastRead keeps the lag gauge live when the cursor read failed: only this projector
// advances the cursor, so the last read position is where the source stands.
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

// seedFromLedger returns the first ledger a source with no cursor scans. Every ledger below the
// seed is proven to hold nothing the stream would return, so starting there loses no event and
// skips the BatchLimit-per-Interval crawl from ledger 0. A failed seek falls back to 0, the slow
// but lossless crawl.
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

// findSeed seeks the source's first matching event in [floor, tip] using the same tip bound a
// cycle scans to, so the range it proves empty is exactly what a scan from the floor would read.
// With no match it returns tip, and the one-ledger scan [tip, tip] then writes the cursor. settled
// is false while nothing is durable at or above the floor (freshSourceStart).
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

// holdForWidenedGate ends a cycle whose read admitted new contracts mid-window (a factory announced
// a pool), whose events in [from, to] the starting prefilter had filtered out. The cursor stays put
// so the next cycle re-reads with the widened prefilter.
func (p *Projector) holdForWidenedGate(source string, from, to uint32, added []string) {
	obs.ProjectorRunsTotal.WithLabelValues(source, "gate_widened").Inc()
	p.logger.Info("projector: contract gate widened mid-window — re-reading the window with the new prefilter",
		"source", source, "from", from, "to", to, "added", len(added), "first_added", added[0])
}

// resolveTip returns the upper scan bound for one cycle: the live ledgerstream cursor's last_ledger
// (as gap_detector.go::resolveGapDetectorTip), so the projector never gets ahead of durably-ingested
// ledgers.
//
// In CH feed-switch mode (lakeEvents set) the bound is also clamped to the lake's contiguous
// completeness watermark: the live dual-sink can drop ledgers, so reading past a hole would lose
// events. Clamping stalls the source AT the hole until catch-up heals it (ADR-0041).
//
// In soroban_events mode the raw sink commits asynchronously after the cursor advances, so the
// bound is returned only once the raw-event barrier ([Projector.SetRawEventBarrier]) has seen
// every earlier row commit; a barrier that cannot settle fails the cycle.
//
// scanLimit bounds the CH watermark query to one batch window past from. durableTip is the unclamped
// ledgerstream tip, so a stalled watermark shows as rising lag; a watermark error still returns it.
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

// freshSourceStart is the first ledger a source with no cursor row scans: its [Source.Genesis],
// raised in CH mode to the lake's first ledger, since a floor below it is a boundary hole the
// watermark would stall on forever.
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
