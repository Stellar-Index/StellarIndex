package v1

import "testing"

// TestPriceSeriesPublishable_OnePredicateForBothPaths is the wave-D
// MSP-04 / MSP-05 regression.
//
// The listing's sparkline attach and the detail path's series
// suppression asserted the SAME product rule — "may this payload carry a
// price-over-time claim?" — in two places, and drifted. The listing
// excluded declared-peg rows; the detail path did not. So a declared-peg
// asset got no sparkline on /v1/assets while /v1/assets/{id} served a
// full price_history_24h/7d drawn from the dust market the substance
// gate had refused (MSP-04).
//
// And suppressScamIssuerPricing nulled six fields but not ATH, so a
// directory-flagged issuer published an all-time-HIGH dollar price next
// to price_usd: null — a published USD valuation for a token the
// platform decided must publish none (MSP-05).
//
// Both are now one predicate with two callers. This table is the
// contract; the cases marked RED-PRE-FIX fail against the old code.
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
			// RED PRE-FIX on the detail path: withholdPriceSeriesWhenUnpriced
			// tested only PriceUSD != nil, so this returned true there while
			// the listing said false.
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
// refuses (MSP-04), exercised through the suppression helper rather than
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

// A declared-peg asset also publishes no all-time high, and that is a
// DELIBERATE wire change rather than a side effect — pinned here because
// it shipped unpinned and undescribed, and a reviewer had to reproduce
// it to find out whether it was intended (review sweep 2026-08-31).
//
// The reasoning is the same as for the series: GetAssetATH reads the
// asset's own USD-QUOTED market, which for a declared-peg asset is the
// dust market the substance gate refused. The published headline comes
// from the PEG. So an `ath` beside it states a provenance the price does
// not have — a dollar high drawn from a market the platform declined to
// price from.
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

// A scam-flagged issuer must publish no all-time-high dollar price
// (MSP-05).
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
