package timescale

import (
	"go/ast"
	"go/parser"
	"go/token"
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
