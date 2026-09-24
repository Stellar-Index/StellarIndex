package main

// Source-level tripwire, the API twin of
// cmd/stellarindex-aggregator/decimals_boot_guard_test.go — but the two
// have DIFFERENT verdicts. The aggregator's boot refresh is fatal (#368
// M9): it has no readiness surface between it and the orchestrator loop
// that publishes prices. The API sits behind `/v1/readyz`, so RLT-366's
// original fatal-on-boot version (make run() return an error) was
// replaced by Q198's synchronous prime + critical readiness check
// (primeNonstandardDecimalsCache, see nonstandard_decimals_ready_test.go):
// a failed first load now keeps the process up but red, instead of
// putting systemd into a restart loop on a Postgres blip.
//
// This guards the other direction: run() must NOT reintroduce a direct,
// fatal `nonstandardDecimalsCache.Refresh` call, since that would bring
// the restart-loop failure mode back.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

func TestNonstandardDecimalsCacheInitialRefreshIsNotFatal(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "main.go", nil, 0)
	if err != nil {
		t.Fatalf("parse main.go: %v", err)
	}
	var run *ast.FuncDecl
	for _, d := range f.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok && fd.Recv == nil && fd.Name.Name == "run" {
			run = fd
		}
	}
	if run == nil || run.Body == nil {
		t.Fatal("no top-level run() in main.go — re-derive where the API boots")
	}

	ast.Inspect(run.Body, func(n ast.Node) bool {
		// A refresh inside a goroutine or closure is the periodic loop —
		// its Warn-and-continue is fine and out of scope here.
		if _, ok := n.(*ast.FuncLit); ok {
			return false
		}
		ifStmt, ok := n.(*ast.IfStmt)
		if !ok || !initCallsRefreshOn(ifStmt, "nonstandardDecimalsCache") {
			return true
		}
		if blockReturns(ifStmt.Body) {
			t.Errorf("main.go:%d — run() aborts startup directly on a failed "+
				"nonstandardDecimalsCache.Refresh again. That was reverted (RLT-366 -> Q198): "+
				"it puts systemd into a restart loop on a Postgres blip. Cold-cache safety "+
				"belongs in the critical readiness check (primeNonstandardDecimalsCache), not "+
				"a fatal return here.", fset.Position(ifStmt.Pos()).Line)
		}
		return true
	})
}

// initCallsRefreshOn matches `if err := <recv>.Refresh(ctx); err != nil`.
func initCallsRefreshOn(ifStmt *ast.IfStmt, recv string) bool {
	assign, ok := ifStmt.Init.(*ast.AssignStmt)
	if !ok || len(assign.Rhs) != 1 {
		return false
	}
	call, ok := assign.Rhs[0].(*ast.CallExpr)
	if !ok {
		return false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "Refresh" {
		return false
	}
	id, ok := sel.X.(*ast.Ident)
	return ok && id.Name == recv
}

// blockReturns reports whether a block returns at its own statement level.
func blockReturns(body *ast.BlockStmt) bool {
	for _, stmt := range body.List {
		if _, ok := stmt.(*ast.ReturnStmt); ok {
			return true
		}
	}
	return false
}
