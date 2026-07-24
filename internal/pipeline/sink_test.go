package pipeline

import (
	"context"
	"go/ast"
	"go/token"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/consumer"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/sources/band"
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
// Structural rather than behavioural because these call sites take a
// concrete *timescale.Store: the policy itself is proven behaviourally
// by the retryInfra / classifyFault tests and end-to-end by
// TestPersistEvents_DataFaultEventIsCountedAsDropped.
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
