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
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/config"
)

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
