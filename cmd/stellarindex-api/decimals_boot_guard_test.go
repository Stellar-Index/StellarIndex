package main

// Source-level tripwire, the API twin of
// cmd/stellarindex-aggregator/decimals_boot_guard_test.go: the
// nonstandard-decimals cache's BOOT-time refresh must abort startup.
//
// The cache is fail-open, which is right for a periodic refresh (the
// last-good snapshot stays). At boot there is no last-good snapshot, so a
// failed first refresh leaves Lookup answering "not flagged" for every
// confirmed non-7-decimals asset and /v1/price, /v1/history, the SSE
// stream and every other normalizing surface serve those legs raw — wrong
// by 10^(7-decimals). run() needs a config file, Postgres and Redis before
// it reaches this line, so this asserts on the wiring itself.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

func TestNonstandardDecimalsCacheInitialRefreshIsFatal(t *testing.T) {
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

	found := 0
	ast.Inspect(run.Body, func(n ast.Node) bool {
		// A refresh inside a goroutine or closure is the periodic loop, and
		// a return there leaves the goroutine, not run().
		if _, ok := n.(*ast.FuncLit); ok {
			return false
		}
		ifStmt, ok := n.(*ast.IfStmt)
		if !ok || !initCallsRefreshOn(ifStmt, "nonstandardDecimalsCache") {
			return true
		}
		found++
		if !blockReturns(ifStmt.Body) {
			t.Errorf("main.go:%d — the nonstandard-decimals cache's initial refresh no longer "+
				"aborts startup on failure. A cold cache serves every confirmed non-7-decimals "+
				"leg unnormalized (wrong by 10^(7-decimals)); refusing to start is the "+
				"recoverable failure.", fset.Position(ifStmt.Pos()).Line)
		}
		return true
	})
	if found != 1 {
		t.Fatalf("found %d boot-time nonstandardDecimalsCache.Refresh call(s) directly in run(), "+
			"want exactly 1 — a refresh only inside the background goroutine lets the API "+
			"serve from a cold cache", found)
	}
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
