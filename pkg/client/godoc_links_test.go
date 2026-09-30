package client

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestGodocClientLinksResolve: a [Client.X] doc link to a method that
// does not exist renders as plain text on pkg.go.dev and sends SDK
// callers looking for an API the package never shipped.
func TestGodocClientLinksResolve(t *testing.T) {
	paths, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	methods := map[string]bool{}
	links := map[string][]string{}
	link := regexp.MustCompile(`\[Client\.(\w+)\]`)
	for _, p := range paths {
		if strings.HasSuffix(p, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, p, nil, parser.ParseComments)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range f.Decls {
			if fn, ok := decl.(*ast.FuncDecl); ok && fn.Recv != nil && receiverIsClient(fn.Recv) {
				methods[fn.Name.Name] = true
			}
		}
		for _, cg := range f.Comments {
			for _, m := range link.FindAllStringSubmatch(cg.Text(), -1) {
				links[m[1]] = append(links[m[1]], fset.Position(cg.Pos()).String())
			}
		}
	}
	if len(methods) == 0 {
		t.Fatal("no *Client methods found — the scan is broken")
	}
	for name, at := range links {
		if !methods[name] {
			t.Errorf("[Client.%s] has no matching method; referenced at %v", name, at)
		}
	}
}

func receiverIsClient(recv *ast.FieldList) bool {
	typ := recv.List[0].Type
	if star, ok := typ.(*ast.StarExpr); ok {
		typ = star.X
	}
	id, ok := typ.(*ast.Ident)
	return ok && id.Name == "Client"
}
