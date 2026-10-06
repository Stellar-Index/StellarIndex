package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"testing"
)

// TestRun_RegistersTheClickhouseCheckOnTheFailedDialPathToo is the
// wiring guard for F122: a ClickHouse readiness checker must be
// registered on a path that a FAILED boot dial also takes.
//
// The defect it pins is positional, not logical — `checks = append(
// checks, clickhouseChecker{r: er})` sat inside the `else` of the dial's
// error check, which is the branch a failed dial does not take. So a
// ClickHouse that was already down when the API started published no
// `stellarindex_dependency_up{dependency="clickhouse"}` series at all,
// the `== 0` alert (deliberately not absent(), per its own in-file
// rationale) had nothing to match, and the state the annotation calls
// "the only signal that it is gone" was the state with no signal.
//
// The checker is built in the wiring package and registered from run(),
// so both halves are checked: the wiring package constructs a
// ClickhouseChecker outside an else-branch, and run() appends
// wiring.ClickhouseReadyChecks to its checks outside one.
//
// Read from the AST so the assertion is about the code rather than
// about a list someone has to remember to update.
func TestRun_RegistersTheClickhouseCheckOnTheFailedDialPathToo(t *testing.T) {
	fset := token.NewFileSet()
	checkers, err := parser.ParseFile(fset, filepath.Join(wiringDir, "checkers.go"), nil, 0)
	if err != nil {
		t.Fatalf("parse %s/checkers.go: %v", wiringDir, err)
	}
	constructed, constructedInElse := countOutsideElse(checkers, func(n ast.Node) bool {
		lit, ok := n.(*ast.CompositeLit)
		if !ok {
			return false
		}
		id, ok := lit.Type.(*ast.Ident)
		return ok && id.Name == "ClickhouseChecker"
	})
	if constructed == 0 {
		t.Fatal("the wiring package constructs no ClickhouseChecker — ClickHouse has no readiness checker at all, so stellarindex_dependency_up{dependency=\"clickhouse\"} is never published")
	}
	if constructedInElse == constructed {
		t.Errorf("every ClickhouseChecker in the wiring package (%d) is constructed inside an else-branch — a boot dial that FAILS takes the if-branch, so the one state the dependency gauge exists to report is the one state it is absent for", constructed)
	}

	mainFile, err := parser.ParseFile(fset, "main.go", nil, 0)
	if err != nil {
		t.Fatalf("parse main.go: %v", err)
	}
	run := findFuncBody(mainFile, "run")
	if run == nil {
		t.Fatal("run() not found in main.go")
	}
	registered, registeredInElse := countOutsideElse(run, isClickhouseChecksAppend)
	if registered == 0 {
		t.Fatal("run() never does `checks = append(checks, wiring.ClickhouseReadyChecks(...)...)` — the checker is built but never registered, so stellarindex_dependency_up{dependency=\"clickhouse\"} is never published")
	}
	if registeredInElse == registered {
		t.Errorf("every wiring.ClickhouseReadyChecks registration in run() (%d) is inside an else-branch — a boot dial that FAILS takes the if-branch, so the one state the dependency gauge exists to report is the one state it is absent for", registered)
	}
}

// countOutsideElse counts the nodes under root that match, and how many
// of them sit inside an else-branch.
func countOutsideElse(root ast.Node, match func(ast.Node) bool) (total, inElse int) {
	var elseRanges [][2]token.Pos
	ast.Inspect(root, func(n ast.Node) bool {
		if ifs, ok := n.(*ast.IfStmt); ok && ifs.Else != nil {
			elseRanges = append(elseRanges, [2]token.Pos{ifs.Else.Pos(), ifs.Else.End()})
		}
		return true
	})
	ast.Inspect(root, func(n ast.Node) bool {
		if n == nil || !match(n) {
			return true
		}
		total++
		for _, r := range elseRanges {
			if n.Pos() >= r[0] && n.Pos() < r[1] {
				inElse++
				break
			}
		}
		return true
	})
	return total, inElse
}

// isClickhouseChecksAppend matches
// `checks = append(checks, wiring.ClickhouseReadyChecks(...)...)`.
func isClickhouseChecksAppend(n ast.Node) bool {
	a, ok := n.(*ast.AssignStmt)
	if !ok || len(a.Lhs) != 1 || len(a.Rhs) != 1 {
		return false
	}
	if lhs, ok := a.Lhs[0].(*ast.Ident); !ok || lhs.Name != "checks" {
		return false
	}
	call, ok := a.Rhs[0].(*ast.CallExpr)
	if !ok {
		return false
	}
	if fn, ok := call.Fun.(*ast.Ident); !ok || fn.Name != "append" {
		return false
	}
	for _, arg := range call.Args {
		inner, ok := arg.(*ast.CallExpr)
		if !ok {
			continue
		}
		sel, ok := inner.Fun.(*ast.SelectorExpr)
		if !ok {
			continue
		}
		if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "wiring" && sel.Sel.Name == "ClickhouseReadyChecks" {
			return true
		}
	}
	return false
}
