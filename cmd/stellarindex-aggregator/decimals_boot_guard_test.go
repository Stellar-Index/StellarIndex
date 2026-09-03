package main

// Source-level tripwire (the freeze_wiring_guard_test.go /
// internal/api/v1/slo_guard_test.go pattern): the decimals cache's
// initial, BOOT-time refresh must be fatal.
//
// #368 M9. The cache is fail-open, and that is right for a periodic
// refresh: a Postgres blip leaves the last-good snapshot in place and
// one newly-confirmed offender's normalization phases in late. At BOOT
// there is no last-good snapshot, so the same fail-open policy makes
// Lookup answer "nothing is flagged" for EVERY confirmed non-7-decimals
// asset, and the orchestrator publishes each of their windows
// unnormalized — wrong by 10^(7-decimals) — into prices_1m and every
// price surface downstream, at one Warn line per minute. A published
// price that is wrong by orders of magnitude is not degraded service; it
// is a false statement about the market, and it persists in the
// continuous aggregates long after the outage ends.
//
// The fix is one line, which is exactly why it needs a tripwire: turning
// `return err` back into `logger.Warn(...)` looks like a resilience
// improvement to anyone who does not know the cold-cache case.
//
// Behavioural coverage is not available here — the branch lives inside
// run(), which needs a config file, a Postgres pool and a Redis client
// before it reaches this line — so this asserts on the wiring itself.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

func TestDecimalsCacheInitialRefreshIsFatal(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "main.go", nil, 0)
	if err != nil {
		t.Fatalf("parse main.go: %v", err)
	}

	found := 0
	ast.Inspect(f, func(n ast.Node) bool {
		ifStmt, ok := n.(*ast.IfStmt)
		if !ok || ifStmt.Init == nil {
			return true
		}
		// Match `if err := decimalsLookup.Refresh(ctx); err != nil {`.
		assign, ok := ifStmt.Init.(*ast.AssignStmt)
		if !ok || len(assign.Rhs) != 1 {
			return true
		}
		call, ok := assign.Rhs[0].(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel == nil || sel.Sel.Name != "Refresh" {
			return true
		}
		recv, ok := sel.X.(*ast.Ident)
		if !ok || recv.Name != "decimalsLookup" {
			return true
		}

		found++
		line := fset.Position(ifStmt.Pos()).Line
		if !bodyReturns(ifStmt.Body) {
			t.Errorf("main.go:%d — the decimals cache's initial refresh no longer aborts startup "+
				"on failure. With an empty snapshot the orchestrator publishes every "+
				"non-7-decimals leg's VWAP unnormalized (wrong by 10^(7-decimals)) into "+
				"prices_1m and every surface reading it. Refusing to start is the "+
				"recoverable failure; publishing wrong prices is not.", line)
		}
		return true
	})

	if found != 1 {
		t.Fatalf("found %d boot-time decimalsLookup.Refresh call(s), want exactly 1 — "+
			"if the wiring moved, re-derive whether the cold-snapshot hazard is still handled", found)
	}
}

// bodyReturns reports whether a block returns from the enclosing
// function at its own statement level (not from a nested literal).
func bodyReturns(body *ast.BlockStmt) bool {
	for _, stmt := range body.List {
		if _, ok := stmt.(*ast.ReturnStmt); ok {
			return true
		}
	}
	return false
}
