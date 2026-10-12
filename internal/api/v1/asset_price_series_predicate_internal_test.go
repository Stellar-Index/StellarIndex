package v1

import "testing"

// TestPriceSeriesPublishable_OnePredicateForBothPaths: the listing sparkline and the detail path's series suppression share
// one predicate so they cannot drift (a declared-peg asset must not get a sparkline yet a price_history drawn from a refused
// dust market). suppressScamIssuerPricing must also null ATH for a flagged issuer; this table is the contract.
func TestPriceSeriesPublishable_OnePredicateForBothPaths(t *testing.T) {
	usd := "0.65"
	for _, tc := range []struct {
		name        string
		detail      AssetDetail
		publishable bool
	}{
		{
			name:        "priced direct market publishes",
			detail:      AssetDetail{PriceUSD: &usd},
			publishable: true,
		},
		{
			name:        "no price publishes nothing",
			detail:      AssetDetail{},
			publishable: false,
		},
		{
			// Guards the detail path: withholdPriceSeriesWhenUnpriced must not test
			// only PriceUSD != nil, or it returns true here while the listing says
			// false.
			name:        "declared peg publishes nothing (MSP-04)",
			detail:      AssetDetail{PriceUSD: &usd, PriceBasis: priceBasisDeclaredPeg},
			publishable: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := tc.detail
			if got := priceSeriesPublishable(&d); got != tc.publishable {
				t.Errorf("priceSeriesPublishable = %v, want %v", got, tc.publishable)
			}
			// The listing predicate must agree, always — that is the point
			// of sharing it.
			if got := sparkline7dEligible(&d); got != tc.publishable {
				t.Errorf("sparkline7dEligible = %v, want %v — the listing and detail "+
					"paths must not disagree about whether a series may be published",
					got, tc.publishable)
			}
		})
	}
}

// A declared-peg detail payload must not keep the series the listing
// refuses, exercised through the suppression helper rather than
// the predicate.
func TestWithholdPriceSeries_DeclaredPegDropsHistory(t *testing.T) {
	usd := "0.65"
	d := &AssetDetail{
		PriceUSD:        &usd,
		PriceBasis:      priceBasisDeclaredPeg,
		PriceHistory24h: []AssetPricePoint{{T: "2026-08-29T22:00:00Z", P: sptrLocal("0.7801")}},
		PriceHistory7d:  []AssetPricePoint{{T: "2026-08-23T00:00:00Z", P: sptrLocal("0.8500")}},
	}
	withholdPriceSeriesWhenUnpriced(d)
	if d.PriceHistory24h != nil || d.PriceHistory7d != nil {
		t.Error("a declared-peg detail kept price_history_* — the dust series the " +
			"listing already refuses to draw, charted beside a peg-derived price " +
			"whose provenance it does not share")
	}
	// The peg price itself is NOT withheld: the peg is the published
	// claim, and dropping it would delete a legitimate number.
	if d.PriceUSD == nil {
		t.Error("the declared-peg price itself must survive — only the series goes")
	}
}

// A declared-peg asset deliberately publishes no all-time high: GetAssetATH reads the asset's own USD-quoted dust market,
// which the substance gate refused, while the headline comes from the peg, so an `ath` would state a provenance the price
// lacks.
func TestWithholdPriceSeries_DeclaredPegDropsATH(t *testing.T) {
	usd := "0.65"
	d := &AssetDetail{
		PriceUSD:   &usd,
		PriceBasis: priceBasisDeclaredPeg,
		ATH:        &AssetATH{USD: "0.9100", At: "2025-11-02T00:00:00Z"},
	}
	withholdPriceSeriesWhenUnpriced(d)
	if d.ATH != nil {
		t.Errorf("declared-peg detail kept ath %+v — it is drawn from the same "+
			"USD market the substance gate refused, while the headline price "+
			"comes from the peg", d.ATH)
	}
	if d.PriceUSD == nil {
		t.Error("the peg price must still survive")
	}
}

// A scam-flagged issuer must publish no all-time-high dollar price.
func TestSuppressScamIssuerPricing_NullsATH(t *testing.T) {
	usd := "0.65"
	d := &AssetDetail{
		IssuerDirectoryTags: []string{"malicious", "unsafe"},
		PriceUSD:            &usd,
		ATH:                 &AssetATH{USD: "0.0091", At: "2025-11-02T00:00:00Z"},
	}
	suppressScamIssuerPricing(d)
	if d.PriceUSD != nil {
		t.Fatal("headline price survived scam suppression")
	}
	if d.ATH != nil {
		t.Errorf("ath survived scam suppression: %+v — an all-time HIGH is a USD "+
			"price claim from the same CAGG as price_usd, so publishing it beside "+
			"price_usd:null hands the client the valuation the gate just refused",
			d.ATH)
	}
}

func sptrLocal(s string) *string { return &s }
