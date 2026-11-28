package blend

import (
	"errors"
	"math/big"
	"math/rand/v2"
	"testing"
)

func accrualCfg() ReserveConfig {
	return ReserveConfig{Decimals: 7, CFactor: 9_000_000, LFactor: 9_000_000, Util: 8_000_000, RBase: 100_000, ROne: 400_000, RTwo: 2_000_000, RThree: 50_000_000}
}

// refLoadDRate transcribes the d_rate Reserve::load stores (pool/reserve.rs load, pool/interest.rs calc_accrual), in the
// contract's own rounding: utilization fixed_div_ceil, time_weight floored, the accrual factor and d_rate fixed_mul_ceil.
func refLoadDRate(rd ReserveData, cfg ReserveConfig, now uint64) *big.Int {
	s7, s12 := bi(10_000_000), bi(1_000_000_000_000)
	if now == rd.LastTime || rd.BSupply.Sign() == 0 {
		return rd.DRate
	}
	liabilities, supply := refCeil(mul(rd.DSupply, rd.DRate), s12), refFloor(mul(rd.BSupply, rd.BRate), s12)
	var util *big.Int
	switch {
	case liabilities.Sign() == 0:
		return rd.DRate
	case liabilities.Cmp(supply) >= 0:
		util = s7
	default:
		util = refCeil(mul(liabilities, s7), supply)
	}
	timeWeight := refFloor(mul(new(big.Int).SetUint64(now-rd.LastTime), s12), bi(31_536_000))
	accrual := new(big.Int).Add(s12, refCeil(mul(timeWeight, refBorrowRate(cfg, util, rd.IRMod)), s7))
	return refCeil(mul(accrual, rd.DRate), s12)
}

// TestAccrualCeiling_DominatesLoad: for any reserve and interval, the ceiling is at least the d_rate the contract's load
// stores and at least the b_rate it then stores with the whole interest going to suppliers (bstop_rate 0, the most supply
// can earn).
func TestAccrualCeiling_DominatesLoad(t *testing.T) {
	rng := rand.New(rand.NewPCG(7, 11))
	cfg := accrualCfg()
	for i := range 2000 {
		dRate := bi(1_000_000_000_000 + rng.Int64N(400_000_000_000))
		bRate := bi(1_000_000_000_000 + rng.Int64N(200_000_000_000))
		bSupply := bi(1 + rng.Int64N(1_000_000_000_000))
		dSupply := bi(rng.Int64N(bSupply.Int64() + 1))
		prev := ReserveData{
			DRate: dRate, BRate: bRate, BSupply: bSupply, DSupply: dSupply, IRMod: bi(1_000_000 + rng.Int64N(99_000_000)),
			BackstopCredit: bi(0), LastTime: 1_700_000_000, Version: PoolV2,
		}
		now := prev.LastTime + uint64(rng.Int64N(400*86_400))
		up, err := AccrualCeiling(prev, cfg, now)
		if err != nil {
			t.Fatal(err)
		}
		newD := refLoadDRate(prev, cfg, now)
		if up.DRate.Cmp(newD) < 0 {
			t.Fatalf("case %d: d_rate ceiling %s below accrued %s", i, up.DRate, newD)
		}
		if loaded := refLoad(prev, newD, 0, now); up.BRate.Cmp(loaded.BRate) < 0 {
			t.Fatalf("case %d: b_rate ceiling %s below loaded %s", i, up.BRate, loaded.BRate)
		}
		if prev.DRate.Cmp(dRate) != 0 || prev.BRate.Cmp(bRate) != 0 {
			t.Fatal("the stored rates were mutated")
		}
	}
}

func TestAccrualCeiling_NothingPendingOrUnbounded(t *testing.T) {
	rd := ReserveData{DRate: bi(1), BRate: bi(1), BSupply: bi(1), DSupply: bi(1), IRMod: bi(1), LastTime: 50, Version: PoolV2}
	if up, err := AccrualCeiling(rd, accrualCfg(), 50); err != nil || up.DRate != rd.DRate || up.BRate != rd.BRate {
		t.Fatalf("no interval: %+v %v", up, err)
	}
	rd.Version = PoolV1
	if _, err := AccrualCeiling(rd, accrualCfg(), 51); !errors.Is(err, ErrAccrualUnbounded) {
		t.Fatalf("v1: %v", err)
	}
	rd.Version, rd.IRMod = PoolV2, nil
	if _, err := AccrualCeiling(rd, accrualCfg(), 51); !errors.Is(err, ErrAccrualUnbounded) {
		t.Fatalf("no ir_mod: %v", err)
	}
}
