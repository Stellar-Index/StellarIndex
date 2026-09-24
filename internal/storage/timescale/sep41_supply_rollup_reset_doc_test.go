package timescale

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestResetSEP41SupplyRollupFoldDoc_NamesBothCallers is the regression
// for the residual half of finding F107 (audit 2026-09-02, duplicate of
// F024's core defect, which internal/ops/ingest's
// resetSEP41RollupAfterReplay + reportSEP41RollupReset already fixed):
// [Store.ResetSEP41SupplyRollupFold]'s own docstring described only
// `ch-rebuild -sep41 -write` as a caller, even after `stellarindex-ops
// projector-replay -source sep41_supply` became the second one. An
// operator reading the function's doc to decide whether a recovery path
// needs a fold reset would see exactly one prescribed command and could
// reasonably conclude the projector's replay path was exempt — which is
// the same "docstring undersells its own callers" gap the audit's
// skeptic pass called out by name.
//
// Read the doc comment from the AST rather than grep the raw source so
// the assertion is anchored to the function's actual doc comment (and
// not, say, a stray mention of "projector-replay" anywhere else in the
// file) and cannot silently pass once the function moves or is renamed.
func TestResetSEP41SupplyRollupFoldDoc_NamesBothCallers(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "sep41_supply_events.go", nil, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse sep41_supply_events.go: %v", err)
	}

	const fnName = "ResetSEP41SupplyRollupFold"
	var doc *ast.CommentGroup
	for _, d := range file.Decls {
		fn, ok := d.(*ast.FuncDecl)
		if !ok || fn.Name.Name != fnName {
			continue
		}
		doc = fn.Doc
	}
	if doc == nil {
		t.Fatalf("%s is gone from sep41_supply_events.go (or lost its doc comment) — this guard has moved", fnName)
	}
	text := doc.Text()

	for _, want := range []string{"ch-rebuild", "projector-replay"} {
		if !strings.Contains(text, want) {
			t.Errorf("%s doc comment does not mention %q as a caller — an operator reading this doc "+
				"cannot tell that BOTH recovery paths require this reset (F024/F107)", fnName, want)
		}
	}
}

// TestSEP41SupplyRollupMigrationsNeverPrescribeDestructiveReset pins the
// migrations' own guidance to the non-destructive reset (T364). Migration
// 0085's header told operators a below-checkpoint re-derive "requires a
// `TRUNCATE sep41_supply_rollup`", which also wipes migration 0088's seeded
// genesis baseline and silently under-reports supply.
func TestSEP41SupplyRollupMigrationsNeverPrescribeDestructiveReset(t *testing.T) {
	paths, err := filepath.Glob("../../../migrations/*.sql")
	if err != nil || len(paths) == 0 {
		t.Fatalf("glob migrations: %d files, err=%v", len(paths), err)
	}
	destructive := regexp.MustCompile("(?i)(TRUNCATE\\s+(TABLE\\s+)?|DELETE\\s+FROM\\s+)`?sep41_supply_rollup")
	for _, p := range paths {
		raw, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("read %s: %v", p, err)
		}
		if loc := destructive.FindIndex(raw); loc != nil {
			t.Errorf("%s: %q — never TRUNCATE/DELETE sep41_supply_rollup; it drops the migration-0088 "+
				"genesis baseline. Reset the fold via Store.ResetSEP41SupplyRollupFold instead",
				filepath.Base(p), raw[loc[0]:loc[1]])
		}
	}

	header, err := os.ReadFile("../../../migrations/0085_create_sep41_supply_rollup.up.sql")
	if err != nil {
		t.Fatalf("read migration 0085: %v", err)
	}
	if !strings.Contains(string(header), "ResetSEP41SupplyRollupFold") {
		t.Error("migration 0085's header no longer names ResetSEP41SupplyRollupFold as the below-checkpoint reset")
	}
}
