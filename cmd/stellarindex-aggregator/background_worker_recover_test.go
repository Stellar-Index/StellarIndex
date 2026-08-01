package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
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
// The metrics HTTP-server goroutine is the one deliberate exemption: if the
// listener dies the process has no reason to live, and recovering would leave
// a running process serving nothing. It is allow-listed BY ITS CONTENT (it
// must call ListenAndServe/Serve), not by line number, so the exemption cannot
// silently widen to cover an unrelated worker.
//
// Proven red: deleting any one `defer worker.Recover(...)` fails this test
// naming the enclosing worker's line.
func TestBackgroundWorkersRecover(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "main.go", nil, 0)
	if err != nil {
		t.Fatalf("parse main.go: %v", err)
	}

	var checked, exempt int
	ast.Inspect(f, func(n ast.Node) bool {
		g, ok := n.(*ast.GoStmt)
		if !ok {
			return true
		}
		lit, ok := g.Call.Fun.(*ast.FuncLit)
		if !ok || lit.Body == nil {
			return true
		}
		line := fset.Position(g.Pos()).Line

		if bodyServesHTTP(lit.Body) {
			// The metrics-listener goroutine — see the doc comment. Assert
			// it is genuinely the listener rather than trusting a position.
			exempt++
			if bodyRecoversWorker(lit.Body) {
				t.Errorf("go func() at main.go:%d serves HTTP but registers "+
					"worker.Recover — recovering the listener leaves a live "+
					"process with nothing listening, which is worse than crashing", line)
			}
			return true
		}

		checked++
		if !bodyRecoversWorker(lit.Body) {
			t.Errorf("detached goroutine at main.go:%d does not "+
				"`defer worker.Recover(logger, \"<name>\")`. An unrecovered "+
				"panic in a goroutine terminates the WHOLE process, crash-looping "+
				"the aggregator over a single non-serving background worker.", line)
		}
		return true
	})

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

// bodyRecoversWorker reports whether the body defers worker.Recover. It
// matches the qualified call `worker.Recover(...)` so a bare inline
// `recover()` that merely swallows the panic without logging does not satisfy
// the guard — the whole point is the Error-level log plus stack.
func bodyRecoversWorker(body *ast.BlockStmt) bool {
	found := false
	ast.Inspect(body, func(n ast.Node) bool {
		d, ok := n.(*ast.DeferStmt)
		if !ok {
			return true
		}
		sel, ok := d.Call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		pkg, ok := sel.X.(*ast.Ident)
		if ok && pkg.Name == "worker" && sel.Sel.Name == "Recover" {
			found = true
		}
		return true
	})
	return found
}

// bodyServesHTTP reports whether the body runs an http.Server's accept loop.
func bodyServesHTTP(body *ast.BlockStmt) bool {
	found := false
	ast.Inspect(body, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok || sel.Sel == nil {
			return true
		}
		if sel.Sel.Name == "ListenAndServe" || sel.Sel.Name == "Serve" {
			found = true
		}
		return true
	})
	return found
}
