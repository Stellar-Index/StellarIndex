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
