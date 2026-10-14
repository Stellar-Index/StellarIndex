package main

import (
	"context"
	"go/ast"
	"strings"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/astguard"

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

// rulePriceAlertSeamIsGated is this binary's half of the seam guard in cmd/stellarindex-api, which parses only the
// API's main.go. The subject set is derived from the store call, not method names: any price-serving reader here must
// read a closed bucket.
func rulePriceAlertSeamIsGated(t *testing.T) {
	_, f := astguard.ParseFile(t, "main.go")

	seams := 0
	ast.Inspect(f, func(n ast.Node) bool {
		fn, ok := n.(*ast.FuncDecl)
		if !ok || fn.Recv == nil || len(fn.Recv.List) == 0 {
			return true
		}
		if !astguard.Mentions(fn, func(m string) bool { return strings.Contains(m, "ClosedVWAP") }) {
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
