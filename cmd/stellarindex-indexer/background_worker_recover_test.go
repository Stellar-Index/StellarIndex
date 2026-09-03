package main

import (
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/worker/guardscan"
)

// TestBackgroundWorkersRecover is a guard-coverage test: every detached
// background goroutine in this binary must either register panic recovery
// or be a documented, content-checked CRASH site.
//
// An unrecovered panic in ANY goroutine terminates the whole Go process —
// it is not confined to the goroutine that panicked. Before #368 M4 the
// indexer had twelve `go` statements and zero recoveries, so a fault in
// the decoder-stats flusher, the hashdb drift verifier, a metrics watcher
// or either attribution tagger stopped ingestion for the whole network.
// Worse, most of those goroutines carry `defer close(done)`, so the
// channel main waits on still closed and the shutdown path read a
// panicked worker as a cleanly-finished one — an invisible death.
//
// The guard is deliberately NOT uniform, and the exemptions are the
// interesting part. Two goroutines are load-bearing enough that recovering
// them would leave a process that looks healthy while persisting nothing,
// which is strictly worse than exiting; the reasons live next to each one
// in main.go and are summarised here:
//
//   - the SINK (pipeline.PersistEvents): a guard there is cosmetic — the
//     writes happen in child goroutines recover() cannot reach — and if
//     the frame did unwind, nothing would read `events` again while the
//     process kept serving /metrics with a frozen cursor.
//   - the METRICS LISTENER (ListenAndServe): net/http already isolates
//     per-request panics, so a panic here is an accept-loop fault, and
//     the guard's only output is a counter read over the endpoint that
//     just died.
//
// The ledgerstream PRODUCER is a third case and is checked separately
// below: it recovers, but only to convert the panic into the binary's
// existing fatal-error path so the sink drain runs first.
//
// Proven red: deleting any one `defer worker.Recover(...)` fails this test
// naming the goroutine's line.
func TestBackgroundWorkersRecover(t *testing.T) {
	sites, err := guardscan.ScanFile("main.go", guardscan.Config{
		Guards: []string{"worker.Recover", "worker.Report"},
	})
	if err != nil {
		t.Fatalf("scan main.go: %v", err)
	}

	var checked, exemptListener, exemptSink int
	for _, s := range sites {
		if s.Kind == guardscan.KindUnresolved {
			t.Errorf("go %s at main.go:%d could not be resolved to a function body (%s). "+
				"Wrap it in `go func(){ defer worker.Recover(logger, \"<name>\"); … }()` "+
				"so the guard is visible where the goroutine starts.", s.Target, s.Line, s.Reason)
			continue
		}

		// Content-checked exemptions. Each asserts what the body actually
		// does, so an exemption cannot drift onto an unrelated worker.
		if s.Calls("ListenAndServe") || s.Calls("Serve") {
			exemptListener++
			if s.Recovers {
				t.Errorf("go func() at main.go:%d serves HTTP but registers a worker guard — "+
					"recovering the accept loop leaves a live process with nothing on "+
					"/metrics, and the guard's own counter is read over that endpoint", s.Line)
			}
			continue
		}
		if s.Calls("pipeline.PersistEvents") {
			exemptSink++
			if s.Recovers {
				t.Errorf("the sink goroutine at main.go:%d registers a worker guard. It must not: "+
					"the guard cannot reach the persist workers PersistEvents fans out, and if "+
					"this frame unwinds nothing drains `events` while the process keeps "+
					"reporting healthy", s.Line)
			}
			continue
		}

		checked++
		if !s.Recovers {
			t.Errorf("detached goroutine at main.go:%d (go %s, body at %s) does not defer "+
				"worker.Recover/worker.Report. An unrecovered panic terminates the WHOLE "+
				"indexer — and because these goroutines close their done channel on the way "+
				"out, main reads the death as a clean finish.", s.Line, s.Target, s.Origin)
		}
	}

	// Guard against the guard covering nothing (e.g. the spawn idiom
	// changes and the AST match stops finding anything). 11 guarded
	// goroutines exist as of #368 M4; this is a floor, not an exact count.
	if checked < 11 {
		t.Errorf("only %d guarded goroutine(s) discovered, expected at least 11 — "+
			"the discovery in this test has drifted from the code and is no longer "+
			"protecting anything", checked)
	}
	if exemptListener != 1 {
		t.Errorf("found %d metrics-listener goroutine(s), want exactly 1", exemptListener)
	}
	if exemptSink != 1 {
		t.Errorf("found %d sink goroutine(s), want exactly 1 — if PersistEvents moved, "+
			"re-derive whether the crash-by-design argument still holds", exemptSink)
	}
}

// TestLedgerstreamProducerPanicStaysFatal pins the one goroutine that
// recovers WITHOUT becoming survivable.
//
// The producer drives ingestion: ledgerstream calls its closure
// synchronously, so a panic in the walk arrives on this stack. Recovering
// it in place would be the worst outcome available — nothing would send on
// streamErr, main would block in its select until SIGTERM, and the process
// would keep answering /metrics and /healthz with the cursor frozen. So
// the recovery exists only to (a) move stellarindex_worker_panics_total
// and log the stack, and (b) hand the fault to main as a fatal error, so
// the up-to-256 already-buffered events are drained before exit rather
// than discarded — the same hole #368 M2 closed on the error path.
//
// This test is what stops a later "simplification" from turning that into
// an ordinary log-and-continue guard: the site must recover AND still
// signal on streamErr.
func TestLedgerstreamProducerPanicStaysFatal(t *testing.T) {
	sites, err := guardscan.ScanFile("main.go", guardscan.Config{
		Guards: []string{"worker.Recover", "worker.Report"},
	})
	if err != nil {
		t.Fatalf("scan main.go: %v", err)
	}

	found := 0
	for _, s := range sites {
		if !s.Calls("ledgerstream.StreamArchiveThenLive") {
			continue
		}
		found++
		if !s.Recovers {
			t.Errorf("the ledgerstream producer at main.go:%d does not recover its panic. "+
				"A panic in a non-main goroutine skips main's defers, so the buffered "+
				"events for already-cursored ledgers are lost and nothing counts the fault.", s.Line)
		}
		if s.Guard != "worker.Report" {
			t.Errorf("the ledgerstream producer at main.go:%d guards with %q. It must use "+
				"worker.Report: worker.Recover swallows the panic and returns, which would "+
				"leave streamErr silent forever and the process alive with a frozen cursor.",
				s.Line, s.Guard)
		}
		if !s.SendsOn("streamErr") {
			t.Errorf("the ledgerstream producer at main.go:%d recovers but never sends on "+
				"streamErr. The panic must stay FATAL — recovery here is only for draining "+
				"and counting on the way out, not for surviving.", s.Line)
		}
	}
	if found != 1 {
		t.Fatalf("found %d ledgerstream producer goroutine(s), want exactly 1", found)
	}
}
