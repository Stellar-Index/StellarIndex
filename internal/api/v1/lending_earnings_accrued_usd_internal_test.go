package v1

import "testing"

// TestValueEarningsLegs_AccruedUSDPerPoolAndTotal sums each pool's and the account's accrued interest exactly: A earns $0.25
// and pays $0.10; B (BLNT) earns 2^70 base units of a 6-dp $1 token, exact past 2^64.
func TestValueEarningsLegs_AccruedUSDPerPoolAndTotal(t *testing.T) {
	legs := []LendingEarningsLeg{
		{Protocol: "blend", Pool: "A", Side: earningsSideSupply, Asset: strRef("x"), CurrentValue: strRef("10000000"), Accrued: strRef("1000000")},
		{Protocol: "blend", Pool: "A", Side: earningsSideBorrow, Asset: strRef("x"), CurrentValue: strRef("4000000"), Accrued: strRef("400000")},
		{Protocol: "blnt", Pool: "B", Side: earningsSideSupply, Asset: strRef("y"), CurrentValue: strRef("1"), Accrued: strRef("1180591620717411303424")},
	}
	reserves := map[string]map[string]ReserveView{
		"A": {"x": usdTestReserve("2.5", 7, f64(0.05), f64(0.1))},
		"B": {"y": usdTestReserve("1", 6, f64(0.02), f64(0.09))},
	}
	pools, totals := valueEarningsLegs(legs, reserves)
	want := []struct{ sup, bor, net string }{
		{"0.25", "0.10", "0.15"}, {"1180591620717411.30", "0.00", "1180591620717411.30"}, {"1180591620717411.55", "0.10", "1180591620717411.45"},
	}
	for i, g := range []LendingAccruedUSD{pools[0].LendingAccruedUSD, pools[1].LendingAccruedUSD, totals.LendingAccruedUSD} {
		w := want[i]
		if g.AccruedSupplyUSD != w.sup || g.AccruedBorrowUSD != w.bor || strOr(g.AccruedUSD) != w.net || g.AccruedPartial ||
			len(g.AccruedExcluded) != 0 || g.AccruedUSDReason != nil {
			t.Errorf("%d accrued = %+v (net %s), want %+v", i, g, strOr(g.AccruedUSD), w)
		}
	}
}

// TestValueEarningsLegs_AccruedPartialNamesEveryLeg: an unreconciled leg, an unpriced leg and a backstop leg are each named
// in accrued_excluded, the sums are flagged partial and accrued_usd is withheld.
func TestValueEarningsLegs_AccruedPartialNamesEveryLeg(t *testing.T) {
	legs := []LendingEarningsLeg{
		{Protocol: "blend", Pool: "C", Side: earningsSideSupply, Asset: strRef("x"), CurrentValue: strRef("10000000"), Accrued: strRef("1000000")},
		{Protocol: "blend", Pool: "C", Side: earningsSideBorrow, Asset: strRef("x"), CurrentValue: strRef("4000000")},
		{Protocol: "blend", Pool: "C", Side: earningsSideSupply, Asset: strRef("z"), CurrentValue: strRef("5"), Accrued: strRef("1")},
		{Protocol: "blend", Pool: "C", Side: earningsSideBackstop, CurrentValue: strRef("5"), Accrued: strRef("1")},
	}
	reserves := map[string]map[string]ReserveView{"C": {"x": usdTestReserve("2.5", 7, f64(0.05), f64(0.1)), "z": {}}}
	pools, totals := valueEarningsLegs(legs, reserves)
	for _, g := range []LendingAccruedUSD{pools[0].LendingAccruedUSD, totals.LendingAccruedUSD} {
		if g.AccruedSupplyUSD != "0.25" || g.AccruedBorrowUSD != "0.00" || g.AccruedUSD != nil || strOr(g.AccruedUSDReason) != netInputsPartial ||
			!g.AccruedPartial || len(g.AccruedExcluded) != 3 ||
			g.AccruedExcluded[0].Kind != earningsSideBorrow || g.AccruedExcluded[0].Reason != earningsAccruedUnavailable ||
			g.AccruedExcluded[1].Reason != earningsUSDUnpriced || g.AccruedExcluded[2].Kind != earningsSideBackstop {
			t.Errorf("accrued = %+v", g)
		}
	}
}
