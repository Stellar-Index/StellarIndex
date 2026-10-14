package pipeline

import (
	"bytes"
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"io/fs"
	"log/slog"
	"math"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/consumer"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/sources/band"
	"github.com/Stellar-Index/StellarIndex/internal/sources/reflector"
	"github.com/Stellar-Index/StellarIndex/internal/sources/sdex"
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

// TestPersistEvents_DrainsBufferedEventsOnShutdown: a cancelled ctx must still drain events already in the channel, since the cursor advances before the sink writes.
func TestPersistEvents_DrainsBufferedEventsOnShutdown(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	in := make(chan consumer.Event, 10)

	// Pre-fill the buffer so we can prove the drain reads them
	// after ctx is cancelled.
	const buffered = 10
	for i := 0; i < buffered; i++ {
		in <- fakeEvent{id: i}
	}

	// Cancel the ctx FIRST so PersistEvents enters the drain path on its first iteration; cancelling mid-iteration would
	// race `case <-ctx.Done()` against `case ev, ok := <-in` and flake.
	cancel()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	done := make(chan struct{})
	go func() {
		defer close(done)
		// Nil store: test events hit the default switch case, which never touches the store; a future dereference there surfaces as a panic.
		PersistEvents(ctx, logger, nil, in, SinkModeAll, nil)
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

// TestPersistEvents_DrainTimeoutBoundsHang sanity-checks that drainTimeout is non-zero and operationally sane (a real hang is not simulable in unit time).
func TestPersistEvents_DrainTimeoutBoundsHang(t *testing.T) {
	if drainTimeout <= 0 {
		t.Errorf("drainTimeout = %v; must be positive", drainTimeout)
	}
	if drainTimeout > 5*time.Minute {
		t.Errorf("drainTimeout = %v; > 5min defeats the bounded-shutdown invariant", drainTimeout)
	}
}

// TestDrainBudget_FitsShutdownDeadline: drain plus final pass must finish inside the process shutdown window, or the undrained-range report never fires.
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

// checkShutdownDeadlineMainUsesConstant pins by AST that the indexer's shutdown context is built from ShutdownDeadline, not a drifting literal.
func checkShutdownDeadlineMainUsesConstant(t *testing.T) {
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

// TestIndexerStopTimeout_UnitsCoverDrainBudgets: the unit's TimeoutStopSec must cover the sinks drained after ShutdownDeadline, or SIGKILL discards their buffers.
func TestIndexerStopTimeout_UnitsCoverDrainBudgets(t *testing.T) {
	sum := ShutdownDeadline + CHLiveSinkStopBudget + RawEventDrainBudget + DiscoveryDrainBudget
	if IndexerStopTimeout <= sum {
		t.Errorf("IndexerStopTimeout %v leaves no margin over the summed drain budgets %v", IndexerStopTimeout, sum)
	}
	for _, unit := range []string{
		repoDir("deploy", "systemd", "stellarindex-indexer.service"),
		repoDir("configs", "ansible", "roles", "archival-node", "templates", "systemd", "stellarindex-indexer.service.j2"),
	} {
		got, ok := unitTimeoutStopSec(t, unit)
		if !ok {
			t.Errorf("%s sets no TimeoutStopSec — systemd's 90s default is below IndexerStopTimeout %v and SIGKILLs mid-drain", unit, IndexerStopTimeout)
			continue
		}
		if got < IndexerStopTimeout {
			t.Errorf("%s TimeoutStopSec=%v < IndexerStopTimeout %v — SIGKILL lands mid-drain", unit, got, IndexerStopTimeout)
		}
	}
}

// unitTimeoutStopSec returns the unit's TimeoutStopSec. Only the
// seconds/minutes spellings are accepted; anything else fails the test
// rather than being guessed at.
func unitTimeoutStopSec(t *testing.T, path string) (time.Duration, bool) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	for _, line := range strings.Split(string(raw), "\n") {
		v, ok := strings.CutPrefix(strings.TrimSpace(line), "TimeoutStopSec=")
		if !ok {
			continue
		}
		v = strings.TrimSpace(v)
		if n, err := strconv.Atoi(v); err == nil {
			return time.Duration(n) * time.Second, true
		}
		d, err := time.ParseDuration(strings.Replace(v, "min", "m", 1))
		if err != nil {
			t.Fatalf("%s: unparseable TimeoutStopSec=%q: %v", path, v, err)
		}
		return d, true
	}
	return 0, false
}

// checkShutdownBudgetsMainWiresConstants: the budget sum only bounds the real drains if main passes each budget into
// its sink. A literal, or inheriting the sink default, lets them drift apart (e.g. a 90s sink drain inside a 30s
// process).
func checkShutdownBudgetsMainWiresConstants(t *testing.T) {
	fset := token.NewFileSet()
	main := parseFile(t, fset, repoDir("cmd", "stellarindex-indexer", "main.go"))
	want := map[string]string{
		"DrainTimeout": "DiscoveryDrainBudget",
		"DrainGrace":   "RawEventDrainBudget",
		"StopTimeout":  "CHLiveSinkStopBudget",
	}
	found := map[string]bool{}
	ast.Inspect(main, func(n ast.Node) bool {
		kv, ok := n.(*ast.KeyValueExpr)
		if !ok {
			return true
		}
		key, ok := kv.Key.(*ast.Ident)
		if !ok {
			return true
		}
		sel, ok := kv.Value.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "pipeline" && want[key.Name] == sel.Sel.Name {
			found[key.Name] = true
		}
		return true
	})
	for field, constant := range want {
		if !found[field] {
			t.Errorf("cmd/stellarindex-indexer/main.go does not set %s: pipeline.%s — the unit's TimeoutStopSec is sized from that constant, so the sink's real drain is unbounded by it", field, constant)
		}
	}
}

// checkSinkDrainNonTradeWritesAreResilient: drain sites must call persistEventResilient, never `_ = HandleEvent(...)`, which would drop rows while the cursor advanced.
// Structural (a whole-package wiring invariant); only the handleEvent binders in sinkEventBinders may call it directly.
func checkSinkDrainNonTradeWritesAreResilient(t *testing.T) {
	resilient := map[string]int{}
	for _, decl := range packageFuncDecls(t) {
		name := decl.Name.Name
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
				if !sinkEventBinders[name] {
					t.Errorf("%s calls %s directly — its error handling bypasses persistEventResilient, so a Postgres infrastructure fault drops the event while the cursor advances (REL-08). Route it through persistEventResilient", name, id.Name)
				}
			case "persistEventResilient":
				resilient[name]++
			}
			return true
		})
	}
	// Shutdown-drain canaries: the scan must reach these drains too, not
	// only persistWorker and drainBufferedEvents.
	for _, fn := range []string{"persistWorker", "drainBufferedEvents", "drainInFlightNow", "persistCarried", "drainFinalPass"} {
		if resilient[fn] == 0 {
			t.Errorf("drain site %s has no persistEventResilient call — non-trade served-tier writes are unprotected on that path (REL-08), or the scan no longer reaches it", fn)
		}
	}
}

// sinkEventBinders are the only functions that may call handleEvent
// directly: the exported wrapper and the eventPersister every drain uses.
var sinkEventBinders = map[string]bool{"HandleEvent": true, "storeEventPersister": true}

// packageFuncDecls parses every non-test source in this package and
// returns its top-level functions and methods.
func packageFuncDecls(t *testing.T) []*ast.FuncDecl {
	t.Helper()
	pkgs, err := parser.ParseDir(token.NewFileSet(), ".", func(fi fs.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatalf("parse package: %v", err)
	}
	var out []*ast.FuncDecl
	found := false
	for _, pkg := range pkgs {
		for _, f := range pkg.Files {
			for _, d := range f.Decls {
				if fd, ok := d.(*ast.FuncDecl); ok {
					out = append(out, fd)
					found = found || fd.Name.Name == "persistEventResilient"
				}
			}
		}
	}
	if !found {
		t.Fatal("package scan found no persistEventResilient — it would pass vacuously")
	}
	return out
}

// TestPersistEvents_DataFaultEventIsCountedAsDropped: a permanently invalid oracle update is isolated and counted as a drop, not retried forever or silently discarded.
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
		PersistEvents(context.Background(), slog.New(slog.NewTextHandler(io.Discard, nil)), nil, in, SinkModeAll, nil)
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
		PersistEvents(ctx, logger, nil, in, SinkModeAll, nil)
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

// TestDrainFinalPass_SkipInSinkExcludedFromReport: projector-owned events skipped by skipInSink must not inflate undrained counts or widen the re-derive range.
// Calls drainFinalPass directly since the outer select race cannot be made deterministic.
func TestDrainFinalPass_SkipInSinkExcludedFromReport(t *testing.T) {
	in := make(chan consumer.Event, 2)
	// Projector-owned under SinkModeSkipProjected (see
	// IsProjectedEvent) — must never count toward the report.
	in <- reflector.UpdateEvent{}
	// Not projector-owned, and a guaranteed permanent data fault without touching the DB (OracleUpdate.Validate rejects a
	// bare Source), the same trick as TestPersistEvents_DataFaultEventIsCountedAsDropped; safe with a nil store.
	in <- band.UpdateEvent{Update: canonical.OracleUpdate{Source: band.SourceName}}
	close(in)

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))

	drainFinalPass(in, logger, storeEventPersister(logger, nil), nil, SinkModeSkipProjected, nil)

	out := buf.String()
	if !strings.Contains(out, "undrained_events=1") {
		t.Errorf("log output = %q; want undrained_events=1 — the skipInSink event must be excluded from the count", out)
	}
	if strings.Contains(out, "undrained_events=2") {
		t.Errorf("log output = %q; the skipped projector-owned event inflated undrained_events to 2", out)
	}
}

// TestDrainBufferedEvents_UndrainedRowsAreCounted: rows a bounded drain cannot land advance the undrained-rows counter by exact row count, per kind.
// Both fakes return the ctx error and the channel is closed, so the count is deterministic without waiting out drainTimeout.
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

	lt := &lossTracker{}
	drainBufferedEvents(in, discardLogger(), ep, store, SinkModeSkipProjected, time.Now().Add(time.Minute), lt)

	if n := store.landedCount(); n != 0 {
		t.Fatalf("landed %d trades through a store that refuses every write; want 0", n)
	}
	// The loss PersistEvents hands back is what backfill caps its resume
	// cursor on; the ledger-less events make any cap unsafe, so say so.
	if got, want := lt.snapshot(), (ShutdownLoss{Rows: trades + events, MinLedger: 700, LedgerUnknown: true}); got != want {
		t.Errorf("ShutdownLoss = %+v; want %+v", got, want)
	}
	if got := counter(t, obs.SinkUndrainedRowsTotal, obs.SinkPersistEvents, "trade") - tradesBefore; got != trades {
		t.Errorf("sink_undrained_rows{persist_events,trade} delta = %v; want %d (one per abandoned trade row)", got, trades)
	}
	if got := counter(t, obs.SinkUndrainedRowsTotal, obs.SinkPersistEvents, "event") - eventsBefore; got != events {
		t.Errorf("sink_undrained_rows{persist_events,event} delta = %v; want %d (one per abandoned non-trade event)", got, events)
	}
}

// TestDrainBufferedEvents_CleanDrainCountsNoUndrainedRows is the non-vacuity half: a clean drain leaves the loss counter at zero.
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

	lt := &lossTracker{}
	drainBufferedEvents(in, discardLogger(), ep, store, SinkModeSkipProjected, time.Now().Add(time.Minute), lt)

	if n := store.landedCount(); n != 2 {
		t.Fatalf("landed %d trades; want 2 — the fixture's clean path did not run, so the zero-delta below would be vacuous", n)
	}
	if got := lt.snapshot(); got != (ShutdownLoss{}) {
		t.Errorf("ShutdownLoss = %+v on a clean drain; want zero", got)
	}
	if got := counter(t, obs.SinkUndrainedRowsTotal, obs.SinkPersistEvents, "trade") - tradesBefore; got != 0 {
		t.Errorf("sink_undrained_rows{persist_events,trade} delta = %v on a clean drain; want 0", got)
	}
	if got := counter(t, obs.SinkUndrainedRowsTotal, obs.SinkPersistEvents, "event") - eventsBefore; got != 0 {
		t.Errorf("sink_undrained_rows{persist_events,event} delta = %v on a clean drain; want 0", got)
	}
}

// TestShutdownSafeCtx_LiveCtxPassedThroughUnchanged pins the live-context half: a still-live parent context is
// returned as-is, not wrapped, so the normal path through persistWorker is unaffected.
func TestShutdownSafeCtx_LiveCtxPassedThroughUnchanged(t *testing.T) {
	ctx := context.Background()
	got, cancel := shutdownSafeCtx(ctx, &drainDeadline{})
	defer cancel()
	if got != ctx {
		t.Errorf("shutdownSafeCtx(live ctx) returned a different context; want the same ctx passed through unchanged")
	}
}

// TestShutdownSafeCtx_CancelledParentGetsFreshBoundedCtx: select has no priority, so a cancelled parent must yield a different, live ctx with a deadline
// about drainTimeout out, rather than failing every write instantly.
func TestShutdownSafeCtx_CancelledParentGetsFreshBoundedCtx(t *testing.T) {
	parent, parentCancel := context.WithCancel(context.Background())
	parentCancel() // simulate the racy-select window: ctx already done

	got, cancel := shutdownSafeCtx(parent, &drainDeadline{})
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

// TestBlendEmitterUnlockTime_overflowSentinelRejected: an UnlockTime near MaxUint64 must not wrap negative through an int64 cast into a bogus 1970 time.
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

// TestRecordOracleMetricIfMapped_SkipsUnmappedRawAsset: a raw:<symbol> row must not mint a gauge label (unbounded cardinality); mapped assets pass through.
func TestRecordOracleMetricIfMapped_SkipsUnmappedRawAsset(t *testing.T) {
	const source = "q099-test-source"

	raw, err := canonical.NewOracleRawAsset("Q099UNMAPPEDTESTSYMBOL")
	if err != nil {
		t.Fatalf("NewOracleRawAsset: %v", err)
	}
	mapped := canonical.NativeAsset()

	seriesBefore := testutil.CollectAndCount(obs.OracleLastUpdateUnix)

	recordOracleMetricIfMapped(source, raw, 111)
	if got := testutil.CollectAndCount(obs.OracleLastUpdateUnix); got != seriesBefore {
		t.Fatalf("raw asset %s: OracleLastUpdateUnix series count = %d, want unchanged at %d (raw rows must not acquire a label)",
			raw, got, seriesBefore)
	}

	recordOracleMetricIfMapped(source, mapped, 222)
	if got := testutil.CollectAndCount(obs.OracleLastUpdateUnix); got != seriesBefore+1 {
		t.Fatalf("mapped asset %s: OracleLastUpdateUnix series count = %d, want %d", mapped, got, seriesBefore+1)
	}
	if got := testutil.ToFloat64(obs.OracleLastUpdateUnix.WithLabelValues(source, mapped.String())); got != 222 {
		t.Errorf("mapped asset OracleLastUpdateUnix = %v, want 222", got)
	}
}

// checkPersistEventsGodocMatchesConcurrencyModel keeps the stale "One goroutine drains" language out of PersistEvents' godoc, which launches PersistWorkers (8) concurrent drains.
func checkPersistEventsGodocMatchesConcurrencyModel(t *testing.T) {
	src, err := os.ReadFile("sink.go")
	if err != nil {
		t.Fatalf("read sink.go: %v", err)
	}
	text := string(src)

	stale := []string{
		"One goroutine drains; per-event work is sequential",
		"a small worker pool of 4",
	}
	for _, s := range stale {
		if strings.Contains(text, s) {
			t.Errorf("sink.go godoc still contains stale single-goroutine claim %q, contradicting PersistWorkers = %d", s, PersistWorkers)
		}
	}

	if PersistWorkers != 8 {
		t.Fatalf("PersistWorkers changed to %d; update this test's expectations", PersistWorkers)
	}
	if !strings.Contains(text, "launches [PersistWorkers] concurrent drain") {
		t.Error("sink.go godoc no longer documents PersistEvents as launching PersistWorkers concurrent drain goroutines")
	}
}

// TestSourceShapeRules pins the sink's source shapes that no behavioural test can reach: each row parses the
// package or the indexer main with go/ast and fails on the same defect it did as a standalone test.
func TestSourceShapeRules(t *testing.T) {
	rules := []struct {
		name string
		run  func(t *testing.T)
	}{
		{"shutdown deadline: main uses the constant", checkShutdownDeadlineMainUsesConstant},
		{"shutdown budgets: main wires the constants", checkShutdownBudgetsMainWiresConstants},
		{"sink drain: non-trade writes are resilient", checkSinkDrainNonTradeWritesAreResilient},
		{"PersistEvents godoc matches the concurrency model", checkPersistEventsGodocMatchesConcurrencyModel},
	}
	for _, r := range rules {
		t.Run(r.name, r.run)
	}
}
