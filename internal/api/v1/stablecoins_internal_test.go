package v1

import (
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/currency"
)

func TestStablecoinSummarise_TotalEqualsReAddedRows(t *testing.T) {
	big, small := "118059162071741.13", "0.07"
	assets := []StablecoinAsset{
		{Ticker: "A", Peg: "USD", SupplyUSD: &big},
		{Ticker: "B", Peg: "USD", SupplyUSD: &small},
		{Ticker: "C", Peg: "USD"},
	}
	tot, _ := stablecoinSummarise(assets, nil, false)
	if tot.SupplyUSD == nil || *tot.SupplyUSD != "118059162071741.20" {
		t.Fatalf("total = %v", tot.SupplyUSD)
	}
	if !tot.LowerBound || tot.AssetsUnvalued != 1 {
		t.Fatalf("lower_bound=%v unvalued=%d", tot.LowerBound, tot.AssetsUnvalued)
	}
}

func TestStablecoinSummarise_PegSubtotalMarksItsFloor(t *testing.T) {
	one, two := "1.00", "2.00"
	assets := []StablecoinAsset{
		{Ticker: "A", Peg: "USD", SupplyUSD: &one},
		{Ticker: "B", Peg: "USD"},
		{Ticker: "E", Peg: "EUR", SupplyUSD: &two, CirculatingSupplyLowerBound: true},
		{Ticker: "F", Peg: "GBP", SupplyUSD: &two},
	}
	got := func(pegs []StablecoinPegTotal) map[string]StablecoinPegTotal {
		m := map[string]StablecoinPegTotal{}
		for _, p := range pegs {
			m[p.Peg] = p
		}
		return m
	}
	_, pegs := stablecoinSummarise(assets, nil, false)
	m := got(pegs)
	if p := m["USD"]; !p.LowerBound || p.Assets != 2 || p.AssetsValued != 1 || p.SupplyUSD == nil || *p.SupplyUSD != "1.00" {
		t.Fatalf("USD with an unvalued member = %+v, want lower_bound, 1 of 2 valued", p)
	}
	if p := m["EUR"]; !p.LowerBound || p.AssetsValued != 1 {
		t.Fatalf("EUR summing a floor supply = %+v, want lower_bound", p)
	}
	if p := m["GBP"]; p.LowerBound || p.AssetsValued != 1 {
		t.Fatalf("GBP fully valued = %+v, want exact", p)
	}
	_, pegs = stablecoinSummarise(assets, nil, true)
	if p := got(pegs)["GBP"]; !p.LowerBound {
		t.Fatalf("GBP over a truncated member list = %+v, want lower_bound", p)
	}
}

// A wrapper is left out only when its underlying (by asset id) is in the set; it is then always named in not_summed and,
// valued or not, never makes the headline a floor, since its backing is summed. lower_bound is true exactly when a
// member that should be summed is unvalued.
func TestStablecoinSummarise_WrapperFollowsItsUnderlying(t *testing.T) {
	one := "1.00"
	usdc := StablecoinAsset{AssetID: "USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN", Ticker: "USDC", Peg: "USD", SupplyUSD: &one}
	wrapper := func(valued bool) StablecoinAsset {
		a := StablecoinAsset{
			AssetID: "yUSDC-GDGTVWSM4MGS4T7Z6W4RPWOCHE2I6RDFCIFZGS3DOA63LWQTRNZNTTFF", Ticker: "yUSDC", Peg: "USD", YieldBearing: true,
			ValuationStatus: stablecoinSupplyUnavailable,
		}
		if valued {
			a.SupplyUSD, a.ValuationStatus = &one, stablecoinPublished
		}
		return a
	}
	for _, valued := range []bool{false, true} {
		tot, pegs := stablecoinSummarise([]StablecoinAsset{usdc, wrapper(valued)}, nil, false)
		if len(tot.NotSummed) != 1 || tot.NotSummed[0] != (StablecoinExclusion{Ticker: "yUSDC", Reason: stablecoinReasonYield}) {
			t.Errorf("valued=%v underlying in set: not_summed = %v, want yUSDC/%s", valued, tot.NotSummed, stablecoinReasonYield)
		}
		if tot.LowerBound || scStrInternal(tot.SupplyUSD) != "1.00" {
			t.Errorf("valued=%v underlying in set: total = %s lower_bound=%v, want 1.00 exact", valued, scStrInternal(tot.SupplyUSD), tot.LowerBound)
		}
		if len(pegs) != 1 || pegs[0].LowerBound || scStrInternal(pegs[0].SupplyUSD) != "1.00" {
			t.Errorf("valued=%v underlying in set: by_peg = %+v, want USD 1.00 exact", valued, pegs)
		}
	}

	// Underlying absent from the set: the wrapper is an ordinary member.
	tot, pegs := stablecoinSummarise([]StablecoinAsset{wrapper(true)}, nil, false)
	if len(tot.NotSummed) != 0 || tot.LowerBound || scStrInternal(tot.SupplyUSD) != "1.00" || scStrInternal(pegs[0].SupplyUSD) != "1.00" {
		t.Errorf("valued, no underlying: total = %s lower_bound=%v not_summed=%v by_peg=%+v, want it summed exactly",
			scStrInternal(tot.SupplyUSD), tot.LowerBound, tot.NotSummed, pegs)
	}
	tot, pegs = stablecoinSummarise([]StablecoinAsset{wrapper(false)}, nil, false)
	if !tot.LowerBound || !pegs[0].LowerBound {
		t.Errorf("unvalued, no underlying: lower_bound total=%v peg=%v, want both true", tot.LowerBound, pegs[0].LowerBound)
	}
}

func scStrInternal(s *string) string {
	if s == nil {
		return "<nil>"
	}
	return *s
}

func TestStablecoinAliasSupply_FindsReadingUnderAnAliasForm(t *testing.T) {
	// native's alias set includes crypto:XLM; a reading keyed there must be found.
	circ, _ := stablecoinAliasSupply("native", nil, map[string]string{"crypto:XLM": "5"}, nil)
	if circ != "5" {
		t.Fatalf("circ = %q, want 5", circ)
	}
}

func TestStablecoinPrewarmSet_KeysMembersToTheirSAC(t *testing.T) {
	cat, err := currency.LoadEmbedded()
	if err != nil {
		t.Fatal(err)
	}
	got := (&Server{Options: Options{VerifiedCurrencies: cat}}).stablecoinPrewarmSet()
	const usdc = "USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
	sac, ok := classicSACContractID(usdc)
	if !ok || got[usdc] != sac {
		t.Fatalf("prewarm set = %v, want USDC keyed to %s", got, sac)
	}
	if len(got) != 5 {
		t.Errorf("prewarm set has %d members, want the 5 Stellar-issued stablecoins", len(got))
	}
}
