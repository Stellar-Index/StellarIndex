package main

import (
	"os"
	"strings"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/worker/guardscan"
)

// TestBackgroundWorkersRecover is a guard-coverage test: every detached
// background goroutine started in this binary must register panic recovery.
//
// An unrecovered panic in any goroutine terminates the whole process, and main() spawns many
// detached workers. An AST walk, not a behavioural test, derives the worker set from the source
// so the next worker added without the defer fails too. The walk covers `go namedFunc(…)` as
// well as `go func(){…}()`; [guardscan] resolves a named callee and fails on one it cannot
// resolve rather than passing quietly.
//
// The http.Server goroutine is the one exemption, matched by content (it must call
// ListenAndServe/Serve), not by line: if the listener dies the process should die with it.
func TestBackgroundWorkersRecover(t *testing.T) {
	sites, err := guardscan.ScanFile("main.go", guardscan.Config{
		Guards: []string{"recoverBackgroundWorker"},
	})
	if err != nil {
		t.Fatalf("scan main.go: %v", err)
	}

	var checked, exempt int
	for _, s := range sites {
		if s.Kind == guardscan.KindUnresolved {
			t.Errorf("go %s at main.go:%d could not be resolved to a function body (%s). "+
				"Wrap it in `go func(){ defer recoverBackgroundWorker(logger, \"<name>\"); … }()` "+
				"so the guard is visible where the goroutine starts.", s.Target, s.Line, s.Reason)
			continue
		}

		if s.Calls("ListenAndServe") || s.Calls("Serve") {
			// The listener goroutine — see the doc comment. Assert it is
			// genuinely the listener rather than trusting a position.
			exempt++
			if s.Recovers {
				t.Errorf("go func() at main.go:%d serves HTTP but registers "+
					"recoverBackgroundWorker — recovering the listener leaves a live "+
					"process with nothing listening, which is worse than crashing", s.Line)
			}
			continue
		}

		checked++
		if !s.Recovers {
			t.Errorf("detached goroutine at main.go:%d (go %s, body at %s) does not "+
				"`defer recoverBackgroundWorker(logger, \"<name>\")`. An unrecovered "+
				"panic in a goroutine terminates the WHOLE process, taking the API "+
				"down over a non-serving background worker.", s.Line, s.Target, s.Origin)
		}
	}

	// Guard against the guard covering nothing (e.g. the spawn idiom changes
	// and the AST match stops finding anything). 20 detached goroutines exist;
	// this is a floor, not an exact count.
	if checked < 20 {
		t.Errorf("only %d background goroutine(s) discovered, expected at least 20 — "+
			"the discovery in this test has drifted from the code and is no longer "+
			"protecting anything", checked)
	}
	if exempt != 1 {
		t.Errorf("found %d HTTP-listener goroutine(s), want exactly 1 — if the listener "+
			"was restructured, re-check that the exemption still applies to it alone", exempt)
	}
}

// TestBackgroundWorkersJoinShutdownGroup pins that run() waits for its workers before returning,
// so the process does not exit mid-write (the webhook sender between the customer's accept and
// MarkDelivered would repeat a delivery on the next boot).
//
// Scoped to literals started inside run(), where bgWG is declared. Excluded by content: the
// listener (httpSrv.Shutdown drains it) and the goroutine that waits on the group (it would
// deadlock). The three named-function warmers hold no durable write, so waiting would only slow
// deploys.
func TestBackgroundWorkersJoinShutdownGroup(t *testing.T) {
	sites, err := guardscan.ScanFile("main.go", guardscan.Config{
		Guards: []string{"recoverBackgroundWorker"},
	})
	if err != nil {
		t.Fatalf("scan main.go: %v", err)
	}

	joined := 0
	for _, s := range sites {
		if s.Kind != guardscan.KindFuncLit || s.Enclosing != "run" {
			continue
		}
		if s.Calls("ListenAndServe") || s.Calls("Serve") || s.Calls("bgWG.Wait") {
			continue
		}
		if !s.DefersCall("bgWG.Done") {
			t.Errorf("detached worker at main.go:%d does not `defer bgWG.Done()` (and run() "+
				"does not bgWG.Add(1) for it). Shutdown would return while it is still "+
				"running, which for any worker holding a durable write means the write is "+
				"abandoned mid-flight.", s.Line)
			continue
		}
		joined++
	}
	if joined < 15 {
		t.Errorf("only %d worker(s) join the shutdown WaitGroup, expected at least 15 — "+
			"the discovery has drifted and this test is no longer protecting anything", joined)
	}

	// Add and Done must balance: a missing Add lets the wait return while a worker still runs, and
	// a surplus Add burns the full 30s shutdown budget. Counted on the source text because Add()
	// is written outside the goroutine body it belongs to.
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	adds := strings.Count(string(src), "bgWG.Add(1)")
	dones := strings.Count(string(src), "defer bgWG.Done()")
	if adds != dones {
		t.Errorf("bgWG.Add(1) appears %d time(s) but `defer bgWG.Done()` %d — "+
			"an imbalance either defeats the shutdown wait or hangs it for the "+
			"whole budget", adds, dones)
	}
}
