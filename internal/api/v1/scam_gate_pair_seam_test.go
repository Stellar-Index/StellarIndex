package v1_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/pricingguard"
)

// TestProductionScamGateIsPairAware pins the type assertion in
// v1.scamWithheld to the type that actually runs in production.
//
// scamWithheld falls back to the BASE-ONLY question for a gate that
// does not implement the pair form. That fallback is a migration seam
// for gates written before the pair question existed — if the
// production gate ever stopped satisfying it, every handler would
// quietly go back to serving a flagged issuer's price through the quote
// leg with no compile error and no failing behavioural test that used a
// fake. This assertion is what makes that impossible.
func TestProductionScamGateIsPairAware(t *testing.T) {
	var gate v1.PriceScamGate = (*pricingguard.ScamGate)(nil)
	if _, ok := gate.(v1.PriceScamPairGate); !ok {
		t.Fatal("*pricingguard.ScamGate no longer implements v1.PriceScamPairGate — " +
			"v1.scamWithheld would silently fall back to the base-only question on " +
			"every handler, re-opening the reciprocal-price bypass (F002)")
	}
}

// scamPairPendingFiles are the handlers that still ask the BASE-ONLY
// question. Each is a live instance of the same defect — the price is
// republished by naming the flagged asset as the quote — and each lives
// in a file outside the change that made the gate pair-aware
// (F019/F032/T039 carry them).
//
// This is a shrinking ratchet, not an allow-list: it exists so a NEW
// handler cannot join them silently, and so the follow-up is named in
// code rather than in a tracker. Migrating a file means deleting its
// entry here, not adding one.
var scamPairPendingFiles = map[string]string{
	"price_tip.go":    "T039 — /v1/price/tip computeTip",
	"price_stream.go": "T039 — the closed-price SSE stream",
}

// TestScamGateIsAskedThePairQuestion fails when a handler spells
// `s.scam.Withheld(...)` — the base-only question — anywhere but the
// files already known to be mid-migration.
//
// An AST guard rather than a behavioural test for the same reason the
// API binary's chokepoint guard is one: a behavioural test only covers
// the endpoint somebody remembered to write a case for, and the whole
// class is "the surface nobody remembered".
func TestScamGateIsAskedThePairQuestion(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}

	fset := token.NewFileSet()
	scanned := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(".", name), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		scanned++
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "Withheld" {
				return true
			}
			recv, ok := sel.X.(*ast.SelectorExpr)
			if !ok || recv.Sel.Name != "scam" {
				return true
			}
			if _, pending := scamPairPendingFiles[name]; pending {
				return true
			}
			t.Errorf("%s: asks the scam gate the BASE-ONLY question (s.scam.Withheld) at %s "+
				"— call scamWithheld(ctx, s.scam, base, quote, surface) instead. Keyed on "+
				"the base alone, this surface republishes a flagged issuer's withheld price "+
				"as its exact reciprocal whenever the client names it as the quote (F002).",
				name, fset.Position(call.Pos()))
			return true
		})
	}

	// A guard whose subject set is empty passes forever.
	if scanned == 0 {
		t.Fatal("scanned no package files — the guard is broken, not the code clean")
	}
	for name := range scamPairPendingFiles {
		if _, err := os.Stat(name); err != nil {
			t.Errorf("pending file %q no longer exists — delete its entry rather than "+
				"leaving a stale exemption that could match a future file of the same name", name)
		}
	}
}
