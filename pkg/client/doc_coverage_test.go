package client

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

// TestDocCoverageNamesEveryMethod: doc.go's Coverage section is the SDK's
// table of contents; a method missing from it is invisible to readers who
// never open the full godoc index.
func TestDocCoverageNamesEveryMethod(t *testing.T) {
	src, err := os.ReadFile("doc.go")
	if err != nil {
		t.Fatal(err)
	}
	doc := string(src)
	start := strings.Index(doc, "// # Coverage")
	if start < 0 {
		t.Fatal("doc.go has no # Coverage section — re-derive this test")
	}
	section := doc[start:]
	if end := strings.Index(section, "NOT every server endpoint"); end >= 0 {
		section = section[:end]
	}
	named := map[string]bool{}
	for _, w := range regexp.MustCompile(`[A-Za-z0-9]+`).FindAllString(section, -1) {
		named[w] = true
	}

	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	var methods int
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range f.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok || fd.Recv == nil || !fd.Name.IsExported() || !isClientRecv(fd.Recv) {
				continue
			}
			methods++
			if !named[fd.Name.Name] {
				t.Errorf("Client.%s (%s) is not named in doc.go's # Coverage section", fd.Name.Name, name)
			}
		}
	}
	if methods == 0 {
		t.Fatal("found no exported *Client methods — re-derive this test")
	}
}

func isClientRecv(recv *ast.FieldList) bool {
	if len(recv.List) != 1 {
		return false
	}
	star, ok := recv.List[0].Type.(*ast.StarExpr)
	if !ok {
		return false
	}
	id, ok := star.X.(*ast.Ident)
	return ok && id.Name == "Client"
}

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
