package main

import (
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/worker/guardscan"
)

// TestBackgroundWorkersRecover is a guard-coverage test: every detached
// background goroutine started in this binary must register panic recovery.
//
// An unrecovered panic in ANY goroutine terminates the whole Go process — it
// is not confined to the goroutine that panicked. This binary spawns a large
// fleet of detached workers (baseline / supply / change-summary / rollup /
// gap-detector / mev / decimals / price-alert refreshers, plus the metrics
// HTTP listener). Before W4-cmd-1 NONE of the workers recovered — the API
// binary had recoverBackgroundWorker on every worker but the aggregator had
// zero panic isolation, so a single worker panic crash-looped the whole price
// pipeline.
//
// Why an AST guard rather than a behavioural test: the failure mode that
// actually bites is the NEXT worker someone adds without the defer. A
// behavioural test would have to drive each worker to panic through a real
// dependency and would still only cover the ones that exist today. So this
// derives the worker set from the source itself and fails if any of them lacks
// recovery. Same discipline as the API binary's TestBackgroundWorkersRecover
// and the v1 package's TestSSEProducerGoroutinesRecover (AGT-12). The shared
// guard's own recover-and-log behaviour is proven separately in
// internal/worker (TestRecover_ContainsPanicAndLogs).
//
// The walk lives in internal/worker/guardscan, shared with the indexer and
// API guards (#368 M1). Every `go` statement here is a literal today, but
// the shared scanner also resolves `go namedFunc(…)` — the spelling that
// used to be invisible to the per-binary copies of this test — and fails on
// any callee it cannot follow.
//
// The metrics HTTP-server goroutine is the one deliberate exemption: if the
// listener dies the process has no reason to live, and recovering would leave
// a running process serving nothing. It is allow-listed BY ITS CONTENT (it
// must call ListenAndServe/Serve), not by line number, so the exemption cannot
// silently widen to cover an unrelated worker.
//
// Proven red: deleting any one `defer worker.Recover(...)` fails this test
// naming the enclosing worker's line.
func TestBackgroundWorkersRecover(t *testing.T) {
	sites, err := guardscan.ScanFile("main.go", guardscan.Config{
		Guards: []string{"worker.Recover", "worker.Report"},
	})
	if err != nil {
		t.Fatalf("scan main.go: %v", err)
	}

	var checked, exempt int
	for _, s := range sites {
		if s.Kind == guardscan.KindUnresolved {
			t.Errorf("go %s at main.go:%d could not be resolved to a function body (%s). "+
				"Wrap it in `go func(){ defer worker.Recover(logger, \"<name>\"); … }()` "+
				"so the guard is visible where the goroutine starts.", s.Target, s.Line, s.Reason)
			continue
		}

		if s.Calls("ListenAndServe") || s.Calls("Serve") {
			// The metrics-listener goroutine — see the doc comment. Assert
			// it is genuinely the listener rather than trusting a position.
			exempt++
			if s.Recovers {
				t.Errorf("go func() at main.go:%d serves HTTP but registers "+
					"worker.Recover — recovering the listener leaves a live "+
					"process with nothing listening, which is worse than crashing", s.Line)
			}
			continue
		}

		checked++
		if !s.Recovers {
			t.Errorf("detached goroutine at main.go:%d (go %s, body at %s) does not "+
				"`defer worker.Recover(logger, \"<name>\")`. An unrecovered "+
				"panic in a goroutine terminates the WHOLE process, crash-looping "+
				"the aggregator over a single non-serving background worker.",
				s.Line, s.Target, s.Origin)
		}
	}

	// Guard against the guard covering nothing (e.g. the spawn idiom
	// changes and the AST match stops finding anything). 14 detached
	// workers exist as of W4-cmd-1; this is a floor, not an exact count.
	if checked < 14 {
		t.Errorf("only %d background goroutine(s) discovered, expected at least 14 — "+
			"the discovery in this test has drifted from the code and is no longer "+
			"protecting anything", checked)
	}
	if exempt != 1 {
		t.Errorf("found %d HTTP-listener goroutine(s), want exactly 1 — if the metrics "+
			"listener was restructured, re-check that the exemption still applies to it alone", exempt)
	}
}
