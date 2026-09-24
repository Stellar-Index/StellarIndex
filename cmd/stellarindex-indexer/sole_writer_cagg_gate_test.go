package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// TestRunGatesPhase4OnCAGGCoverage pins that run() passes the selected sink
// mode through pipeline.VerifySoleWriterCAGGCoverage, and returns its error,
// before the sink goroutine starts — otherwise persist_per_source = false
// could be deployed on projector-lag evidence alone.
func TestRunGatesPhase4OnCAGGCoverage(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "main.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var run *ast.FuncDecl
	for _, d := range f.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok && fd.Name.Name == "run" && fd.Recv == nil {
			run = fd
		}
	}
	if run == nil {
		t.Fatal("main.go has no func run")
	}
	var gate, persist token.Pos
	gateReturned := false
	ast.Inspect(run.Body, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.IfStmt:
			if call := assignedCall(n.Init); isPipelineCall(call, "VerifySoleWriterCAGGCoverage") {
				gate = call.Pos()
				last, _ := call.Args[len(call.Args)-1].(*ast.Ident)
				if last == nil || last.Name != "sinkMode" {
					t.Errorf("VerifySoleWriterCAGGCoverage is not passed sinkMode")
				}
				gateReturned = len(n.Body.List) > 0 && isReturn(n.Body.List[0])
			}
		case *ast.CallExpr:
			if isPipelineCall(n, "PersistEvents") && persist == token.NoPos {
				persist = n.Pos()
			}
		}
		return true
	})
	switch {
	case gate == token.NoPos:
		t.Fatal("run() never calls pipeline.VerifySoleWriterCAGGCoverage in an if-init")
	case !gateReturned:
		t.Error("run() does not return when pipeline.VerifySoleWriterCAGGCoverage fails")
	case persist == token.NoPos || persist < gate:
		t.Errorf("pipeline.VerifySoleWriterCAGGCoverage (%s) must run before pipeline.PersistEvents (%s)",
			fset.Position(gate), fset.Position(persist))
	}
}

func assignedCall(s ast.Stmt) *ast.CallExpr {
	as, ok := s.(*ast.AssignStmt)
	if !ok || len(as.Rhs) != 1 {
		return nil
	}
	call, _ := as.Rhs[0].(*ast.CallExpr)
	return call
}

func isPipelineCall(call *ast.CallExpr, name string) bool {
	if call == nil {
		return false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != name {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	return ok && pkg.Name == "pipeline"
}

func isReturn(s ast.Stmt) bool {
	_, ok := s.(*ast.ReturnStmt)
	return ok
}
