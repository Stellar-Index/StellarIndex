package pipeline

import (
	"bytes"
	"context"
	"go/ast"
	"go/token"
	"io"
	"log/slog"
	"math"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/consumer"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/sources/aquarius"
	"github.com/Stellar-Index/StellarIndex/internal/sources/band"
	"github.com/Stellar-Index/StellarIndex/internal/sources/reflector"
	"github.com/Stellar-Index/StellarIndex/internal/sources/sdex"
	"github.com/Stellar-Index/StellarIndex/internal/sources/soroswap"
)

// fakeEvent is a consumer.Event that hits the sink's default
// (unhandled) case so the test exercises the buffered-drain logic
// without needing a real postgres store.
type fakeEvent struct {
	id int
}

func (fakeEvent) EventKind() string { return "test.fake" }
func (fakeEvent) Source() string    { return "test-fake-source" }

var _ consumer.Event = fakeEvent{}

// TestPersistEvents_DrainsBufferedEventsOnShutdown — the load-bearing
// safety property: when the parent ctx is cancelled mid-stream,
// PersistEvents must still consume every event already in the
// channel buffer before returning. Without this, the indexer's
// per-ledger cursor advance (which happens AFTER the producer
// enqueues events to `in`, BEFORE the sink writes them) would
// silently lose up to cap(in) events on every SIGTERM.
func TestPersistEvents_DrainsBufferedEventsOnShutdown(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	in := make(chan consumer.Event, 10)

	// Pre-fill the buffer so we can prove the drain reads them
	// after ctx is cancelled.
	const buffered = 10
	for i := 0; i < buffered; i++ {
		in <- fakeEvent{id: i}
	}

	// Cancel the ctx FIRST so PersistEvents enters the drain path
	// on its first iteration. (If we cancelled mid-iteration, the
	// race between `case <-ctx.Done()` and `case ev, ok := <-in`
	// would be Go-runtime-dependent and the test would be flaky.)
	cancel()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	done := make(chan struct{})
	go func() {
		defer close(done)
		// Pass nil store — the test events hit the default
		// (unhandled) switch case which only logs + increments a
		// counter, never touching the store. If a future
		// handleOneEvent change makes the default case dereference
		// the store, this test surfaces it as a panic — which is
		// the correct signal.
		PersistEvents(ctx, logger, nil, in, SinkModeAll)
	}()

	// Close the channel so drain can exit cleanly without hitting
	// the 30-second drainTimeout fallback.
	close(in)

	select {
	case <-done:
		// PersistEvents returned — verify it drained everything by
		// checking the channel is empty (an undrained channel would
		// still have events).
		if got := len(in); got != 0 {
			t.Errorf("after shutdown drain, channel still has %d events; want 0", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("PersistEvents didn't return within 5s after ctx cancel + channel close")
	}
}

// TestPersistEvents_DrainTimeoutBoundsHang — paranoid safety net:
// if the drain path's `handleOneEvent` ever blocks (e.g. a future
// store call hangs), [drainTimeout] still bounds the shutdown. We
// can't easily simulate a hang in unit-time, so we just sanity-check
// the timeout constant is non-zero and within an operationally-sane
// bound.
func TestPersistEvents_DrainTimeoutBoundsHang(t *testing.T) {
	if drainTimeout <= 0 {
		t.Errorf("drainTimeout = %v; must be positive", drainTimeout)
	}
	if drainTimeout > 5*time.Minute {
		t.Errorf("drainTimeout = %v; > 5min defeats the bounded-shutdown invariant", drainTimeout)
	}
}

// TestDrainBudget_FitsShutdownDeadline — CON-10 (audit-2026-07-23).
// The sink's post-cancellation drain, PLUS the final best-effort pass
// that runs after it, must finish INSIDE the process-level shutdown
// window; otherwise main hard-exits first and worker 0's deadline arm —
// the only thing that reports the exact undrained ledger range for
// re-derive — never runs. Production had drainTimeout=90s against a 30s
// process deadline, so the loss report could never fire.
func TestDrainBudget_FitsShutdownDeadline(t *testing.T) {
	if drainTimeout <= 0 {
		t.Fatalf("drainTimeout = %v; must be positive", drainTimeout)
	}
	if drainFinalPassBudget <= 0 {
		t.Fatalf("drainFinalPassBudget = %v; must be positive", drainFinalPassBudget)
	}
	// The sink's whole shutdown sequence: one shared drain deadline, then
	// the final best-effort pass, then the ERROR report.
	if total := drainTimeout + drainFinalPassBudget; total > ShutdownDeadline {
		t.Errorf("sink drain budget %v (drainTimeout %v + final pass %v) exceeds ShutdownDeadline %v — the undrained-ledger-range ERROR can never fire before the process is killed (CON-10)",
			total, drainTimeout, drainFinalPassBudget, ShutdownDeadline)
	}
	// And leave room for the report itself to be written.
	if slack := ShutdownDeadline - (drainTimeout + drainFinalPassBudget); slack < drainReportMargin {
		t.Errorf("only %v of slack before the hard exit; want >= %v for the undrained-range ERROR to be emitted", slack, drainReportMargin)
	}
}

// TestShutdownDeadline_MainUsesConstant — the other half of CON-10: the
// budgets above are only consistent if the indexer's shutdown context is
// actually built from [ShutdownDeadline]. A literal there is exactly how
// the two drifted apart (90s of sink drain inside a 30s process
// deadline), so pin the wiring by AST rather than by hope.
func TestShutdownDeadline_MainUsesConstant(t *testing.T) {
	fset := token.NewFileSet()
	main := parseFile(t, fset, repoDir("cmd", "stellarindex-indexer", "main.go"))

	found := false
	ast.Inspect(main, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "WithTimeout" || len(call.Args) != 2 {
			return true
		}
		if pkg, ok := sel.X.(*ast.Ident); !ok || pkg.Name != "context" {
			return true
		}
		arg, ok := call.Args[1].(*ast.SelectorExpr)
		if !ok {
			return true
		}
		pkg, ok := arg.X.(*ast.Ident)
		if ok && pkg.Name == "pipeline" && arg.Sel.Name == "ShutdownDeadline" {
			found = true
		}
		return true
	})
	if !found {
		t.Error("cmd/stellarindex-indexer/main.go does not build its shutdown context from pipeline.ShutdownDeadline — the sink derives its drain budgets from that constant, so a literal here silently re-opens CON-10 (drain budget > process deadline ⇒ the undrained-ledger-range ERROR never fires)")
	}
}

// TestSinkDrain_NonTradeWritesAreResilient — REL-08(c)
// (audit-2026-07-23). The dispatcher drain carries served-tier writes
// NOBODY else makes (band oracle_updates, external.UpdateEvent, the
// supply observers' LedgerEntry observations, soroswap_router swaps,
// defindex flows). Every drain site used to call `_ = HandleEvent(...)`,
// discarding the error, so a Postgres infra fault dropped the row while
// the cursor advanced. They must all go through persistEventResilient
// (block-and-retry on infra, isolate+count on a permanent data fault).
//
// Structural because it is a wiring invariant over every call site in
// those two functions, which no single behavioural test can cover. The
// policy itself is proven behaviourally by the retryInfra /
// classifyFault tests, end-to-end by
// TestPersistEvents_DataFaultEventIsCountedAsDropped, and — for the
// shutdown-race half — by
// TestPersistWorker_ShutdownRacingInFlightEventWrite_EventLandsNotLost,
// which the eventPersister seam (#368 M3) made possible.
func TestSinkDrain_NonTradeWritesAreResilient(t *testing.T) {
	fset := token.NewFileSet()
	sink := parseFile(t, fset, "sink.go")

	for _, fn := range []string{"persistWorker", "drainBufferedEvents"} {
		decl := funcDecl(t, sink, fn)
		resilient := 0
		ast.Inspect(decl, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			id, ok := call.Fun.(*ast.Ident)
			if !ok {
				return true
			}
			switch id.Name {
			case "HandleEvent", "handleEvent":
				t.Errorf("%s calls %s directly — its error is discarded, so a Postgres infrastructure fault drops the event while the cursor advances (REL-08). Route it through persistEventResilient", fn, id.Name)
			case "persistEventResilient":
				resilient++
			}
			return true
		})
		if resilient == 0 {
			t.Errorf("%s has no persistEventResilient call — non-trade served-tier writes are unprotected on the drain path (REL-08)", fn)
		}
	}
}

// TestPersistEvents_DataFaultEventIsCountedAsDropped — REL-08(c)
// end-to-end through the real drain: an oracle update the store rejects
// as permanently invalid (canonical.ErrInvalidOracle, returned by
// Validate before any SQL runs) must be ISOLATED — counted on the
// ADR-0041 drop label and skipped — and must NOT wedge the drain in a
// retry loop. Before the fix the error was discarded: no drop was
// counted anywhere, so the loss was invisible to metrics.
func TestPersistEvents_DataFaultEventIsCountedAsDropped(t *testing.T) {
	before := testutil.ToFloat64(obs.SourceInsertErrorsTotal.WithLabelValues(band.SourceName, "dropped"))

	in := make(chan consumer.Event, 1)
	// Missing tx_hash/timestamp/assets → OracleUpdate.Validate returns
	// ErrInvalidOracle, a permanent data fault, without touching the DB
	// (so a nil store is never dereferenced).
	in <- band.UpdateEvent{Update: canonical.OracleUpdate{Source: band.SourceName}}
	close(in)

	done := make(chan struct{})
	go func() {
		defer close(done)
		PersistEvents(context.Background(), slog.New(slog.NewTextHandler(io.Discard, nil)), nil, in, SinkModeAll)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("PersistEvents did not return — a permanent data fault must be isolated, not retried forever")
	}

	got := testutil.ToFloat64(obs.SourceInsertErrorsTotal.WithLabelValues(band.SourceName, "dropped")) - before
	if got != 1 {
		t.Errorf("source_insert_errors{band,dropped} delta = %v; want 1 — every event the sink gives up on must be counted so the loss is never silent (REL-08)", got)
	}
}

// TestPersistEvents_NormalCloseStillWorks — natural completion
// (channel closed without ctx cancel) is the common case for a
// bounded backfill. Make sure the new drain path didn't break it.
func TestPersistEvents_NormalCloseStillWorks(t *testing.T) {
	ctx := context.Background()
	in := make(chan consumer.Event, 5)

	// Buffer some events, then close. PersistEvents should consume
	// all of them and return without needing ctx cancellation.
	for i := 0; i < 5; i++ {
		in <- fakeEvent{id: i}
	}
	close(in)

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	done := make(chan struct{})
	go func() {
		defer close(done)
		PersistEvents(ctx, logger, nil, in, SinkModeAll)
	}()

	select {
	case <-done:
		// Counter sanity: every event reached handleOneEvent.
		// We don't have a direct hook, so we re-use the channel
		// length: drained == empty.
		if got := len(in); got != 0 {
			t.Errorf("len(in)=%d after PersistEvents return; want 0", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("PersistEvents didn't return after channel close within 5s")
	}
}

// processedCount lets future tests verify drain counts without
// scraping prometheus globals; not used by the current test set
// but kept here as a hook for the next test that needs it.
var processedCount atomic.Int64

func init() { processedCount.Store(0) }

// TestDrainFinalPass_SkipInSinkExcludedFromReport is the regression
// test for REL-02 (audit-2026-07-23, low): drainBufferedEvents' final
// best-effort pass used to bump undrained_events/undrained_trades
// (and widen the ledger range) for EVERY event still in the channel,
// including ones skipInSink was about to skip — a projector-owned
// event that was never going to be persisted by this drain even on a
// clean shutdown. That inflated the drain-timeout recovery hint,
// telling an operator to re-derive a range the projector already
// durably owns.
//
// Calls drainFinalPass directly (extracted from drainBufferedEvents'
// ctx.Done() case) rather than racing drainBufferedEvents' outer
// select against its own already-expired ctx.Done() — see
// drainFinalPass's godoc for why that race can't be made
// deterministic through the public API.
func TestDrainFinalPass_SkipInSinkExcludedFromReport(t *testing.T) {
	in := make(chan consumer.Event, 2)
	// Projector-owned under SinkModeSkipProjected (see
	// IsProjectedEvent) — must never count toward the report.
	in <- reflector.UpdateEvent{}
	// NOT projector-owned; guaranteed permanent data fault WITHOUT
	// touching the DB (OracleUpdate.Validate rejects a bare Source),
	// the same trick TestPersistEvents_DataFaultEventIsCountedAsDropped
	// uses — safe with a nil store.
	in <- band.UpdateEvent{Update: canonical.OracleUpdate{Source: band.SourceName}}
	close(in)

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))

	drainFinalPass(in, logger, storeEventPersister(logger, nil), nil, SinkModeSkipProjected)

	out := buf.String()
	if !strings.Contains(out, "undrained_events=1") {
		t.Errorf("log output = %q; want undrained_events=1 — the skipInSink event must be excluded from the count", out)
	}
	if strings.Contains(out, "undrained_events=2") {
		t.Errorf("log output = %q; the skipped projector-owned event inflated undrained_events to 2", out)
	}
}

// TestDrainBufferedEvents_UndrainedRowsAreCounted pins the served-tier
// shutdown-loss counter: a sink shut down with buffered rows its bounded
// drain cannot land must advance
// stellarindex_sink_undrained_rows_total{persist_events,<kind>} by the
// exact ROW count, per kind. Before the counter the only trace of this
// loss was the ERROR log line, while the ledger cursor — upserted per
// ledger BEFORE the sink writes — had already advanced past the rows, so
// a deploy that caught Postgres slow or down lost served-tier rows with
// nothing alerting (the CH live-sink half of the same class already had
// its `dropped` counter + rules).
//
// Both fakes answer the way a store answers a drain whose budget has
// expired — with the ctx error. flushTradeBatch hands such a batch
// straight back (no per-row pass, no backoff) and persistEventResilient
// returns it unchanged, so the drain's only remaining move is to REPORT,
// which makes the count deterministic without waiting out a real
// drainTimeout. The channel is closed so drainBufferedEvents takes its
// `!ok` flush path rather than racing its own deadline arm.
func TestDrainBufferedEvents_UndrainedRowsAreCounted(t *testing.T) {
	tradesBefore := counter(t, obs.SinkUndrainedRowsTotal, obs.SinkPersistEvents, "trade")
	eventsBefore := counter(t, obs.SinkUndrainedRowsTotal, obs.SinkPersistEvents, "event")

	const trades, events = 3, 2
	in := make(chan consumer.Event, trades+events)
	for i := uint32(0); i < trades; i++ {
		in <- sdex.TradeEvent{Trade: mkTrade("sdex", 700+i)}
	}
	for i := 0; i < events; i++ {
		// NOT projector-owned under SinkModeSkipProjected (see
		// TestDrainFinalPass_SkipInSinkExcludedFromReport), so the drain
		// must try to persist it.
		in <- band.UpdateEvent{Update: canonical.OracleUpdate{Source: band.SourceName}}
	}
	close(in)

	store := &fakeTradeStore{failErr: context.DeadlineExceeded} // stays unhealthy
	ep := func(context.Context, consumer.Event, bool) error { return context.DeadlineExceeded }

	drainBufferedEvents(in, discardLogger(), ep, store, SinkModeSkipProjected, time.Now().Add(time.Minute))

	if n := store.landedCount(); n != 0 {
		t.Fatalf("landed %d trades through a store that refuses every write; want 0", n)
	}
	if got := counter(t, obs.SinkUndrainedRowsTotal, obs.SinkPersistEvents, "trade") - tradesBefore; got != trades {
		t.Errorf("sink_undrained_rows{persist_events,trade} delta = %v; want %d (one per abandoned trade row)", got, trades)
	}
	if got := counter(t, obs.SinkUndrainedRowsTotal, obs.SinkPersistEvents, "event") - eventsBefore; got != events {
		t.Errorf("sink_undrained_rows{persist_events,event} delta = %v; want %d (one per abandoned non-trade event)", got, events)
	}
}

// TestDrainBufferedEvents_CleanDrainCountsNoUndrainedRows is the
// non-vacuity half of the test above: the same buffered rows through a
// store that accepts them must leave the counter untouched. The counter
// means LOSS — a clean deploy must read as zero, or the alert on it
// tickets every restart.
func TestDrainBufferedEvents_CleanDrainCountsNoUndrainedRows(t *testing.T) {
	tradesBefore := counter(t, obs.SinkUndrainedRowsTotal, obs.SinkPersistEvents, "trade")
	eventsBefore := counter(t, obs.SinkUndrainedRowsTotal, obs.SinkPersistEvents, "event")

	in := make(chan consumer.Event, 4)
	in <- sdex.TradeEvent{Trade: mkTrade("sdex", 800)}
	in <- sdex.TradeEvent{Trade: mkTrade("sdex", 801)}
	in <- band.UpdateEvent{Update: canonical.OracleUpdate{Source: band.SourceName}}
	close(in)

	store := &fakeTradeStore{}
	store.healthy.Store(true)
	ep := func(context.Context, consumer.Event, bool) error { return nil }

	drainBufferedEvents(in, discardLogger(), ep, store, SinkModeSkipProjected, time.Now().Add(time.Minute))

	if n := store.landedCount(); n != 2 {
		t.Fatalf("landed %d trades; want 2 — the fixture's clean path did not run, so the zero-delta below would be vacuous", n)
	}
	if got := counter(t, obs.SinkUndrainedRowsTotal, obs.SinkPersistEvents, "trade") - tradesBefore; got != 0 {
		t.Errorf("sink_undrained_rows{persist_events,trade} delta = %v on a clean drain; want 0", got)
	}
	if got := counter(t, obs.SinkUndrainedRowsTotal, obs.SinkPersistEvents, "event") - eventsBefore; got != 0 {
		t.Errorf("sink_undrained_rows{persist_events,event} delta = %v on a clean drain; want 0", got)
	}
}

// TestShutdownSafeCtx_LiveCtxPassedThroughUnchanged pins the
// no-op-on-live-ctx half of CON-09's fix: a still-live parent context
// must be returned as-is (not wrapped), so the normal (non-racy) path
// through persistWorker is unaffected.
func TestShutdownSafeCtx_LiveCtxPassedThroughUnchanged(t *testing.T) {
	ctx := context.Background()
	got, cancel := shutdownSafeCtx(ctx)
	defer cancel()
	if got != ctx {
		t.Errorf("shutdownSafeCtx(live ctx) returned a different context; want the same ctx passed through unchanged")
	}
}

// TestShutdownSafeCtx_CancelledParentGetsFreshBoundedCtx is the
// regression test for CON-09 (audit-2026-07-23): persistWorker's
// flushTicker and `<-in` arms used to pass the worker's ctx straight
// into flush()/persistEventResilient() with no check. Go's select has
// no priority, so on the exact iteration the parent ctx is cancelled,
// one of those arms can still win the race against `<-ctx.Done()` —
// passing the already-dead ctx through makes every write fail
// instantly (ctx.Err() short-circuits before the DB is ever touched),
// silently abandoning work that the worker's own shutdown path a
// moment later would have given a fair shot at landing within
// [drainTimeout].
//
// Asserts the corrected behaviour: given an ALREADY-CANCELLED parent,
// shutdownSafeCtx must return a DIFFERENT, still-LIVE context (so the
// caller's next write isn't dead on arrival) with a real deadline
// roughly [drainTimeout] out.
func TestShutdownSafeCtx_CancelledParentGetsFreshBoundedCtx(t *testing.T) {
	parent, parentCancel := context.WithCancel(context.Background())
	parentCancel() // simulate the racy-select window: ctx already done

	got, cancel := shutdownSafeCtx(parent)
	defer cancel()

	if got == parent {
		t.Fatalf("shutdownSafeCtx(cancelled ctx) returned the SAME dead context — every write through it fails instantly (ctx.Err() short-circuits), reproducing CON-09")
	}
	if err := got.Err(); err != nil {
		t.Errorf("shutdownSafeCtx(cancelled ctx).Err() = %v, want nil — the fresh context must be usable for a write attempt", err)
	}
	deadline, ok := got.Deadline()
	if !ok {
		t.Fatalf("shutdownSafeCtx(cancelled ctx) has no deadline — want one bounded by drainTimeout so a stuck write can't hang shutdown forever")
	}
	if remaining := time.Until(deadline); remaining <= 0 || remaining > drainTimeout+time.Second {
		t.Errorf("shutdownSafeCtx(cancelled ctx) deadline %v from now; want roughly drainTimeout (%v) out", remaining, drainTimeout)
	}
}

// TestPersistWorker_UsesShutdownSafeCtxOnFlushAndPersistArms is a
// structural pin (same style as TestSinkDrain_NonTradeWritesAreResilient)
// that persistWorker actually WIRES shutdownSafeCtx into its
// flushTicker and `<-in` select arms, rather than the fix regressing
// to a direct `flush(ctx)` / `persistEventResilient(ctx, ...)` call —
// which would compile and pass every other test while silently
// reopening CON-09.
func TestPersistWorker_UsesShutdownSafeCtxOnFlushAndPersistArms(t *testing.T) {
	fset := token.NewFileSet()
	sink := parseFile(t, fset, "sink.go")
	decl := funcDecl(t, sink, "persistWorker")

	calls := 0
	ast.Inspect(decl, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if id, ok := call.Fun.(*ast.Ident); ok && id.Name == "shutdownSafeCtx" {
			calls++
		}
		return true
	})
	// One call in the flushTicker arm, one in the batch-full flush
	// inside the `<-in` arm, one guarding persistEventResilient in the
	// `<-in` arm's non-trade branch.
	if calls < 3 {
		t.Errorf("persistWorker calls shutdownSafeCtx %d times, want >= 3 (flushTicker arm, `<-in` batch-flush branch, `<-in` persistEventResilient branch) — CON-09's fix must guard every flush/persist call reachable from the racy select, not just ctx.Done()'s own arm", calls)
	}
}

// TestBlendEmitterUnlockTime_overflowSentinelRejected is the
// regression test for RLT-115 at this call site: a Blend Emitter
// UnlockTime near math.MaxUint64 (a plausible "unlimited" sentinel)
// wrapped NEGATIVE under a bare int64(e.UnlockTime) cast, landing near
// the 1970 epoch — a bogus but postgres-representable time that would
// silently corrupt the stored unlock time instead of leaving it unset.
func TestBlendEmitterUnlockTime_overflowSentinelRejected(t *testing.T) {
	if got := blendEmitterUnlockTime(math.MaxUint64); !got.IsZero() {
		t.Errorf("blendEmitterUnlockTime(MaxUint64) = %v, want zero time — got a wrapped near-epoch time instead of the overflow being rejected", got)
	}
}

// TestBlendEmitterUnlockTime_farFutureHonoured pins that a legitimate
// queued (q_swap) UnlockTime — days beyond its own ledger close time —
// is preserved rather than clamped to a close-time window.
func TestBlendEmitterUnlockTime_farFutureHonoured(t *testing.T) {
	raw := uint64(2_000_000_000) // 2033, far beyond any 24h window
	want := time.Unix(2_000_000_000, 0).UTC()
	if got := blendEmitterUnlockTime(raw); !got.Equal(want) {
		t.Errorf("blendEmitterUnlockTime(%d) = %v, want %v", raw, got, want)
	}
}

// persistCall is what the recording event persister saw for one event.
type persistCall struct {
	ctx        context.Context
	countEvent bool
}

// entriesBumpReachesStore reports whether bumpEntryCount under ctx would
// write source_entry_counts: with a nil store, reaching the store panics.
func entriesBumpReachesStore(ctx context.Context) (reached bool) {
	defer func() {
		if recover() != nil {
			reached = true
		}
	}()
	bumpEntryCount(ctx, discardLogger(), nil, "entries-probe")
	return false
}

// TestPersistWorker_Phase3ParallelWriteCountsOnlyInProjector pins GH-1021:
// under SinkModeSkipSoleWriter (persist_per_source=true) the dispatcher and
// the projector both persist every un-promoted projected event, and both
// used to count it — doubling the `entries` column and
// stellarindex_source_events_total for every projected source. The
// dispatcher's copy must count nothing; events only it writes (sdex, band)
// and every event under SinkModeAll (no projector) still count once.
func TestPersistWorker_Phase3ParallelWriteCountsOnlyInProjector(t *testing.T) {
	cases := []struct {
		name          string
		mode          SinkMode
		wantProjected bool
	}{
		{"phase3_parallel", SinkModeSkipSoleWriter, false},
		{"no_projector", SinkModeAll, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			soroswapBefore := counter(t, obs.SourceEventsTotal, soroswap.SourceName)
			sdexBefore := counter(t, obs.SourceEventsTotal, sdex.SourceName)

			var mu sync.Mutex
			calls := map[string]persistCall{}
			ep := func(ctx context.Context, ev consumer.Event, countEvent bool) error {
				mu.Lock()
				defer mu.Unlock()
				calls[ev.EventKind()] = persistCall{ctx: ctx, countEvent: countEvent}
				return nil
			}
			tw := &fakeTradeStore{}
			tw.healthy.Store(true)

			in := make(chan consumer.Event, 4)
			in <- soroswap.TradeEvent{Trade: mkTrade(soroswap.SourceName, 1)}
			in <- sdex.TradeEvent{Trade: mkTrade(sdex.SourceName, 2)}
			in <- aquarius.KillEvent{}
			in <- band.UpdateEvent{}
			close(in)
			persistWorker(context.Background(), discardLogger(), ep, tw, in, tc.mode, 1, nil)

			if got := tw.landedCount(); got != 2 {
				t.Fatalf("trades landed = %d, want 2 (the parallel write must still land)", got)
			}
			wantSoroswap := 0.0
			if tc.wantProjected {
				wantSoroswap = 1
			}
			if d := counter(t, obs.SourceEventsTotal, soroswap.SourceName) - soroswapBefore; d != wantSoroswap {
				t.Errorf("source_events_total{soroswap} delta = %v, want %v", d, wantSoroswap)
			}
			if d := counter(t, obs.SourceEventsTotal, sdex.SourceName) - sdexBefore; d != 1 {
				t.Errorf("source_events_total{sdex} delta = %v, want 1", d)
			}

			kill, ok := calls[aquarius.KillEvent{}.EventKind()]
			if !ok {
				t.Fatal("aquarius kill event never reached the persister")
			}
			if kill.countEvent != tc.wantProjected {
				t.Errorf("aquarius countEvent = %v, want %v", kill.countEvent, tc.wantProjected)
			}
			if got := entriesBumpReachesStore(kill.ctx); got != tc.wantProjected {
				t.Errorf("aquarius entries bump reaches source_entry_counts = %v, want %v", got, tc.wantProjected)
			}
			bandCall, ok := calls[band.UpdateEvent{}.EventKind()]
			if !ok {
				t.Fatal("band update never reached the persister")
			}
			if !bandCall.countEvent || !entriesBumpReachesStore(bandCall.ctx) {
				t.Errorf("band (dispatcher-only) countEvent = %v, entries counted = %v; want both true",
					bandCall.countEvent, entriesBumpReachesStore(bandCall.ctx))
			}
		})
	}
}
