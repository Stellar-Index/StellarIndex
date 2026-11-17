package v1

import (
	"math/big"
	"testing"
)

func usdTestReserve(price string, decimals uint32, supplyAPR, borrowAPR *float64) ReserveView {
	return ReserveView{PriceUSD: strRef(price), Decimals: &decimals, SupplyAPR: supplyAPR, BorrowAPR: borrowAPR}
}

func f64(v float64) *float64 { return &v }

func strOr(s *string) string {
	if s == nil {
		return "null"
	}
	return *s
}

// TestValueEarningsLegs_TwoPoolsSumIntoTotals: pool A supplies 1 token ($2.50, accrued $0.25) at 0.05 and borrows 0.4 ($1.00)
// at 0.1; pool B supplies 30 tokens of a 6-dp asset ($30.00) at 0.02 under history_unverified, which concerns net_flow only.
// A: net 1.50, APR (0.125-0.1)/1.5 = 0.01666 -> 0.0167. B: net 30.00, APR 0.0200.
// Totals: 32.50 / 1.00 / 31.50, APR (0.125+0.6-0.1)/31.5 = 0.019841 -> 0.0198.
func TestValueEarningsLegs_TwoPoolsSumIntoTotals(t *testing.T) {
	legs := []LendingEarningsLeg{
		{Protocol: "blend", Pool: "A", Side: earningsSideSupply, Asset: strRef("x"), CurrentValue: strRef("10000000"), Accrued: strRef("1000000")},
		{Protocol: "blend", Pool: "A", Side: earningsSideBorrow, Asset: strRef("x"), CurrentValue: strRef("4000000"), Accrued: strRef("0")},
		{
			Protocol: "blnt", Pool: "B", Side: earningsSideSupply, Asset: strRef("y"), CurrentValue: strRef("30000000"),
			Reason: strRef(earningsReasonHistoryUnverified),
		},
	}
	reserves := map[string]map[string]ReserveView{
		"A": {"x": usdTestReserve("2.5", 7, f64(0.05), f64(0.1))},
		"B": {"y": usdTestReserve("1", 6, f64(0.02), f64(0.09))},
	}
	pools, totals := valueEarningsLegs(legs, reserves)
	if strOr(legs[0].ValueUSD) != "2.50" || strOr(legs[0].AccruedUSD) != "0.25" || strOr(legs[1].ValueUSD) != "1.00" ||
		strOr(legs[1].AccruedUSD) != "0.00" || strOr(legs[2].ValueUSD) != "30.00" || legs[2].AccruedUSD != nil || legs[2].UsdReason != nil {
		t.Fatalf("legs = %+v", legs)
	}
	want := []struct{ pool, sup, bor, net, apr string }{
		{"A", "2.50", "1.00", "1.50", "0.0167"}, {"B", "30.00", "0.00", "30.00", "0.0200"}, {"", "32.50", "1.00", "31.50", "0.0198"},
	}
	got := []LendingUSDTotals{pools[0].LendingUSDTotals, pools[1].LendingUSDTotals, totals}
	if len(pools) != 2 || pools[0].Pool != "A" || pools[1].Pool != "B" || pools[1].Protocol != "blnt" {
		t.Fatalf("pools = %+v", pools)
	}
	for i, w := range want {
		g := got[i]
		if g.SuppliedUSD != w.sup || g.BorrowedUSD != w.bor || strOr(g.NetUSD) != w.net || strOr(g.NetAPR) != w.apr || g.LowerBound ||
			len(g.Excluded) != 0 || g.NetUSDReason != nil || g.NetAPRReason != nil {
			t.Fatalf("%s totals = %+v apr %s", w.pool, g, strOr(g.NetAPR))
		}
	}
}

// TestValueEarningsLegs_BackstopIsNamed: a backstop leg is unpriced, named as kind backstop, and makes supplied_usd a floor.
func TestValueEarningsLegs_BackstopIsNamed(t *testing.T) {
	legs := []LendingEarningsLeg{
		{Protocol: "blend", Pool: "A", Side: earningsSideSupply, Asset: strRef("x"), CurrentValue: strRef("10000000")},
		{Protocol: "blend", Pool: "A", Side: earningsSideBackstop, Asset: strRef("bt"), CurrentValue: strRef("5")},
	}
	_, totals := valueEarningsLegs(legs, map[string]map[string]ReserveView{"A": {"x": usdTestReserve("2.5", 7, f64(0.05), nil)}})
	if strOr(legs[1].UsdReason) != earningsUSDUnpriced || legs[1].ValueUSD != nil {
		t.Fatalf("backstop leg = %+v", legs[1])
	}
	ex := totals.Excluded
	if totals.SuppliedUSD != "2.50" || !totals.LowerBound || len(ex) != 1 || ex[0].Kind != "backstop" || strOr(ex[0].Pool) != "A" ||
		strOr(ex[0].Asset) != "bt" || ex[0].Reason != earningsUSDUnpriced || totals.NetUSD != nil || strOr(totals.NetUSDReason) != netInputsPartial ||
		totals.NetAPR != nil || strOr(totals.NetAPRReason) != netInputsPartial {
		t.Fatalf("totals = %+v", totals)
	}
}

// TestValueEarningsLegs_AprUnavailable: a valued supply leg whose reserve serves no APR leaves net_usd served and net_apr null.
func TestValueEarningsLegs_AprUnavailable(t *testing.T) {
	legs := []LendingEarningsLeg{{Protocol: "blend", Pool: "A", Side: earningsSideSupply, Asset: strRef("x"), CurrentValue: strRef("10000000")}}
	_, totals := valueEarningsLegs(legs, map[string]map[string]ReserveView{"A": {"x": usdTestReserve("2.5", 7, nil, nil)}})
	if strOr(totals.NetUSD) != "2.50" || totals.NetAPR != nil || strOr(totals.NetAPRReason) != netAPRUnavailable || totals.LowerBound {
		t.Fatalf("totals = %+v", totals)
	}
}

// TestValueEarningsLegs_ZeroValueUnpricedAccrued: a zero current_value is $0 without a price, but its non-zero accrued is not,
// so accrued_usd is null with a reason and the leg still counts.
func TestValueEarningsLegs_ZeroValueUnpricedAccrued(t *testing.T) {
	legs := []LendingEarningsLeg{{Protocol: "blend", Pool: "A", Side: earningsSideSupply, Asset: strRef("x"), CurrentValue: strRef("0"), Accrued: strRef("5000000")}}
	_, totals := valueEarningsLegs(legs, map[string]map[string]ReserveView{"A": {"x": {}}})
	if strOr(legs[0].ValueUSD) != "0.00" || legs[0].AccruedUSD != nil || strOr(legs[0].UsdReason) != earningsUSDUnpriced ||
		totals.LowerBound || strOr(totals.NetUSD) != "0.00" {
		t.Fatalf("leg = %+v totals = %+v", legs[0], totals)
	}
}

// TestValueEarningsLegs_ServedValueIsValued: a balance_mismatch leg still serves current_value, so it is valued; its decimals,
// unread from the reserve state, come from the /reserves view that priced it; a -$0.0000001 accrued renders unsigned.
func TestValueEarningsLegs_ServedValueIsValued(t *testing.T) {
	legs := []LendingEarningsLeg{{
		Protocol: "blend", Pool: "A", Side: earningsSideSupply, Asset: strRef("x"), CurrentValue: strRef("20000000"),
		Accrued: strRef("-1"), Reason: strRef(earningsReasonBalanceMismatch),
	}}
	_, totals := valueEarningsLegs(legs, map[string]map[string]ReserveView{"A": {"x": usdTestReserve("1", 7, f64(0.05), nil)}})
	if strOr(legs[0].ValueUSD) != "2.00" || strOr(legs[0].AccruedUSD) != "0.00" || legs[0].Decimals == nil || *legs[0].Decimals != 7 ||
		strOr(totals.NetUSD) != "2.00" {
		t.Fatalf("leg = %+v totals = %+v", legs[0], totals)
	}
}

// TestRatString pins half-away-from-zero rounding and an unsigned zero.
func TestRatString(t *testing.T) {
	for _, tc := range []struct {
		r    *big.Rat
		prec int
		want string
	}{
		{big.NewRat(-1, 1000), 2, "0.00"},
		{big.NewRat(-1, 10_000_000), 4, "0.0000"},
		{big.NewRat(-5, 1000), 2, "-0.01"},
		{big.NewRat(5, 1000), 2, "0.01"},
		{big.NewRat(-1234, 100), 2, "-12.34"},
		{big.NewRat(0, 1), 2, "0.00"},
	} {
		if got := ratString(tc.r, tc.prec); got != tc.want {
			t.Errorf("ratString(%s, %d) = %q, want %q", tc.r, tc.prec, got, tc.want)
		}
	}
}
