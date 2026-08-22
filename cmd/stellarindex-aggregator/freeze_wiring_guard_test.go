package main

// Source-level tripwire (the internal/api/v1/slo_guard_test.go pattern):
// the freeze WRITER must never again be gated on the Phase 1 anomaly
// checker.
//
// 2026-08-22, r1 XLM/GBP incident: the writer was built under
// `if checker != nil && rdb != nil`, but the Phase 2 confidence
// lifecycle (orchestrator.stepPhase2Freeze) runs on every scored bucket
// regardless of cfg.Anomaly and REFUSES publication when its 3-signal
// AND fires. A Phase-1-off / [anomaly.phase2]-tuned deployment (exactly
// r1's TOML) therefore engaged real freezes that wrote NO Redis marker:
// the frozen 5m/1h windows kept serving their last value with
// flags.frozen absent — a stale price presented as fresh, the precise
// state the marker exists to prevent. This test walks main.go's AST,
// finds the if-statement whose body constructs freeze.NewWriter, and
// fails if its condition mentions the Phase 1 `checker` again.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

func TestFreezeWriterNotGatedOnPhase1Checker(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "main.go", nil, 0)
	if err != nil {
		t.Fatalf("parse main.go: %v", err)
	}

	found := false
	ast.Inspect(f, func(n ast.Node) bool {
		ifStmt, ok := n.(*ast.IfStmt)
		if !ok {
			return true
		}
		if !bodyCallsFreezeNewWriter(ifStmt.Body) {
			return true
		}
		found = true
		ast.Inspect(ifStmt.Cond, func(cn ast.Node) bool {
			if id, ok := cn.(*ast.Ident); ok && id.Name == "checker" {
				t.Errorf("freeze.NewWriter construction is gated on the Phase 1 `checker` again — " +
					"Phase 2 freezes fire with Anomaly nil and would refuse publication with no " +
					"Redis marker (stale price served with flags.frozen absent; r1 XLM/GBP 2026-08-22)")
			}
			return true
		})
		return true
	})
	if !found {
		t.Fatal("could not locate the if-statement constructing freeze.NewWriter in main.go — " +
			"update this guard to follow the refactor rather than deleting it")
	}
}

func bodyCallsFreezeNewWriter(body *ast.BlockStmt) bool {
	calls := false
	ast.Inspect(body, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "freeze" && sel.Sel.Name == "NewWriter" {
			calls = true
			return false
		}
		return true
	})
	return calls
}
