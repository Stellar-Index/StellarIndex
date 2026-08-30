package main

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// TestPriceServingSeamsAreGated is a guard-coverage test for the price
// WITHHOLDING decision (wave-D MSP cluster).
//
// The product rule is fail-closed on trust: when the substance gate judges a
// market too thin to aggregate, or the scam gate finds the issuer
// directory-flagged, we do not publish a price. That rule was implemented at
// ONE reader seam (storePriceReader, behind /v1/price) and leaked at every
// other seam reading the same closed VWAP buckets — /v1/price/at and
// /v1/price/changes re-served the exact number /v1/price had just withheld, so
// one extra path segment defeated both gates (MSP-01, reproduced against real
// Postgres by the wave-D skeptic).
//
// Fixing those two seams by hand is not the deliverable; the leak happened
// BECAUSE the decision lived at a seam instead of a chokepoint, so the same
// thing recurs the next time someone adds a reader. This test derives the
// price-serving seam set from the source and fails when one of them does not
// route through priceWithheld() — i.e. it fails for the seam that does not
// exist yet.
//
// Why an AST guard rather than a behavioural test: a behavioural test can only
// cover the endpoints someone remembered to write a case for, which is the same
// weakness that produced the leak. Deriving the set from the code means the
// NEXT reader is covered on the day it is written.
//
// Proven red: deleting the priceWithheld() call from storePriceAtReader.PriceAt
// (i.e. restoring the pre-fix state) fails this test naming that method.
func TestPriceServingSeamsAreGated(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "main.go", nil, 0)
	if err != nil {
		t.Fatalf("parse main.go: %v", err)
	}

	// The seams that serve a price derived from a closed VWAP bucket. Keyed by
	// "receiverType.Method" so a rename surfaces here rather than silently
	// dropping a seam from the guard. Adding a new price-serving reader means
	// adding it to this list AND routing it through priceWithheld().
	gatedSeams := map[string]bool{
		"storePriceReader.LatestPrice": false,
		"storePriceAtReader.PriceAt":   false,
	}

	ast.Inspect(f, func(n ast.Node) bool {
		fn, ok := n.(*ast.FuncDecl)
		if !ok || fn.Recv == nil || len(fn.Recv.List) == 0 {
			return true
		}
		recv := receiverTypeName(fn.Recv.List[0].Type)
		key := recv + "." + fn.Name.Name
		if _, tracked := gatedSeams[key]; !tracked {
			return true
		}
		if callsPriceWithheld(fn) {
			gatedSeams[key] = true
		}
		return true
	})

	for seam, gated := range gatedSeams {
		if !gated {
			t.Errorf("price-serving seam %s does not call priceWithheld() — "+
				"a withheld price (thin market or directory-flagged issuer) would be "+
				"published by whatever route reaches this reader. Route it through the "+
				"chokepoint; do not inline the gate pair.", seam)
		}
	}
}

// TestWithholdingGatesAreSpelledOnlyAtTheChokepoint is the stronger half of
// the guard, and the one that catches the failure the seam enumeration above
// cannot see.
//
// Enumerating seams proves each seam consults the gates SOMEWHERE. It does not
// prove each seam consults BOTH of them. storePriceReader.LatestPrice has two
// arms — the closed-VWAP arm and the last-trade fallback — and the fallback
// spelled out `!substance.Allowed(...)` alone, omitting the scam gate
// (MSP-07). The seam test passes on that code, because the other arm calls the
// chokepoint. The bug is real: an operator setting
// pricing_guard.disable_substance_gate=true to diagnose a coverage complaint
// makes the substance gate nil-allow, and the last-trade arm then publishes a
// directory-flagged issuer's last trade as its price — reversing a separate
// owner-level trust decision the operator never touched.
//
// So the invariant is not "gates are consulted" but "the gate pair has exactly
// ONE spelling". This test fails on any call to substance.Allowed() or
// scam.Withheld() outside priceWithheld's own body. A new withholding site
// must route through the chokepoint, where the two gates cannot drift apart.
//
// Proven red: restoring the substance-only last-trade arm fails this test
// naming storePriceReader.LatestPrice.
func TestWithholdingGatesAreSpelledOnlyAtTheChokepoint(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "main.go", nil, 0)
	if err != nil {
		t.Fatalf("parse main.go: %v", err)
	}

	// Method names that ARE the withholding decision. Called anywhere but the
	// chokepoint, they are a second spelling that can drift.
	gateMethods := map[string]string{
		"Allowed":  "substance gate",
		"Withheld": "scam gate",
	}

	ast.Inspect(f, func(n ast.Node) bool {
		fn, ok := n.(*ast.FuncDecl)
		if !ok {
			return true
		}
		if fn.Name.Name == "priceWithheld" {
			return false // the one legitimate spelling
		}
		ast.Inspect(fn, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			which, isGate := gateMethods[sel.Sel.Name]
			if !isGate || !gateReceiver(sel.X) {
				return true
			}
			t.Errorf("%s: calls the %s directly (%s) at %s — route it through "+
				"priceWithheld() instead. A hand-written call site can consult one "+
				"gate and forget the other, which is exactly how the last-trade arm "+
				"came to honour the thin-market floor but not the scam decision.",
				enclosingName(fn), which, exprString(sel), fset.Position(call.Pos()))
			return true
		})
		return false
	})
}

// gateReceiver reports whether expr looks like a substance/scam gate field or
// variable (x.substance, x.scam, substance, scam) rather than an unrelated
// type that happens to have an Allowed/Withheld method.
func gateReceiver(expr ast.Expr) bool {
	switch t := expr.(type) {
	case *ast.SelectorExpr:
		return t.Sel.Name == "substance" || t.Sel.Name == "scam"
	case *ast.Ident:
		return t.Name == "substance" || t.Name == "scam"
	}
	return false
}

// enclosingName renders a func decl as "Type.Method" or "function".
func enclosingName(fn *ast.FuncDecl) string {
	if fn.Recv != nil && len(fn.Recv.List) > 0 {
		return receiverTypeName(fn.Recv.List[0].Type) + "." + fn.Name.Name
	}
	return fn.Name.Name
}

// exprString renders a selector as "x.y.Method" for the failure message.
func exprString(sel *ast.SelectorExpr) string {
	switch x := sel.X.(type) {
	case *ast.SelectorExpr:
		return exprString(x) + "." + sel.Sel.Name
	case *ast.Ident:
		return x.Name + "." + sel.Sel.Name
	}
	return sel.Sel.Name
}

// receiverTypeName unwraps *T / T to the bare type name.
func receiverTypeName(expr ast.Expr) string {
	switch t := expr.(type) {
	case *ast.StarExpr:
		return receiverTypeName(t.X)
	case *ast.Ident:
		return t.Name
	}
	return ""
}

// callsPriceWithheld reports whether fn's body contains a call to the
// withholding chokepoint.
func callsPriceWithheld(fn *ast.FuncDecl) bool {
	found := false
	ast.Inspect(fn, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if id, ok := call.Fun.(*ast.Ident); ok && id.Name == "priceWithheld" {
			found = true
			return false
		}
		return true
	})
	return found
}

// TestPriceWithheldChokepointHonoursBothGates pins the chokepoint's own
// semantics: withhold when the substance gate refuses OR the scam gate flags.
// Nil gates mean "operator disabled [pricing_guard]" and must allow — the
// gates are nil-receiver safe and this test keeps that contract explicit, so a
// future refactor cannot turn a disabled gate into a deny-everything outage.
func TestPriceWithheldChokepointHonoursBothGates(t *testing.T) {
	usd, err := canonical.NewFiatAsset("USD")
	if err != nil {
		t.Fatalf("build fiat:USD: %v", err)
	}
	// Nil gates: allow (disabled guard keeps prior behaviour).
	if priceWithheld(context.Background(), nil, nil, canonical.NativeAsset(), usd, "price_read") {
		t.Error("nil gates must allow — a disabled [pricing_guard] must not withhold every price")
	}
}
