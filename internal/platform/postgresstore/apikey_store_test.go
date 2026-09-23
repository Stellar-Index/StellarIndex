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

// TestNonNilStringArray_NilBecomesEmpty — a nil input must
// surface to the pgx driver as a non-nil zero-length
// []string, which serialises to the SQL `'{}'` array
// literal. F-1262 (codex audit-2026-05-13): pre-fix a
// bare nil `[]string` emitted SQL NULL, which
// violated the migration-0027 NOT NULL constraint on
// `api_keys.referer_allowlist` and surfaced as a 500 on the
// default dashboard create-key request shape.
func TestNonNilStringArray_NilBecomesEmpty(t *testing.T) {
	got := nonNilStringArray(nil)
	if got == nil {
		t.Fatal("nonNilStringArray(nil) returned nil; want non-nil zero-length slice")
	}
	if len(got) != 0 {
		t.Errorf("len = %d, want 0", len(got))
	}
}

// TestNonNilStringArray_NonNilPassthrough — a non-nil input
// round-trips through unchanged. The wrapper is purely a
// nil-defence boundary; it must NOT mutate or copy values
// when the slice is already non-nil.
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

// TestNonNilStringArray_EmptyNonNilPassthrough — an explicit
// empty (non-nil) input is preserved as zero-length. This is
// the case the audit cared about least (it's already a safe
// shape) but the helper must not accidentally allocate a new
// array literal from it.
func TestNonNilStringArray_EmptyNonNilPassthrough(t *testing.T) {
	got := nonNilStringArray([]string{})
	if got == nil {
		t.Fatal("nonNilStringArray([]string{}) returned nil; want non-nil zero-length")
	}
	if len(got) != 0 {
		t.Errorf("len = %d, want 0", len(got))
	}
}

// apiKeysBoundedMarkers are the shapes that keep a read of api_keys
// independent of the revoked history, which is kept forever and grows
// without bound under a create/revoke loop (GH-766): an explicit LIMIT, an
// aggregate or EXISTS probe, a unique-key lookup, or the active set (capped
// per account at insert time).
var apiKeysBoundedMarkers = []string{
	"LIMIT", "COUNT(", "EXISTS", "WHERE id = $", "WHERE key_hash = $", "revoked_at IS NULL",
}

// TestAPIKeysQueriesAreBounded is the lint behind GH-766: every SQL literal
// in this package that reads FROM api_keys must be bounded. An unbounded
// per-account list put the whole revoked history on the mint path, the
// dashboard list and the admin suspend kill switch.
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
