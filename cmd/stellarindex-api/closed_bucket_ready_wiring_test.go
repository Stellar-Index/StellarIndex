package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// TestRun_RegistersTheClosedBucketCheckUnconditionally pins the runtime
// half of ADR-0015's closed-bucket chokepoint: most CAGG readers carry no
// `bucket <= now() - INTERVAL` predicate and are correct only while every
// served view is materialized_only. The readiness set built in run() must
// carry the checker as a literal element of the []v1.ReadyChecker it starts
// from — not behind a conditional append that a config path can skip.
func TestRun_RegistersTheClosedBucketCheckUnconditionally(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "main.go", nil, 0)
	if err != nil {
		t.Fatalf("parse main.go: %v", err)
	}

	found := 0
	ast.Inspect(file, func(n ast.Node) bool {
		lit, ok := n.(*ast.CompositeLit)
		if !ok || !isReadyCheckerSlice(lit.Type) {
			return true
		}
		for _, elt := range lit.Elts {
			call, ok := elt.(*ast.CallExpr)
			if !ok {
				continue
			}
			if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "NewClosedBucketChecker" {
				found++
			}
		}
		return true
	})
	if found == 0 {
		t.Fatal("run() builds no v1.NewClosedBucketChecker into its []v1.ReadyChecker literal — " +
			"an out-of-band materialized_only = false on a price CAGG would serve the open bucket " +
			"on every unguarded reader behind a green /readyz")
	}
}

func isReadyCheckerSlice(expr ast.Expr) bool {
	arr, ok := expr.(*ast.ArrayType)
	if !ok || arr.Len != nil {
		return false
	}
	sel, ok := arr.Elt.(*ast.SelectorExpr)
	return ok && sel.Sel.Name == "ReadyChecker"
}
