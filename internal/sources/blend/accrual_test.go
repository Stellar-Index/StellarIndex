package blend

import (
	"math/big"
	"math/rand/v2"
	"testing"
)

type rdIn struct {
	dRate, bRate, bSupply, dSupply, bc string
	lt                                 uint64
}

func mustBig(t testing.TB, s string) *big.Int {
	t.Helper()
	n, ok := new(big.Int).SetString(s, 10)
	if !ok {
		t.Fatalf("bad decimal %q", s)
	}
	return n
}

func (r rdIn) data(t testing.TB, v PoolVersion) ReserveData {
	t.Helper()
	return ReserveData{
		DRate: mustBig(t, r.dRate), BRate: mustBig(t, r.bRate), IRMod: big.NewInt(1),
		BSupply: mustBig(t, r.bSupply), DSupply: mustBig(t, r.dSupply),
		BackstopCredit: mustBig(t, r.bc), LastTime: r.lt, Version: v,
	}
}

type accrualWant struct {
	gross, backstop, intended, realised, bRate, delta, residual string
	matches                                                     bool
}

func assertAccrual(t *testing.T, got Accrual, w accrualWant) {
	t.Helper()
	for _, c := range []struct {
		name string
		got  *big.Int
		want string
	}{
		{"Gross", got.Gross, w.gross},
		{"Backstop", got.Backstop, w.backstop},
		{"SupplierIntended", got.SupplierIntended, w.intended},
		{"SupplierRealised", got.SupplierRealised, w.realised},
		{"ExpectedBRate", got.ExpectedBRate, w.bRate},
		{"BackstopCreditDelta", got.BackstopCreditDelta, w.delta},
		{"BackstopResidual", got.BackstopResidual, w.residual},
	} {
		if c.got.Cmp(mustBig(t, c.want)) != 0 {
			t.Errorf("%s = %s, want %s", c.name, c.got, c.want)
		}
	}
	if got.BRateMatches != w.matches {
		t.Errorf("BRateMatches = %v, want %v", got.BRateMatches, w.matches)
	}
}

// Hand-computed from reserve.rs::load. The *_rounding cases make every rounding step fractional, so
// flipping any one floor/ceil in AccrueBetween moves an asserted value by one base unit.
func TestAccrueBetween_Vectors(t *testing.T) {
	cases := []struct {
		name       string
		v          PoolVersion
		prev, next rdIn
		bstop      uint32
		want       accrualWant
	}{
		// blend-contracts v1.0.0 pool/src/pool/reserve.rs::test_load_reserve (default_reserve_meta, ts 617280).
		{
			"v1_contract_test_load_reserve", PoolV1,
			rdIn{"1345678123", "1123456789", "990000000", "650000000", "0", 0},
			rdIn{"1349657800", "1125547124", "990000000", "650000000", "517358", 617280},
			2_000_000,
			accrualWant{"2586790", "517358", "2069432", "2069431", "1125547124", "517358", "0", true},
		},
		// blend-contracts-v2 v2.0.0 pool/src/pool/reserve.rs::test_load_reserve.
		{
			"v2_contract_test_load_reserve", PoolV2,
			rdIn{"1345678123000", "1123456789000", "990000000", "650000000", "0", 0},
			rdIn{"1349657798173", "1125547124242", "990000000", "650000000", "517357", 617280},
			2_000_000,
			accrualWant{"2586789", "517357", "2069432", "2069431", "1125547124242", "517357", "0", true},
		},
		// blend-contracts-v2 v2.0.0 pool/src/pool/reserve.rs::test_load_reserve_accrues_b_rate (18 decimals).
		{
			"v2_contract_test_load_reserve_accrues_b_rate", PoolV2,
			rdIn{"1500000000000", "1300000000000", "10000000000000000000000000000", "100000000000000000000000000", "0", 995},
			rdIn{"1500000002562", "1300000000020", "10000000000000000000000000000", "100000000000000000000000000", "51240000000000000", 1000},
			2_000_000,
			accrualWant{"256200000000000000", "51240000000000000", "204960000000000000", "200000000000000000", "1300000000020", "51240000000000000", "0", true},
		},
		{
			"v1_rounding", PoolV1,
			rdIn{"1000000001", "1000000001", "7", "3", "0", 10},
			rdIn{"2000000001", "1285714285", "7", "3", "1", 20},
			5_000_000,
			accrualWant{"3", "1", "2", "1", "1285714285", "1", "0", true},
		},
		// A supply and a borrow after load() move next's supplies; accrual reads only prev's.
		{
			"actions_after_load_ignored", PoolV1,
			rdIn{"1000000001", "1000000001", "7", "3", "0", 10},
			rdIn{"2000000001", "1285714285", "70000", "30000", "1", 20},
			5_000_000,
			accrualWant{"3", "1", "2", "1", "1285714285", "1", "0", true},
		},
		{
			"v2_rounding", PoolV2,
			rdIn{"1000000000001", "1000000000001", "7", "3", "0", 10},
			rdIn{"2000000000001", "1285714285714", "7", "3", "1", 20},
			5_000_000,
			accrualWant{"3", "1", "2", "1", "1285714285714", "1", "0", true},
		},
		{
			"v1_bstop_rate_zero", PoolV1,
			rdIn{"1000000001", "1000000001", "7", "3", "0", 10},
			rdIn{"2000000001", "1428571428", "7", "3", "0", 20},
			0,
			accrualWant{"3", "0", "3", "2", "1428571428", "0", "0", true},
		},
		{
			"v1_above_2^63", PoolV1,
			rdIn{"1000000000", "1000000000", "1000000000000000000000000000000", "1000000000000000000000000000000", "0", 1},
			rdIn{"1500000000", "1400000000", "1000000000000000000000000000000", "1000000000000000000000000000000", "100000000000000000000000000000", 2},
			2_000_000,
			accrualWant{"500000000000000000000000000000", "100000000000000000000000000000", "400000000000000000000000000000", "400000000000000000000000000000", "1400000000", "100000000000000000000000000000", "0", true},
		},
		{
			"v2_near_2^127", PoolV2,
			rdIn{"1000000000000", "1000000000000", "150000000000000000000000000000000000000", "10000000000000000000000000000000000000", "0", 1},
			rdIn{"1100000000000", "1006666666666", "150000000000000000000000000000000000000", "10000000000000000000000000000000000000", "0", 2},
			0,
			accrualWant{"1000000000000000000000000000000000000", "0", "1000000000000000000000000000000000000", "999999999900000000000000000000000000", "1006666666666", "0", "0", true},
		},
		{
			"same_timestamp_zero", PoolV2,
			rdIn{"1300000000000", "1200000000000", "500", "300", "9", 50},
			rdIn{"1300000000000", "1200000000000", "800", "100", "9", 50},
			2_000_000,
			accrualWant{"0", "0", "0", "0", "1200000000000", "0", "0", true},
		},
		// ceil(1·1.000000001) == ceil(1·1.000000005): d_rate moves, liabilities do not.
		{
			"d_rate_moved_gross_zero", PoolV1,
			rdIn{"1000000001", "1000000000", "10", "1", "7", 1},
			rdIn{"1000000005", "1000000000", "10", "1", "7", 2},
			2_000_000,
			accrualWant{"0", "0", "0", "0", "1000000000", "0", "0", true},
		},
		{
			"zero_b_supply_time_moves", PoolV2,
			rdIn{"1000000000000", "1000000000000", "0", "0", "0", 1},
			rdIn{"1000000000000", "1000000000000", "0", "0", "0", 9},
			2_000_000,
			accrualWant{"0", "0", "0", "0", "1000000000000", "0", "0", true},
		},
		{
			"zero_utilisation_time_moves", PoolV1,
			rdIn{"1000000000", "1100000000", "100", "0", "4", 1},
			rdIn{"1000000000", "1100000000", "100", "0", "4", 9},
			2_000_000,
			accrualWant{"0", "0", "0", "0", "1100000000", "0", "0", true},
		},
		// A b_rate below accrual (V2 default) and an extra credit (gulp) are reported, not absorbed.
		{
			"unexplained_b_rate_and_gulp_reported", PoolV1,
			rdIn{"1000000001", "1000000001", "7", "3", "0", 10},
			rdIn{"2000000001", "1285714280", "7", "3", "6", 20},
			5_000_000,
			accrualWant{"3", "1", "2", "1", "1285714285", "6", "5", false},
		},
		{
			"interest_auction_draw_reported", PoolV1,
			rdIn{"1000000001", "1000000001", "7", "3", "100", 10},
			rdIn{"2000000001", "1285714285", "7", "3", "1", 20},
			5_000_000,
			accrualWant{"3", "1", "2", "1", "1285714285", "-99", "-100", true},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := AccrueBetween(c.prev.data(t, c.v), c.next.data(t, c.v), c.bstop)
			if err != nil {
				t.Fatalf("AccrueBetween: %v", err)
			}
			assertAccrual(t, got, c.want)
		})
	}
}

func TestAccrueBetween_RefusesImpossiblePairs(t *testing.T) {
	base := rdIn{"1000000001", "1000000001", "7", "3", "0", 10}
	moved := rdIn{"2000000001", "1285714285", "7", "3", "1", 20}
	with := func(r rdIn, f func(*rdIn)) rdIn { f(&r); return r }
	cases := []struct {
		name       string
		pv, nv     PoolVersion
		prev, next rdIn
		bstop      uint32
		mutate     func(prev, next *ReserveData)
	}{
		{"d_rate_decreasing", PoolV1, PoolV1, moved, with(base, func(r *rdIn) { r.lt = 30 }), 0, nil},
		{"negative_input", PoolV1, PoolV1, with(base, func(r *rdIn) { r.bSupply = "-7" }), moved, 0, nil},
		{"missing_field", PoolV1, PoolV1, base, moved, 0, func(_, n *ReserveData) { n.BackstopCredit = nil }},
		{"zero_b_supply_with_accrual", PoolV1, PoolV1, with(base, func(r *rdIn) { r.bSupply = "0" }), moved, 0, nil},
		{"zero_d_supply_d_rate_moved", PoolV1, PoolV1, with(base, func(r *rdIn) { r.dSupply = "0" }), moved, 0, nil},
		{"same_timestamp_d_rate_moved", PoolV1, PoolV1, base, with(moved, func(r *rdIn) { r.lt = 10 }), 0, nil},
		{"last_time_backwards", PoolV1, PoolV1, base, with(base, func(r *rdIn) { r.lt = 9 }), 0, nil},
		{"unknown_version", PoolVersionUnknown, PoolVersionUnknown, base, moved, 0, nil},
		{"version_mismatch", PoolV1, PoolV2, base, moved, 0, nil},
		{"bstop_rate_100pct", PoolV1, PoolV1, base, moved, 10_000_000, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			prev, next := c.prev.data(t, c.pv), c.next.data(t, c.nv)
			if c.mutate != nil {
				c.mutate(&prev, &next)
			}
			if got, err := AccrueBetween(prev, next, c.bstop); err == nil {
				t.Fatalf("AccrueBetween = %+v, want error", got)
			}
		})
	}
}

// A fork's fee hook may take at most the whole of gross; anything else is refused, not clamped.
func TestAccrueBetweenWithFee_RefusesFeeOutsideGross(t *testing.T) {
	prev := rdIn{"1000000001", "1000000001", "7", "3", "0", 10}.data(t, PoolV1)
	next := rdIn{"2000000001", "1285714285", "7", "3", "1", 20}.data(t, PoolV1)
	for _, f := range []*big.Int{nil, big.NewInt(-1), big.NewInt(4)} {
		if got, err := AccrueBetweenWithFee(prev, next, 5_000_000, func(*big.Int) *big.Int { return f }); err == nil {
			t.Errorf("fee %v on gross 3: got %+v, want error", f, got)
		}
	}
	a, err := AccrueBetweenWithFee(prev, next, 5_000_000, func(*big.Int) *big.Int { return big.NewInt(3) })
	if err != nil || a.Backstop.Sign() != 0 || a.SupplierIntended.Sign() != 0 {
		t.Errorf("whole-gross fee: %+v, %v; want zero backstop and supplier share", a, err)
	}
	if a, err := AccrueBetween(prev, next, 5_000_000); err != nil || a.ProtocolFee.Sign() != 0 {
		t.Errorf("Blend accrual: protocol fee %v, %v; want 0", a.ProtocolFee, err)
	}
}

// V2's update_pool (config.rs execute_update_pool) loads and stores every reserve at the OLD
// bstop_rate before writing the new one, so that version reconciles only at the old rate.
func TestAccrueBetween_V2RateChangeAccruesAtOldRate(t *testing.T) {
	prev := rdIn{"1000000000001", "1000000000001", "7", "3", "0", 10}.data(t, PoolV2)
	next := rdIn{"2000000000001", "1428571428571", "7", "3", "0", 20}.data(t, PoolV2)
	for _, c := range []struct {
		name  string
		bstop uint32
		want  accrualWant
	}{
		{"old_rate", 2_000_000, accrualWant{"3", "0", "3", "2", "1428571428571", "0", "0", true}},
		{"new_rate", 5_000_000, accrualWant{"3", "1", "2", "1", "1285714285714", "0", "-1", false}},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, err := AccrueBetween(prev, next, c.bstop)
			if err != nil {
				t.Fatal(err)
			}
			assertAccrual(t, got, c.want)
		})
	}
}

// refLoad transliterates Reserve::load after calc_accrual (blend-contracts v1.0.0 and
// blend-contracts-v2 v2.0.0 pool/src/pool/reserve.rs, v2's accrue inlined), given the d_rate
// calc_accrual produced, using fuzz_test.go's independent rounding helpers.
func refLoad(prev ReserveData, newDRate *big.Int, bstopRate uint32, now uint64) ReserveData {
	s := bi(1_000_000_000_000)
	if prev.Version == PoolV1 {
		s = bi(1_000_000_000)
	}
	r := ReserveData{
		DRate: new(big.Int).Set(prev.DRate), BRate: new(big.Int).Set(prev.BRate), IRMod: prev.IRMod,
		BSupply: prev.BSupply, DSupply: prev.DSupply,
		BackstopCredit: new(big.Int).Set(prev.BackstopCredit), LastTime: prev.LastTime, Version: prev.Version,
	}
	if now == r.LastTime {
		return r
	}
	r.LastTime = now
	if r.BSupply.Sign() == 0 || refCeil(mul(r.DSupply, r.DRate), s).Sign() == 0 {
		return r
	}
	preUpdateSupply := refFloor(mul(r.BSupply, r.BRate), s)
	preUpdateLiabilities := refCeil(mul(r.DSupply, r.DRate), s)
	r.DRate = new(big.Int).Set(newDRate)
	accrued := new(big.Int).Sub(refCeil(mul(r.DSupply, r.DRate), s), preUpdateLiabilities)
	if accrued.Sign() > 0 {
		newBackstopCredit := bi(0)
		if bstopRate > 0 {
			newBackstopCredit = refFloor(mul(accrued, bi(int64(bstopRate))), bi(10_000_000))
			r.BackstopCredit.Add(r.BackstopCredit, newBackstopCredit)
		}
		n := new(big.Int).Add(preUpdateSupply, accrued)
		r.BRate = refFloor(mul(n.Sub(n, newBackstopCredit), s), r.BSupply)
	}
	return r
}

// checkAgainstRef runs one transition through refLoad and asserts AccrueBetween reconciles it
// exactly and holds its invariants.
func checkAgainstRef(t *testing.T, prev ReserveData, newDRate *big.Int, bstop uint32, now uint64) {
	t.Helper()
	next := refLoad(prev, newDRate, bstop, now)
	// The action that triggered load then moves the supplies; accrual must not read them.
	next.BSupply = new(big.Int).Add(next.BSupply, bi(12_345))
	next.DSupply = new(big.Int).Mul(next.DSupply, bi(3))
	a, err := AccrueBetween(prev, next, bstop)
	if err != nil {
		t.Fatalf("AccrueBetween(%+v, %+v): %v", prev, next, err)
	}
	if !a.BRateMatches || a.ExpectedBRate.Cmp(next.BRate) != 0 || a.BackstopResidual.Sign() != 0 {
		t.Fatalf("disagrees with contract: prev=%+v next=%+v got=%+v", prev, next, a)
	}
	s := prev.rateScalar()
	dust := new(big.Int).Add(new(big.Int).Quo(prev.BSupply, s), bi(1))
	lost := new(big.Int).Sub(a.SupplierIntended, a.SupplierRealised)
	switch {
	case a.Gross.Sign() < 0:
		t.Fatalf("gross %s < 0", a.Gross)
	case a.Backstop.Sign() < 0 || a.Backstop.Cmp(a.Gross) > 0:
		t.Fatalf("backstop %s outside [0, gross %s]", a.Backstop, a.Gross)
	case lost.Sign() < 0 || lost.Cmp(dust) > 0:
		t.Fatalf("intended %s − realised %s outside [0, %s]", a.SupplierIntended, a.SupplierRealised, dust)
	}
}

func randBits(r *rand.Rand, maxBits int) *big.Int {
	n := new(big.Int)
	for range r.IntN(maxBits) + 1 {
		n.Lsh(n, 1)
		if r.IntN(2) == 1 {
			n.SetBit(n, 0, 1)
		}
	}
	return n
}

func TestAccrueBetween_AgreesWithContract(t *testing.T) {
	r := rand.New(rand.NewPCG(0x5eed, 0xb1e4d))
	for range 2000 {
		v := PoolV1
		if r.IntN(2) == 1 {
			v = PoolV2
		}
		s := ReserveData{Version: v}.rateScalar()
		prev := ReserveData{
			DRate:          new(big.Int).Add(s, randBits(r, 40)),
			BRate:          new(big.Int).Add(new(big.Int).Quo(s, bi(2)), randBits(r, 40)),
			BSupply:        randBits(r, 120),
			DSupply:        randBits(r, 120),
			BackstopCredit: randBits(r, 64),
			LastTime:       uint64(r.IntN(1 << 30)),
			Version:        v,
		}
		newDRate := new(big.Int).Add(prev.DRate, randBits(r, 40))
		checkAgainstRef(t, prev, newDRate, r.Uint32N(10_000_000), prev.LastTime+uint64(r.IntN(3)))
	}
}

func FuzzAccrueBetween(f *testing.F) {
	f.Add(uint64(0), uint64(3), uint64(1_000_000_001), uint64(1_000_000_000), uint64(0), uint64(7), uint64(1_000_000_001), uint32(5_000_000), false)
	f.Add(uint64(0), uint64(650_000_000), uint64(1_345_678_123_000), uint64(3_979_675_173), uint64(0), uint64(990_000_000), uint64(1_123_456_789_000), uint32(2_000_000), true)
	f.Add(^uint64(0)>>1, ^uint64(0), ^uint64(0), ^uint64(0), ^uint64(0)>>1, ^uint64(0), ^uint64(0), uint32(9_999_999), true)
	f.Fuzz(func(t *testing.T, dHi, dLo, dRate, dDelta, bHi, bLo, bRate uint64, bstop uint32, v2 bool) {
		v := PoolV1
		if v2 {
			v = PoolV2
		}
		prev := ReserveData{
			DRate:   new(big.Int).SetUint64(max(dRate, 1)),
			BRate:   new(big.Int).SetUint64(max(bRate, 1)),
			BSupply: u128FromParts(bHi, bLo), DSupply: u128FromParts(dHi, dLo),
			BackstopCredit: bi(0), LastTime: 100, Version: v,
		}
		newDRate := new(big.Int).Add(prev.DRate, new(big.Int).SetUint64(dDelta))
		checkAgainstRef(t, prev, newDRate, bstop%10_000_000, 101)
	})
}
