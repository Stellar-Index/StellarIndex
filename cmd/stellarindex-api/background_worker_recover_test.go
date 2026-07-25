package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"
)

// TestBackgroundWorkersRecover is a guard-coverage test: every detached
// background goroutine started in this binary must register panic recovery.
//
// An unrecovered panic in ANY goroutine terminates the whole Go process — it is
// not confined to the goroutine that panicked. This main() spawns ten detached
// workers (forex poller, TLS-cert probe, two cache refreshers, the supply/wealth
// prewarm, stream publisher, customer-webhook sender, usage rollup, signup
// reaper, and the http.Server) and before this guard NONE of them recovered, so
// a panic in any one took the entire API down along with every healthy request
// in flight.
//
// Why an AST guard rather than a behavioural test: the failure mode that
// actually bites is the ELEVENTH worker someone adds next year without the
// defer. A behavioural test would have to drive each existing worker to panic
// through a real dependency, and would still only cover the ones that exist
// today. So this derives the worker set from the source itself and fails if any
// of them lacks recovery — find every call site of the thing being guarded, not
// a sample. Same discipline as TestSSEProducerGoroutinesRecover (AGT-12).
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
			// The listener goroutine — see the doc comment. Assert it is
			// genuinely the listener rather than trusting a position.
			exempt++
			if bodyRecoversBackgroundWorker(lit.Body) {
				t.Errorf("go func() at main.go:%d serves HTTP but registers "+
					"recoverBackgroundWorker — recovering the listener leaves a live "+
					"process with nothing listening, which is worse than crashing", line)
			}
			return true
		}

		checked++
		if !bodyRecoversBackgroundWorker(lit.Body) {
			t.Errorf("detached goroutine at main.go:%d does not "+
				"`defer recoverBackgroundWorker(logger, \"<name>\")`. An unrecovered "+
				"panic in a goroutine terminates the WHOLE process, taking the API "+
				"down over a non-serving background worker.", line)
		}
		return true
	})

	// Guard against the guard covering nothing (e.g. the spawn idiom changes
	// and the AST match stops finding anything).
	if checked < 9 {
		t.Errorf("only %d background goroutine(s) discovered, expected at least 9 — "+
			"the discovery in this test has drifted from the code and is no longer "+
			"protecting anything", checked)
	}
	if exempt != 1 {
		t.Errorf("found %d HTTP-listener goroutine(s), want exactly 1 — if the listener "+
			"was restructured, re-check that the exemption still applies to it alone", exempt)
	}
}

// bodyRecoversBackgroundWorker reports whether the body defers
// recoverBackgroundWorker. It matches the call by name so an inline
// `recover()` that merely swallows the panic without logging does not satisfy
// the guard — the whole point is the Error-level log plus stack.
func bodyRecoversBackgroundWorker(body *ast.BlockStmt) bool {
	found := false
	ast.Inspect(body, func(n ast.Node) bool {
		d, ok := n.(*ast.DeferStmt)
		if !ok {
			return true
		}
		if ident, ok := d.Call.Fun.(*ast.Ident); ok && ident.Name == "recoverBackgroundWorker" {
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
		if strings.HasPrefix(sel.Sel.Name, "ListenAndServe") || sel.Sel.Name == "Serve" {
			found = true
		}
		return true
	})
	return found
}
