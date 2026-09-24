// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package orchestrator

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strconv"
	"strings"
	"testing"
)

// The producer's ceilings. A baseline window of D days holds at most
// D×1440 one-minute buckets, i.e. D×1440−1 bucket-to-bucket returns, so
// baselineAgeDays can never read above (43,199+1)/1440 = 30.
const (
	maxDay30Returns    = 30*1440 - 1
	maxBaselineAgeDays = float64(maxDay30Returns+1) / 1440
)

var maxReturnsByWindow = map[string]int{
	"Day1":  1*1440 - 1,
	"Day7":  7*1440 - 1,
	"Day30": maxDay30Returns,
}

// TestFixturesStayInsideTheProducerRange fails on a test fixture this
// package's producers cannot emit: a baseline.Baseline N above its
// window's return count, or a BaselineAgeDays above 30. Such a fixture
// once hid that the bootstrap cap could never lift in production.
func TestFixturesStayInsideTheProducerRange(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	fset := token.NewFileSet()
	scanned := 0
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, e.Name(), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", e.Name(), err)
		}
		scanned++
		for _, v := range fixtureRangeViolations(fset, f) {
			t.Error(v)
		}
	}
	if scanned == 0 {
		t.Fatal("scanned no test files; the guard is not looking at this package")
	}
}

func fixtureRangeViolations(fset *token.FileSet, f *ast.File) []string {
	var out []string
	windowed := map[*ast.CompositeLit]string{}
	ast.Inspect(f, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.KeyValueExpr:
			key, ok := n.Key.(*ast.Ident)
			if !ok {
				return true
			}
			if _, isWindow := maxReturnsByWindow[key.Name]; isWindow {
				if lit := baselineLit(n.Value); lit != nil {
					windowed[lit] = key.Name
				}
			}
			if key.Name == "BaselineAgeDays" {
				if v, ok := numericLit(n.Value); ok && v > maxBaselineAgeDays {
					out = append(out, fset.Position(n.Pos()).String()+": BaselineAgeDays "+
						strconv.FormatFloat(v, 'g', -1, 64)+" exceeds the producer's 30-day ceiling")
				}
			}
		case *ast.CompositeLit:
			if baselineLit(n) == nil {
				return true
			}
			window := windowed[n]
			if window == "" {
				window = "Day30"
			}
			if v, ok := baselineN(n); ok && v > float64(maxReturnsByWindow[window]) {
				out = append(out, fset.Position(n.Pos()).String()+": "+window+" baseline N "+
					strconv.FormatFloat(v, 'f', 0, 64)+" exceeds the window's "+
					strconv.Itoa(maxReturnsByWindow[window])+" returns")
			}
		}
		return true
	})
	return out
}

// baselineLit unwraps `baseline.Baseline{...}` or `&baseline.Baseline{...}`.
func baselineLit(e ast.Expr) *ast.CompositeLit {
	if u, ok := e.(*ast.UnaryExpr); ok && u.Op == token.AND {
		e = u.X
	}
	lit, ok := e.(*ast.CompositeLit)
	if !ok {
		return nil
	}
	sel, ok := lit.Type.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "Baseline" {
		return nil
	}
	if pkg, ok := sel.X.(*ast.Ident); !ok || pkg.Name != "baseline" {
		return nil
	}
	return lit
}

func baselineN(lit *ast.CompositeLit) (float64, bool) {
	for _, el := range lit.Elts {
		kv, ok := el.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		if key, ok := kv.Key.(*ast.Ident); ok && key.Name == "N" {
			return numericLit(kv.Value)
		}
	}
	return 0, false
}

func numericLit(e ast.Expr) (float64, bool) {
	lit, ok := e.(*ast.BasicLit)
	if !ok || (lit.Kind != token.INT && lit.Kind != token.FLOAT) {
		return 0, false
	}
	v, err := strconv.ParseFloat(strings.ReplaceAll(lit.Value, "_", ""), 64)
	return v, err == nil
}
