// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package chops

// ch_rebuild.go's tradeOf mirrors pipeline.tradeFromEvent by hand, and its
// doc comment says so — but nothing checked it, and the drift is not
// harmless. `scripts/ops/ch-rebuild-projected.sh` is the sanctioned
// clean-slate key repair: it DELETEs a window of trades for the projected
// trade sources and then re-derives it with `ch-rebuild -write`. A trade
// source present in that DELETE list but missing from tradeOf has its rows
// deleted and never rewritten — silent data loss, in the one procedure
// operators reach for when trades are already wrong.
//
// This walks both switches with go/ast, the same way
// pipeline/lockstep_ast_test.go walks its five sites.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/config"
)

// tradeOfExempt registers trade-shaped event types that belong in
// pipeline.tradeFromEvent but deliberately NOT in tradeOf. Keep reasons.
var tradeOfExempt = map[string]string{
	"external.TradeEvent": "off-chain CEX/FX venues. They have no Soroban events and no ledger, so ch-rebuild (a lake re-derive) can never produce them; their writer is the external poller.",
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

// TestLockstep_ChRebuildTradeOfCoversEveryProjectedTradeSource is the guard.
func TestLockstep_ChRebuildTradeOfCoversEveryProjectedTradeSource(t *testing.T) {
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

// TestChRebuildProjectedScript_DeletesOnlySourcesTradeOfCanRewrite pins the
// other half of the pairing: every source named in the repair script's
// trades DELETE must be a source ch-rebuild can actually re-derive.
func TestChRebuildProjectedScript_DeletesOnlySourcesTradeOfCanRewrite(t *testing.T) {
	deleted := deletedTradeSources(t, "../../../scripts/ops/ch-rebuild-projected.sh")
	if len(deleted) == 0 {
		t.Fatal("no trades DELETE source list found in ch-rebuild-projected.sh")
	}
	cat, _, err := buildReconciliationCatalogue(config.Config{})
	if err != nil {
		t.Fatalf("buildReconciliationCatalogue: %v", err)
	}
	known := map[string]bool{}
	for _, src := range cat {
		known[src.name] = true
	}
	for _, name := range deleted {
		if !known[name] {
			t.Errorf("ch-rebuild-projected.sh deletes trades for %q, which is not in the reconciliation catalogue — "+
				"the repair would delete rows nothing re-derives", name)
		}
	}
}

// deletedTradeSources reads the source names out of the repair script's
// `DELETE FROM trades WHERE source IN (...)` statement.
func deletedTradeSources(t *testing.T, path string) []string {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	m := regexp.MustCompile(`DELETE FROM trades WHERE source IN \(([^)]*)\)`).FindSubmatch(body)
	if m == nil {
		return nil
	}
	var out []string
	for _, raw := range strings.Split(string(m[1]), ",") {
		if name := strings.Trim(strings.TrimSpace(raw), "'"); name != "" {
			out = append(out, name)
		}
	}
	return out
}
