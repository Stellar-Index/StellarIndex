package v1

import (
	"math/big"
	"testing"
)

func emissionUint(t *testing.T, s string) *big.Int {
	t.Helper()
	n, ok := new(big.Int).SetString(s, 10)
	if !ok {
		t.Fatalf("bad int %q", s)
	}
	return n
}

// TestEmissionRate_PricedUnpricedEndedAndAbove2To64 pins the per-day rate: exact base units, USD at the token price, nothing
// for an ended stream, and no truncation when eps x 86400 passes 2^64.
func TestEmissionRate_PricedUnpricedEndedAndAbove2To64(t *testing.T) {
	blnd := kpiTokenPrice{price: "0.05", decimals: 7}
	eps := emissionUint(t, "100000000000000") // one whole token a second at eps scale 10^7
	cases := []struct {
		name                string
		eps                 *big.Int
		live                bool
		p                   kpiTokenPrice
		tokens, usd, usdWhy string
	}{
		{"priced", eps, true, blnd, "864000000000", "4320.00", ""},
		{"unpriced", eps, true, kpiTokenPrice{reason: kpiUSDUnpriced}, "864000000000", "", kpiUSDUnpriced},
		{"unverified", eps, true, kpiTokenPrice{reason: emissionUSDTokenUnverified}, "864000000000", "", emissionUSDTokenUnverified},
		{"ended", eps, false, blnd, "0", "0.00", ""},
		{"ended unpriced", eps, false, kpiTokenPrice{reason: kpiUSDUnpriced}, "0", "0.00", ""},
		{"fractional base units", big.NewInt(1), true, blnd, "0.00864", "0.00", ""},
		{
			"above 2^64", emissionUint(t, "18446744073709551615"), true,
			kpiTokenPrice{price: "1", decimals: 7},
			"159379868796850525.9536", "15937986879.69", "",
		},
	}
	for _, tc := range cases {
		got := emissionRate(tc.eps, tc.live, tc.p)
		if got.TokensPerDay != tc.tokens || deref(got.USDPerDay) != tc.usd || deref(got.USDReason) != tc.usdWhy {
			t.Errorf("%s: got tokens=%s usd=%s reason=%s, want %s %s %s", tc.name, got.TokensPerDay, deref(got.USDPerDay),
				deref(got.USDReason), tc.tokens, tc.usd, tc.usdWhy)
		}
	}
}

func withBorrowed(rv ReserveView, borrowed string) ReserveView {
	rv.Borrowed = borrowed
	return rv
}

// TestEmissionAPR_SidesAndReasons: annual USD over the side's current USD size; an empty, unpriced or unread side, or an unpriced
// token, is null with a reason and never divides by zero; an ended stream is zero.
func TestEmissionAPR_SidesAndReasons(t *testing.T) {
	blnd := kpiTokenPrice{price: "0.05", decimals: 7}
	eps := emissionUint(t, "100000000000000")
	seven := uint32(7)
	rv := ReserveView{Asset: "CUSDC", Decimals: &seven, PriceUSD: strRef("1"), Supplied: "100000000000000", Borrowed: "0"}
	unpricedRV := ReserveView{Asset: "CX", Decimals: &seven, Supplied: "100000000000000", Borrowed: "1"}
	cases := []struct {
		name     string
		live     bool
		side     string
		rv       ReserveView
		found    bool
		p        kpiTokenPrice
		apr, why string
	}{
		// 86400 x 365 x 0.05 = 1,576,800 USD a year over 10,000,000 USD supplied.
		{"supply priced", true, "supply", rv, true, blnd, "0.1577", ""},
		{"borrow side empty", true, "borrow", rv, true, blnd, "", emissionAPRSideEmpty},
		{"side unpriced", true, "supply", unpricedRV, true, blnd, "", emissionAPRSideUnpriced},
		{"reserve unread", true, "supply", ReserveView{}, false, blnd, "", emissionAPRReserveUnread},
		{"token unpriced", true, "supply", rv, true, kpiTokenPrice{reason: kpiUSDUnpriced}, "", kpiUSDUnpriced},
		{"ended", false, "borrow", rv, true, blnd, "0.0000", ""},
		// The $1 side floor: one stroop would otherwise serve 1,576,800 / 0.0000001 = 1.5768e13.
		{"side one stroop", true, "borrow", withBorrowed(rv, "1"), true, blnd, "", emissionAPRSideDust},
		{"side just below floor", true, "borrow", withBorrowed(rv, "9999999"), true, blnd, "", emissionAPRSideDust},
		{"side at floor", true, "borrow", withBorrowed(rv, "10000000"), true, blnd, "1576800.0000", ""},
		{"side just above floor", true, "borrow", withBorrowed(rv, "10000001"), true, blnd, "1576799.8423", ""},
	}
	for _, tc := range cases {
		apr, why := emissionAPR(eps, tc.live, tc.side, tc.rv, tc.found, tc.p)
		if deref(apr) != tc.apr || deref(why) != tc.why {
			t.Errorf("%s: apr=%s reason=%s, want %s %s", tc.name, deref(apr), deref(why), tc.apr, tc.why)
		}
	}
}
