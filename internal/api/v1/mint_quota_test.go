// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestCreateAPIKeyRequestSitesNameMonthlyQuota fails when an HTTP mint
// path builds an auth.CreateAPIKeyRequest without naming MonthlyQuota:
// an omitted field persists 0, which the quota middleware reads as
// unmetered, so every mint site must state its cap explicitly.
func TestCreateAPIKeyRequestSitesNameMonthlyQuota(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	sites := 0
	fset := token.NewFileSet()
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		f, err := parser.ParseFile(fset, name, src, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			lit, ok := n.(*ast.CompositeLit)
			if !ok || !isCreateAPIKeyRequest(lit.Type) {
				return true
			}
			sites++
			if !namesField(lit, "MonthlyQuota") {
				t.Errorf("%s: auth.CreateAPIKeyRequest omits MonthlyQuota (persists an unmetered key)", fset.Position(lit.Pos()))
			}
			return true
		})
	}
	if sites == 0 {
		t.Fatal("found no auth.CreateAPIKeyRequest literals; the guard is not scanning the mint paths")
	}
}

func isCreateAPIKeyRequest(e ast.Expr) bool {
	sel, ok := e.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "CreateAPIKeyRequest" {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	return ok && pkg.Name == "auth"
}

func namesField(lit *ast.CompositeLit, field string) bool {
	for _, elt := range lit.Elts {
		kv, ok := elt.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		if k, ok := kv.Key.(*ast.Ident); ok && k.Name == field {
			return true
		}
	}
	return false
}
