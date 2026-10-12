package main

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// The price-alert evaluator is a price-serving surface that pushes the closed prices_1m number to a customer webhook,
// in a binary the API's seam guard cannot see. Without the substance and scam-issuer gates a price /v1/price refuses
// could be delivered. These tests pin that the PAIR decision is consulted here, not a base-only copy.

// alertScamDirectory flags exactly the listed G-addresses.
type alertScamDirectory struct {
	flagged map[string]bool
	asked   []string
}

func (d *alertScamDirectory) DirectoryEntryByAddress(_ context.Context, address string) (timescale.DirectoryEntry, bool, error) {
	d.asked = append(d.asked, address)
	if d.flagged[address] {
		return timescale.DirectoryEntry{Address: address, Tags: []string{"unsafe"}}, true, nil
	}
	return timescale.DirectoryEntry{}, false, nil
}

const alertScamIssuer = "GA7QYNF7SOWQ3GLR2BGMZEHXAVIRZA4KVWLTJJFC7MGXUA74P7UJVSGZ"

// TestPriceAlertSeamIsGated is this binary's half of the seam guard in cmd/stellarindex-api, which parses only the
// API's main.go. The subject set is derived from the store call, not method names: any price-serving reader here must
// read a closed bucket.
func TestPriceAlertSeamIsGated(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "main.go", nil, 0)
	if err != nil {
		t.Fatalf("parse main.go: %v", err)
	}

	seams := 0
	ast.Inspect(f, func(n ast.Node) bool {
		fn, ok := n.(*ast.FuncDecl)
		if !ok || fn.Recv == nil || len(fn.Recv.List) == 0 {
			return true
		}
		if !readsClosedVWAPBucket(fn) {
			return true
		}
		seams++
		if callsWithholdingChokepoint(fn) {
			return true
		}
		t.Errorf("%s reads a closed VWAP bucket without calling "+
			"a pricingguard.Gate — whatever consumes it (a signed customer "+
			"webhook, here) would publish a price the gates refuse. Route it through "+
			"the chokepoint the API binary shares.", fn.Name.Name)
		return true
	})

	// A guard whose subject set is empty passes forever.
	if seams == 0 {
		t.Fatal("found no closed-VWAP read seams in main.go — the scan is broken, " +
			"not the code clean")
	}
}

// readsClosedVWAPBucket reports whether fn references the closed 1m VWAP store read on any receiver, including a
// method value or a store alias, which read the same bucket.
func readsClosedVWAPBucket(fn *ast.FuncDecl) bool {
	found := false
	ast.Inspect(fn, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok {
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

// TestClosedVWAPBucketScanCatchesMethodValues pins that the seam scan sees
// a read however it is spelled, not only as r.store.X(...).
func TestClosedVWAPBucketScanCatchesMethodValues(t *testing.T) {
	cases := []struct {
		name, src string
		want      bool
	}{
		{"direct call", `func (r R) f() { r.store.LatestClosedVWAP1mForPair(ctx, a, b) }`, true},
		{"method value", `func (r R) f() { g := r.store.LatestClosedVWAP1mForPair; g(ctx, a, b) }`, true},
		{"method passed as an argument", `func (r R) f() { apply(r.store.RecentClosedVWAP1mForPair) }`, true},
		{"store alias", `func (r R) f() { st := r.store; st.LatestClosedVWAP1mForPair(ctx, a, b) }`, true},
		{"unrelated store read", `func (r R) f() { r.store.LatestTrade(ctx, a, b) }`, false},
	}
	for _, c := range cases {
		f, err := parser.ParseFile(token.NewFileSet(), "planted.go", "package p\n"+c.src, 0)
		if err != nil {
			t.Fatalf("parse %q: %v", c.src, err)
		}
		if got := readsClosedVWAPBucket(f.Decls[0].(*ast.FuncDecl)); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

// callsWithholdingChokepoint reports whether fn asks a pricingguard.Gate.
// Call-only on purpose: a bare reference to PriceWithheld asks nothing.
func callsWithholdingChokepoint(fn *ast.FuncDecl) bool {
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
		switch sel.Sel.Name {
		case "PriceWithheld", "PriceWithholding", "PriceWithholdingAt":
			found = true
			return false
		}
		return true
	})
	return found
}
