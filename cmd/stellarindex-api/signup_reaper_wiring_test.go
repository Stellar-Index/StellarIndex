package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// TestRun_SignupReaperBindsToThePostgresAccountStoreNotTheDashboardBundle
// pins NS02: POST /v1/register (ADR-0049) is wired onto
// platformAccountStore/registerAccountStore whenever Postgres is
// reachable — independent of api.dashboard.base_url. The reaper that
// cleans up its signup-race orphans must bind to that same seam. Asserting
// the OrphanStore type against dashboardBundle.accounts instead means a
// Postgres-only, no-dashboard deployment (the shipped example.toml default:
// no [api.dashboard] base_url) accepts registrations but never runs the
// reaper, because buildDashboardBundle returns the zero value when
// BaseURL == "".
func TestRun_SignupReaperBindsToThePostgresAccountStoreNotTheDashboardBundle(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "main.go", nil, 0)
	if err != nil {
		t.Fatalf("parse main.go: %v", err)
	}

	var assertedOn string
	ast.Inspect(file, func(n ast.Node) bool {
		ta, ok := n.(*ast.TypeAssertExpr)
		if !ok {
			return true
		}
		sel, ok := ta.Type.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "OrphanStore" {
			return true
		}
		switch x := ta.X.(type) {
		case *ast.Ident:
			assertedOn = x.Name
		case *ast.SelectorExpr:
			if base, ok := x.X.(*ast.Ident); ok {
				assertedOn = base.Name + "." + x.Sel.Name
			} else {
				assertedOn = x.Sel.Name
			}
		}
		return true
	})

	if assertedOn == "" {
		t.Fatal("run() asserts no value against signupreaper.OrphanStore — the reaper wiring moved or was removed")
	}
	if assertedOn != "platformAccountStore" {
		t.Fatalf("signup-reaper's OrphanStore assertion binds to %q, want %q (the same store POST /v1/register uses); "+
			"binding it to dashboardBundle.accounts instead leaves the reaper unwired in a Postgres-only, "+
			"no-dashboard deployment even though registration is active there", assertedOn, "platformAccountStore")
	}
}
