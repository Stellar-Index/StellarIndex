// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// TestAPIKeyBudgetStoresAreWiredThroughTheBackendAwareConstructor is the
// WIRING half of findings F056 / K050 / Q145.
//
// Until 2026-09-19 main.go set `apiKeyBudgets.CacheInvalidator =
// auth.NewRedisKeyCacheInvalidator(rdb)` whenever Redis was configured.
// Under the default auth_backend=redis the key that invalidator DELs is
// the canonical credential, so every admin PATCH that changed an
// override, a status or a tier permanently destroyed the account's
// /v1/register keys. The behavioural proof runs against real Redis and
// Postgres in test/integration (TestAdminAccountPatch_Preserves
// RegisterCredential_RedisBackend) through v1.NewAPIKeyBudgetStores;
// what that test cannot see is whether THIS binary still goes through
// that constructor, because the wiring sits inside run(), which needs a
// live Postgres. Same AST-guard shape as
// TestDEXTVLGateIsWiredThroughTheNilAbleBuilder.
func TestAPIKeyBudgetStoresAreWiredThroughTheBackendAwareConstructor(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "main.go", nil, 0)
	if err != nil {
		t.Fatalf("parse main.go: %v", err)
	}

	constructorCalls := 0
	ast.Inspect(f, func(n ast.Node) bool {
		switch node := n.(type) {
		case *ast.CallExpr:
			sel, ok := node.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			switch sel.Sel.Name {
			case "NewRedisKeyCacheInvalidator":
				t.Errorf("main.go calls %s at %s: an unconditional key-cache invalidator DELs the "+
					"canonical credential under auth_backend=redis; build the stores with "+
					"v1.NewAPIKeyBudgetStores, which decides from auth_backend",
					exprText(node.Fun), fset.Position(node.Pos()))
			case "NewAPIKeyBudgetStores":
				constructorCalls++
				if len(node.Args) != 3 {
					t.Errorf("NewAPIKeyBudgetStores called with %d args at %s, want 3",
						len(node.Args), fset.Position(node.Pos()))
					return true
				}
				if got := exprText(node.Args[2]); got != "cfg.API.AuthBackend" {
					t.Errorf("NewAPIKeyBudgetStores is given auth backend %q at %s, want the "+
						"configured cfg.API.AuthBackend — a literal here re-opens the defect",
						got, fset.Position(node.Args[2].Pos()))
				}
			}
		case *ast.AssignStmt:
			for _, lhs := range node.Lhs {
				sel, ok := lhs.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "CacheInvalidator" {
					continue
				}
				if base, ok := sel.X.(*ast.Ident); ok && base.Name == "apiKeyBudgets" {
					t.Errorf("main.go assigns apiKeyBudgets.CacheInvalidator by hand at %s; the field "+
						"must come from v1.NewAPIKeyBudgetStores so auth_backend decides it",
						fset.Position(node.Pos()))
				}
			}
		}
		return true
	})
	if constructorCalls != 1 {
		t.Fatalf("main.go calls v1.NewAPIKeyBudgetStores %d times, want exactly 1 — the admin "+
			"tier-clamp / register stores lost their backend-aware wiring, or this guard needs re-aiming",
			constructorCalls)
	}
}
