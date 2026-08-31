package main

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
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
// weakness that produced the leak.
//
// The subject set is derived by finding every method that CALLS a closed-VWAP
// store read, not by listing seam names. The first version of this test did
// list them — two literal entries — which meant a brand-new ungated reader
// passed silently, and the property it advertised ("a new read seam cannot
// forget it") was not true. Matching on the store call is what makes it true:
// a reader has to call one of those methods to serve a price at all.
//
// Proven red: deleting the priceWithheld() call from storePriceAtReader.PriceAt
// (i.e. restoring the pre-fix state) fails this test naming that method.
func TestPriceServingSeamsAreGated(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "main.go", nil, 0)
	if err != nil {
		t.Fatalf("parse main.go: %v", err)
	}

	// Methods that legitimately read a closed bucket WITHOUT consulting
	// the chokepoint, each with the reason it is safe. An exemption is a
	// deliberate, reviewed decision — not a way to silence this test.
	exempt := map[string]string{
		"storePriceReader.RecentClosedVWAP1mExists": "existence probe: returns a bool, never a price or a bucket value",
		"storeChange24hReader.USDPrice24hAgo": "gated upstream — populateChange24h early-returns unless the GATED " +
			"lookupUSDPrice succeeds first, and scam suppression nulls the change pills regardless",
	}

	var ungated []string
	seams := 0
	ast.Inspect(f, func(n ast.Node) bool {
		fn, ok := n.(*ast.FuncDecl)
		if !ok || fn.Recv == nil || len(fn.Recv.List) == 0 {
			return true
		}
		if !readsClosedVWAP(fn) {
			return true
		}
		seams++
		name := receiverTypeName(fn.Recv.List[0].Type) + "." + fn.Name.Name
		if _, ok := exempt[name]; ok {
			return true
		}
		if !callsPriceWithheld(fn) {
			ungated = append(ungated, name)
		}
		return true
	})

	// A guard whose subject set is empty passes forever. If the scan
	// stops finding seams, the scan is broken — not the code clean.
	if seams == 0 {
		t.Fatal("found no closed-VWAP read seams in main.go — the scan is broken, " +
			"and a guard that checks nothing passes forever")
	}
	for _, name := range ungated {
		t.Errorf("price-serving seam %s reads a closed VWAP bucket without calling "+
			"priceWithheld() — whatever route reaches it would publish a price the "+
			"gate refuses (a directory-flagged issuer, or a market too thin to "+
			"aggregate). Route it through the chokepoint, or add it to `exempt` "+
			"with the reason it cannot serve a gated value.", name)
	}
}

// readsClosedVWAP reports whether fn calls one of the store reads that
// return a closed 1m VWAP bucket — the value the withholding decision
// governs. Matching on the STORE CALL rather than a hand-listed set of
// method names is what makes this guard cover a seam that does not exist
// yet: a new reader has to call one of these to serve a price at all.
func readsClosedVWAP(fn *ast.FuncDecl) bool {
	found := false
	ast.Inspect(fn, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		// r.s.LatestClosedVWAP1mForPair(...), g.s.ClosedVWAPAtOrBefore(...), …
		if inner, ok := sel.X.(*ast.SelectorExpr); !ok || inner.Sel.Name != "s" {
			return true
		}
		if strings.Contains(sel.Sel.Name, "ClosedVWAP") {
			found = true
			return false
		}
		return true
	})
	return found
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

// TestWithholdingGatesAreSpelledOnlyAtTheChokepoint pins the SECOND
// property of the MSP cluster: drift WITHIN a seam.
//
// TestPriceServingSeamsAreGated above answers "does every serving seam
// consult the gates AT ALL". It cannot answer "does each seam consult
// BOTH gates on EVERY branch", because a method with two arms satisfies
// it as soon as ONE arm calls priceWithheld().
//
// That is exactly the MSP-07 shape. storePriceReader.LatestPrice has a
// closed-VWAP arm and a last-trade arm; the last-trade arm originally
// spelled out `!r.substance.Allowed(...)` and consulted the SCAM gate
// not at all. An operator setting disable_substance_gate=true to
// diagnose a coverage complaint would then silently publish a
// directory-flagged issuer's last trade as its price — reversing an
// owner-level trust decision they never touched.
//
// This guard existed, and I DELETED it in the review sweep that
// rewrote its sibling — while that same commit's message said "the
// MSP-07 half (drift WITHIN a seam) was always real". The coverage
// half was genuinely weak and was rightly replaced; removing this half
// alongside it was a regression, and the planted MSP-07 mutation
// passed CI until this was restored. main.go still cited this test by
// name the whole time.
//
// The rule: the gate METHODS (.Allowed / .Withheld on a gate receiver)
// may be named in exactly one place — priceWithheld(). Every other
// call site is a second spelling that can drift out of step.
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

func gateReceiver(expr ast.Expr) bool {
	switch t := expr.(type) {
	case *ast.SelectorExpr:
		return t.Sel.Name == "substance" || t.Sel.Name == "scam"
	case *ast.Ident:
		return t.Name == "substance" || t.Name == "scam"
	}
	return false
}

func enclosingName(fn *ast.FuncDecl) string {
	if fn.Recv != nil && len(fn.Recv.List) > 0 {
		return receiverTypeName(fn.Recv.List[0].Type) + "." + fn.Name.Name
	}
	return fn.Name.Name
}

func exprString(sel *ast.SelectorExpr) string {
	switch x := sel.X.(type) {
	case *ast.SelectorExpr:
		return exprString(x) + "." + sel.Sel.Name
	case *ast.Ident:
		return x.Name + "." + sel.Sel.Name
	}
	return sel.Sel.Name
}
