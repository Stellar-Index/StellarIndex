package metadata_test

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

// TestConstructorsHaveProductionCaller fails when an exported New* in this
// package has no non-test caller outside it. A read-through cache here once
// shipped with tests, a metric, a Redis key family and runbooks while no
// binary ever constructed it, so every operator signal it fed was dark.
func TestConstructorsHaveProductionCaller(t *testing.T) {
	root := moduleRoot(t)
	ctors := exportedConstructors(t, filepath.Join(root, "internal", "metadata"))
	if len(ctors) == 0 {
		t.Fatal("found no exported New* constructors in internal/metadata; the scan is broken")
	}
	callers := productionSources(t, root)
	for _, name := range ctors {
		needle := "metadata." + name + "("
		found := false
		for _, src := range callers {
			if strings.Contains(src, needle) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("metadata.%s has no non-test caller outside internal/metadata: wire it into a binary or delete it", name)
		}
	}
}

func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found above the test's working directory")
		}
		dir = parent
	}
}

func exportedConstructors(t *testing.T, pkgDir string) []string {
	t.Helper()
	pkgs, err := parser.ParseDir(token.NewFileSet(), pkgDir, func(fi fs.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, pkg := range pkgs {
		for _, f := range pkg.Files {
			for _, d := range f.Decls {
				fn, ok := d.(*ast.FuncDecl)
				if ok && fn.Recv == nil && fn.Name.IsExported() && strings.HasPrefix(fn.Name.Name, "New") {
					out = append(out, fn.Name.Name)
				}
			}
		}
	}
	return out
}

// productionSources returns every non-test .go file in the module outside
// internal/metadata. Nested agent worktrees (.claude) and fixtures are
// skipped so a checkout containing sibling copies of the repo cannot
// satisfy the guard on this copy's behalf.
func productionSources(t *testing.T, root string) []string {
	t.Helper()
	skip := map[string]bool{".git": true, ".claude": true, "testdata": true, "node_modules": true, "vendor": true, "web": true}
	self := filepath.Join(root, "internal", "metadata")
	var out []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path != root && (skip[d.Name()] || path == self) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		out = append(out, string(b))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}
