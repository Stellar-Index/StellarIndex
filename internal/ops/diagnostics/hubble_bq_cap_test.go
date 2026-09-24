package diagnostics

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"cloud.google.com/go/bigquery"
	"google.golang.org/api/option"
)

// TestHubbleQueriesAllCarryByteCap pins the chokepoint: the only BigQuery
// Query(...) call in this package's non-test code is inside hubbleBQ.query,
// which stamps MaxBytesBilled. A direct client.Query elsewhere would run
// uncapped against a multi-TB public table.
func TestHubbleQueriesAllCarryByteCap(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	var direct []string
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		file, err := parser.ParseFile(fset, f, src, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil || isHubbleBQQuery(fn) {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Query" {
					direct = append(direct, fset.Position(call.Pos()).String())
				}
				return true
			})
		}
	}
	if len(direct) > 0 {
		t.Fatalf("BigQuery Query() built outside hubbleBQ.query, so it carries no MaxBytesBilled: %v", direct)
	}
}

func isHubbleBQQuery(fn *ast.FuncDecl) bool {
	if fn.Name.Name != "query" || fn.Recv == nil || len(fn.Recv.List) != 1 {
		return false
	}
	id, ok := fn.Recv.List[0].Type.(*ast.Ident)
	return ok && id.Name == "hubbleBQ"
}

func TestHubbleBQQueryStampsCap(t *testing.T) {
	client, err := bigquery.NewClient(context.Background(), "p", option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close() }()
	q := hubbleBQ{client: client, maxBytes: 12345}.query("SELECT 1")
	if q.MaxBytesBilled != 12345 {
		t.Fatalf("MaxBytesBilled = %d, want 12345", q.MaxBytesBilled)
	}
	if defaultMaxBytesBilled < 1 {
		t.Fatalf("defaultMaxBytesBilled = %d; BigQuery reads < 1 as uncapped", defaultMaxBytesBilled)
	}
}
