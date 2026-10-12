package main

import (
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/worker/guardscan"
)

// TestBackgroundWorkersRecover is an AST guard: every detached goroutine must register panic recovery, since one
// unrecovered panic kills the process. The worker set is derived from source (internal/worker/guardscan). Only the
// metrics listener is exempt, matched by content (ListenAndServe/Serve).
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
	// workers exist today; this is a floor, not an exact count.
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
