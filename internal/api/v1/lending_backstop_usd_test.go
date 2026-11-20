package v1_test

import (
	"math/big"
	"testing"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/sources/blend"
	"github.com/Stellar-Index/StellarIndex/internal/sources/blnt"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/xdrjson"
)

// TestLendingPoolBackstop_BLNTTierUSD values a BLNT XLM tier like a reserve, exactly above 2^63; a BlntXlm LP-share tier is never
// valued even when its token has a price, a tier with no balance is null with a reason, and the total is a lower bound.
func TestLendingPoolBackstop_BLNTTierUSD(t *testing.T) {
	xlmSAC, ok := xdrjson.SACContractID("native", "Public Global Stellar Network ; September 2015")
	if !ok {
		t.Fatal("native SAC")
	}
	unknown := blend.PoolVersionUnknown
	twoTo64, _ := new(big.Int).SetString("18446744073709551616", 10)
	ex := &stubBackstopExplorer{stubExplorerReader: &stubExplorerReader{}, state: clickhouse.LendingBackstopState{
		Instance: &clickhouse.PoolInstance{Backstop: blnt.MainnetBackstop},
		Tiers: []clickhouse.BackstopTierState{
			{Tier: "FirstLoss", Asset: "Xlm", Token: xlmSAC, Balance: &clickhouse.BackstopBalance{Shares: twoTo64, Tokens: twoTo64, Q4W: big.NewInt(0)}},
			// The LP tier's token is given a price so only the asset-kind gate can keep it unvalued.
			{Tier: "SecondLoss", Asset: "BlntXlm", Token: xlmSAC, Balance: &clickhouse.BackstopBalance{Shares: big.NewInt(5), Tokens: big.NewInt(5e7), Q4W: big.NewInt(0)}},
			{Tier: "ThirdLoss"},
		},
	}}
	srv := v1.New(v1.Options{
		Explorer: ex, Prices: &assetKeyedPriceReader{usd: map[string]string{"native": "0.3"}},
		Lending: &stubBLNTLendingReader{stubLendingReader: stubLendingReader{version: &unknown}, blntAssets: []string{mkCStrkey(t, 3)}},
	})
	got := getBackstop(t, srv, blnt.MainnetFixedPool)
	if len(got.Tiers) != 3 {
		t.Fatalf("tiers = %+v", got.Tiers)
	}
	// 2^64 stroops / 10^7 x 0.3 = 553402322211.2865 USD.
	if ft := got.Tiers[0]; strOrNil(ft.TokensUSD) != "553402322211.29" || strOrNil(ft.PriceUSD) != "0.3" || ft.USDReason != nil {
		t.Errorf("FirstLoss = %+v, want tokens_usd 553402322211.29 at 0.3", ft)
	}
	if st := got.Tiers[1]; st.TokensUSD != nil || st.PriceUSD != nil || strOrNil(st.USDReason) != "lp_share_unvalued" {
		t.Errorf("LP-share SecondLoss = %+v, want null tokens_usd with lp_share_unvalued", st)
	}
	if tt := got.Tiers[2]; tt.TokensUSD != nil || strOrNil(tt.USDReason) != "balance_unavailable" {
		t.Errorf("ThirdLoss = %+v, want balance_unavailable", tt)
	}
	if strOrNil(got.TotalUSD) != "553402322211.29" || !got.LowerBound {
		t.Errorf("total_usd = %v lower_bound = %v, want 553402322211.29 as a lower bound", strOrNil(got.TotalUSD), got.LowerBound)
	}
}

// TestLendingPoolBackstop_BLNTUnknownAssetIsWithheld withholds a tier whose asset kind is not a known plain token, though its
// token is priced: a new BackstopAsset must not be valued until it is classified.
func TestLendingPoolBackstop_BLNTUnknownAssetIsWithheld(t *testing.T) {
	xlmSAC, _ := xdrjson.SACContractID("native", "Public Global Stellar Network ; September 2015")
	unknown := blend.PoolVersionUnknown
	ex := &stubBackstopExplorer{stubExplorerReader: &stubExplorerReader{}, state: clickhouse.LendingBackstopState{
		Instance: &clickhouse.PoolInstance{Backstop: blnt.MainnetBackstop},
		Tiers: []clickhouse.BackstopTierState{
			{Tier: "FirstLoss", Asset: "BlntEurc", Token: xlmSAC, Balance: &clickhouse.BackstopBalance{Shares: big.NewInt(1e7), Tokens: big.NewInt(1e7), Q4W: big.NewInt(0)}},
			{Tier: "SecondLoss", Asset: "Xlm", Balance: &clickhouse.BackstopBalance{Shares: big.NewInt(1), Tokens: big.NewInt(1), Q4W: big.NewInt(0)}},
		},
	}}
	srv := v1.New(v1.Options{
		Explorer: ex, Prices: &assetKeyedPriceReader{usd: map[string]string{"native": "0.3"}},
		Lending: &stubBLNTLendingReader{stubLendingReader: stubLendingReader{version: &unknown}, blntAssets: []string{mkCStrkey(t, 3)}},
	})
	got := getBackstop(t, srv, blnt.MainnetFixedPool)
	if len(got.Tiers) != 2 || got.Tiers[0].TokensUSD != nil || strOrNil(got.Tiers[0].USDReason) != "token_unknown" || got.TotalUSD != nil {
		t.Errorf("unknown asset kind = %+v, want token_unknown and a null total", got)
	}
	// A balance with no token read is not a missing balance.
	if st := got.Tiers[1]; st.TokensUSD != nil || strOrNil(st.USDReason) != "token_unknown" {
		t.Errorf("tokenless SecondLoss = %+v, want token_unknown", st)
	}
}

// TestLendingPoolBackstop_BlendLPShareIsUnvalued keeps a Blend backstop's Comet LP share unvalued: null total, never a lower bound
// of zero.
func TestLendingPoolBackstop_BlendLPShareIsUnvalued(t *testing.T) {
	v2 := blend.PoolV2
	ex := &stubBackstopExplorer{stubExplorerReader: &stubExplorerReader{}, state: clickhouse.LendingBackstopState{
		Instance: &clickhouse.PoolInstance{Backstop: mkCStrkey(t, 4)}, Token: mkCStrkey(t, 5),
		Tiers: []clickhouse.BackstopTierState{{Balance: &clickhouse.BackstopBalance{Shares: big.NewInt(10), Tokens: big.NewInt(10), Q4W: big.NewInt(0)}}},
	}}
	srv := v1.New(v1.Options{Explorer: ex, Lending: &stubLendingReader{version: &v2}})
	got := getBackstop(t, srv, mkCStrkey(t, 6))
	if len(got.Tiers) != 1 || got.Tiers[0].TokensUSD != nil || strOrNil(got.Tiers[0].USDReason) != "lp_share_unvalued" ||
		got.TotalUSD != nil || got.LowerBound {
		t.Errorf("blend backstop = %+v, want lp_share_unvalued and a null, non-lower-bound total", got)
	}
}
