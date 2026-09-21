package client_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"
)

// TestRevokeKeyDoc_Mentions409 — T528. The server's DELETE
// /v1/account/keys/{keyID} handler (internal/api/v1/account.go,
// handleAccountKeysRevoke) returns a distinct 409 when keyID names
// the credential the request itself authenticated with. RevokeKey's
// godoc must document that status alongside 401/403/404 so an SDK
// caller reading the doc knows to handle it, rather than treating it
// as an unexpected APIError. Parses the source directly so the
// assertion is on the actual shipped doc comment, not a copy of it.
func TestRevokeKeyDoc_Mentions409(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "endpoints.go", nil, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse endpoints.go: %v", err)
	}
	var doc string
	var found bool
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "RevokeKey" || fn.Doc == nil {
			continue
		}
		found = true
		doc = fn.Doc.Text()
	}
	if !found {
		t.Fatal("RevokeKey function (with a doc comment) not found in endpoints.go")
	}
	if !strings.Contains(doc, "409") {
		t.Errorf("RevokeKey doc does not mention 409, only documents:\n%s", doc)
	}
}
