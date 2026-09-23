package main

// Source-level tripwire (the freeze_wiring_guard_test.go /
// decimals_boot_guard_test.go pattern): changesummary.New, the ClickHouse
// close-time reader, and the supply / cross-check refresher builders must
// be constructed before run()'s dry-run early-return, so a config that
// fails one of them is caught by -dry-run instead of only surfacing at a
// real start (T184).
//
// Behavioural coverage is not available here — the calls live inside
// run(), which needs a config file, a Postgres pool and a Redis client
// before it reaches this line — so this asserts on the wiring itself.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"
)

func TestSupplyStartupConstructorsRunBeforeDryRunReturn(t *testing.T) {
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

	// clickhouse.NewExplorerReader is called from several unrelated
	// constructors in run() (the mev tx-order resolver, the decimals
	// resolver, the priceless-coverage SAC reader) — only the one feeding
	// the supply aggregator-refresh path is in scope here, identified by
	// its assignment to `supplyCloseTimes`.
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

// findDryRunReturnPos locates the `if dryRun { logger.Info("dry-run
// complete — exiting"); return nil }` statement and returns its Pos.
// Matched on the dry-run-complete log line, not merely `if dryRun`, since
// run() also branches on dryRun earlier (the redis ping) without exiting.
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
