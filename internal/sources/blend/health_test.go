package blend

import (
	"errors"
	"math/big"
	"math/rand/v2"
	"strings"
	"testing"
)

const (
	assetA = "CA_SEVEN_DECIMALS"
	assetB = "CB_SIX_DECIMALS"
)

func decInt(s string) *big.Int {
	v, ok := new(big.Int).SetString(strings.ReplaceAll(s, "_", ""), 10)
	if !ok {
		panic(s)
	}
	return v
}

func goldenReserves(version PoolVersion, bRateA, dRateA, bRateB, dRateB string) map[string]HealthReserve {
	cfgA := ReserveConfig{Index: 0, Decimals: 7, CFactor: 7_500_000, LFactor: 9_000_000}
	cfgB := ReserveConfig{Index: 1, Decimals: 6, CFactor: 7_500_000, LFactor: 9_000_000}
	rdA := ReserveData{BRate: decInt(bRateA), DRate: decInt(dRateA), Version: version}
	rdB := ReserveData{BRate: decInt(bRateB), DRate: decInt(dRateB), Version: version}
	return map[string]HealthReserve{assetA: {Data: &rdA, Config: &cfgA}, assetB: {Data: &rdB, Config: &cfgB}}
}

func goldenPrices() map[string]OraclePrice {
	return map[string]OraclePrice{
		assetA: {Price: big.NewInt(1_234_567), Decimals: 7},  // 0.1234567
		assetB: {Price: big.NewInt(10_000_100), Decimals: 7}, // 1.00001
	}
}

// Collateral 1_000_000_000 b-tokens of A (7 decimals), liability 5_000_000 d-tokens of B (6 decimals),
// c_factor 0.75 and l_factor 0.9 on both, oracle prices at 7 decimals.
//
// V2 (rates at 1e12): b_rate_A = 1_000_000_123_457, d_rate_B = 1_100_000_000_007.
//
//	A underlying  = floor(1_000_000_000 * 1_000_000_123_457 / 1e12) = floor(1_000_000_123.457) = 1_000_000_123
//	A effective   = floor(1_000_000_123 * 7_500_000 / 1e7)          = floor(750_000_092.25)     =   750_000_092
//	A base        = floor(1_234_567 * 750_000_092 / 1e7)
//	              = floor(925_925_363_580_164 / 1e7)                = floor(92_592_536.358)     =    92_592_536
//	B underlying  = ceil(5_000_000 * 1_100_000_000_007 / 1e12)      = ceil(5_500_000.000035)    =     5_500_001
//	B effective   = ceil(5_500_001 * 1e7 / 9_000_000)               = ceil(6_111_112.222)       =     6_111_113
//	B base        = ceil(10_000_100 * 6_111_113 / 1e6)
//	              = ceil(61_111_741_111_300 / 1e6)                  = ceil(61_111_741.1113)     =    61_111_742
//	HF 4dp        = floor(92_592_536 * 1e4 / 61_111_742) = floor(15_151.349…) = 15_151 -> "1.5151"
//
// V1 (rates at 1e9): b_rate_A = 1_000_000_123, d_rate_B = 1_100_000_001.
//
//	A underlying  = floor(1_000_000_000 * 1_000_000_123 / 1e9) = 1_000_000_123 (exact), so A base = 92_592_536
//	B underlying  = ceil(5_000_000 * 1_100_000_001 / 1e9) = ceil(5_500_000.005) = 5_500_001, so B base = 61_111_742
func TestAccountHealth_GoldenHandComputed(t *testing.T) {
	pos := Positions{
		Collateral:  map[string]*big.Int{assetA: big.NewInt(1_000_000_000)},
		Liabilities: map[string]*big.Int{assetB: big.NewInt(5_000_000)},
	}
	cases := []struct {
		name     string
		version  PoolVersion
		reserves map[string]HealthReserve
	}{
		{"v2", PoolV2, goldenReserves(PoolV2, "1_000_000_123_457", "1_000_000_000_000", "1_000_000_000_000", "1_100_000_000_007")},
		{"v1", PoolV1, goldenReserves(PoolV1, "1_000_000_123", "1_000_000_000", "1_000_000_000", "1_100_000_001")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, l, err := AccountHealth(pos, tc.reserves, goldenPrices(), tc.version)
			if err != nil {
				t.Fatal(err)
			}
			if c.Cmp(big.NewInt(92_592_536)) != 0 || l.Cmp(big.NewInt(61_111_742)) != 0 {
				t.Fatalf("collateral_base=%s liability_base=%s, want 92592536 and 61111742", c, l)
			}
			if hf, ok := HFFloor4(c, l); !ok || hf != "1.5151" {
				t.Fatalf("HFFloor4 = %q, %v; want 1.5151", hf, ok)
			}
			if liq, err := Liquidatable(tc.version, c, l); err != nil || liq {
				t.Fatalf("Liquidatable = %v, %v; want false", liq, err)
			}
		})
	}
}

func TestAccountHealth_VersionMismatchRefused(t *testing.T) {
	pos := Positions{Collateral: map[string]*big.Int{assetA: big.NewInt(1)}}
	v1Reserves := goldenReserves(PoolV1, "1_000_000_000", "1_000_000_000", "1_000_000_000", "1_000_000_000")
	if _, _, err := AccountHealth(pos, v1Reserves, goldenPrices(), PoolV2); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("V1-scaled reserve valued as V2: err = %v, want ErrInvalidInput", err)
	}
	if _, _, err := AccountHealth(pos, v1Reserves, goldenPrices(), PoolVersionUnknown); !errors.Is(err, ErrUnknownPoolVersion) {
		t.Fatalf("unknown version: err = %v", err)
	}
	if _, err := Liquidatable(PoolVersionUnknown, big.NewInt(1), big.NewInt(2)); !errors.Is(err, ErrUnknownPoolVersion) {
		t.Fatalf("Liquidatable unknown version: err = %v", err)
	}
	if _, err := EffectiveCollateral(*v1Reserves[assetA].Data, *v1Reserves[assetA].Config, 7, big.NewInt(1)); !errors.Is(err, ErrUnknownPoolVersion) {
		t.Fatalf("EffectiveCollateral unknown version: err = %v", err)
	}
}

// One reserve, decimals 0, price 1, c_factor = l_factor = 1.0, b_rate exact, d_rate one part in
// 1e12 above 1: a single d-token owes ceil(1.000000000001) = 2 units, so liability_base is 2.
func boundaryHealth(t *testing.T, version PoolVersion, collateral int64) (c, l *big.Int) {
	t.Helper()
	one := "1_000_000_000_000"
	dRate := "1_000_000_000_001"
	if version == PoolV1 {
		one, dRate = "1_000_000_000", "1_000_000_001"
	}
	rd := ReserveData{BRate: decInt(one), DRate: decInt(dRate), Version: version}
	cfg := ReserveConfig{Decimals: 0, CFactor: 10_000_000, LFactor: 10_000_000}
	pos := Positions{
		Collateral:  map[string]*big.Int{assetA: big.NewInt(collateral)},
		Liabilities: map[string]*big.Int{assetA: big.NewInt(1)},
	}
	c, l, err := AccountHealth(pos, map[string]HealthReserve{assetA: {Data: &rd, Config: &cfg}},
		map[string]OraclePrice{assetA: {Price: big.NewInt(1), Decimals: 0}}, version)
	if err != nil {
		t.Fatal(err)
	}
	return c, l
}

func TestLiquidatable_BoundaryAtHFOne(t *testing.T) {
	cases := []struct {
		version    PoolVersion
		collateral int64
		want       bool
	}{
		// liability_base is 2 only because d-tokens round up; a floor would make it 1 and
		// leave collateral 1 healthy on V2.
		{PoolV2, 1, true},
		{PoolV2, 2, false}, // HF exactly 1: V2 requires liability > collateral
		{PoolV2, 3, false},
		{PoolV1, 1, true},
		{PoolV1, 2, true}, // HF exactly 1: V1 only refuses liability < collateral
		{PoolV1, 3, false},
	}
	for _, tc := range cases {
		c, l := boundaryHealth(t, tc.version, tc.collateral)
		if l.Cmp(big.NewInt(2)) != 0 || c.Cmp(big.NewInt(tc.collateral)) != 0 {
			t.Fatalf("v%d collateral %d: bases %s/%s, want %d/2", tc.version, tc.collateral, c, l, tc.collateral)
		}
		got, err := Liquidatable(tc.version, c, l)
		if err != nil || got != tc.want {
			t.Errorf("v%d collateral %d: Liquidatable = %v, %v; want %v", tc.version, tc.collateral, got, err, tc.want)
		}
	}
}

func TestAccountHealth_MissingInputsNamed(t *testing.T) {
	pos := Positions{
		Collateral:  map[string]*big.Int{assetA: big.NewInt(10)},
		Liabilities: map[string]*big.Int{assetB: big.NewInt(10)},
	}
	full := func() map[string]HealthReserve {
		return goldenReserves(PoolV2, "1_000_000_000_000", "1_000_000_000_000", "1_000_000_000_000", "1_000_000_000_000")
	}
	noRate := full()
	rd := *noRate[assetB].Data
	rd.DRate = nil
	noRate[assetB] = HealthReserve{Data: &rd, Config: noRate[assetB].Config}
	cases := []struct {
		name     string
		reserves map[string]HealthReserve
		prices   map[string]OraclePrice
		want     error
	}{
		{"reserve", func() map[string]HealthReserve { r := full(); delete(r, assetB); return r }(), goldenPrices(), ErrMissingReserve},
		{"config", func() map[string]HealthReserve {
			r := full()
			r[assetB] = HealthReserve{Data: r[assetB].Data}
			return r
		}(), goldenPrices(), ErrMissingConfig},
		{"rate", noRate, goldenPrices(), ErrMissingRate},
		{"price", full(), map[string]OraclePrice{assetA: goldenPrices()[assetA]}, ErrMissingPrice},
		{"price base", full(), map[string]OraclePrice{assetA: {Price: big.NewInt(1), Decimals: 7}, assetB: {Price: big.NewInt(1), Decimals: 14}}, ErrPriceBaseMismatch},
		{"zero price", full(), map[string]OraclePrice{assetA: goldenPrices()[assetA], assetB: {Price: big.NewInt(0), Decimals: 7}}, ErrInvalidInput},
	}
	for _, tc := range cases {
		_, _, err := AccountHealth(pos, tc.reserves, tc.prices, PoolV2)
		if !errors.Is(err, tc.want) {
			t.Errorf("%s: err = %v, want %v", tc.name, err, tc.want)
			continue
		}
		if tc.name != "price base" && !strings.Contains(err.Error(), assetB) {
			t.Errorf("%s: error %q does not name %s", tc.name, err, assetB)
		}
	}
	// A zero balance never loads its reserve, as in health_factor.rs.
	zero := Positions{Collateral: map[string]*big.Int{"CNOT_LISTED": new(big.Int)}}
	if c, l, err := AccountHealth(zero, nil, nil, PoolV2); err != nil || c.Sign() != 0 || l.Sign() != 0 {
		t.Fatalf("zero-balance entry: %v %v %v", c, l, err)
	}
	bad := Positions{Collateral: map[string]*big.Int{assetA: big.NewInt(-1)}}
	if _, _, err := AccountHealth(bad, full(), goldenPrices(), PoolV2); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("negative balance: err = %v", err)
	}
}

// The same real price 1.0 at oracle decimals 0 and 1 decides a 1-unit account differently: the
// price step rounds per reserve, so oracle decimals matter at the boundary.
func TestAccountHealth_OracleDecimalsMatterAtBoundary(t *testing.T) {
	rd := ReserveData{BRate: decInt("1_000_000_000_000"), DRate: decInt("1_000_000_000_000"), Version: PoolV2}
	cfg := ReserveConfig{Decimals: 1, CFactor: 10_000_000, LFactor: 10_000_000}
	pos := Positions{
		Collateral:  map[string]*big.Int{assetA: big.NewInt(1)},
		Liabilities: map[string]*big.Int{assetA: big.NewInt(1)},
	}
	reserves := map[string]HealthReserve{assetA: {Data: &rd, Config: &cfg}}
	// decimals 0: c = floor(1*1/10) = 0, l = ceil(1*1/10) = 1. decimals 1: c = l = floor/ceil(10*1/10) = 1.
	for _, tc := range []struct {
		price    int64
		decimals uint32
		c, l     int64
		liq      bool
	}{{1, 0, 0, 1, true}, {10, 1, 1, 1, false}} {
		c, l := mustHealth(t, pos, reserves, map[string]OraclePrice{assetA: {Price: big.NewInt(tc.price), Decimals: tc.decimals}})
		liq, _ := Liquidatable(PoolV2, c, l)
		if c.Int64() != tc.c || l.Int64() != tc.l || liq != tc.liq {
			t.Errorf("price %d at %d decimals: c=%s l=%s liquidatable=%v", tc.price, tc.decimals, c, l, liq)
		}
	}
}

func TestHFFloor4_NoDebtAndRounding(t *testing.T) {
	if hf, ok := HFFloor4(big.NewInt(5), new(big.Int)); ok || hf != "" {
		t.Fatalf("no debt: %q, %v", hf, ok)
	}
	// 19999/10000 = 1.9999 exactly; 29999/15000 = 1.99993… rounds down to 1.9999, not 2.0000.
	for _, tc := range []struct{ c, l int64 }{{19_999, 10_000}, {29_999, 15_000}} {
		if hf, ok := HFFloor4(big.NewInt(tc.c), big.NewInt(tc.l)); !ok || hf != "1.9999" {
			t.Errorf("HFFloor4(%d,%d) = %q", tc.c, tc.l, hf)
		}
	}
	if hf, _ := HFFloor4(big.NewInt(1), big.NewInt(3)); hf != "0.3333" {
		t.Errorf("1/3 = %q", hf)
	}
}

func TestInBand_ExactRational(t *testing.T) {
	// HF = 105/100 exactly is not below 1.05 and is below 1.1.
	c, l := big.NewInt(105), big.NewInt(100)
	if InBand(c, l, big.NewInt(105), big.NewInt(100)) {
		t.Error("HF 1.05 reported below 1.05")
	}
	if !InBand(c, l, big.NewInt(11), big.NewInt(10)) {
		t.Error("HF 1.05 not below 1.1")
	}
	if !InBand(big.NewInt(104_999_999), big.NewInt(100_000_000), big.NewInt(105), big.NewInt(100)) {
		t.Error("HF 1.04999999 not below 1.05")
	}
	if InBand(big.NewInt(0), new(big.Int), big.NewInt(1), big.NewInt(1)) {
		t.Error("no debt reported in a band")
	}
}

// shockHF is HF with X's legs scaled by mult, computed exactly.
func shockHF(cx, lx, co, lo *big.Int, mult *big.Rat) *big.Rat {
	num := new(big.Rat).Add(new(big.Rat).SetInt(co), new(big.Rat).Mul(new(big.Rat).SetInt(cx), mult))
	den := new(big.Rat).Add(new(big.Rat).SetInt(lo), new(big.Rat).Mul(new(big.Rat).SetInt(lx), mult))
	return num.Quo(num, den)
}

func TestShockThreshold_HFExactlyOne(t *testing.T) {
	one := big.NewRat(1, 1)
	// Long X: collateral 900 in X, debt 300 in X and 400 elsewhere, collateral 50 elsewhere.
	cx, lx, co, lo := big.NewInt(900), big.NewInt(300), big.NewInt(50), big.NewInt(400)
	s := ShockThreshold(cx, lx, co, lo, PriceFall)
	if s == nil || s.Cmp(big.NewRat(5, 12)) != 0 {
		t.Fatalf("fall s* = %v, want 5/12", s)
	}
	if hf := shockHF(cx, lx, co, lo, new(big.Rat).Sub(one, s)); hf.Cmp(one) != 0 {
		t.Fatalf("HF at fall s* = %v", hf)
	}
	if ShockThreshold(cx, lx, co, lo, PriceRise) != nil {
		t.Fatal("a rise cannot hurt a net-long account")
	}
	// Short X: debt 600 in X, collateral 100 in X and 1000 elsewhere.
	cx, lx, co, lo = big.NewInt(100), big.NewInt(600), big.NewInt(1000), big.NewInt(0)
	s = ShockThreshold(cx, lx, co, lo, PriceRise)
	if s == nil || s.Cmp(big.NewRat(1, 1)) != 0 {
		t.Fatalf("rise s* = %v, want 1", s)
	}
	if hf := shockHF(cx, lx, co, lo, new(big.Rat).Add(one, s)); hf.Cmp(one) != 0 {
		t.Fatalf("HF at rise s* = %v", hf)
	}
	if ShockThreshold(cx, lx, co, lo, PriceFall) != nil {
		t.Fatal("a fall cannot hurt a net-short account")
	}
	// Everything else alone covers the debt: no fall crosses HF = 1.
	if ShockThreshold(big.NewInt(10), big.NewInt(1), big.NewInt(100), big.NewInt(50), PriceFall) != nil {
		t.Fatal("over-covered account reported a fall threshold")
	}
}

func randomAccount(r *rand.Rand, n int) (Positions, map[string]HealthReserve, map[string]OraclePrice) {
	pos := Positions{Collateral: map[string]*big.Int{}, Liabilities: map[string]*big.Int{}, Supply: map[string]*big.Int{}}
	reserves := map[string]HealthReserve{}
	prices := map[string]OraclePrice{}
	for i := range n {
		asset := "C" + string(rune('A'+i))
		rd := ReserveData{
			BRate:   new(big.Int).Add(scalar12, big.NewInt(r.Int64N(1_000_000_000_000))),
			DRate:   new(big.Int).Add(scalar12, big.NewInt(r.Int64N(1_000_000_000_000))),
			Version: PoolV2,
		}
		cfg := ReserveConfig{Index: uint32(i), Decimals: uint32(r.IntN(19)), CFactor: uint32(r.IntN(10_000_001)), LFactor: uint32(1 + r.IntN(10_000_000))}
		reserves[asset] = HealthReserve{Data: &rd, Config: &cfg}
		prices[asset] = OraclePrice{Price: big.NewInt(1 + r.Int64N(1<<50)), Decimals: 14}
		switch r.IntN(4) {
		case 0:
			pos.Collateral[asset] = big.NewInt(r.Int64N(1 << 62))
		case 1:
			pos.Liabilities[asset] = big.NewInt(r.Int64N(1 << 62))
		case 2:
			pos.Collateral[asset] = big.NewInt(r.Int64N(1 << 62))
			pos.Liabilities[asset] = big.NewInt(r.Int64N(1 << 62))
		}
		if r.IntN(2) == 0 {
			pos.Supply[asset] = big.NewInt(r.Int64N(1 << 62))
		}
	}
	return pos, reserves, prices
}

func mustHealth(t *testing.T, pos Positions, reserves map[string]HealthReserve, prices map[string]OraclePrice) (c, l *big.Int) {
	t.Helper()
	c, l, err := AccountHealth(pos, reserves, prices, PoolV2)
	if err != nil {
		t.Fatal(err)
	}
	return c, l
}

func TestAccountHealth_Properties(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 2))
	for range 500 {
		pos, reserves, prices := randomAccount(r, 1+r.IntN(30))
		c, l := mustHealth(t, pos, reserves, prices)
		liq, _ := Liquidatable(PoolV2, c, l)

		// Scaling every price by 10^k leaves the decision unchanged outside the rounding slack:
		// each held reserve rounds by under one unit per side, so |c-l| > 2n cannot flip.
		k := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(1+r.IntN(6))), nil)
		scaled := map[string]OraclePrice{}
		for a, p := range prices {
			scaled[a] = OraclePrice{Price: new(big.Int).Mul(p.Price, k), Decimals: p.Decimals + 6}
		}
		c2, l2 := mustHealth(t, pos, reserves, scaled)
		slack := big.NewInt(int64(2 * len(heldAssets(pos))))
		if liq2, _ := Liquidatable(PoolV2, c2, l2); liq2 != liq && new(big.Int).Sub(c, l).CmpAbs(slack) > 0 {
			t.Fatalf("price scale by %s flipped Liquidatable %v -> %v", k, liq, liq2)
		}

		// Raising a collateral-only reserve's price never makes a healthy account liquidatable.
		for a := range pos.Collateral {
			if _, owed := pos.Liabilities[a]; owed {
				continue
			}
			up := map[string]OraclePrice{}
			for b, p := range prices {
				up[b] = p
			}
			up[a] = OraclePrice{Price: new(big.Int).Add(prices[a].Price, big.NewInt(1+r.Int64N(1<<40))), Decimals: 14}
			c3, l3 := mustHealth(t, pos, reserves, up)
			if liq3, _ := Liquidatable(PoolV2, c3, l3); !liq && liq3 {
				t.Fatalf("raising %s's price made a healthy account liquidatable", a)
			}
			break
		}

		// Non-collateral supply has no effect.
		noSupply := Positions{Collateral: pos.Collateral, Liabilities: pos.Liabilities}
		if c4, l4 := mustHealth(t, noSupply, reserves, prices); c4.Cmp(c) != 0 || l4.Cmp(l) != 0 {
			t.Fatal("non-collateral supply changed health")
		}

		// No liabilities: no debt, never a number.
		noDebt := Positions{Collateral: pos.Collateral}
		c5, l5 := mustHealth(t, noDebt, reserves, prices)
		if hf, ok := HFFloor4(c5, l5); ok || hf != "" || l5.Sign() != 0 {
			t.Fatalf("empty liabilities gave HF %q", hf)
		}
	}
}

func FuzzAccountHealth(f *testing.F) {
	f.Add(uint64(1), uint8(1))
	f.Add(uint64(42), uint8(30))
	f.Fuzz(func(t *testing.T, seed uint64, n uint8) {
		r := rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))
		pos, reserves, prices := randomAccount(r, int(n%31))
		c, l, err := AccountHealth(pos, reserves, prices, PoolV2)
		if err != nil {
			t.Fatal(err)
		}
		v2, err := Liquidatable(PoolV2, c, l)
		if err != nil || v2 != (c.Cmp(l) < 0) {
			t.Fatalf("V2 Liquidatable = %v, %v for c=%s l=%s", v2, err, c, l)
		}
		v1, err := Liquidatable(PoolV1, c, l)
		if err != nil || v1 != (l.Sign() > 0 && c.Cmp(l) <= 0) {
			t.Fatalf("V1 Liquidatable = %v, %v for c=%s l=%s", v1, err, c, l)
		}
	})
}
