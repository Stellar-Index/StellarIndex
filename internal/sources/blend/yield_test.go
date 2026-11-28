package blend

import (
	"errors"
	"math"
	"math/big"
	"math/rand"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/sources/blend/blendtest"
)

var yieldT0 = time.Unix(1_795_491_604, 0).UTC()

func after(seconds int64) time.Time { return yieldT0.Add(time.Duration(seconds) * time.Second) }

func rat(a, b int64) *big.Rat { return big.NewRat(a, b) }

// Expected values are derived by hand from the 365-day year of upstream SECONDS_PER_YEAR.
func TestRealisedAPR_HandDerived(t *testing.T) {
	for _, c := range []struct {
		name   string
		r0, r1 string
		secs   int64
		want   *big.Rat
		render string
	}{
		// growth 0.05 over 31,536,000 s = 1 year -> 1/20
		{"v2_one_year", "1000000000000", "1050000000000", 31_536_000, rat(1, 20), "0.0500"},
		// growth 1/1000 over 86,400 s = 1/365 year -> 365/1000 = 73/200
		{"v2_one_day", "1000000000000", "1001000000000", 86_400, rat(73, 200), "0.3650"},
		// V1 scale: growth 1/100 over 6,307,200 s = 73/365 = 1/5 year -> 1/20
		{"v1_73_days", "1000000000", "1010000000", 6_307_200, rat(1, 20), "0.0500"},
		// a V2 default: growth -12/1200 = -1/100 over 2,592,000 s = 30/365 = 6/73 year -> -73/600
		{"v2_default_negative", "1200000000000", "1188000000000", 2_592_000, rat(-73, 600), "-0.1217"},
		// b_rate clamped to 0 by a default: growth -1 over one year -> -1
		{"v2_b_rate_zeroed", "1200000000000", "0", 31_536_000, rat(-1, 1), "-1.0000"},
		// mainnet b_rates of BlendV2Supplies rows 0 and 1 over exactly one hour: growth 685289/1160594019282,
		// x 8760 -> 6003131640/1160594019282 = 1000521940/193432336547
		{"mainnet_shaped_hour", "1160594019282", "1160594704571", 3_600, rat(1_000_521_940, 193_432_336_547), "0.0052"},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, err := RealisedAPR(mustBig(t, c.r0), mustBig(t, c.r1), yieldT0, after(c.secs))
			if err != nil {
				t.Fatal(err)
			}
			if got.Cmp(c.want) != 0 {
				t.Fatalf("RealisedAPR = %s, want %s", got.RatString(), c.want.RatString())
			}
			if s := FormatRate(got); s != c.render {
				t.Fatalf("FormatRate = %q, want %q", s, c.render)
			}
		})
	}
}

func TestRealisedAPR_Errors(t *testing.T) {
	r := big.NewInt(1_000_000_000_000)
	for _, c := range []struct {
		name   string
		r0, r1 *big.Int
		t1     time.Time
		want   error
	}{
		{"nil_start", nil, r, after(3_600), errRealisedStartRate},
		{"zero_start", big.NewInt(0), r, after(3_600), errRealisedStartRate},
		{"negative_start", big.NewInt(-1), r, after(3_600), errRealisedStartRate},
		{"nil_end", r, nil, after(3_600), errRealisedEndRate},
		{"negative_end", r, big.NewInt(-1), after(3_600), errRealisedEndRate},
		{"same_time", r, r, yieldT0, errRealisedInterval},
		{"reversed", r, r, after(-3_600), errRealisedInterval},
		{"under_minimum", r, r, after(3_599), errRealisedInterval},
	} {
		t.Run(c.name, func(t *testing.T) {
			if _, err := RealisedAPR(c.r0, c.r1, yieldT0, c.t1); !errors.Is(err, c.want) {
				t.Fatalf("RealisedAPR err = %v, want %v", err, c.want)
			}
			if _, err := RealisedAPY(c.r0, c.r1, yieldT0, c.t1); !errors.Is(err, c.want) {
				t.Fatalf("RealisedAPY err = %v, want %v", err, c.want)
			}
		})
	}
	if _, err := RealisedAPR(r, r, yieldT0, after(3_600)); err != nil {
		t.Fatalf("exactly the minimum interval: %v", err)
	}
}

func TestRealisedAPY_HandDerived(t *testing.T) {
	for _, c := range []struct {
		name   string
		r1     string
		secs   int64
		render string
	}{
		{"one_year", "1050000000000", 31_536_000, "0.0500"},  // 1.05^1 - 1
		{"half_year", "1050000000000", 15_768_000, "0.1025"}, // 1.05^2 - 1
		{"two_years", "1210000000000", 63_072_000, "0.1000"}, // 1.21^(1/2) - 1
		{"zeroed", "0", 31_536_000, "-1.0000"},               // 0^1 - 1
	} {
		t.Run(c.name, func(t *testing.T) {
			got, err := RealisedAPY(big.NewInt(1_000_000_000_000), mustBig(t, c.r1), yieldT0, after(c.secs))
			if err != nil || got != c.render {
				t.Fatalf("RealisedAPY = %q, %v; want %q", got, err, c.render)
			}
		})
	}
}

func TestFormatRate(t *testing.T) {
	for _, c := range []struct {
		in   *big.Rat
		want string
	}{
		{rat(1, 20), "0.0500"},
		{rat(1, 20_000), "0.0001"},   // exactly half a unit rounds away from zero
		{rat(-1, 20_000), "-0.0001"}, // and keeps its sign
		{rat(1, 30_000), "0.0000"},
		{rat(-1, 30_000), "0.0000"}, // rounds to zero: unsigned
		{rat(-73, 600), "-0.1217"},
	} {
		if got := FormatRate(c.in); got != c.want {
			t.Errorf("FormatRate(%s) = %q, want %q", c.in.RatString(), got, c.want)
		}
	}
}

func randRate(r *rand.Rand) *big.Int {
	return new(big.Int).Add(big.NewInt(1_000_000_000), big.NewInt(r.Int63n(2_000_000_000_000)))
}

// randNear is base moved by up to ±0.1%, so a float APY compounded from a one-hour window stays
// finite and non-zero (0.999^8760 ≈ e^-8.8).
func randNear(r *rand.Rand, base *big.Int) *big.Int {
	n := new(big.Int).Mul(base, big.NewInt(999_000+r.Int63n(2_001)))
	return n.Quo(n, big.NewInt(1_000_000))
}

func mustAPR(t *testing.T, r0, r1 *big.Int, t0, t1 time.Time) *big.Rat {
	t.Helper()
	v, err := RealisedAPR(r0, r1, t0, t1)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestRealisedAPR_Properties(t *testing.T) {
	r := rand.New(rand.NewSource(7))
	for i := 0; i < 2_000; i++ {
		r0 := randRate(r)
		r1 := randNear(r, r0)
		secs := 3_600 + r.Int63n(63_072_000)
		t1 := after(secs)
		apr := mustAPR(t, r0, r1, yieldT0, t1)
		bigger := new(big.Int).Add(r1, big.NewInt(1+r.Int63n(1_000_000)))
		if mustAPR(t, r0, bigger, yieldT0, t1).Cmp(apr) <= 0 {
			t.Fatalf("not strictly increasing in r1: r0=%s r1=%s", r0, r1)
		}
		thousand := big.NewInt(1_000) // V1's 1e9 scale to V2's 1e12
		if scaled := mustAPR(t, new(big.Int).Mul(r0, thousand), new(big.Int).Mul(r1, thousand), yieldT0, t1); scaled.Cmp(apr) != 0 {
			t.Fatalf("rate decimals changed the APR: %s vs %s", scaled.RatString(), apr.RatString())
		}
		checkSplit(t, r, r0, r1, secs)
	}
}

// checkSplit pins how realised rates compose over [t0, tm] + [tm, t1]: APR x years adds only up to
// the cross term (a1·y1)(a2·y2), exactly, while APY compounds back to the whole window's.
func checkSplit(t *testing.T, r *rand.Rand, r0, r1 *big.Int, secs int64) {
	t.Helper()
	if secs < 2*3_600 {
		return
	}
	m := 3_600 + r.Int63n(secs-2*3_600+1)
	tm, t1 := after(m), after(secs)
	rm := randNear(r, r0)
	years := func(s int64) *big.Rat { return rat(s, 31_536_000) }
	a := new(big.Rat).Mul(mustAPR(t, r0, r1, yieldT0, t1), years(secs))
	a1 := new(big.Rat).Mul(mustAPR(t, r0, rm, yieldT0, tm), years(m))
	a2 := new(big.Rat).Mul(mustAPR(t, rm, r1, tm, t1), years(secs-m))
	sum := new(big.Rat).Add(a1, a2)
	if sum.Add(sum, new(big.Rat).Mul(a1, a2)).Cmp(a) != 0 {
		t.Fatalf("APR split identity: %s + %s + cross != %s", a1.RatString(), a2.RatString(), a.RatString())
	}
	p, err0 := realisedAPYFloat(r0, r1, yieldT0, t1)
	p1, err1 := realisedAPYFloat(r0, rm, yieldT0, tm)
	p2, err2 := realisedAPYFloat(rm, r1, tm, t1)
	if err := errors.Join(err0, err1, err2); err != nil {
		t.Fatal(err)
	}
	whole := math.Log1p(p) * float64(secs)
	parts := math.Log1p(p1)*float64(m) + math.Log1p(p2)*float64(secs-m)
	if math.Abs(whole-parts) > 1e-9*math.Max(1, math.Abs(whole)) {
		t.Fatalf("APY split identity: whole %g, parts %g", whole, parts)
	}
}

// refAccrualFactor transcribes the accrual calc_accrual returns on V2 (v2:pool/interest.rs:93-97):
// SCALAR_12 + ceil(floor(dt·SCALAR_12 / SECONDS_PER_YEAR) · cur_ir / SCALAR_7).
func refAccrualFactor(curIR *big.Int, dt int64) *big.Int {
	tw := refFloor(mul(bi(dt), bi(1_000_000_000_000)), bi(31_536_000))
	return new(big.Int).Add(bi(1_000_000_000_000), refCeil(mul(tw, curIR), bi(10_000_000)))
}

// holdUtilization moves d_supply, as a borrow or repay after the accrual would, until utilization is u.
func holdUtilization(t *testing.T, rd ReserveData, u *big.Int) ReserveData {
	t.Helper()
	target := new(big.Rat).SetFrac(mul(mul(u, rd.SuppliedUnderlying()), bi(1_000_000_000_000)), mul(bi(10_000_000), rd.DRate))
	rd.DSupply = new(big.Int).Quo(target.Num(), target.Denom())
	for range 8 {
		switch rd.Utilization().Cmp(u) {
		case 0:
			return rd
		case 1:
			rd.DSupply = new(big.Int).Sub(rd.DSupply, bi(1))
		default:
			rd.DSupply = new(big.Int).Add(rd.DSupply, bi(1))
		}
	}
	t.Fatalf("cannot hold utilization at %s", u)
	return rd
}

// stepBound bounds |g − s·δ| for one accrual, g the step's b_rate growth and s the model supply rate:
// the model's two 7-dp floors (2e-7·δ), U's ceil against the true L/S ((A/R−1)(1e-7 + 1/S)),
// time_weight's floor and the accrual's ceil ((irf+1)/R), accrued's three ceils ((D/R + 1)/S), the
// backstop floor and the supply floor (3/S), and the b_rate floor (1/b).
func stepBound(rd ReserveData, curIR, accrual *big.Int, delta *big.Rat) *big.Rat {
	const r = 1_000_000_000_000
	s := new(big.Rat).SetInt(rd.SuppliedUnderlying())
	inv := func(x *big.Rat) *big.Rat { return new(big.Rat).Inv(x) }
	e := new(big.Rat).Mul(rat(2, 10_000_000), delta)
	grow := new(big.Rat).SetFrac(new(big.Int).Sub(accrual, bi(r)), bi(r))
	e.Add(e, new(big.Rat).Mul(grow, new(big.Rat).Add(rat(1, 10_000_000), inv(s))))
	e.Add(e, new(big.Rat).SetFrac(new(big.Int).Add(curIR, bi(10_000_000)), mul(bi(10_000_000), bi(r))))
	dOverR := new(big.Rat).SetFrac(rd.DSupply, bi(r))
	e.Add(e, new(big.Rat).Quo(dOverR.Add(dOverR, rat(4, 1)), s))
	e.Add(e, new(big.Rat).SetFrac(bi(1), rd.BRate))
	return e
}

func ratPow(x *big.Rat, n int) *big.Rat {
	out := rat(1, 1)
	for range n {
		out.Mul(out, x)
	}
	return out
}

// Realised APR over hourly accruals at constant utilization must equal the model supply rate,
// compounded over the window and annualised linearly, within the derived rounding bound.
func TestRealisedAPR_MatchesModelSupplyRate(t *testing.T) {
	const steps, dt = 720, int64(3_600)
	rc := interestRefConfig()
	rc.Reactivity = 0 // calc_accrual's rate_dif is then 0: ir_mod, and with it the model rate, stays constant
	bstop := blendtest.BlendV2Supplies.BstopRate
	rd := mainnetRows(blendtest.BlendV2Supplies)[0].rd.data(t, PoolV2)
	rd.IRMod = big.NewInt(10_000_000)
	u := rd.Utilization()
	curIR := refBorrowRate(rc, u, rd.IRMod)
	model := new(big.Rat).SetFrac(SupplyRate(rc.BorrowRate(u, rd.IRMod), u, bstop), scalar7)
	delta := rat(dt, 31_536_000)
	accrual := refAccrualFactor(curIR, dt)

	start, epsSum, epsMax := rd, new(big.Rat), new(big.Rat)
	for range steps {
		if rd.Utilization().Cmp(u) != 0 {
			t.Fatalf("utilization drifted to %s from %s", rd.Utilization(), u)
		}
		eps := stepBound(rd, curIR, accrual, delta)
		epsSum.Add(epsSum, eps)
		if eps.Cmp(epsMax) > 0 {
			epsMax = eps
		}
		next := refLoad(rd, refCeil(mul(accrual, rd.DRate), bi(1_000_000_000_000)), bstop, rd.LastTime+uint64(dt))
		rd = holdUtilization(t, next, u)
	}
	got := mustAPR(t, start.BRate, rd.BRate, time.Unix(int64(start.LastTime), 0), time.Unix(int64(rd.LastTime), 0))

	span := new(big.Rat).Mul(delta, rat(steps, 1))
	step := new(big.Rat).Add(rat(1, 1), new(big.Rat).Mul(model, delta))
	ideal := new(big.Rat).Sub(ratPow(step, steps), rat(1, 1))
	ideal.Quo(ideal, span)
	tol := new(big.Rat).Mul(ratPow(new(big.Rat).Add(step, epsMax), steps-1), epsSum)
	tol.Quo(tol, span)
	diff := new(big.Rat).Sub(got, ideal)
	t.Logf("model %s, realised %s, compounded model %s, |diff| %s, bound %s",
		model.FloatString(7), got.FloatString(10), ideal.FloatString(10), new(big.Rat).Abs(diff).FloatString(12), tol.FloatString(12))
	if diff.Abs(diff).Cmp(tol) > 0 {
		t.Fatalf("realised %s, model %s compounded %s: |diff| %s > bound %s",
			got.FloatString(10), model.FloatString(7), ideal.FloatString(10), diff.FloatString(12), tol.FloatString(12))
	}
	if tol.Cmp(rat(1, 1_000_000)) >= 0 {
		t.Fatalf("bound %s is too loose to pin anything", tol.FloatString(12))
	}
}

func TestComposeNetYield(t *testing.T) {
	v := func(a, b int64) YieldPart { return YieldPart{Value: rat(a, b)} }
	why := func(s string) YieldPart { return YieldPart{Reason: s} }
	ex := func(c YieldComponent, r string) YieldExclusion { return YieldExclusion{c, r} }
	for _, c := range []struct {
		name            string
		side            YieldSide
		base, emissions YieldPart
		want            *big.Rat
		lowerBound      bool
		excluded        []YieldExclusion
	}{
		{"supply_whole", YieldSupply, v(5, 100), v(2, 100), rat(7, 100), false, nil},
		{"supply_no_emissions", YieldSupply, v(5, 100), why("unpriced"), rat(5, 100), true, []YieldExclusion{ex(ComponentEmissions, "unpriced")}},
		{"supply_default_negative", YieldSupply, v(-73, 600), v(2, 100), rat(-61, 600), false, nil},
		{"borrow_whole", YieldBorrow, v(8, 100), v(3, 100), rat(-5, 100), false, nil},
		{"borrow_no_emissions", YieldBorrow, v(8, 100), why("side_dust"), rat(-8, 100), true, []YieldExclusion{ex(ComponentEmissions, "side_dust")}},
		{"borrow_emissions_exceed", YieldBorrow, v(1, 100), v(3, 100), rat(2, 100), false, nil},
		{"no_base", YieldSupply, why("rate_unavailable"), v(2, 100), nil, false, []YieldExclusion{ex(ComponentBase, "rate_unavailable")}},
		{
			"nothing", YieldBorrow, why("rate_unavailable"), why("unpriced"), nil, false,
			[]YieldExclusion{ex(ComponentBase, "rate_unavailable"), ex(ComponentEmissions, "unpriced")},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, err := ComposeNetYield(c.side, c.base, c.emissions, collectable())
			if err != nil {
				t.Fatal(err)
			}
			if (got.Value == nil) != (c.want == nil) || (c.want != nil && got.Value.Cmp(c.want) != 0) {
				t.Fatalf("value = %v, want %v", got.Value, c.want)
			}
			if got.LowerBound != c.lowerBound || len(got.Excluded) != len(c.excluded) {
				t.Fatalf("got %+v, want lower_bound %v excluded %v", got, c.lowerBound, c.excluded)
			}
			for i := range c.excluded {
				if got.Excluded[i] != c.excluded[i] {
					t.Fatalf("excluded[%d] = %+v, want %+v", i, got.Excluded[i], c.excluded[i])
				}
			}
		})
	}
}

func TestComposeNetYield_RefusesMalformedParts(t *testing.T) {
	ok := YieldPart{Value: rat(1, 100)}
	for _, c := range []struct {
		name            string
		base, emissions YieldPart
		want            error
	}{
		{"base_both", YieldPart{Value: rat(1, 100), Reason: "x"}, ok, errYieldPartAmbiguous},
		{"base_neither", YieldPart{}, ok, errYieldPartAmbiguous},
		{"emissions_neither", ok, YieldPart{}, errYieldPartAmbiguous},
		{"emissions_negative", ok, YieldPart{Value: rat(-1, 100)}, errYieldNegativeEmiss},
	} {
		t.Run(c.name, func(t *testing.T) {
			if _, err := ComposeNetYield(YieldSupply, c.base, c.emissions, collectable()); !errors.Is(err, c.want) {
				t.Fatalf("err = %v, want %v", err, c.want)
			}
		})
	}
}

// collectable is an active pool at 80% utilization whose backstop holds no debt in the reserve.
func collectable() CollectabilityFacts {
	no := false
	return CollectabilityFacts{PoolStatusKnown: true, PoolActive: true, Utilization: big.NewInt(8_000_000), BackstopHoldsLiabilities: &no}
}

func TestRankable(t *testing.T) {
	yes := true
	for _, c := range []struct {
		name  string
		facts func(*CollectabilityFacts)
		base  YieldPart
		want  []Caveat
	}{
		{"collectable", func(*CollectabilityFacts) {}, YieldPart{Value: rat(5, 100)}, nil},
		{"lower_bound_never_ranks", func(*CollectabilityFacts) {}, YieldPart{Value: rat(5, 100)}, []Caveat{CaveatNetLowerBound}},
		{"status_unknown", func(f *CollectabilityFacts) { f.PoolStatusKnown = false }, YieldPart{Value: rat(5, 100)}, []Caveat{CaveatPoolStatusUnknown}},
		{"not_active", func(f *CollectabilityFacts) { f.PoolActive = false }, YieldPart{Value: rat(5, 100)}, []Caveat{CaveatPoolNotActive}},
		{"utilization_unknown", func(f *CollectabilityFacts) { f.Utilization = nil }, YieldPart{Value: rat(5, 100)}, []Caveat{CaveatUtilizationUnknown}},
		{"utilization_full", func(f *CollectabilityFacts) { f.Utilization = big.NewInt(10_000_000) }, YieldPart{Value: rat(5, 100)}, []Caveat{CaveatUtilizationFull}},
		{"utilization_just_below_full", func(f *CollectabilityFacts) { f.Utilization = big.NewInt(9_999_999) }, YieldPart{Value: rat(5, 100)}, nil},
		{"backstop_unknown", func(f *CollectabilityFacts) { f.BackstopHoldsLiabilities = nil }, YieldPart{Value: rat(5, 100)}, []Caveat{CaveatBackstopLiabilitiesUnknown}},
		{"backstop_holds_debt", func(f *CollectabilityFacts) { f.BackstopHoldsLiabilities = &yes }, YieldPart{Value: rat(5, 100)}, []Caveat{CaveatBackstopHoldsLiabilities}},
		{"no_value", func(*CollectabilityFacts) {}, YieldPart{Reason: "rate_unavailable"}, []Caveat{CaveatValueUnknown}},
		{"frozen_full_with_bad_debt", func(f *CollectabilityFacts) {
			f.PoolActive, f.Utilization, f.BackstopHoldsLiabilities = false, big.NewInt(10_000_000), &yes
		}, YieldPart{Value: rat(9, 10)}, []Caveat{CaveatPoolNotActive, CaveatUtilizationFull, CaveatBackstopHoldsLiabilities}},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := collectable()
			c.facts(&f)
			emissions := YieldPart{Value: rat(1, 100)}
			if c.name == "lower_bound_never_ranks" {
				emissions = YieldPart{Reason: "unpriced"}
			}
			n, err := ComposeNetYield(YieldSupply, c.base, emissions, f)
			if err != nil {
				t.Fatal(err)
			}
			ok, got := Rankable(n)
			if ok != (len(c.want) == 0) || len(got) != len(c.want) {
				t.Fatalf("Rankable = %v %v, want %v", ok, got, c.want)
			}
			for i := range c.want {
				if got[i] != c.want[i] {
					t.Fatalf("reason[%d] = %s, want %s", i, got[i], c.want[i])
				}
			}
		})
	}
}

// A unit of r0 annualised over the window must be strictly below 0.00005: r0·years > 20000.
func TestRealisedAPR_LowResolution(t *testing.T) {
	for _, c := range []struct {
		name   string
		r0, r1 int64
		secs   int64
		ok     bool
	}{
		{"partial_default_hour", 1_000, 1_001, 3_600, false}, // would render 8.7600
		{"boundary_year", 20_000, 20_001, 31_536_000, false}, // unit = 1/20000 = exactly half a decimal
		{"above_boundary_year", 20_001, 20_002, 31_536_000, true},
		{"boundary_hour", 175_200_000, 175_200_001, 3_600, false}, // 175,200,000/8760 = 20000
		{"above_boundary_hour", 175_200_001, 175_200_002, 3_600, true},
		{"v1_scalar_hour", 1_000_000_000, 1_000_000_001, 3_600, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := RealisedAPR(big.NewInt(c.r0), big.NewInt(c.r1), yieldT0, after(c.secs))
			_, errAPY := RealisedAPY(big.NewInt(c.r0), big.NewInt(c.r1), yieldT0, after(c.secs))
			if c.ok && (err != nil || errAPY != nil) {
				t.Fatalf("want a value, got %v / %v", err, errAPY)
			}
			if !c.ok && (!errors.Is(err, ErrLowResolution) || !errors.Is(errAPY, ErrLowResolution)) {
				t.Fatalf("want ErrLowResolution, got %v / %v", err, errAPY)
			}
		})
	}
}
