package main

// Source-level tripwire (the freeze_wiring_guard_test.go pattern): the
// point-in-time price reader must apply the serving-sanity guard to a
// prices_1m answer.
//
// Finding F031: storePriceAtReader.PriceAt is the seam behind
// /v1/price/at AND every /v1/price/changes horizon, and
// ClosedVWAPAtOrBefore's ladder puts the raw prices_1m CAGG bucket
// FIRST for any instant inside 48 h — the same bare Σ(quote)/Σ(base)
// bucket /v1/price serves. It carried the withholding gates but NOT
// pricingguard, so one extra path segment republished the manipulated
// minute /v1/price refuses, with pricingguard's own package doc
// claiming it covered "every raw prices_1m closed-bucket serving path".
//
// LatestPrice-style end-to-end coverage is not available from this
// package (storePriceAtReader.s is a concrete *timescale.Store with an
// unexported db field — see price_freshness_seam_test.go), so this
// walks main.go's AST and fails if the guard call leaves PriceAt again.
// The decision itself is proven in internal/pricingguard.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestPriceAtAppliesServedVWAPGuard(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "main.go", nil, 0)
	if err != nil {
		t.Fatalf("parse main.go: %v", err)
	}

	fn := findMethod(f, "storePriceAtReader", "PriceAt")
	if fn == nil {
		t.Fatal("could not locate storePriceAtReader.PriceAt in main.go — " +
			"update this guard to follow the refactor rather than deleting it")
	}
	if !bodyCallsPkgFunc(fn.Body, "pricingguard", "GuardServedVWAP1mAt") {
		t.Error("storePriceAtReader.PriceAt no longer applies " +
			"pricingguard.GuardServedVWAP1mAt — /v1/price/at and every " +
			"/v1/price/changes horizon would serve the raw prices_1m bucket " +
			"unfiltered again (F031): a single manipulated print in the " +
			"served minute, republished as a confident price")
	}
	// The guard is only meaningful on the 1m rung; the coarser rungs are
	// hour/day bars a trailing 1-minute baseline cannot judge.
	if !bodyMentionsSelector(fn.Body, "timescale", "Granularity1m") {
		t.Error("storePriceAtReader.PriceAt no longer scopes the guard to " +
			"timescale.Granularity1m — either the 1m rung is unguarded or a " +
			"coarse bar is being judged against a 1-minute baseline")
	}
	// A refused bucket exists: it must surface as withheld, never as the
	// no-data sentinel the handlers render as "no closed bucket".
	if !bodyMentionsSelector(fn.Body, "v1", "ErrPriceAtGuarded") {
		t.Error("storePriceAtReader.PriceAt does not return v1.ErrPriceAtGuarded " +
			"when the guard refuses the bucket — a withheld instant would read " +
			"as no data on /v1/price/at and as a young pair on /v1/price/changes")
	}
}

// rawPrices1mReads are the timescale.Store methods that return a bare
// prices_1m CAGG bucket (or series) with no outlier filter.
var rawPrices1mReads = []string{
	"LatestClosedVWAP1mForPair", "RecentClosedVWAP1mForPair",
	"ClosedVWAP1mAtOrBefore", "ClosedVWAPAtOrBefore",
	"RecentClosedVWAP1mCombined", "ClosedVWAP1mCombinedBefore",
}

// guardEntryPoints are the pricingguard functions that judge such a read.
var guardEntryPoints = []string{
	"GuardServedVWAP1m", "GuardServedVWAP1mConfidence",
	"GuardServedVWAP1mAt", "GuardServedVWAP1mSeries",
}

// TestRawPrices1mReadersPassTheGuard: every function under cmd/ that calls
// a raw prices_1m store read must call a pricingguard entry point in the
// same body, and be named in pricingguard's package-doc enumeration — so a
// new raw-bucket reader cannot reach a response unguarded or undocumented.
func TestRawPrices1mReadersPassTheGuard(t *testing.T) {
	doc, err := os.ReadFile(filepath.Join("..", "..", "internal", "pricingguard", "guard.go"))
	if err != nil {
		t.Fatalf("read pricingguard doc: %v", err)
	}
	files, err := filepath.Glob(filepath.Join("..", "*", "*.go"))
	if err != nil {
		t.Fatal(err)
	}
	readers := 0
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		for _, d := range f.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Body == nil || !bodyCallsAnyMethod(fn.Body, rawPrices1mReads) {
				continue
			}
			readers++
			name := funcDisplayName(fn)
			guarded := false
			for _, g := range guardEntryPoints {
				guarded = guarded || bodyCallsPkgFunc(fn.Body, "pricingguard", g)
			}
			if !guarded {
				t.Errorf("%s: %s reads a raw prices_1m bucket without a pricingguard entry point — "+
					"a single manipulated minute would reach the response unfiltered", path, name)
			}
			if !strings.Contains(string(doc), name) {
				t.Errorf("%s: %s is a raw prices_1m reader missing from pricingguard's WIRED call-site list", path, name)
			}
		}
	}
	if readers == 0 {
		t.Fatal("found no raw prices_1m reader under cmd/ — the scan is not reading the sources")
	}
}

// bodyCallsAnyMethod reports whether body calls x.m for any m in names.
func bodyCallsAnyMethod(body *ast.BlockStmt, names []string) bool {
	found := false
	ast.Inspect(body, func(n ast.Node) bool {
		if call, ok := n.(*ast.CallExpr); ok {
			if sel, ok := call.Fun.(*ast.SelectorExpr); ok && slices.Contains(names, sel.Sel.Name) {
				found = true
			}
		}
		return !found
	})
	return found
}

// funcDisplayName renders fn as Recv.Name (or Name for a plain function).
func funcDisplayName(fn *ast.FuncDecl) string {
	if fn.Recv == nil || len(fn.Recv.List) != 1 {
		return fn.Name.Name
	}
	t := fn.Recv.List[0].Type
	if star, ok := t.(*ast.StarExpr); ok {
		t = star.X
	}
	if id, ok := t.(*ast.Ident); ok {
		return id.Name + "." + fn.Name.Name
	}
	return fn.Name.Name
}

// findMethod returns the declaration of method `name` on receiver type
// `recv` (value or pointer), or nil.
func findMethod(f *ast.File, recv, name string) *ast.FuncDecl {
	for _, d := range f.Decls {
		fn, ok := d.(*ast.FuncDecl)
		if !ok || fn.Recv == nil || fn.Name.Name != name || len(fn.Recv.List) != 1 {
			continue
		}
		t := fn.Recv.List[0].Type
		if star, ok := t.(*ast.StarExpr); ok {
			t = star.X
		}
		if id, ok := t.(*ast.Ident); ok && id.Name == recv {
			return fn
		}
	}
	return nil
}

// bodyCallsPkgFunc reports whether body contains a call to pkg.fn.
func bodyCallsPkgFunc(body *ast.BlockStmt, pkg, fn string) bool {
	called := false
	ast.Inspect(body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == fn {
			if id, ok := sel.X.(*ast.Ident); ok && id.Name == pkg {
				called = true
				return false
			}
		}
		return true
	})
	return called
}

// bodyMentionsSelector reports whether body references pkg.name.
func bodyMentionsSelector(body *ast.BlockStmt, pkg, name string) bool {
	found := false
	ast.Inspect(body, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if id, ok := sel.X.(*ast.Ident); ok && id.Name == pkg && sel.Sel.Name == name {
			found = true
			return false
		}
		return true
	})
	return found
}
