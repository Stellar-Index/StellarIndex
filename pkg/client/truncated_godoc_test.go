package client

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"
)

// TestTruncatedGodocsMatchServerSemantics: when a window hits the trade
// cap the server drops the OLDEST rows (internal/api/v1/ohlc.go; the
// spec's OHLCBar.truncated says the same), so Close is exact and the
// early-window values are the suspect ones. The SDK godocs once said the
// opposite ("chronologically-first N trades"), steering callers to trust
// exactly the wrong fields. Pin the godoc to the spec's direction.
func TestTruncatedGodocsMatchServerSemantics(t *testing.T) {
	spec := loadSpec(t)
	bar := resolveRef(spec, "#/components/schemas/OHLCBar")
	props, _ := bar["properties"].(map[string]any)
	truncated, _ := props["truncated"].(map[string]any)
	specDesc, _ := truncated["description"].(string)
	if !strings.Contains(strings.ToLower(specDesc), "drops the oldest") {
		t.Fatalf("spec OHLCBar.truncated no longer says the oldest rows are dropped (%q) — re-derive this test", specDesc)
	}

	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "types.go", nil, parser.ParseComments)
	if err != nil {
		t.Fatal(err)
	}
	docs := map[string]string{}
	for _, decl := range f.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok || gd.Doc == nil {
			continue
		}
		for _, spec := range gd.Specs {
			if ts, ok := spec.(*ast.TypeSpec); ok {
				docs[ts.Name.Name] = gd.Doc.Text()
			}
		}
	}
	for _, typ := range []string{"OHLCBar", "VWAPResult"} {
		doc := strings.Join(strings.Fields(docs[typ]), " ")
		if !strings.Contains(doc, "drops the OLDEST") && !strings.Contains(doc, "drop the OLDEST") {
			t.Errorf("%s godoc must say the server drops the OLDEST trades on truncation: %q", typ, doc)
		}
		if strings.Contains(doc, "chronologically-first") {
			t.Errorf("%s godoc says the chronologically-first trades are kept — inverted: %q", typ, doc)
		}
	}
}
