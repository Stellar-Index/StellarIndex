package main

import (
	"go/ast"
	"go/parser"
	"go/token"
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
// Read from the AST so the assertion is about the code rather than
// about a list someone has to remember to update.
func TestRun_RegistersTheClickhouseCheckOnTheFailedDialPathToo(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "main.go", nil, 0)
	if err != nil {
		t.Fatalf("parse main.go: %v", err)
	}

	elseRanges := [][2]token.Pos{}
	ast.Inspect(file, func(n ast.Node) bool {
		if ifs, ok := n.(*ast.IfStmt); ok && ifs.Else != nil {
			elseRanges = append(elseRanges, [2]token.Pos{ifs.Else.Pos(), ifs.Else.End()})
		}
		return true
	})
	inElse := func(p token.Pos) bool {
		for _, r := range elseRanges {
			if p >= r[0] && p < r[1] {
				return true
			}
		}
		return false
	}

	var registrations, elseOnly int
	ast.Inspect(file, func(n ast.Node) bool {
		lit, ok := n.(*ast.CompositeLit)
		if !ok {
			return true
		}
		id, ok := lit.Type.(*ast.Ident)
		if !ok || id.Name != "clickhouseChecker" {
			return true
		}
		registrations++
		if inElse(lit.Pos()) {
			elseOnly++
		}
		return true
	})

	if registrations == 0 {
		t.Fatal("main.go constructs no clickhouseChecker — ClickHouse has no readiness checker at all, so stellarindex_dependency_up{dependency=\"clickhouse\"} is never published")
	}
	if elseOnly == registrations {
		t.Errorf("every clickhouseChecker in main.go (%d) is constructed inside an else-branch — a boot dial that FAILS takes the if-branch, so the one state the dependency gauge exists to report is the one state it is absent for", registrations)
	}
}
