// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package astguard

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// ParseFile parses one Go source file, comments included, failing the test when it does not parse.
func ParseFile(t testing.TB, path string) (*token.FileSet, *ast.File) {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return fset, f
}

// Planted parses one function declared in src (an anti-vacuity case; "package p" is prepended) and returns it.
func Planted(t testing.TB, src string) *ast.FuncDecl {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), "planted.go", "package p\n"+src, 0)
	if err != nil {
		t.Fatalf("parse %q: %v", src, err)
	}
	for _, d := range f.Decls {
		if fn, ok := d.(*ast.FuncDecl); ok {
			return fn
		}
	}
	t.Fatalf("planted source declares no function: %q", src)
	return nil
}

// Mentions reports whether a selector under n names something match accepts, however the use is spelled: a call, a
// method value, an argument or a call through an aliased receiver all count. A guard that only looked at call
// expressions would miss the method value `g := r.store.Read; g(ctx)`.
func Mentions(n ast.Node, match func(name string) bool) bool {
	found := false
	ast.Inspect(n, func(n ast.Node) bool {
		if found {
			return false
		}
		if sel, ok := n.(*ast.SelectorExpr); ok && match(sel.Sel.Name) {
			found = true
			return false
		}
		return true
	})
	return found
}

// MentionsAny is Mentions over an exact set of names.
func MentionsAny(n ast.Node, names ...string) bool {
	return Mentions(n, func(name string) bool {
		for _, want := range names {
			if name == want {
				return true
			}
		}
		return false
	})
}
