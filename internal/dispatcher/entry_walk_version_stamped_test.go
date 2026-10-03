package dispatcher

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
)

// TestEntryWalkVersionIsStampedByAWriter fails when EntryWalkVersion is
// referenced by nothing but its own declaration: then no row records the walk
// version and bumping it changes nothing the upsert guards compare.
func TestEntryWalkVersionIsStampedByAWriter(t *testing.T) {
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
		t.Errorf("EntryWalkVersion has %d non-test reference(s), all its declaration: no writer stamps the walk version", uses)
	}
}
