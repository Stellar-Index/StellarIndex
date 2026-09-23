package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"
)

// TestDryRunExitsBeforeBackgroundWorkers pins that `-dry-run` ("load config +
// open connections + exit without ingesting") returns from run() before any
// background worker is launched. The routed-via and signer taggers issue
// UPDATEs against trades on their immediate first sweep and again on
// shutdown, and the decoder-stats flusher writes on its shutdown drain, so a
// launcher that precedes the dry-run return turns a config check into a
// production write.
func TestDryRunExitsBeforeBackgroundWorkers(t *testing.T) {
	fset := token.NewFileSet()
	body := runBody(t, fset)

	exit := -1
	for i, stmt := range body.List {
		if ifs, ok := stmt.(*ast.IfStmt); ok {
			if id, ok := ifs.Cond.(*ast.Ident); ok && id.Name == "dryRun" {
				exit = i
				break
			}
		}
	}
	if exit < 0 {
		t.Fatal("run() has no top-level `if dryRun { ... }` exit")
	}

	for _, stmt := range body.List[:exit] {
		ast.Inspect(stmt, func(n ast.Node) bool {
			if launch := backgroundLaunch(n); launch != "" {
				t.Errorf("run() launches %s at %s, before the dry-run exit; "+
					"move it below `if dryRun { return nil }`", launch, fset.Position(n.Pos()))
			}
			return true
		})
	}

	// Non-vacuity: the launchers this test exists for must still be called
	// by run(), after the exit. A rename that drops one out of the scan
	// fails here instead of passing silently.
	after := map[string]bool{}
	for _, stmt := range body.List[exit+1:] {
		ast.Inspect(stmt, func(n ast.Node) bool {
			if call, ok := n.(*ast.CallExpr); ok {
				if id, ok := call.Fun.(*ast.Ident); ok {
					after[id.Name] = true
				}
			}
			return true
		})
	}
	for _, want := range []string{"startRoutedViaTagger", "startSignerTagger", "startCHLiveSink", "watchPostgresPing"} {
		if !after[want] {
			t.Errorf("run() no longer calls %s after the dry-run exit; update this guard's launcher list", want)
		}
	}
}

// backgroundLaunch names n if it starts background work: a `go` statement, a
// package-local start*/watch* launcher, or a .Start()/.Run() method call.
func backgroundLaunch(n ast.Node) string {
	switch v := n.(type) {
	case *ast.GoStmt:
		return "a goroutine"
	case *ast.CallExpr:
		switch fun := v.Fun.(type) {
		case *ast.Ident:
			if strings.HasPrefix(fun.Name, "start") || strings.HasPrefix(fun.Name, "watch") {
				return fun.Name + "()"
			}
		case *ast.SelectorExpr:
			if fun.Sel.Name == "Start" || fun.Sel.Name == "Run" {
				return "." + fun.Sel.Name + "()"
			}
		}
	}
	return ""
}

func runBody(t *testing.T, fset *token.FileSet) *ast.BlockStmt {
	t.Helper()
	f, err := parser.ParseFile(fset, "main.go", nil, 0)
	if err != nil {
		t.Fatalf("parse main.go: %v", err)
	}
	for _, d := range f.Decls {
		if fn, ok := d.(*ast.FuncDecl); ok && fn.Recv == nil && fn.Name.Name == "run" {
			return fn.Body
		}
	}
	t.Fatal("main.go has no func run")
	return nil
}
