package main

import (
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/worker/guardscan"
)

// TestBackgroundWorkersRecover is a guard-coverage test: every detached background goroutine must register panic
// recovery or be a documented, content-checked CRASH site (the sink and metrics listener, where recovering would leave a
// healthy-looking process persisting nothing). Deleting a `defer worker.Recover(...)` fails it.
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
	// goroutines exist today; this is a floor, not an exact count.
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

// TestLedgerstreamProducerPanicStaysFatal pins the one goroutine that recovers WITHOUT becoming
// survivable.
//
// ledgerstream calls the producer closure synchronously, so a walk panic arrives on this stack.
// Recovering in place would be the worst outcome: nothing would send on streamErr, main would
// block until SIGTERM and /metrics and /healthz would answer with the cursor frozen. The
// recovery only (a) moves stellarindex_worker_panics_total and logs the stack, and (b) hands
// the fault to main as a fatal error so already-buffered events drain before exit.
//
// The site must recover AND still signal on streamErr; this stops a log-and-continue rewrite.
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
