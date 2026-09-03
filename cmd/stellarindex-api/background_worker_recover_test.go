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
// An unrecovered panic in ANY goroutine terminates the whole Go process — it is
// not confined to the goroutine that panicked. This main() spawns a fleet of
// detached workers (forex poller, TLS-cert probe, four cache refreshers, two
// prewarms, stream publisher + subscriber, customer-webhook sender, usage
// rollup, three auth reapers, the ingestion-snapshot refresher and the
// self-prewarm loop) and before this guard NONE of them recovered, so a panic
// in any one took the entire API down along with every healthy request in
// flight.
//
// Why an AST guard rather than a behavioural test: the failure mode that
// actually bites is the NEXT worker someone adds without the defer. A
// behavioural test would have to drive each existing worker to panic
// through a real dependency, and would still only cover the ones that exist
// today. So this derives the worker set from the source itself and fails if any
// of them lacks recovery — find every call site of the thing being guarded, not
// a sample. Same discipline as TestSSEProducerGoroutinesRecover (AGT-12).
//
// It used to see only the `go func(){…}()` spelling. Four workers were
// started as `go namedFunc(…)` and the walk returned early on every one of
// them, so they were not merely unchecked — they did not exist as far as
// this test's own "did I find enough workers?" floor was concerned (#368
// M1). [guardscan] now resolves a named callee to its declaration (this
// package, or another package of this module, located from go.mod) and
// checks the guard there; a callee it cannot resolve fails the test rather
// than passing quietly.
//
// The http.Server goroutine is the one deliberate exemption: if the listener
// dies the process has no reason to live, and recovering would leave a running
// process serving nothing. It is allow-listed BY ITS CONTENT (it must call
// ListenAndServe/Serve), not by line number, so the exemption cannot silently
// widen to cover an unrelated worker.
//
// Proven red: deleting any one `defer recoverBackgroundWorker(...)` fails this
// test naming the enclosing worker's line.
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
	// and the AST match stops finding anything). 20 detached goroutines exist
	// as of #368 M1; this is a floor, not an exact count.
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

// TestBackgroundWorkersJoinShutdownGroup pins the other half of the
// worker contract: run() waits for its workers before returning, so the
// process does not exit on top of a worker that is mid-write. The
// concrete case is the customer-webhook sender, which can be between
// "the customer accepted the POST" and "MarkDelivered" — exiting there
// is how a delivery gets repeated on the next boot (#368 LOW).
//
// Scoped to literals started inside run(), because bgWG is declared
// there. Two content-checked exclusions:
//
//   - the listener, which httpSrv.Shutdown drains instead; and
//   - the goroutine that WAITS on the group, which cannot be a member of
//     it (it would deadlock).
//
// The three warmers started as named functions are excluded by the same
// scope rule and by design: they hold no durable write to finish, so
// waiting on them would lengthen every deploy for nothing. That is a
// judgement about those three specifically — a new worker added as a
// literal in run() must join.
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

	// Add and Done must balance. A missing Add lets the wait return
	// while that worker is still running — the defect this group exists
	// to fix, silently reintroduced; a surplus Add makes every shutdown
	// burn the full 30s budget waiting for a worker that never started.
	// Counted on the source text because Add() is written OUTSIDE the
	// goroutine body it belongs to, so no per-site walk can see the pair.
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
