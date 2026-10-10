// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package diagnostics

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSourceShapeRules pins source shapes that no behavioural test can reach.
// Each rule parses the package's sources with go/ast and fails on the same
// defect it did as a standalone test.
func TestSourceShapeRules(t *testing.T) {
	rules := []struct {
		name string
		run  func(t *testing.T)
	}{
		{"verify handlers end by returning silentVerdict", ruleVerifyHandlersReturnSilentVerdict},
		{"every hubble BigQuery query carries the byte cap", ruleHubbleQueriesCarryByteCap},
	}
	for _, r := range rules {
		t.Run(r.name, r.run)
	}
}

// The handlers need a live datastore or live vendors, so pin the wiring
// instead: each must END by returning silentVerdict, so no later edit can
// fall back to an unconditional `return nil` after the table.
func ruleVerifyHandlersReturnSilentVerdict(t *testing.T) {
	for file, fn := range map[string]string{
		"verify_decoders.go": "verifyDecoders",
		"verify_external.go": "verifyExternal",
	} {
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, file, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		var body *ast.BlockStmt
		for _, d := range f.Decls {
			if fd, ok := d.(*ast.FuncDecl); ok && fd.Name.Name == fn {
				body = fd.Body
			}
		}
		if body == nil || len(body.List) == 0 {
			t.Fatalf("%s: func %s not found", file, fn)
		}
		if !returnsCallTo(body.List[len(body.List)-1], "silentVerdict") {
			t.Errorf("%s: %s does not end with `return silentVerdict(...)`; its exit code ignores the silent count", file, fn)
		}
	}
}

func returnsCallTo(stmt ast.Stmt, callee string) bool {
	ret, ok := stmt.(*ast.ReturnStmt)
	if !ok || len(ret.Results) != 1 {
		return false
	}
	call, ok := ret.Results[0].(*ast.CallExpr)
	if !ok {
		return false
	}
	id, ok := call.Fun.(*ast.Ident)
	return ok && id.Name == callee
}

// ruleHubbleQueriesCarryByteCap pins the chokepoint: the only BigQuery
// Query(...) call in this package's non-test code is inside hubbleBQ.query,
// which stamps MaxBytesBilled. A direct client.Query elsewhere would run
// uncapped against a multi-TB public table.
func ruleHubbleQueriesCarryByteCap(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	var direct []string
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		file, err := parser.ParseFile(fset, f, src, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil || isHubbleBQQuery(fn) {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Query" {
					direct = append(direct, fset.Position(call.Pos()).String())
				}
				return true
			})
		}
	}
	if len(direct) > 0 {
		t.Fatalf("BigQuery Query() built outside hubbleBQ.query, so it carries no MaxBytesBilled: %v", direct)
	}
}

func isHubbleBQQuery(fn *ast.FuncDecl) bool {
	if fn.Name.Name != "query" || fn.Recv == nil || len(fn.Recv.List) != 1 {
		return false
	}
	id, ok := fn.Recv.List[0].Type.(*ast.Ident)
	return ok && id.Name == "hubbleBQ"
}
