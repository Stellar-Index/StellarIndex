package timescale

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"
)

// TestSEP41SupplyCursorDoc_NamesTheProjectorsActualCursorWrite is the
// regression for a docstring left behind by F159 (7f2a32655): the
// sep41SupplyCursorSource / sep41SupplyCursorSub doc comment described the
// projector's cursor commit as `UpsertCursor(ctx, "projector", src.Name,
// commitTo)`. F159 moved that write to a compare-and-swap —
// internal/projector's commitCursor now calls Store.AdvanceCursorFrom, and
// UpsertCursor left the projector's store interface entirely — so a reader
// following the old comment to UpsertCursor would be pointed at a call the
// projector no longer makes for this cursor.
//
// Read the doc comment from the AST, anchored to the actual const decl,
// so the assertion cannot silently pass once the declaration moves, and
// cannot silently miss a reintroduced stale reference elsewhere in the
// file it was never scoped to check.
func TestSEP41SupplyCursorDoc_NamesTheProjectorsActualCursorWrite(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "sep41_supply_events.go", nil, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse sep41_supply_events.go: %v", err)
	}

	const constName = "sep41SupplyCursorSource"
	var doc *ast.CommentGroup
	for _, d := range file.Decls {
		gd, ok := d.(*ast.GenDecl)
		if !ok || gd.Tok != token.CONST {
			continue
		}
		for _, spec := range gd.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for _, name := range vs.Names {
				if name.Name == constName {
					doc = gd.Doc
				}
			}
		}
	}
	if doc == nil {
		t.Fatalf("%s is gone from sep41_supply_events.go (or lost its doc comment) — this guard has moved", constName)
	}
	text := doc.Text()

	// The stale claim named the exact call the projector used to make;
	// banning that literal call shape (rather than the bare identifier)
	// lets the comment go on mentioning UpsertCursor for contrast — e.g.
	// explaining why AdvanceCursorFrom replaced it — without failing this
	// guard.
	const staleCall = `UpsertCursor(ctx, "projector", src.Name, commitTo)`
	if strings.Contains(text, staleCall) {
		t.Errorf("%s doc comment still describes the projector's cursor commit as `%s` — "+
			"F159 (7f2a32655) moved that write to AdvanceCursorFrom (a compare-and-swap) at "+
			"internal/projector's commitCursor", constName, staleCall)
	}
	if !strings.Contains(text, "AdvanceCursorFrom") {
		t.Errorf("%s doc comment does not name AdvanceCursorFrom — a reader would not know the projector's "+
			"cursor commit is a compare-and-swap (F159) rather than a plain upsert", constName)
	}
}
