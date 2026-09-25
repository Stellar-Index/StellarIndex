package v1

import (
	"strings"
	"testing"
)

// TestRWASummarise_EveryCapMakesALowerBound pins CA2-A06-correct-3: each
// rebuild cap, not only the issuer cap, makes a fully-valued served set's
// total a lower bound, and the basis names the cap instead of calling the
// set complete.
func TestRWASummarise_EveryCapMakesALowerBound(t *testing.T) {
	cap100 := "100.00"
	assets := []RWAAsset{{
		AssetID:   "USTRY-GCRYUGD5NVARGXT56XEZI5CIFCQETYHAPQQTHO2O3IQZTHDH4LATMYWC",
		Valuation: RWAValuation{Status: RWAValuationPublished, MarketCapUSD: &cap100},
	}}
	for _, tc := range []struct {
		name  string
		trunc rwaTruncation
		names string
	}{
		{"issuer cap", rwaTruncation{overIssuerCap: 2}, "issuer cap turned away 2 qualifying asset(s)"},
		{"contract scan cap", rwaTruncation{overContractCap: 5}, "contract scan cap left 5 recognised contract(s) unevaluated"},
		{"full issuer page", rwaTruncation{issuerPagesFull: 1}, "1 member issuer(s) have more classic assets"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sum := rwaSummarise(assets, tc.trunc)
			if sum.AssetsUnvalued != 0 || sum.MarketCapUSD == nil || *sum.MarketCapUSD != "100.00" {
				t.Fatalf("unvalued=%d cap=%v, want one valued row totalling 100.00", sum.AssetsUnvalued, sum.MarketCapUSD)
			}
			if !sum.LowerBound || !sum.Truncated {
				t.Errorf("lower_bound/truncated = %v/%v, want true/true", sum.LowerBound, sum.Truncated)
			}
			if strings.Contains(sum.Basis, "Every asset in the set publishes a valuation") {
				t.Errorf("basis claims a complete set: %q", sum.Basis)
			}
			if !strings.Contains(sum.Basis, tc.names) {
				t.Errorf("basis = %q, want it to name %q", sum.Basis, tc.names)
			}
		})
	}

	sum := rwaSummarise(assets, rwaTruncation{})
	if sum.LowerBound || sum.Truncated {
		t.Errorf("untruncated fully-valued set: lower_bound/truncated = %v/%v, want false/false", sum.LowerBound, sum.Truncated)
	}
	if !strings.Contains(sum.Basis, "Every asset in the set publishes a valuation") {
		t.Errorf("untruncated basis = %q", sum.Basis)
	}
}
