package aggregate

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// TestFilterFreshAggregatorRows_LeavesInputUntouched pins that the
// freshness filter does not overwrite the caller's backing array with
// its survivors.
func TestFilterFreshAggregatorRows_LeavesInputUntouched(t *testing.T) {
	now := time.Now()
	rows := []canonical.OracleUpdate{
		{Source: "stale", Timestamp: now.Add(-time.Hour)},
		{Source: "fresh", Timestamp: now},
	}
	got := filterFreshAggregatorRows(rows, time.Minute)
	if len(got) != 1 || got[0].Source != "fresh" {
		t.Fatalf("filtered = %+v, want only the fresh row", got)
	}
	if rows[0].Source != "stale" || rows[1].Source != "fresh" {
		t.Errorf("input mutated to [%s %s], want [stale fresh]", rows[0].Source, rows[1].Source)
	}
}

// TestNoInPlaceSliceFilter fails on any `x[:0]` in non-test code under
// internal/aggregate. Filtering into x[:0] overwrites the caller's
// backing array, so a caller that keeps the pre-filter slice (for a
// drop count, a pre-filter total, a second pass) reads survivors where
// its dropped rows were. Allocate the output instead.
func TestNoInPlaceSliceFilter(t *testing.T) {
	var hits []string
	scanned := 0
	err := filepath.WalkDir(".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		fset := token.NewFileSet()
		f, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			return perr
		}
		scanned++
		ast.Inspect(f, func(n ast.Node) bool {
			if s, ok := n.(*ast.SliceExpr); ok && isZeroLenReslice(s) {
				hits = append(hits, fset.Position(s.Pos()).String())
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if scanned == 0 {
		t.Fatal("scanned 0 Go files; the guard is not looking at internal/aggregate")
	}
	for _, h := range hits {
		t.Errorf("%s: in-place filter over x[:0] aliases the caller's backing array; allocate the output", h)
	}
}

func isZeroLenReslice(s *ast.SliceExpr) bool {
	if s.Low != nil && !isIntLit(s.Low, "0") {
		return false
	}
	return s.High != nil && isIntLit(s.High, "0") && s.Max == nil
}

func isIntLit(e ast.Expr, v string) bool {
	lit, ok := e.(*ast.BasicLit)
	return ok && lit.Kind == token.INT && lit.Value == v
}
