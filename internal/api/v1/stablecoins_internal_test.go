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
	got := (&Server{verifiedCurrencies: cat}).stablecoinPrewarmSet()
	const usdc = "USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
	sac, ok := classicSACContractID(usdc)
	if !ok || got[usdc] != sac {
		t.Fatalf("prewarm set = %v, want USDC keyed to %s", got, sac)
	}
	if len(got) != 5 {
		t.Errorf("prewarm set has %d members, want the 5 Stellar-issued stablecoins", len(got))
	}
}
