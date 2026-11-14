package v1

import (
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// A price the substance gate could not measure is not a market that produced none; a row a later fill priced is not unmeasured.
func TestRWAValuationOf_UnmeasuredAndUncheckedAreTheirOwnStatuses(t *testing.T) {
	price := "1.04"
	cases := []struct {
		name string
		d    AssetDetail
		want string
	}{
		{"no price, gate answered", AssetDetail{}, RWAValuationUnpriced},
		{"no price, gate unanswered", AssetDetail{priceUnmeasured: true}, RWAValuationUnmeasured},
		{"gate unanswered, a later fill priced it", AssetDetail{priceUnmeasured: true, PriceUSD: &price}, RWAValuationNoSupply},
		{"directory unanswered", AssetDetail{issuerDirectoryUnchecked: true, PriceUSD: &price}, RWAValuationIssuerUnchecked},
		{"flagged wins over unanswered", AssetDetail{
			issuerDirectoryUnchecked: true, IssuerDirectoryTags: []string{"malicious"},
		}, RWAValuationIssuerFlagged},
	}
	for _, c := range cases {
		if got := rwaValuationOf(c.d); got.Status != c.want {
			t.Errorf("%s: status = %q, want %q", c.name, got.Status, c.want)
		}
	}
	if v := rwaValuationOf(AssetDetail{issuerDirectoryUnchecked: true, PriceUSD: &price}); v.PriceUSD != nil {
		t.Errorf("an unchecked issuer's row kept a price: %s", *v.PriceUSD)
	}
}

// The curator arm attaches a dollar reference too, so it refuses the same two issuer states the oracle arm does.
func TestRWAApplyCuratorReference_RefusesAnUncheckedIssuer(t *testing.T) {
	for valuation, want := range map[string]string{
		RWAValuationIssuerFlagged:   RWAPremiumIssuerFlagged,
		RWAValuationIssuerUnchecked: RWAPremiumIssuerUnchecked,
	} {
		supply := "10000000"
		a := RWAAsset{Code: "FUND", CirculatingSupply: &supply, Valuation: RWAValuation{Status: valuation}}
		rwaApplyCuratorReference(&a, timescale.CuratedRWAEntry{Address: "CFUND", AssetCode: "FUND"}, time.Now())
		if a.Reference != nil || a.ReferenceValuation.ValueUSD != nil || a.ReferenceValuation.Status != want {
			t.Errorf("%s: reference = %+v valuation = %+v, want refused as %q", valuation, a.Reference, a.ReferenceValuation, want)
		}
	}
}

// A total that sums a figure computed from a floor supply is a floor, even with every row valued.
func TestRWASummarise_FloorSupplyMakesBothTotalsLowerBounds(t *testing.T) {
	capUSD, refUSD := "100.00", "120.00"
	row := func(floor bool) RWAAsset {
		return RWAAsset{
			Code:                        "USTRY",
			CirculatingSupplyLowerBound: floor,
			Valuation:                   RWAValuation{Status: RWAValuationPublished, MarketCapUSD: &capUSD},
			ReferenceValuation:          RWAReferenceValuation{Status: RWAReferenceValuationPublished, ValueUSD: &refUSD},
		}
	}
	exact := rwaSummarise([]RWAAsset{row(false), row(false)}, rwaTruncation{})
	if exact.LowerBound || exact.ReferenceValuation.LowerBound {
		t.Fatalf("fully valued exact rows read as a lower bound: %+v", exact)
	}
	floor := rwaSummarise([]RWAAsset{row(false), row(true)}, rwaTruncation{})
	if !floor.LowerBound {
		t.Error("market-cap total over a floor supply is not marked a lower bound")
	}
	if !floor.ReferenceValuation.LowerBound {
		t.Error("reference total over a floor supply is not marked a lower bound")
	}
	// A floor row that contributes nothing does not make the total a floor on that basis.
	unvalued := row(true)
	unvalued.ReferenceValuation = RWAReferenceValuation{Status: RWAReferenceValuationNoSupply}
	if rwaSumsAFloor([]RWAAsset{unvalued}, func(a RWAAsset) *string { return a.ReferenceValuation.ValueUSD }) {
		t.Error("a floor row with no reference figure was counted into the reference total")
	}
}

// An issuer group is measured against every set member the issuer has: one valued member beside an excluded one is a floor.
func TestRWAHistoryIssuerGroups_ExcludedMemberMakesTheGroupALowerBound(t *testing.T) {
	day := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	member := func(issuer string) rwaHistoryMember {
		return rwaHistoryMember{assetID: "A-" + issuer, issuer: issuer, days: []rwaHistoryDay{{day: day, valueUSD: "100.00"}}}
	}
	hist := rwaValueHistory{
		members:     []rwaHistoryMember{member("GPARTIAL"), member("GWHOLE")},
		setByIssuer: map[string]int{"GPARTIAL": 2, "GWHOLE": 1},
	}
	for _, g := range rwaHistoryIssuerGroups(hist, time.Time{}) {
		if len(g.Points) != 1 {
			t.Fatalf("%s: points = %d, want 1", g.Key, len(g.Points))
		}
		p := g.Points[0]
		wantUnvalued := hist.setByIssuer[g.Key] - 1
		if p.AssetsUnvalued != wantUnvalued || p.LowerBound != (wantUnvalued > 0) {
			t.Errorf("%s: assets_unvalued = %d lower_bound = %v, want %d and %v",
				g.Key, p.AssetsUnvalued, p.LowerBound, wantUnvalued, wantUnvalued > 0)
		}
	}
}
