package v1

import (
	"math/big"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/sources/blend"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

func auctionTestValuer(backstop func(string, *big.Int) (*big.Rat, string)) auctionValuer {
	seven := uint32(7)
	rate := func(s string) *big.Int { n, _ := new(big.Int).SetString(s, 10); return n }
	return auctionValuer{
		states: map[string]clickhouse.BlendReserveState{
			"CUSDC":   {Asset: "CUSDC", Data: blend.ReserveData{Version: blend.PoolV2, BRate: rate("1100000000000"), DRate: rate("1000000000000")}},
			"CXLM":    {Asset: "CXLM", Data: blend.ReserveData{Version: blend.PoolV2, BRate: rate("1000000000000"), DRate: rate("1250000000000")}},
			"CODD":    {Asset: "CODD", Data: blend.ReserveData{Version: blend.PoolV2, BRate: rate("1000000000000"), DRate: rate("1000000000000")}},
			"CNORATE": {Asset: "CNORATE"},
		},
		views: map[string]ReserveView{
			"CUSDC":   {Asset: "CUSDC", Decimals: &seven, PriceUSD: strRef("1")},
			"CXLM":    {Asset: "CXLM", Decimals: &seven, PriceUSD: strRef("0.3")},
			"CODD":    {Asset: "CODD", Decimals: &seven},
			"CNORATE": {Asset: "CNORATE", Decimals: &seven, PriceUSD: strRef("1")},
		},
		backstop: backstop,
	}
}

func blendBackstop(string, *big.Int) (*big.Rat, string) { return nil, backstopUSDLPShare }

// TestAuctionValuer_LegKindsPerAuctionType values bToken and dToken legs through the reserve's current b_rate / d_rate, an
// underlying leg directly, and withholds a Blend backstop-token leg and an unknown auction type with a reason.
func TestAuctionValuer_LegKindsPerAuctionType(t *testing.T) {
	av := auctionTestValuer(blendBackstop)
	cases := []struct {
		name                     string
		typ                      uint32
		bid, lot                 map[string]string
		bidUSD, lotUSD           string
		bidLower, lotLower       bool
		bidUnvalued, lotUnvalued string // "asset/kind/reason" of the single unvalued leg, if any
	}{
		// 1000 bTokens x 1.1 = 1100 USDC; 2000 dTokens x 1.25 = 2500 XLM x 0.3 = 750.
		{
			"user liquidation", 0,
			map[string]string{"CXLM": "20000000000"},
			map[string]string{"CUSDC": "10000000000"},
			"750.00", "1100.00", false, false, "", "",
		},
		{
			"user liquidation mixed lot", 0,
			map[string]string{},
			map[string]string{"CUSDC": "10000000000", "CODD": "5"},
			"0.00", "1100.00", false, true, "", "CODD/b_token/unpriced",
		},
		{
			"bad debt blend", 1,
			map[string]string{"CUSDC": "10000000"},
			map[string]string{"CBACKSTOP": "70000000"},
			"1.00", "", false, false, "", "CBACKSTOP/backstop_token/lp_share_unvalued",
		},
		// 5 USDC of backstop credit, paid for in backstop tokens.
		{
			"interest", 2,
			map[string]string{"CBACKSTOP": "1"},
			map[string]string{"CUSDC": "50000000"},
			"", "5.00", false, false, "CBACKSTOP/backstop_token/lp_share_unvalued", "",
		},
		{
			"protocol fee unknown", 3,
			map[string]string{"CUSDC": "1"},
			map[string]string{"CUSDC": "1"},
			"", "", false, false, "CUSDC/unknown/leg_kind_unknown", "CUSDC/unknown/leg_kind_unknown",
		},
		{
			"reserve unread", 0,
			map[string]string{"CGONE": "1"},
			map[string]string{"CNORATE": "1"},
			"", "", false, false, "CGONE/d_token/reserve_unread", "CNORATE/b_token/rate_unread",
		},
		// 2^65 bTokens at b_rate 1.0: 3689348814741.9103232 USDC, exact.
		{
			"above 2^64", 0,
			map[string]string{},
			map[string]string{"CXLM": "36893488147419103232"},
			"0.00", "1106804644422.57", false, false, "", "",
		},
	}
	for _, tc := range cases {
		v := LendingAuctionView{AuctionType: tc.typ, Bid: tc.bid, Lot: tc.lot}
		av.value(&v)
		u := v.LendingAuctionUSDView
		if u.ValuedAt != "current" || deref(u.BidUSD) != tc.bidUSD || deref(u.LotUSD) != tc.lotUSD ||
			u.BidUSDLowerBound != tc.bidLower || u.LotUSDLowerBound != tc.lotLower ||
			oneUnvalued(u.BidUnvalued) != tc.bidUnvalued || oneUnvalued(u.LotUnvalued) != tc.lotUnvalued {
			t.Errorf("%s: got %+v", tc.name, u)
		}
	}
}

// TestAuctionValuer_BLNTBackstopLegPriced values a BLNT backstop-token leg through the backstop valuer.
func TestAuctionValuer_BLNTBackstopLegPriced(t *testing.T) {
	av := auctionTestValuer(func(token string, amount *big.Int) (*big.Rat, string) {
		if token != "CBLNT" {
			return nil, backstopUSDUnpriced
		}
		return new(big.Rat).SetFrac(amount, big.NewInt(1e9)), "" // 0.01 USD per whole 7-dp token
	})
	v := LendingAuctionView{AuctionType: 2, Bid: map[string]string{"CBLNT": "5000000000"}, Lot: map[string]string{"CUSDC": "50000000"}}
	av.value(&v)
	if deref(v.BidUSD) != "5.00" || v.BidUSDLowerBound || len(v.BidUnvalued) != 0 || deref(v.LotUSD) != "5.00" {
		t.Errorf("BLNT interest auction = %+v", v.LendingAuctionUSDView)
	}
}

func oneUnvalued(legs []LendingAuctionUnvaluedLeg) string {
	if len(legs) != 1 {
		return ""
	}
	return legs[0].Asset + "/" + legs[0].Kind + "/" + legs[0].Reason
}
