package blend

import (
	"math/big"
	"testing"
)

// Hand-computed from user.rs::default_liabilities. Each case makes the rounding it names decide the result, so
// flipping a ceil to a floor, cutting on the summed d-tokens, or cutting before accrual moves an asserted b_rate.
func TestDefaultedBRate_Vectors(t *testing.T) {
	for _, c := range []struct {
		name                  string
		dRate, bRate, bSupply string
		dTokens               []int64
		want                  string
	}{
		// ceil(55555555555·d_rate/S) = 68587105007 (remainder 147462283265); ceil(68587105007·S/b_supply)
		// = 69444443750 (remainder 317044590737).
		{"both_steps_fractional", "1234567890123", "1050000000001", "987654321987", []int64{55_555_555_555}, "980555556251"},
		// The cut floors to 0 but the contract rounds it up to 1.
		{"cut_that_floors_to_zero", "1234567890123", "1050000000001", "5000000000000", []int64{1}, "1050000000000"},
		// One base unit over b_supply 1 is a cut of 2·S: b_rate stops at 0.
		{"tiny_b_supply_clamps", "1234567890123", "1050000000001", "1", []int64{1}, "0"},
		// Cuts of 3 and 5 per event; the summed d-tokens would cut 6.
		{"two_defaults_compose_per_event", "1234567890123", "1050000000001", "987654321987", []int64{1, 3}, "1049999999993"},
		{"zero_d_tokens", "1234567890123", "1050000000001", "987654321987", []int64{0}, "1050000000001"},
	} {
		t.Run(c.name, func(t *testing.T) {
			rd := ReserveData{DRate: mustBig(t, c.dRate), BRate: mustBig(t, c.bRate), BSupply: mustBig(t, c.bSupply), Version: PoolV2}
			for _, d := range c.dTokens {
				b, err := DefaultedBRate(rd, big.NewInt(d))
				if err != nil {
					t.Fatal(err)
				}
				rd.BRate = b
			}
			if rd.BRate.Cmp(mustBig(t, c.want)) != 0 {
				t.Errorf("b_rate = %s, want %s", rd.BRate, c.want)
			}
		})
	}
}

// load accrues before the default, so the cut reads the accrued d_rate and lands on the accrued b_rate.
func TestDefaultedBRate_AfterAccrualInSameTransaction(t *testing.T) {
	prev := rdIn{"1000000000001", "1000000000001", "7", "3", "0", 10}.data(t, PoolV2)
	next := rdIn{"2000000000001", "999999999999", "7", "2", "0", 20}.data(t, PoolV2)
	a, err := AccrueBetween(prev, next, 2_000_000)
	if err != nil {
		t.Fatal(err)
	}
	if a.ExpectedBRate.String() != "1428571428571" || a.BRateMatches {
		t.Fatalf("accrual b_rate %s (matches %v), want 1428571428571 and a mismatch", a.ExpectedBRate, a.BRateMatches)
	}
	// ceil(1·2000000000001/S) = 3; ceil(3·S/7) = 428571428572. Cutting at prev's d_rate would cut 285714285715.
	got, err := DefaultedBRate(ReserveData{DRate: next.DRate, BRate: a.ExpectedBRate, BSupply: prev.BSupply, Version: PoolV2}, big.NewInt(1))
	if err != nil {
		t.Fatal(err)
	}
	if got.Cmp(next.BRate) != 0 {
		t.Errorf("b_rate = %s, want the stored %s", got, next.BRate)
	}
}

func TestDefaultedBRate_Refuses(t *testing.T) {
	ok := ReserveData{DRate: big.NewInt(1), BRate: big.NewInt(1), BSupply: big.NewInt(1), Version: PoolV2}
	for name, c := range map[string]struct {
		rd ReserveData
		d  *big.Int
	}{
		"v1":              {ReserveData{DRate: ok.DRate, BRate: ok.BRate, BSupply: ok.BSupply, Version: PoolV1}, big.NewInt(1)},
		"no_b_supply":     {ReserveData{DRate: ok.DRate, BRate: ok.BRate, BSupply: big.NewInt(0), Version: PoolV2}, big.NewInt(1)},
		"nil_d_tokens":    {ok, nil},
		"negative_tokens": {ok, big.NewInt(-1)},
		"nil_b_rate":      {ReserveData{DRate: ok.DRate, BSupply: ok.BSupply, Version: PoolV2}, big.NewInt(1)},
	} {
		if _, err := DefaultedBRate(c.rd, c.d); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}
