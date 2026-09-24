// Copyright 2026 Stellar Index contributors
// SPDX-License-Identifier: Apache-2.0

package opsutil

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
)

func TestRangeCursorKey(t *testing.T) {
	if got := RangeCursorKey(56_100_000, 56_300_000); got != "56100000-56300000" {
		t.Fatalf("RangeCursorKey = %q", got)
	}
}

// TestOpsCheckpointSubIsNeverAFixedString: ops checkpoints are range-scoped
// and UpsertCursor is monotone-forward, so a sub_source that is a literal or
// constant lets a finished later range swallow an earlier one. Build range
// checkpoint keys with RangeCursorKey.
func TestOpsCheckpointSubIsNeverAFixedString(t *testing.T) {
	var hits []string
	calls := 0
	fset := token.NewFileSet()
	err := filepath.WalkDir("..", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return err
		}
		f, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			return perr
		}
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "UpsertCursor" || len(call.Args) != 4 {
				return true
			}
			calls++
			if isFixedString(call.Args[2]) {
				hits = append(hits, fset.Position(call.Pos()).String())
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if calls < 5 {
		t.Fatalf("found %d UpsertCursor calls under internal/ops; the walk is not seeing the tree", calls)
	}
	if len(hits) > 0 {
		t.Errorf("UpsertCursor with a fixed sub_source; key the checkpoint with opsutil.RangeCursorKey:\n%s",
			strings.Join(hits, "\n"))
	}
}

// isFixedString reports whether e is a string literal, a constant, another
// package's identifier, or a local initialised from one of those.
func isFixedString(e ast.Expr) bool {
	switch x := e.(type) {
	case *ast.BasicLit:
		return true
	case *ast.SelectorExpr:
		id, ok := x.X.(*ast.Ident)
		return ok && id.Obj == nil // pkg.Name: never derived from the run's range
	case *ast.Ident:
		if x.Obj == nil {
			return false
		}
		return x.Obj.Kind == ast.Con || initialisedFromFixed(x)
	}
	return false
}

func initialisedFromFixed(id *ast.Ident) bool {
	switch d := id.Obj.Decl.(type) {
	case *ast.ValueSpec:
		for i, n := range d.Names {
			if n.Name == id.Name && i < len(d.Values) {
				return isFixedString(d.Values[i])
			}
		}
	case *ast.AssignStmt:
		if len(d.Lhs) != len(d.Rhs) {
			return false
		}
		for i, l := range d.Lhs {
			if l, ok := l.(*ast.Ident); ok && l.Name == id.Name {
				return isFixedString(d.Rhs[i])
			}
		}
	}
	return false
}
