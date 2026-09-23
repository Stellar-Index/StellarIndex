package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// TestRunHoldsInstanceLock is a source-level tripwire: run() must take the
// indexer's Postgres instance lock before it starts any goroutine, and
// surface a lost lock in its exit error. Without it nothing stops a second
// indexer racing the cursors and double-writing every sink.
func TestRunHoldsInstanceLock(t *testing.T) {
	assertRunHoldsInstanceLock(t, "IndexerInstanceLockName")
}

func assertRunHoldsInstanceLock(t *testing.T, lockName string) {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "main.go", nil, 0)
	if err != nil {
		t.Fatalf("parse main.go: %v", err)
	}
	var run *ast.FuncDecl
	for _, d := range f.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok && fd.Name.Name == "run" && fd.Recv == nil {
			run = fd
		}
	}
	if run == nil {
		t.Fatal("no run() in main.go — update this guard to follow the refactor rather than deleting it")
	}
	var holdAt, firstGo token.Pos
	ast.Inspect(run.Body, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.GoStmt:
			if !firstGo.IsValid() {
				firstGo = n.Pos()
			}
		case *ast.CallExpr:
			sel, ok := n.Fun.(*ast.SelectorExpr)
			if ok && sel.Sel.Name == "HoldInstanceLock" && callNamesLock(n, lockName) && !holdAt.IsValid() {
				holdAt = n.Pos()
			}
		}
		return true
	})
	if !holdAt.IsValid() {
		t.Fatalf("run() never calls store.HoldInstanceLock(..., timescale.%s, ...): a second instance can start against the same database", lockName)
	}
	if firstGo.IsValid() && firstGo < holdAt {
		t.Errorf("run() starts a goroutine at %s before taking the instance lock at %s", fset.Position(firstGo), fset.Position(holdAt))
	}
	last, ok := run.Body.List[len(run.Body.List)-1].(*ast.ReturnStmt)
	if !ok || !mentionsSelector(last, "instanceLock", "Lost") {
		t.Error("run()'s final return does not surface instanceLock.Lost(): an instance that lost its lock would exit 0 and never be restarted or paged")
	}
}

func callNamesLock(call *ast.CallExpr, lockName string) bool {
	for _, a := range call.Args {
		if sel, ok := a.(*ast.SelectorExpr); ok && sel.Sel.Name == lockName {
			if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "timescale" {
				return true
			}
		}
	}
	return false
}

func mentionsSelector(n ast.Node, recv, name string) bool {
	found := false
	ast.Inspect(n, func(c ast.Node) bool {
		if sel, ok := c.(*ast.SelectorExpr); ok && sel.Sel.Name == name {
			if id, ok := sel.X.(*ast.Ident); ok && id.Name == recv {
				found = true
			}
		}
		return !found
	})
	return found
}
