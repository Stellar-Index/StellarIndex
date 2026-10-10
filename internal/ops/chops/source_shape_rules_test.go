// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package chops

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// TestSourceShapeRules pins source shapes that no behavioural test can reach.
// Each rule parses the named files with go/ast and fails on the same defect
// it did as a standalone test.
func TestSourceShapeRules(t *testing.T) {
	rules := []struct {
		name string
		run  func(t *testing.T)
	}{
		{"soroswap seed error is returned by both commands", ruleSoroswapSeedErrorIsReturned},
		{"projected rebuild resets the SEP-41 rollup after the run", ruleProjectedRebuildResetsSEP41RollupAfterRun},
		{"ch-rebuild tradeOf covers every projected trade source", ruleChRebuildTradeOfCoversProjectedTradeSources},
	}
	for _, r := range rules {
		t.Run(r.name, r.run)
	}
}

// The call sites. seedSoroswapForRecon returning an error is worth nothing if
// the command prints it and carries on, which is exactly what both did. Read
// from the AST (precedent: cmd/stellarindex-api/*_wiring_test.go) because both
// commands open Postgres before they reach the seed and cannot be run here:
// every call must be the init of an `if … err != nil` whose body RETURNS.
func ruleSoroswapSeedErrorIsReturned(t *testing.T) {
	for file, fn := range map[string]string{
		"compute_completeness.go":  "computeCompleteness",
		"verify_reconciliation.go": "verifyReconciliation",
	} {
		t.Run(fn, func(t *testing.T) {
			parsed, err := parser.ParseFile(token.NewFileSet(), file, nil, 0)
			if err != nil {
				t.Fatalf("parse %s: %v", file, err)
			}
			var body *ast.BlockStmt
			for _, d := range parsed.Decls {
				if fd, ok := d.(*ast.FuncDecl); ok && fd.Name.Name == fn {
					body = fd.Body
				}
			}
			if body == nil {
				t.Fatalf("%s: func %s not found", file, fn)
			}
			calls, guarded := seedCallSites(body)
			if calls == 0 {
				t.Fatalf("%s never calls seedSoroswapForRecon — the re-derive decoder is never seeded", fn)
			}
			if guarded != calls {
				t.Errorf("%s: %d of %d seedSoroswapForRecon call(s) are not `if err := …; err != nil { return … }` — "+
					"a failed seed is logged or dropped and the run continues on a partial pair registry (RLT-416)",
					fn, calls-guarded, calls)
			}
		})
	}
}

// seedCallSites counts calls to seedSoroswapForRecon under root, and how many
// of them are the init statement of an if whose body ends in a return carrying
// a value.
func seedCallSites(root ast.Node) (calls, guarded int) {
	isSeedCall := func(n ast.Node) bool {
		c, ok := n.(*ast.CallExpr)
		if !ok {
			return false
		}
		id, ok := c.Fun.(*ast.Ident)
		return ok && id.Name == "seedSoroswapForRecon"
	}
	ast.Inspect(root, func(n ast.Node) bool {
		if isSeedCall(n) {
			calls++
		}
		ifs, ok := n.(*ast.IfStmt)
		if !ok || ifs.Init == nil || len(ifs.Body.List) == 0 {
			return true
		}
		as, ok := ifs.Init.(*ast.AssignStmt)
		if !ok || len(as.Rhs) != 1 || !isSeedCall(as.Rhs[0]) {
			return true
		}
		ret, ok := ifs.Body.List[len(ifs.Body.List)-1].(*ast.ReturnStmt)
		if !ok || len(ret.Results) != 1 {
			return true
		}
		if id, isIdent := ret.Results[0].(*ast.Ident); isIdent && id.Name == "nil" {
			return true // `return nil` swallows the error as surely as a log line
		}
		guarded++
		return true
	})
	return calls, guarded
}

// ruleProjectedRebuildResetsSEP41RollupAfterRun pins the wiring: the
// reset is only worth anything if projectedRebuild reaches it, and only
// AFTER RunProjectedRebuild — a reset before the writes lets the worker
// re-advance the checkpoint past rows the run has yet to write.
func ruleProjectedRebuildResetsSEP41RollupAfterRun(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), "projected_rebuild.go", nil, 0)
	if err != nil {
		t.Fatalf("parse projected_rebuild.go: %v", err)
	}
	var run, reset token.Pos
	for _, d := range file.Decls {
		fn, ok := d.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "projectedRebuild" {
			continue
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			if id, ok := call.Fun.(*ast.Ident); ok {
				switch id.Name {
				case "RunProjectedRebuild":
					run = call.Pos()
				case "resetSEP41RollupAfterRebuild":
					reset = call.Pos()
				}
			}
			return true
		})
	}
	if run == token.NoPos {
		t.Fatal("projectedRebuild no longer calls RunProjectedRebuild — this guard has moved")
	}
	if reset == token.NoPos {
		t.Fatal("projectedRebuild never calls resetSEP41RollupAfterRebuild — a -write rebuild of sep41_supply " +
			"leaves its rows below the sep41_supply_rollup checkpoint, invisible to served supply")
	}
	if reset < run {
		t.Error("projectedRebuild resets the sep41 rollup BEFORE RunProjectedRebuild — the reset must follow the writes")
	}
}

// ruleChRebuildTradeOfCoversProjectedTradeSources is the guard.
func ruleChRebuildTradeOfCoversProjectedTradeSources(t *testing.T) {
	want := caseTypesOfSwitch(t, "../../pipeline/sink.go", "tradeFromEvent")
	got := caseTypesOfSwitch(t, "ch_rebuild.go", "tradeOf")

	for typeName := range want {
		if got[typeName] {
			continue
		}
		if reason, exempt := tradeOfExempt[typeName]; exempt {
			t.Logf("%s exempt from tradeOf: %s", typeName, reason)
			continue
		}
		t.Errorf("%s is a trade-shaped event in pipeline.tradeFromEvent but has no arm in ch_rebuild.go tradeOf — "+
			"scripts/ops/ch-rebuild-projected.sh would DELETE its trades and never rewrite them. "+
			"Add the case, or register an exemption with a reason in tradeOfExempt.", typeName)
	}

	for typeName := range got {
		if !want[typeName] {
			t.Errorf("%s has an arm in ch_rebuild.go tradeOf but is not a trade-shaped event in "+
				"pipeline.tradeFromEvent — one of the two is stale", typeName)
		}
	}

	for typeName := range tradeOfExempt {
		if !want[typeName] {
			t.Errorf("stale tradeOfExempt entry %s — it is no longer in pipeline.tradeFromEvent", typeName)
		}
	}
}

// caseTypesOfSwitch returns the type names (`pkg.Type`) of every case
// clause in the type-switch inside the named function of the parsed file.
func caseTypesOfSwitch(t *testing.T, path, fn string) map[string]bool {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	out := map[string]bool{}
	var found bool
	for _, decl := range f.Decls {
		fd, ok := decl.(*ast.FuncDecl)
		if !ok || fd.Name == nil || fd.Name.Name != fn {
			continue
		}
		found = true
		ast.Inspect(fd, func(n ast.Node) bool {
			cc, ok := n.(*ast.CaseClause)
			if !ok {
				return true
			}
			for _, expr := range cc.List {
				sel, ok := expr.(*ast.SelectorExpr)
				if !ok {
					continue
				}
				pkg, ok := sel.X.(*ast.Ident)
				if !ok {
					continue
				}
				out[pkg.Name+"."+sel.Sel.Name] = true
			}
			return true
		})
	}
	if !found {
		t.Fatalf("function %s not found in %s", fn, path)
	}
	if len(out) == 0 {
		t.Fatalf("%s in %s has no type-switch cases — the walk found nothing to check", fn, path)
	}
	return out
}

// tradeOfExempt registers trade-shaped event types that belong in
// pipeline.tradeFromEvent but deliberately NOT in tradeOf. Keep reasons.
var tradeOfExempt = map[string]string{
	"external.TradeEvent": "off-chain CEX/FX venues. They have no Soroban events and no ledger, so ch-rebuild (a lake re-derive) can never produce them; their writer is the external poller.",
}
