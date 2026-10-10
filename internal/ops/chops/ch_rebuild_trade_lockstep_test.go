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
	"sort"
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

// reconciliationTargetsBySource maps each catalogue source name to the
// set of table names ch-rebuild can re-derive for it.
func reconciliationTargetsBySource(t *testing.T) map[string]map[string]bool {
	t.Helper()
	cat, _, err := buildReconciliationCatalogue(config.Config{})
	if err != nil {
		t.Fatalf("buildReconciliationCatalogue: %v", err)
	}
	out := make(map[string]map[string]bool, len(cat))
	for _, src := range cat {
		tables := make(map[string]bool, len(src.targets)+1)
		tables["trades"] = true // every projected trade source also has an implicit trades target
		for _, tgt := range src.targets {
			tables[tgt.table] = true
		}
		out[src.name] = tables
	}
	return out
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// caseArmDeletes parses window_delete_sql's `case "$s" in ... esac` block
// and returns, for each case-arm label, the table names of every
// `DELETE FROM <table>` line nested inside that arm.
func caseArmDeletes(t *testing.T, path string) map[string][]string {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	text := string(body)
	start := strings.Index(text, `case "$s" in`)
	if start < 0 {
		t.Fatalf("case \"$s\" in ... esac block not found in %s", path)
	}
	esacRE := regexp.MustCompile(`\n\s*esac`)
	loc := esacRE.FindStringIndex(text[start:])
	if loc == nil {
		t.Fatalf("closing esac not found after case \"$s\" in %s", path)
	}
	block := text[start : start+loc[0]]
	deleteTable := regexp.MustCompile(`DELETE FROM (\w+)`)
	out := map[string][]string{}
	for _, arm := range strings.Split(block, ";;") {
		paren := strings.Index(arm, ")")
		if paren < 0 {
			continue
		}
		label := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(arm[:paren]), "\n"))
		if label == "" || strings.ContainsAny(label, " \t\n") {
			continue // not a bare case label (e.g. leftover from the previous arm's trailing text)
		}
		for _, m := range deleteTable.FindAllStringSubmatch(arm[paren:], -1) {
			out[label] = append(out[label], m[1])
		}
	}
	return out
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
