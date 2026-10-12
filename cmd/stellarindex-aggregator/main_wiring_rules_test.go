// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"
)

// Wiring in main.go that no behavioural test can reach (run() needs live
// dependencies) is asserted on its syntax tree. One row per rule.
func TestMainWiringRules(t *testing.T) {
	for _, r := range []struct {
		name  string
		check func(t *testing.T)
	}{
		{name: "decimals_refresh_is_fatal", check: ruleDecimalsRefreshIsFatal},
		{name: "supply_constructors_before_dry_run_return", check: ruleSupplyConstructorsBeforeDryRunReturn},
		{name: "decimals_heartbeat_seeded_before_dial", check: ruleDecimalsHeartbeatSeededBeforeDial},
	} {
		t.Run(r.name, r.check)
	}
}

// Source-level tripwire: the decimals cache's BOOT-time refresh must be fatal. With no last-good snapshot, fail-open
// makes Lookup flag nothing and windows publish wrong by 10^(7-decimals) into prices_1m, persisting in the aggregates.
// `return err` to Warn looks like resilience; the branch lives in run(), so the wiring is asserted.

func ruleDecimalsRefreshIsFatal(t *testing.T) {
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

// Source-level tripwire: changesummary.New, the ClickHouse close-time reader and the supply / cross-check refresher
// builders must be constructed before run()'s dry-run early-return, so -dry-run catches a config that fails one.
// Asserted on the wiring because the calls live inside run().

func ruleSupplyConstructorsBeforeDryRunReturn(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "main.go", nil, 0)
	if err != nil {
		t.Fatalf("parse main.go: %v", err)
	}

	runFn := findRunFuncDecl(f)
	if runFn == nil || runFn.Body == nil {
		t.Fatal("could not locate func run(...) in main.go — update this guard to follow the refactor rather than deleting it")
	}

	dryRunReturnPos := findDryRunReturnPos(runFn.Body)
	if dryRunReturnPos == token.NoPos {
		t.Fatal("could not locate the dry-run early-return `if dryRun { ... }` in run() — " +
			"update this guard to follow the refactor rather than deleting it")
	}

	wantNames := []string{
		"changesummary.New",
		"buildSupplyRefreshers",
		"buildCrossCheckRefresher",
	}
	seen := make(map[string]bool, len(wantNames)+1)
	ast.Inspect(runFn.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		name := callName(call)
		tracked := false
		for _, want := range wantNames {
			if want == name {
				tracked = true
				break
			}
		}
		if !tracked {
			return true
		}
		if call.Pos() >= dryRunReturnPos {
			t.Errorf("%s is constructed AFTER run()'s dry-run early-return — a config that fails "+
				"it will pass -dry-run and only fail at a real start (T184)", name)
		}
		seen[name] = true
		return true
	})
	for _, want := range wantNames {
		if !seen[want] {
			t.Errorf("did not find a call to %s in run() — update this guard to follow the refactor rather than deleting it", want)
		}
	}

	// clickhouse.NewExplorerReader is called from several constructors in run(); only the one feeding the supply refresh
	// path is in scope, identified by its assignment to `supplyCloseTimes`.
	supplyCloseTimesPos := token.NoPos
	ast.Inspect(runFn.Body, func(n ast.Node) bool {
		assign, ok := n.(*ast.AssignStmt)
		if !ok || assign.Tok != token.DEFINE || len(assign.Lhs) == 0 {
			return true
		}
		lhs, ok := assign.Lhs[0].(*ast.Ident)
		if !ok || lhs.Name != "supplyCloseTimes" {
			return true
		}
		supplyCloseTimesPos = assign.Pos()
		return false
	})
	if supplyCloseTimesPos == token.NoPos {
		t.Fatal("did not find `supplyCloseTimes := clickhouse.NewExplorerReader(...)` in run() — " +
			"update this guard to follow the refactor rather than deleting it")
	}
	if supplyCloseTimesPos >= dryRunReturnPos {
		t.Error("the supply-refresh ClickHouse close-time reader (supplyCloseTimes) is constructed " +
			"AFTER run()'s dry-run early-return — a config that fails it will pass -dry-run and only " +
			"fail at a real start (T184)")
	}
}

// findDryRunReturnPos locates the `if dryRun { ...; return nil }` statement, matched on the dry-run-complete log line,
// since run() also branches on dryRun earlier (the redis ping) without exiting.
func findDryRunReturnPos(body *ast.BlockStmt) token.Pos {
	pos := token.NoPos
	ast.Inspect(body, func(n ast.Node) bool {
		ifStmt, ok := n.(*ast.IfStmt)
		if !ok || ifStmt.Init != nil {
			return true
		}
		cond, ok := ifStmt.Cond.(*ast.Ident)
		if !ok || cond.Name != "dryRun" {
			return true
		}
		if !bodyLogsDryRunComplete(ifStmt.Body) {
			return true
		}
		pos = ifStmt.Pos()
		return false
	})
	return pos
}

func bodyLogsDryRunComplete(body *ast.BlockStmt) bool {
	found := false
	ast.Inspect(body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel == nil || sel.Sel.Name != "Info" {
			return true
		}
		for _, arg := range call.Args {
			lit, ok := arg.(*ast.BasicLit)
			if ok && strings.Contains(lit.Value, "dry-run complete") {
				found = true
				return false
			}
		}
		return true
	})
	return found
}

func findRunFuncDecl(f *ast.File) *ast.FuncDecl {
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if ok && fn.Name.Name == "run" {
			return fn
		}
	}
	return nil
}

func callName(call *ast.CallExpr) string {
	switch fn := call.Fun.(type) {
	case *ast.SelectorExpr:
		if pkg, ok := fn.X.(*ast.Ident); ok {
			return pkg.Name + "." + fn.Sel.Name
		}
	case *ast.Ident:
		return fn.Name
	}
	return ""
}

// TestDecimalsGuardHeartbeatSeededBeforeDial pins the wiring half of the never-armed arm of
// stellarindex_decimals_guard_sweep_stale. Unseeded, the gauge reads 0 through the cold-boot
// lake dial and the startup Backfill, so time()-0 alerts 15 minutes after every restart and a
// lake-less aggregator looks wedged. The seed must be set inside the lake-configured branch
// and before either of those can block.
func ruleDecimalsHeartbeatSeededBeforeDial(t *testing.T) {
	f, err := parser.ParseFile(token.NewFileSet(), "main.go", nil, 0)
	if err != nil {
		t.Fatalf("parse main.go: %v", err)
	}
	var guardBranch *ast.BlockStmt
	ast.Inspect(f, func(n ast.Node) bool {
		ifs, ok := n.(*ast.IfStmt)
		if ok && firstCallPos(ifs.Body, "", "dialDecimalsResolver") != token.NoPos {
			guardBranch = ifs.Body
		}
		return true
	})
	if guardBranch == nil {
		t.Fatal("no if-branch in main.go calls dialDecimalsResolver; update this test with the guard's new wiring")
	}
	seed := firstCallPos(guardBranch, "decimalsguard", "MarkEnabled")
	dial := firstCallPos(guardBranch, "", "dialDecimalsResolver")
	if seed == token.NoPos {
		t.Fatal("decimalsguard.MarkEnabled is not called in the lake-configured decimals-guard branch — " +
			"the sweep heartbeat stays 0 until the first Sweep, so the staleness alert fires on every cold boot")
	}
	if seed > dial {
		t.Errorf("decimalsguard.MarkEnabled is called after dialDecimalsResolver — a cold lake still reads as epoch-stale")
	}
}

// firstCallPos returns the position of the first call to pkg.name (or the
// bare identifier name when pkg is empty) under n, or token.NoPos.
func firstCallPos(n ast.Node, pkg, name string) token.Pos {
	pos := token.NoPos
	ast.Inspect(n, func(c ast.Node) bool {
		call, ok := c.(*ast.CallExpr)
		if !ok || pos != token.NoPos {
			return pos == token.NoPos
		}
		switch fn := call.Fun.(type) {
		case *ast.Ident:
			if pkg == "" && fn.Name == name {
				pos = call.Pos()
			}
		case *ast.SelectorExpr:
			if x, isIdent := fn.X.(*ast.Ident); isIdent && x.Name == pkg && fn.Sel.Name == name {
				pos = call.Pos()
			}
		}
		return pos == token.NoPos
	})
	return pos
}
