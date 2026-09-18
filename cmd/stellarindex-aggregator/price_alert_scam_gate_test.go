package main

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/pricingguard"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// The price-alert evaluator is a PRICE-SERVING surface: it reads the same
// closed prices_1m bucket the API serves and pushes the number to a
// customer's webhook endpoint. It consulted the thin-market substance
// gate and the scam-issuer gate not at all, in a binary the API's seam
// guard cannot see — so a directory-flagged issuer whose price
// /v1/price refuses to publish could still be delivered, signed, to a
// customer (K001).
//
// These tests pin both halves of the repair: the decision is consulted
// here, and it is the PAIR decision (F002) rather than a second
// base-only copy.

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

// TestPriceAlertReader_WithholdsScamFlaggedIssuer — a flagged issuer on
// EITHER leg is a benign no-op (ok=false), exactly as "no closed bucket"
// is, so the evaluator skips the pair instead of firing.
func TestPriceAlertReader_WithholdsScamFlaggedIssuer(t *testing.T) {
	flagged, err := canonical.NewClassicAsset("RIO", alertScamIssuer)
	if err != nil {
		t.Fatalf("build flagged classic: %v", err)
	}
	native := canonical.NativeAsset()

	for _, tc := range []struct {
		name        string
		base, quote canonical.Asset
	}{
		{"flagged base", flagged, native},
		{"flagged quote", native, flagged},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := &alertScamDirectory{flagged: map[string]bool{alertScamIssuer: true}}
			reader := priceAlertVWAPReader{
				// A healthy bucket with a steady baseline: without the
				// gate this serves a real price, so the test cannot
				// pass for lack of data.
				store:  fakeAlertVWAPStore{latest: alertRow(0, "1.01"), trailing: steadyAlertRows(12)},
				logger: discardLogger(),
				scam:   pricingguard.NewScamGate(dir, pricingguard.ScamGateOptions{}),
			}
			price, _, ok, err := reader.LatestVWAP(context.Background(), tc.base, tc.quote)
			if err != nil {
				t.Fatalf("LatestVWAP: %v", err)
			}
			if ok {
				t.Fatalf("price alert served %q for a directory-flagged issuer — a signed "+
					"webhook would deliver a price /v1/price withholds", price)
			}
			if len(dir.asked) == 0 || dir.asked[0] != alertScamIssuer {
				t.Errorf("directory asked about %v, want the flagged issuer %q — "+
					"the lookup is what proves the leg reached the gate", dir.asked, alertScamIssuer)
			}
		})
	}
}

// TestPriceAlertReader_UnflaggedPairStillServes is the blast-radius
// guard: wiring the gate must not silence ordinary alerts.
func TestPriceAlertReader_UnflaggedPairStillServes(t *testing.T) {
	base, quote := alertUSDAssets(t)
	dir := &alertScamDirectory{flagged: map[string]bool{}}
	reader := priceAlertVWAPReader{
		store:  fakeAlertVWAPStore{latest: alertRow(0, "1.01"), trailing: steadyAlertRows(12)},
		logger: discardLogger(),
		scam:   pricingguard.NewScamGate(dir, pricingguard.ScamGateOptions{}),
	}
	price, _, ok, err := reader.LatestVWAP(context.Background(), base, quote)
	if err != nil || !ok {
		t.Fatalf("expected a served price for an unflagged pair; got ok=%v err=%v", ok, err)
	}
	if price != "1.01" {
		t.Fatalf("served price = %s, want the candidate 1.01 unchanged", price)
	}
}

// TestPriceAlertSeamIsGated is this binary's half of the seam guard that
// cmd/stellarindex-api has had since wave D. That guard parses the API's
// main.go only, which is precisely why a whole second binary reading the
// same closed buckets could serve them ungated and stay green.
//
// The subject set is derived from the STORE CALL, not from a list of
// method names: a new price-serving reader in this binary has to read a
// closed bucket to serve a price at all, so it cannot avoid the scan.
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
			"pricingguard.PriceWithheld — whatever consumes it (a signed customer "+
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

// readsClosedVWAPBucket reports whether fn calls a store read returning a
// closed 1m VWAP bucket.
func readsClosedVWAPBucket(fn *ast.FuncDecl) bool {
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
		if inner, ok := sel.X.(*ast.SelectorExpr); !ok || inner.Sel.Name != "store" {
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

// callsWithholdingChokepoint reports whether fn consults
// pricingguard.PriceWithheld.
func callsWithholdingChokepoint(fn *ast.FuncDecl) bool {
	found := false
	ast.Inspect(fn, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "PriceWithheld" {
			return true
		}
		if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "pricingguard" {
			found = true
			return false
		}
		return true
	})
	return found
}
