package timescale

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

var (
	// An assignment opens a SET list or follows a comma inside one; a WHERE
	// comparison follows WHERE/AND/OR and is not a write.
	homeDomainAssignRE = regexp.MustCompile(`(?i)(\bSET|,)\s*home_domain\s*=[^=]`)
	issuersWriteRE     = regexp.MustCompile(`(?i)\b(UPDATE|INTO)\s+issuers\b`)
)

// TestEveryIssuersHomeDomainWriterUnbindsSep1 fails when a statement that
// writes issuers.home_domain does not route through
// sep1ResetOnHomeDomainChange. The executing proof is
// test/integration/issuer_home_domain_sep1_unbind_test.go; this catches the
// next writer, which that test cannot know about.
func TestEveryIssuersHomeDomainWriterUnbindsSep1(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	writers := 0
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, decl := range f.Decls {
			assigns, touchesIssuers, resets := scanHomeDomainDecl(decl)
			if !assigns || !touchesIssuers {
				continue
			}
			writers++
			if !resets {
				t.Errorf("%s: a statement writes issuers.home_domain without sep1ResetOnHomeDomainChange — "+
					"the old domain's SEP-1 payload would be served under the new one", fset.Position(decl.Pos()))
			}
		}
	}
	if writers == 0 {
		t.Fatal("found no issuers.home_domain writer at all; the scan is not seeing the package")
	}
}

func scanHomeDomainDecl(decl ast.Decl) (assigns, touchesIssuers, resets bool) {
	ast.Inspect(decl, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.BasicLit:
			if x.Kind == token.STRING {
				assigns = assigns || homeDomainAssignRE.MatchString(x.Value)
				touchesIssuers = touchesIssuers || issuersWriteRE.MatchString(x.Value)
			}
		case *ast.Ident:
			resets = resets || x.Name == "sep1ResetOnHomeDomainChange"
		}
		return true
	})
	return assigns, touchesIssuers, resets
}
