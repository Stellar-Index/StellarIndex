package blend

import (
	"errors"
	"math/big"
)

// secondsPerYear is the contract's SECONDS_PER_YEAR in interest.rs::calc_accrual.
var secondsPerYear = big.NewInt(31_536_000)

// ErrAccrualUnbounded means no ceiling on a reserve's accrual since last_time is known: a V1 pool (its 9-decimal ir_mod
// has no verified port) or a reserve whose rate inputs are missing.
var ErrAccrualUnbounded = errors.New("blend: accrual since last_time is not bounded")

// AccrualCeiling returns rd with b_rate and d_rate raised to at least what Reserve::load would set at now: interest
// over now − last_time at the stored borrow rate (BorrowRate at the stored utilization and ir_mod), plus each step's
// rounding. Supply earns only what borrowers pay, so b_rate rises by at most the accrued interest over b_supply.
// Health is monotone in both rates, so AccountHealth at rd and at the ceiling brackets the contract's own answer.
func AccrualCeiling(rd ReserveData, cfg ReserveConfig, now uint64) (ReserveData, error) {
	if rd.LastTime >= now {
		return rd, nil
	}
	if rd.Version != PoolV2 {
		return ReserveData{}, ErrAccrualUnbounded
	}
	for _, f := range [...]*big.Int{rd.DRate, rd.BRate, rd.BSupply, rd.DSupply, rd.IRMod} {
		if f == nil || f.Sign() < 0 {
			return ReserveData{}, ErrAccrualUnbounded
		}
	}
	s := rd.rateScalar()
	den := new(big.Int).Mul(scalar7, secondsPerYear)
	grow := new(big.Int).Mul(cfg.BorrowRate(rd.Utilization(), rd.IRMod), new(big.Int).SetUint64(now-rd.LastTime))
	out := rd
	// calc_accrual's factor and load's fixed_mul_ceil each round up by at most one unit at the rate scale.
	dRate := fixedMulCeil(rd.DRate, new(big.Int).Add(den, grow), den)
	out.DRate = dRate.Add(dRate, new(big.Int).Add(ceilDiv(rd.DRate, s), big.NewInt(1)))
	if rd.BSupply.Sign() > 0 {
		gross := fixedMulCeil(rd.BorrowedUnderlying(), grow, den)
		gross.Add(gross, ceilDiv(rd.DSupply, s))
		gross.Add(gross, big.NewInt(2))
		bRate := fixedDivCeil(new(big.Int).Add(rd.SuppliedUnderlying(), gross), rd.BSupply, s)
		bRate.Add(bRate, big.NewInt(1))
		if bRate.Cmp(rd.BRate) < 0 {
			bRate.Set(rd.BRate)
		}
		out.BRate = bRate
	}
	return out, nil
}
