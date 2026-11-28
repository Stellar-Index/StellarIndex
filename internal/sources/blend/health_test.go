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
	now    = uint64(1_700_000_000)
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
	rdA := ReserveData{BRate: decInt(bRateA), DRate: decInt(dRateA), Version: version, LastTime: now - 600}
	rdB := ReserveData{BRate: decInt(bRateB), DRate: decInt(dRateB), Version: version, LastTime: now - 3600}
	return map[string]HealthReserve{assetA: {Data: &rdA, Config: &cfgA}, assetB: {Data: &rdB, Config: &cfgB}}
}

func goldenPrices() map[string]OraclePrice {
	return map[string]OraclePrice{
		assetA: {Price: big.NewInt(1_234_567), Decimals: 7, Timestamp: now},  // 0.1234567
		assetB: {Price: big.NewInt(10_000_100), Decimals: 7, Timestamp: now}, // 1.00001
	}
}

func mustHealth(t *testing.T, version PoolVersion, pos Positions, reserves map[string]HealthReserve, prices map[string]OraclePrice) Health {
	t.Helper()
	h, err := AccountHealth(pos, reserves, prices, version, now)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func mustClassify(t *testing.T, version PoolVersion, h Health) Status {
	t.Helper()
	s, err := Classify(version, h)
	if err != nil {
		t.Fatal(err)
	}
	return s
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
//	A raw         = floor(1_234_567 * 1_000_000_123 / 1e7)
//	              = floor(1_234_567_151_851_741 / 1e7)              = floor(123_456_715.185)    =   123_456_715
//	B underlying  = ceil(5_000_000 * 1_100_000_000_007 / 1e12)      = ceil(5_500_000.000035)    =     5_500_001
//	B effective   = ceil(5_500_001 * 1e7 / 9_000_000)               = ceil(6_111_112.222)       =     6_111_113
//	B base        = ceil(10_000_100 * 6_111_113 / 1e6)
//	              = ceil(61_111_741_111_300 / 1e6)                  = ceil(61_111_741.1113)     =    61_111_742
//	HF 4dp        = floor(92_592_536 * 1e4 / 61_111_742) = floor(15_151.349…) = 15_151 -> "1.5151"
//
// V1 (rates at 1e9): b_rate_A = 1_000_000_123, d_rate_B = 1_100_000_001.
//
//	A underlying  = floor(1_000_000_000 * 1_000_000_123 / 1e9) = 1_000_000_123 (exact), so A base and raw as above
//	B underlying  = ceil(5_000_000 * 1_100_000_001 / 1e9) = ceil(5_500_000.005) = 5_500_001, so B base = 61_111_742
//
// Reserves were last written 600 s and 3600 s before now, so 3600 s of accrual is pending.
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
			h := mustHealth(t, tc.version, pos, tc.reserves, goldenPrices())
			if h.CollateralBase.Cmp(big.NewInt(92_592_536)) != 0 || h.LiabilityBase.Cmp(big.NewInt(61_111_742)) != 0 ||
				h.CollateralRaw.Cmp(big.NewInt(123_456_715)) != 0 {
				t.Fatalf("collateral=%s liability=%s raw=%s, want 92592536, 61111742, 123456715",
					h.CollateralBase, h.LiabilityBase, h.CollateralRaw)
			}
			if h.AccrualPendingSeconds != 3600 {
				t.Fatalf("AccrualPendingSeconds = %d, want 3600", h.AccrualPendingSeconds)
			}
			if hf, ok := HFFloor4(h.CollateralBase, h.LiabilityBase); !ok || hf != "1.5151" {
				t.Fatalf("HFFloor4 = %q, %v; want 1.5151", hf, ok)
			}
			if s := mustClassify(t, tc.version, h); s != StatusHealthy {
				t.Fatalf("Classify = %v, want healthy", s)
			}
		})
	}
}

// One reserve of decimals 1 holding 3 b-tokens at b_rate 1.5, c_factor 0.7, price 13 (oracle
// decimals 0). Each collateral step leaves a fraction, so rounding any one of them up changes
// the base:
//
//	underlying = floor(3 * 1.5)   = floor(4.5) = 4   (ceil: 5 -> effective floor(3.5) = 3 -> base floor(3.9) = 3)
//	effective  = floor(4 * 0.7)   = floor(2.8) = 2   (ceil: 3 -> base floor(3.9) = 3)
//	base       = floor(13 * 2/10) = floor(2.6) = 2   (ceil: 3)
//	raw        = floor(13 * 4/10) = floor(5.2) = 5
//
// The rate is 1.5 at both scales (1_500_000_000_000 / 1e12 and 1_500_000_000 / 1e9), so V1 shows
// all three steps the same way.
func TestAccountHealth_EachCollateralStepRoundsDown(t *testing.T) {
	for _, tc := range []struct {
		version PoolVersion
		rate    string
	}{{PoolV2, "1_500_000_000_000"}, {PoolV1, "1_500_000_000"}} {
		rd := ReserveData{BRate: decInt(tc.rate), DRate: decInt(tc.rate), Version: tc.version, LastTime: now}
		cfg := ReserveConfig{Decimals: 1, CFactor: 7_000_000, LFactor: 10_000_000}
		h := mustHealth(t, tc.version, Positions{Collateral: map[string]*big.Int{assetA: big.NewInt(3)}},
			map[string]HealthReserve{assetA: {Data: &rd, Config: &cfg}},
			map[string]OraclePrice{assetA: {Price: big.NewInt(13), Timestamp: now}})
		if h.CollateralBase.Int64() != 2 || h.CollateralRaw.Int64() != 5 {
			t.Errorf("v%d: collateral=%s raw=%s, want 2 and 5", tc.version, h.CollateralBase, h.CollateralRaw)
		}
	}
}

func TestAccountHealth_VersionMismatchRefused(t *testing.T) {
	pos := Positions{Collateral: map[string]*big.Int{assetA: big.NewInt(1)}}
	v1Reserves := goldenReserves(PoolV1, "1_000_000_000", "1_000_000_000", "1_000_000_000", "1_000_000_000")
	if _, err := AccountHealth(pos, v1Reserves, goldenPrices(), PoolV2, now); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("V1-scaled reserve valued as V2: err = %v, want ErrInvalidInput", err)
	}
	if _, err := AccountHealth(pos, v1Reserves, goldenPrices(), PoolVersionUnknown, now); !errors.Is(err, ErrUnknownPoolVersion) {
		t.Fatalf("unknown version: err = %v", err)
	}
	if _, err := Classify(PoolVersionUnknown, Health{}); !errors.Is(err, ErrUnknownPoolVersion) {
		t.Fatalf("Classify unknown version: err = %v", err)
	}
	if _, err := ShockThreshold(PoolVersionUnknown, nil, nil, nil, nil, PriceFall); !errors.Is(err, ErrUnknownPoolVersion) {
		t.Fatalf("ShockThreshold unknown version: err = %v", err)
	}
	if _, err := EffectiveCollateral(*v1Reserves[assetA].Data, *v1Reserves[assetA].Config, 7, big.NewInt(1)); !errors.Is(err, ErrUnknownPoolVersion) {
		t.Fatalf("EffectiveCollateral unknown version: err = %v", err)
	}
}

// One reserve, decimals 0, price 1, c_factor = l_factor = 1.0, b_rate exact, d_rate one part in
// 1e12 above 1: a single d-token owes ceil(1.000000000001) = 2 units, so liability_base is 2.
func boundaryHealth(t *testing.T, version PoolVersion, collateral int64) Health {
	t.Helper()
	one, dRate := "1_000_000_000_000", "1_000_000_000_001"
	if version == PoolV1 {
		one, dRate = "1_000_000_000", "1_000_000_001"
	}
	rd := ReserveData{BRate: decInt(one), DRate: decInt(dRate), Version: version, LastTime: now}
	cfg := ReserveConfig{Decimals: 0, CFactor: 10_000_000, LFactor: 10_000_000}
	pos := Positions{
		Collateral:  map[string]*big.Int{assetA: big.NewInt(collateral)},
		Liabilities: map[string]*big.Int{assetA: big.NewInt(1)},
	}
	return mustHealth(t, version, pos, map[string]HealthReserve{assetA: {Data: &rd, Config: &cfg}},
		map[string]OraclePrice{assetA: {Price: big.NewInt(1), Timestamp: now}})
}

func TestClassify_BoundaryAtHFOne(t *testing.T) {
	cases := []struct {
		version    PoolVersion
		collateral int64
		want       Status
	}{
		// liability_base is 2 only because d-tokens round up; a floor would make it 1 and
		// leave collateral 1 healthy on V2.
		{PoolV2, 1, StatusLiquidatable},
		{PoolV2, 2, StatusHealthy}, // HF exactly 1: V2 requires liability > collateral
		{PoolV2, 3, StatusHealthy},
		{PoolV1, 1, StatusLiquidatable},
		{PoolV1, 2, StatusLiquidatable}, // HF exactly 1: V1 only refuses liability < collateral
		{PoolV1, 3, StatusHealthy},
	}
	for _, tc := range cases {
		h := boundaryHealth(t, tc.version, tc.collateral)
		if h.LiabilityBase.Cmp(big.NewInt(2)) != 0 || h.CollateralBase.Cmp(big.NewInt(tc.collateral)) != 0 {
			t.Fatalf("v%d collateral %d: bases %s/%s, want %d/2", tc.version, tc.collateral, h.CollateralBase, h.LiabilityBase, tc.collateral)
		}
		if got := mustClassify(t, tc.version, h); got != tc.want {
			t.Errorf("v%d collateral %d: Classify = %v, want %v", tc.version, tc.collateral, got, tc.want)
		}
	}
}

func TestClassify_BadDebtAndNoDebt(t *testing.T) {
	rd := func(v PoolVersion) *ReserveData {
		r := "1_000_000_000_000"
		if v == PoolV1 {
			r = "1_000_000_000"
		}
		return &ReserveData{BRate: decInt(r), DRate: decInt(r), Version: v, LastTime: now}
	}
	cfg := &ReserveConfig{Decimals: 7, CFactor: 10_000_000, LFactor: 10_000_000}
	for _, v := range []PoolVersion{PoolV1, PoolV2} {
		// Bases (0, 100) with no raw collateral: bad debt, not liquidatable.
		if s := mustClassify(t, v, Health{CollateralBase: new(big.Int), LiabilityBase: big.NewInt(100), CollateralRaw: new(big.Int)}); s != StatusBadDebt {
			t.Errorf("v%d (0,100): %v, want bad debt", v, s)
		}
		// A collateral entry whose raw value floors to 0: floor(1 * 1 / 1e7) = 0.
		reserves := map[string]HealthReserve{assetA: {Data: rd(v), Config: cfg}}
		prices := map[string]OraclePrice{assetA: {Price: big.NewInt(1), Timestamp: now}}
		h := mustHealth(t, v, Positions{
			Collateral:  map[string]*big.Int{assetA: big.NewInt(1)},
			Liabilities: map[string]*big.Int{assetA: big.NewInt(1)},
		}, reserves, prices)
		if h.CollateralRaw.Sign() != 0 || mustClassify(t, v, h) != StatusBadDebt {
			t.Errorf("v%d dust collateral: raw=%s status=%v, want 0 and bad debt", v, h.CollateralRaw, mustClassify(t, v, h))
		}
		// Collateral only: no debt, on both versions (V1's gate refuses it).
		h = mustHealth(t, v, Positions{Collateral: map[string]*big.Int{assetA: big.NewInt(10_000_000)}}, reserves, prices)
		if s := mustClassify(t, v, h); s != StatusNoDebt {
			t.Errorf("v%d no liabilities: %v, want no debt", v, s)
		}
	}
}

func TestAccountHealth_PriceStalenessBoundary(t *testing.T) {
	for _, v := range []PoolVersion{PoolV1, PoolV2} {
		rate := "1_000_000_000_000"
		if v == PoolV1 {
			rate = "1_000_000_000"
		}
		rd := ReserveData{BRate: decInt(rate), DRate: decInt(rate), Version: v, LastTime: now}
		reserves := map[string]HealthReserve{assetA: {Data: &rd, Config: &ReserveConfig{LFactor: 10_000_000, CFactor: 10_000_000}}}
		pos := Positions{Liabilities: map[string]*big.Int{assetA: big.NewInt(1)}}
		for _, tc := range []struct {
			age   uint64
			stale bool
		}{{86_400, false}, {86_401, true}} {
			prices := map[string]OraclePrice{assetA: {Price: big.NewInt(1), Timestamp: now - tc.age}}
			_, err := AccountHealth(pos, reserves, prices, v, now)
			if stale := errors.Is(err, ErrStalePrice); stale != tc.stale || (!tc.stale && err != nil) {
				t.Errorf("v%d age %d: err = %v, want stale=%v", v, tc.age, err, tc.stale)
			}
			if tc.stale && !strings.Contains(err.Error(), assetA) {
				t.Errorf("stale error %q does not name %s", err, assetA)
			}
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
	priceA := goldenPrices()[assetA]
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
		{"price", full(), map[string]OraclePrice{assetA: priceA}, ErrMissingPrice},
		{"price base", full(), map[string]OraclePrice{assetA: priceA, assetB: {Price: big.NewInt(1), Decimals: 14, Timestamp: now}}, ErrPriceBaseMismatch},
		{"zero price", full(), map[string]OraclePrice{assetA: priceA, assetB: {Price: big.NewInt(0), Decimals: 7, Timestamp: now}}, ErrInvalidInput},
	}
	for _, tc := range cases {
		_, err := AccountHealth(pos, tc.reserves, tc.prices, PoolV2, now)
		if !errors.Is(err, tc.want) {
			t.Errorf("%s: err = %v, want %v", tc.name, err, tc.want)
			continue
		}
		if !strings.Contains(err.Error(), assetB) {
			t.Errorf("%s: error %q does not name %s", tc.name, err, assetB)
		}
	}
	// A zero balance never loads its reserve, as in health_factor.rs.
	zero := Positions{Collateral: map[string]*big.Int{"CNOT_LISTED": new(big.Int)}}
	if h, err := AccountHealth(zero, nil, nil, PoolV2, now); err != nil || h.CollateralBase.Sign() != 0 || h.LiabilityBase.Sign() != 0 {
		t.Fatalf("zero-balance entry: %+v %v", h, err)
	}
	bad := Positions{Collateral: map[string]*big.Int{assetA: big.NewInt(-1)}}
	if _, err := AccountHealth(bad, full(), goldenPrices(), PoolV2, now); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("negative balance: err = %v", err)
	}
}

// The same real price 1.0 at oracle decimals 0 and 1 decides a 1-unit account differently: the
// price step rounds per reserve, so oracle decimals matter at the boundary.
func TestAccountHealth_OracleDecimalsMatterAtBoundary(t *testing.T) {
	rd := ReserveData{BRate: decInt("1_000_000_000_000"), DRate: decInt("1_000_000_000_000"), Version: PoolV2, LastTime: now}
	cfg := ReserveConfig{Decimals: 1, CFactor: 5_000_000, LFactor: 10_000_000}
	pos := Positions{
		Collateral:  map[string]*big.Int{assetA: big.NewInt(10)},
		Liabilities: map[string]*big.Int{assetA: big.NewInt(1)},
	}
	reserves := map[string]HealthReserve{assetA: {Data: &rd, Config: &cfg}}
	// Effective collateral 5, liability 1, raw collateral 10 (reserve decimals 1).
	// Oracle decimals 0, price 1:  c = floor(1*5/10) = 0,  raw = floor(1*10/10) = 1,  l = ceil(1*1/10) = 1.
	// Oracle decimals 1, price 10: c = floor(10*5/10) = 5, raw = 10,                  l = ceil(10*1/10) = 1.
	for _, tc := range []struct {
		price    int64
		decimals uint32
		c, l     int64
		want     Status
	}{{1, 0, 0, 1, StatusLiquidatable}, {10, 1, 5, 1, StatusHealthy}} {
		h := mustHealth(t, PoolV2, pos, reserves, map[string]OraclePrice{assetA: {Price: big.NewInt(tc.price), Decimals: tc.decimals, Timestamp: now}})
		if h.CollateralBase.Int64() != tc.c || h.LiabilityBase.Int64() != tc.l || mustClassify(t, PoolV2, h) != tc.want {
			t.Errorf("price %d at %d decimals: c=%s l=%s status=%v", tc.price, tc.decimals, h.CollateralBase, h.LiabilityBase, mustClassify(t, PoolV2, h))
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

func mustShock(t *testing.T, v PoolVersion, cx, lx, co, lo int64, move PriceMove) ShockResult {
	t.Helper()
	r, err := ShockThreshold(v, big.NewInt(cx), big.NewInt(lx), big.NewInt(co), big.NewInt(lo), move)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestShockThreshold(t *testing.T) {
	one := big.NewRat(1, 1)
	// Long X: collateral 900 in X, debt 300 in X and 400 elsewhere, collateral 50 elsewhere.
	r := mustShock(t, PoolV2, 900, 300, 50, 400, PriceFall)
	if r.Outcome != ShockCrosses || r.S.Cmp(big.NewRat(5, 12)) != 0 {
		t.Fatalf("fall = %+v, want crosses at 5/12", r)
	}
	if hf := shockHF(big.NewInt(900), big.NewInt(300), big.NewInt(50), big.NewInt(400), new(big.Rat).Sub(one, r.S)); hf.Cmp(one) != 0 {
		t.Fatalf("HF at fall s* = %v", hf)
	}
	if r := mustShock(t, PoolV2, 900, 300, 50, 400, PriceRise); r.Outcome != ShockNeverCrosses {
		t.Fatalf("a rise cannot hurt a net-long account: %+v", r)
	}
	// Short X: debt 600 in X, collateral 100 in X and 1000 elsewhere.
	r = mustShock(t, PoolV2, 100, 600, 1000, 0, PriceRise)
	if r.Outcome != ShockCrosses || r.S.Cmp(one) != 0 {
		t.Fatalf("rise = %+v, want crosses at 1", r)
	}
	if hf := shockHF(big.NewInt(100), big.NewInt(600), big.NewInt(1000), big.NewInt(0), new(big.Rat).Add(one, r.S)); hf.Cmp(one) != 0 {
		t.Fatalf("HF at rise s* = %v", hf)
	}
	// Everything else alone covers the debt: no fall crosses HF = 1.
	if r := mustShock(t, PoolV2, 10, 1, 100, 50, PriceFall); r.Outcome != ShockNeverCrosses {
		t.Fatalf("over-covered account: %+v", r)
	}
	// Already below 1 today: reported as such, not as "never crosses".
	if r := mustShock(t, PoolV2, 100, 100, 0, 10, PriceFall); r.Outcome != ShockAlreadyLiquidatable {
		t.Fatalf("cx=lx=100 co=0 lo=10: %+v", r)
	}
	if r := mustShock(t, PoolV2, 0, 100, 0, 10, PriceRise); r.Outcome != ShockAlreadyLiquidatable {
		t.Fatalf("cx=0 lx=100 co=0 lo=10 rising: %+v", r)
	}
	// HF exactly 1: V2 is healthy and any harmful move crosses at s = 0; V1 already liquidates.
	if r := mustShock(t, PoolV2, 100, 0, 0, 100, PriceFall); r.Outcome != ShockCrosses || r.S.Sign() != 0 {
		t.Fatalf("V2 at HF 1: %+v", r)
	}
	if r := mustShock(t, PoolV1, 100, 0, 0, 100, PriceFall); r.Outcome != ShockAlreadyLiquidatable {
		t.Fatalf("V1 at HF 1: %+v", r)
	}
}

func rateScale(v PoolVersion) *big.Int {
	if v == PoolV1 {
		return scalar9
	}
	return scalar12
}

func randomAccount(r *rand.Rand, n int, v PoolVersion) (Positions, map[string]HealthReserve, map[string]OraclePrice) {
	pos := Positions{Collateral: map[string]*big.Int{}, Liabilities: map[string]*big.Int{}, Supply: map[string]*big.Int{}}
	reserves := map[string]HealthReserve{}
	prices := map[string]OraclePrice{}
	scale := rateScale(v)
	for i := range n {
		asset := "C" + string(rune('A'+i))
		rd := ReserveData{
			BRate:    new(big.Int).Add(scale, big.NewInt(r.Int64N(scale.Int64()))),
			DRate:    new(big.Int).Add(scale, big.NewInt(r.Int64N(scale.Int64()))),
			Version:  v,
			LastTime: now - uint64(r.IntN(1_000_000)),
		}
		cfg := ReserveConfig{Index: uint32(i), Decimals: uint32(r.IntN(19)), CFactor: uint32(r.IntN(10_000_001)), LFactor: uint32(1 + r.IntN(10_000_000))}
		reserves[asset] = HealthReserve{Data: &rd, Config: &cfg}
		prices[asset] = OraclePrice{Price: big.NewInt(1 + r.Int64N(1<<50)), Decimals: 14, Timestamp: now - uint64(r.IntN(86_401))}
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

func TestAccountHealth_Properties(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 2))
	for range 500 {
		pos, reserves, prices := randomAccount(r, 1+r.IntN(30), PoolV2)
		h := mustHealth(t, PoolV2, pos, reserves, prices)
		status := mustClassify(t, PoolV2, h)

		// Scaling every price by 10^k leaves the decision unchanged outside the rounding slack:
		// each held reserve rounds by under one unit per side, so |c-l| > 2n cannot flip.
		k := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(1+r.IntN(6))), nil)
		scaled := map[string]OraclePrice{}
		for a, p := range prices {
			scaled[a] = OraclePrice{Price: new(big.Int).Mul(p.Price, k), Decimals: p.Decimals + 6, Timestamp: p.Timestamp}
		}
		h2 := mustHealth(t, PoolV2, pos, reserves, scaled)
		slack := big.NewInt(int64(2 * len(heldAssets(pos))))
		far := new(big.Int).Sub(h.CollateralBase, h.LiabilityBase).CmpAbs(slack) > 0 && h.CollateralRaw.Cmp(slack) > 0
		if s2 := mustClassify(t, PoolV2, h2); s2 != status && far {
			t.Fatalf("price scale by %s changed status %v -> %v", k, status, s2)
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
			up[a] = OraclePrice{Price: new(big.Int).Add(prices[a].Price, big.NewInt(1+r.Int64N(1<<40))), Decimals: 14, Timestamp: now}
			if s3 := mustClassify(t, PoolV2, mustHealth(t, PoolV2, pos, reserves, up)); status == StatusHealthy && s3 != StatusHealthy {
				t.Fatalf("raising %s's price moved a healthy account to %v", a, s3)
			}
			break
		}

		// Non-collateral supply has no effect.
		h4 := mustHealth(t, PoolV2, Positions{Collateral: pos.Collateral, Liabilities: pos.Liabilities}, reserves, prices)
		if h4.CollateralBase.Cmp(h.CollateralBase) != 0 || h4.LiabilityBase.Cmp(h.LiabilityBase) != 0 || h4.CollateralRaw.Cmp(h.CollateralRaw) != 0 {
			t.Fatal("non-collateral supply changed health")
		}

		// No liabilities: no debt, never a number.
		h5 := mustHealth(t, PoolV2, Positions{Collateral: pos.Collateral}, reserves, prices)
		if hf, ok := HFFloor4(h5.CollateralBase, h5.LiabilityBase); ok || hf != "" || mustClassify(t, PoolV2, h5) != StatusNoDebt {
			t.Fatalf("empty liabilities gave HF %q", hf)
		}
	}
}

func ratFloor(x *big.Rat) *big.Int { return new(big.Int).Div(x.Num(), x.Denom()) } // Euclidean: floor for Denom > 0

func ratCeil(x *big.Rat) *big.Int {
	f := ratFloor(new(big.Rat).Neg(x))
	return f.Neg(f)
}

// referenceHealth recomputes AccountHealth with exact rationals, flooring or ceiling each
// documented step, without the package's fixed-point helpers.
func referenceHealth(v PoolVersion, pos Positions, reserves map[string]HealthReserve, prices map[string]OraclePrice) (c, l, raw *big.Int) {
	c, l, raw = new(big.Int), new(big.Int), new(big.Int)
	rate := new(big.Rat).SetInt(rateScale(v))
	frac := func(n, d *big.Int) *big.Rat { return new(big.Rat).SetFrac(n, d) }
	mul := func(a *big.Int, b *big.Rat) *big.Rat { return new(big.Rat).Mul(new(big.Rat).SetInt(a), b) }
	for asset, b := range pos.Collateral {
		if b.Sign() == 0 {
			continue
		}
		res, p := reserves[asset], prices[asset].Price
		scalar := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(res.Config.Decimals)), nil)
		u := ratFloor(mul(b, new(big.Rat).Quo(new(big.Rat).SetInt(res.Data.BRate), rate)))
		eff := ratFloor(mul(u, big.NewRat(int64(res.Config.CFactor), 10_000_000)))
		c.Add(c, ratFloor(mul(p, frac(eff, scalar))))
		raw.Add(raw, ratFloor(mul(p, frac(u, scalar))))
	}
	for asset, d := range pos.Liabilities {
		if d.Sign() == 0 {
			continue
		}
		res, p := reserves[asset], prices[asset].Price
		scalar := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(res.Config.Decimals)), nil)
		u := ratCeil(mul(d, new(big.Rat).Quo(new(big.Rat).SetInt(res.Data.DRate), rate)))
		eff := ratCeil(mul(u, big.NewRat(10_000_000, int64(res.Config.LFactor))))
		l.Add(l, ratCeil(mul(p, frac(eff, scalar))))
	}
	return c, l, raw
}

func FuzzAccountHealth(f *testing.F) {
	f.Add(uint64(1), uint8(1), false)
	f.Add(uint64(42), uint8(30), true)
	f.Fuzz(func(t *testing.T, seed uint64, n uint8, v1 bool) {
		v := PoolV2
		if v1 {
			v = PoolV1
		}
		r := rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))
		pos, reserves, prices := randomAccount(r, int(n%31), v)
		h, err := AccountHealth(pos, reserves, prices, v, now)
		if err != nil {
			t.Fatal(err)
		}
		c, l, raw := referenceHealth(v, pos, reserves, prices)
		if h.CollateralBase.Cmp(c) != 0 || h.LiabilityBase.Cmp(l) != 0 || h.CollateralRaw.Cmp(raw) != 0 {
			t.Fatalf("v%d: got %s/%s/%s, reference %s/%s/%s", v, h.CollateralBase, h.LiabilityBase, h.CollateralRaw, c, l, raw)
		}
		want := StatusHealthy
		switch {
		case l.Sign() == 0:
			want = StatusNoDebt
		case raw.Sign() == 0:
			want = StatusBadDebt
		case v == PoolV2 && c.Cmp(l) < 0, v == PoolV1 && c.Cmp(l) <= 0:
			want = StatusLiquidatable
		}
		if got := mustClassify(t, v, h); got != want {
			t.Fatalf("v%d: Classify = %v, want %v for c=%s l=%s raw=%s", v, got, want, c, l, raw)
		}
	})
}
