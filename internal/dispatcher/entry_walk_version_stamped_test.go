package dispatcher

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

// TestEntryWalkVersionIsStampedByAWriter reproduces #1266: EntryWalkVersion
// is referenced by no non-test code besides its own declaration, so no row
// carries the walk version and bumping it changes nothing the guards compare.
// It is red until a writer stamps the version (the walk_version column on the
// observation tables and the ClickHouse version word), a schema change this
// test does not make. Run with GH1266_REPRO=1.
func TestEntryWalkVersionIsStampedByAWriter(t *testing.T) {
	if os.Getenv("GH1266_REPRO") == "" {
		t.Skip("reproduction of open issue #1266; set GH1266_REPRO=1 to run")
	}
	root := filepath.Join("..", "..")
	uses := 0
	fset := token.NewFileSet()
	for _, dir := range []string{"internal", "cmd", "pkg"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return err
			}
			f, perr := parser.ParseFile(fset, path, nil, 0)
			if perr != nil {
				return perr
			}
			ast.Inspect(f, func(n ast.Node) bool {
				if id, ok := n.(*ast.Ident); ok && id.Name == "EntryWalkVersion" {
					uses++
				}
				return true
			})
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", dir, err)
		}
	}
	// One identifier is the declaration itself.
	if uses < 2 {
		t.Errorf("EntryWalkVersion has %d non-test reference(s), all its declaration: no writer stamps the walk version (#1266)", uses)
	}
}
