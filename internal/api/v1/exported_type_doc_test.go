package v1

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"
)

// docCommentCleanFiles are the files whose exported types must all
// carry a doc comment; the rest of the package has not been swept yet.
var docCommentCleanFiles = []string{"anomalies.go"}

// TestExportedTypesHaveDocComments requires every exported type in
// docCommentCleanFiles to carry a doc comment starting with its name.
// These are wire types, and golangci does not enable revive's
// `exported` rule, so nothing else catches a missing one.
func TestExportedTypesHaveDocComments(t *testing.T) {
	var checked int
	for _, name := range docCommentCleanFiles {
		f, err := parser.ParseFile(token.NewFileSet(), name, nil, parser.ParseComments)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, decl := range f.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok || gd.Tok != token.TYPE {
				continue
			}
			for _, spec := range gd.Specs {
				ts, ok := spec.(*ast.TypeSpec)
				if !ok || !ts.Name.IsExported() {
					continue
				}
				checked++
				doc := ts.Doc
				// An ungrouped `type T …` attaches its comment to the GenDecl.
				if doc == nil && !gd.Lparen.IsValid() {
					doc = gd.Doc
				}
				if doc == nil || !strings.HasPrefix(doc.Text(), ts.Name.Name+" ") {
					t.Errorf("%s: exported type %s lacks a doc comment starting %q", name, ts.Name.Name, ts.Name.Name+" ")
				}
			}
		}
	}
	if checked == 0 {
		t.Fatal("no exported types found; the scan did not run")
	}
}
