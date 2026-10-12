package postgresstore

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// TestNonNilStringArray_NilBecomesEmpty: a nil input must reach pgx as a non-nil empty slice (`'{}'`), since SQL NULL violates the NOT NULL on api_keys.referer_allowlist.
func TestNonNilStringArray_NilBecomesEmpty(t *testing.T) {
	got := nonNilStringArray(nil)
	if got == nil {
		t.Fatal("nonNilStringArray(nil) returned nil; want non-nil zero-length slice")
	}
	if len(got) != 0 {
		t.Errorf("len = %d, want 0", len(got))
	}
}

// TestNonNilStringArray_NonNilPassthrough pins that a non-nil slice passes through unchanged: the wrapper only defends against
// nil, never copies or mutates.
func TestNonNilStringArray_NonNilPassthrough(t *testing.T) {
	in := []string{"app.example.com", "console.example.com"}
	got := nonNilStringArray(in)
	if len(got) != len(in) {
		t.Errorf("len = %d, want %d", len(got), len(in))
	}
	if !reflect.DeepEqual(got, in) {
		t.Errorf("got %v, want %v", got, in)
	}
}

// TestNonNilStringArray_EmptyNonNilPassthrough: an explicit empty input is preserved as is.
func TestNonNilStringArray_EmptyNonNilPassthrough(t *testing.T) {
	got := nonNilStringArray([]string{})
	if got == nil {
		t.Fatal("nonNilStringArray([]string{}) returned nil; want non-nil zero-length")
	}
	if len(got) != 0 {
		t.Errorf("len = %d, want 0", len(got))
	}
}

// apiKeysBoundedMarkers are the shapes that keep an api_keys read independent of the forever-growing revoked history: an explicit LIMIT, an aggregate or EXISTS probe, a unique-key lookup, or the capped active set.
var apiKeysBoundedMarkers = []string{
	"LIMIT", "COUNT(", "EXISTS", "WHERE id = $", "WHERE key_hash = $", "revoked_at IS NULL",
}

// TestAPIKeysQueriesAreBounded lints that every SQL literal reading FROM api_keys is bounded; an unbounded per-account list
// put the whole revoked history on the mint path, dashboard list and admin suspend.
func TestAPIKeysQueriesAreBounded(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	fset := token.NewFileSet()
	checked := 0
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING || !strings.Contains(lit.Value, "FROM api_keys") {
				return true
			}
			// A DELETE reads nothing back; the lint is about result sets.
			if strings.HasPrefix(strings.TrimLeft(strings.Trim(lit.Value, "`\""), " \t\n"), "DELETE FROM api_keys") {
				return true
			}
			checked++
			for _, m := range apiKeysBoundedMarkers {
				if strings.Contains(lit.Value, m) {
					return true
				}
			}
			t.Errorf("%s: unbounded read FROM api_keys (add a LIMIT, an aggregate, or restrict to revoked_at IS NULL): %s",
				fset.Position(lit.Pos()), lit.Value)
			return true
		})
	}
	if checked == 0 {
		t.Fatal("no FROM api_keys literal found; the lint is not scanning the store")
	}
}
