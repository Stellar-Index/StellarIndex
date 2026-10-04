package main

import (
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"sort"
	"testing"
	"time"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/pricingguard"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// thinAdmissionSites is where each admission symbol may be named, keyed
// "dir:func". The record releases a thin market only because the two
// gate chokepoints read it; a second reader is a second release rule.
var thinAdmissionSites = map[string]map[string]bool{
	"ThinAdmissionFrom": {
		".:priceWithheld":                               true,
		v1ChokepointDir + ":withheldBy":                 true,
		v1ChokepointDir + ":readPriceWithAliasesServed": true,
	},
	"WithThinAdmission": {
		v1ChokepointDir + ":resolvePriceServeThin":      true,
		v1ChokepointDir + ":resolveBatchRow":            true,
		v1ChokepointDir + ":handlePriceAt":              true,
		v1ChokepointDir + ":priceChangeCurrentThin":     true,
		v1ChokepointDir + ":priceChangeHorizonThin":     true,
		v1ChokepointDir + ":handleAssetGet":             true,
		v1ChokepointDir + ":thinDetailPricePass":        true,
		v1ChokepointDir + ":readPriceWithAliasesServed": true,
		// The raw-tier evidence probe: it never withholds, only reads the verdict.
		v1ChokepointDir + ":thinMarketEvidence": true,
	},
	"Merge":     {v1ChokepointDir + ":readPriceWithAliasesServed": true},
	"AdmitThin": {".:priceWithheld": true},
	"Judge":     {".:priceWithheld": true},
	"Fold":      {v1ChokepointDir + ":withheldBy": true},
	// The context key is private to its file; any other reader bypasses
	// the nil-safe accessors.
	"thinAdmissionKey": {},
}

// thinAdmissionHome is the record's own file, where every symbol is free.
var thinAdmissionHome = filepath.Join(v1ChokepointDir, "thin_admission.go")

func TestThinAdmissionIsReadOnlyAtTheChokepoint(t *testing.T) {
	fset := token.NewFileSet()
	files := []gateSpellingFile{{dir: ".", path: "main.go"}}
	files = append(files, apiSourceFiles(t, apiTreeDir)...)
	seen := map[string]bool{}
	for _, sf := range files {
		if filepath.Clean(sf.path) == filepath.Clean(thinAdmissionHome) {
			continue
		}
		f, err := parser.ParseFile(fset, sf.path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", sf.path, err)
		}
		violations, permitted := thinAdmissionViolations(fset, sf.dir, f)
		for _, v := range violations {
			t.Error(v)
		}
		for _, p := range permitted {
			seen[p] = true
		}
	}
	// Every permitted site must be seen, or the scan has gone blind
	// rather than the tree clean.
	var missing []string
	for sym, sites := range thinAdmissionSites {
		for site := range sites {
			if !seen[sym+"@"+site] {
				missing = append(missing, sym+"@"+site)
			}
		}
	}
	sort.Strings(missing)
	for _, m := range missing {
		t.Errorf("permitted admission site %s not found: the scan is broken or the allowlist is stale", m)
	}
}

func TestThinAdmissionGuardCatchesPlantedSites(t *testing.T) {
	shapes := []struct{ name, dir, src string }{
		{"record read in a handler", v1ChokepointDir, `func (s *Server) h() { _ = ThinAdmissionFrom(ctx) }`},
		{"record installed in another handler", v1ChokepointDir, `func (s *Server) h() { _, _ = WithThinAdmission(ctx, a, q, true) }`},
		{"release spelled in a store method", ".", `func (r storePriceReader) f() { _ = pricingguard.Query{AdmitThin: true} }`},
		{"Judge outside the chokepoint", ".", `func (r storePriceReader) f() { _ = g.Judge(ctx, a, q, "x", qy) }`},
		{"Fold in another package", "../../internal/api/streaming", `func withheldBy() { _ = pricingguard.Fold(false, v, true, "x") }`},
		{"Merge outside the flight", v1ChokepointDir, `func (s *Server) h() { adm.Merge(true, ev) }`},
		{"context key outside its file", v1ChokepointDir, `func h() { _ = ctx.Value(thinAdmissionKey{}) }`},
		{"release set on a query value", ".", `func f() { q.AdmitThin = true }`},
		{"Judge taken as a method value", ".", `func f() { j := gate.Judge; _ = j }`},
		{"unrelated Fold field is not the fold", v1ChokepointDir, `func h() { _ = x.Fold; _ = ThinAdmissionFrom(ctx) }`},
	}
	for _, sh := range shapes {
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, "planted.go", "package p\n"+sh.src, 0)
		if err != nil {
			t.Fatalf("%s: %v", sh.name, err)
		}
		if v, _ := thinAdmissionViolations(fset, sh.dir, f); len(v) != 1 {
			t.Errorf("%s: got %d violations, want 1: %v", sh.name, len(v), v)
		}
	}
}

// thinAdmissionViolations reports each admission symbol named outside
// its permitted sites, and returns the "symbol@dir:func" sites it saw.
func thinAdmissionViolations(fset *token.FileSet, dir string, f *ast.File) (violations, permitted []string) {
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		site := dir + ":" + fn.Name.Name
		check := func(sym string, pos token.Pos) {
			if thinAdmissionSites[sym][site] {
				permitted = append(permitted, sym+"@"+site)
				return
			}
			violations = append(violations, fmt.Sprintf("%s names %s at %s — the include_thin record is "+
				"installed by the two-pass drivers and read only at the gate chokepoints; a second reader "+
				"is a second rule for releasing a thin market", enclosingName(fn), sym, fset.Position(pos)))
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.SelectorExpr:
				if pkg, ok := x.X.(*ast.Ident); ok && pkg.Name == "pricingguard" && x.Sel.Name == "Fold" {
					check("Fold", x.Pos())
				}
			case *ast.Ident:
				if x.Name != "Fold" && thinAdmissionSites[x.Name] != nil {
					check(x.Name, x.Pos())
				}
			}
			return true
		})
	}
	return violations, permitted
}

// TestPriceWithheldReleasesThinUnderAdmission pins the chokepoint's half
// of include_thin: only a covered, opted-in thin verdict is released.
func TestPriceWithheldReleasesThinUnderAdmission(t *testing.T) {
	dust := timescale.MarketSubstance{VolumeUSD: "8.57", Buckets: 1}
	pair := thinMarketTestPair(t)
	substance := pricingguard.NewSubstanceGate(&historyAwareSubstance{trailing: dust, atInstant: dust}, pricingguard.SubstanceGateOptions{})
	other, err := canonical.NewClassicAsset("OTHR", "GA7QYNF7SOWQ3GLR2BGMZEHXAVIRZA4KVWLTJJFC7MGXUA74P7UJVSGZ")
	if err != nil {
		t.Fatal(err)
	}

	offCtx, off := v1.WithThinAdmission(context.Background(), pair.Base, pair.Quote, false)
	if got := priceWithheld(offCtx, substance, nil, pair.Base, pair.Quote, "price_read"); got != pricingguard.WithheldThinMarket {
		t.Fatalf("default: %v, want WithheldThinMarket", got)
	}
	if off.Admitted() || off.Evidence() == nil {
		t.Error("default: the record must collect evidence and admit nothing")
	}

	onCtx, on := v1.WithThinAdmission(context.Background(), pair.Base, pair.Quote, true)
	if got := priceWithheld(onCtx, substance, nil, pair.Base, pair.Quote, "price_read"); got != pricingguard.NotWithheld {
		t.Fatalf("opted in: %v, want the thin market released", got)
	}
	if !on.Admitted() || on.Evidence() == nil || on.Evidence().VolumeUSD != "8.57" {
		t.Errorf("opted in: admitted=%v evidence=%+v, want the measured thin market recorded", on.Admitted(), on.Evidence())
	}
	if got := priceWithheld(onCtx, substance, nil, pair.Base, pair.Quote, "price_read", asOfInstant(time.Date(2021, 3, 1, 9, 0, 0, 0, time.UTC))); got != pricingguard.NotWithheld {
		t.Errorf("opted in, point in time: %v, want released", got)
	}

	if got := priceWithheld(context.Background(), substance, nil, pair.Base, pair.Quote, "price_read"); got != pricingguard.WithheldThinMarket {
		t.Errorf("no record: %v, want WithheldThinMarket", got)
	}

	otherCtx, _ := v1.WithThinAdmission(context.Background(), other, pair.Quote, true)
	if got := priceWithheld(otherCtx, substance, nil, pair.Base, pair.Quote, "price_read"); got != pricingguard.WithheldThinMarket {
		t.Errorf("uncovered leg: %v, want withheld — an opt-in on one asset must not release another's market", got)
	}

	scam := pricingguard.NewScamGate(&flaggingScamDirectory{flagged: map[string]bool{pair.Base.Issuer: true}}, pricingguard.ScamGateOptions{})
	if got := priceWithheld(onCtx, substance, scam, pair.Base, pair.Quote, "price_read"); got != pricingguard.WithheldFlaggedIssuer {
		t.Errorf("flagged issuer: %v, want WithheldFlaggedIssuer — the opt-in releases substance only", got)
	}
}
