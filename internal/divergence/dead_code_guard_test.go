package divergence

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestNoDeadReferenceHelpers guards against the two dead functions
// GH-1004(e) found reachable only from tests reappearing with no
// production caller: decodeChainlinkInt256 (a fourth duplicate int256
// decoder — the live path decodes inline in fetchChainlinkAnswer) and
// CoinGeckoReference.LookupPrices (superseded by the per-pair
// LookupPrice path every [Reference] caller actually uses). Both were
// deleted rather than wired, per the issue's fix direction; this test
// is the "cheap guard for the recurrence" the issue asked for.
func TestNoDeadReferenceHelpers(t *testing.T) {
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(info fs.FileInfo) bool {
		return !strings.HasSuffix(info.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatalf("ParseDir(internal/divergence): %v", err)
	}
	for _, pkg := range pkgs {
		for fname, file := range pkg.Files {
			for _, decl := range file.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok {
					continue
				}
				if fn.Name.Name == "decodeChainlinkInt256" {
					t.Errorf("%s: decodeChainlinkInt256 reintroduced with no production caller (GH-1004e)", fname)
				}
				if fn.Recv != nil && fn.Name.Name == "LookupPrices" {
					t.Errorf("%s: (*CoinGeckoReference).LookupPrices reintroduced with no production caller (GH-1004e)", fname)
				}
			}
		}
	}
}

// TestNoStaleDivergenceCrossReferences fails when a comment or doc cites
// a divergence symbol that no longer exists: the API binary's reference
// builder and the deleted int256 helper (GH-1004).
func TestNoStaleDivergenceCrossReferences(t *testing.T) {
	stale := []string{
		"cmd/stellarindex-api/main.go::" + "buildDivergenceReferences",
		"API binary's " + "helper",
		"decodeChainlink" + "Int256",
	}
	self, err := filepath.Abs("dead_code_guard_test.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, root := range []string{"../../cmd", "../../internal", "../../docs/architecture"} {
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			if ext := filepath.Ext(path); ext != ".go" && ext != ".md" {
				return nil
			}
			if abs, _ := filepath.Abs(path); abs == self {
				return nil
			}
			b, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			for _, s := range stale {
				if strings.Contains(string(b), s) {
					t.Errorf("%s cites %q, which no longer exists; the divergence references are built only by cmd/stellarindex-aggregator/main.go::buildDivergenceReferences", path, s)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", root, err)
		}
	}
}
