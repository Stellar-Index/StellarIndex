package blend

import (
	"errors"
	"fmt"
	"math/big"
)

// Accrual is the interest the pool contract accrued between two consecutive stored versions of one
// reserve, recomputed with reserve.rs::load's own integer arithmetic (blend-contracts v1.0.0 and
// blend-contracts-v2 v2.0.0 agree on it). Amounts are in the reserve asset's base units. The checks
// report what accrual alone cannot explain; they are never coerced to agree.
type Accrual struct {
	Gross *big.Int // borrower interest: total_liabilities at the new d_rate minus at the old
	// ProtocolFee is a fork's fee taken off Gross before the backstop's share; zero for Blend.
	ProtocolFee      *big.Int
	Backstop         *big.Int // backstop's share, floor((Gross − ProtocolFee)·bstop_rate)
	SupplierIntended *big.Int // Gross − ProtocolFee − Backstop
	// SupplierRealised is total_supply at ExpectedBRate minus at the old b_rate. b_rate's floor
	// keeps up to b_supply/scalar+1 units of SupplierIntended in the pool, and can make this negative.
	SupplierRealised *big.Int
	ExpectedBRate    *big.Int
	// BRateMatches is false when something besides accrual moved b_rate in the version (a V2
	// default); the caller must exclude or explain it.
	BRateMatches        bool
	BackstopCreditDelta *big.Int // next.backstop_credit − prev.backstop_credit, signed
	// BackstopResidual is BackstopCreditDelta − Backstop. Non-zero means a gulp, an interest-auction
	// draw, or a bstop_rate other than the one passed was in force for that version.
	BackstopResidual *big.Int
}

// AccrueBetween recomputes the accrual Reserve::load applied between stored versions prev → next of
// one reserve. The caller must pass ADJACENT versions: a skipped one (e.g. a borrow) silently
// computes gross on the wrong d_supply. bstopRate (7 decimals) is the pool's stored bstop_rate
// before next's transaction ran. V2's update_pool accrues every reserve at the old rate before
// storing a new one; V1's does not, so after a V1 change the new rate covers the whole interval up
// to each reserve's next load.
func AccrueBetween(prev, next ReserveData, bstopRate uint32) (Accrual, error) {
	return AccrueBetweenWithFee(prev, next, bstopRate, nil)
}

// AccrueBetweenWithFee is AccrueBetween for a fork that takes fee(gross) off positive gross interest
// before the backstop's share (BLNT). fee is not called when gross is not positive, and must return
// a value in [0, gross]; a nil fee is Blend's zero fee.
func AccrueBetweenWithFee(prev, next ReserveData, bstopRate uint32, fee func(gross *big.Int) *big.Int) (Accrual, error) {
	if err := checkAccrualInputs(prev, next, bstopRate); err != nil {
		return Accrual{}, err
	}
	s := prev.rateScalar()
	// load() reads the STORED prev, so every operand except the new d_rate is prev's.
	gross := new(big.Int).Sub(fixedMulCeil(prev.DSupply, next.DRate, s), prev.BorrowedUnderlying())
	protocolFee := new(big.Int)
	if gross.Sign() > 0 && fee != nil {
		protocolFee = fee(new(big.Int).Set(gross))
		if protocolFee == nil || protocolFee.Sign() < 0 || protocolFee.Cmp(gross) > 0 {
			return Accrual{}, fmt.Errorf("blend: protocol fee %v outside [0, gross %s]", protocolFee, gross)
		}
	}
	net := new(big.Int).Sub(gross, protocolFee)
	backstop := new(big.Int)
	if gross.Sign() > 0 && bstopRate > 0 {
		backstop = fixedMulFloor(net, big.NewInt(int64(bstopRate)), scalar7)
	}
	intended := new(big.Int).Sub(net, backstop)
	preSupply := prev.SuppliedUnderlying()
	expected := new(big.Int).Set(prev.BRate)
	if gross.Sign() > 0 {
		expected = fixedDivFloor(new(big.Int).Add(preSupply, intended), prev.BSupply, s)
	}
	realised := new(big.Int).Sub(fixedMulFloor(prev.BSupply, expected, s), preSupply)
	delta := new(big.Int).Sub(next.BackstopCredit, prev.BackstopCredit)
	return Accrual{
		Gross:               gross,
		ProtocolFee:         protocolFee,
		Backstop:            backstop,
		SupplierIntended:    intended,
		SupplierRealised:    realised,
		ExpectedBRate:       expected,
		BRateMatches:        next.BRate.Cmp(expected) == 0,
		BackstopCreditDelta: delta,
		BackstopResidual:    new(big.Int).Sub(delta, backstop),
	}, nil
}

// checkAccrualInputs refuses version pairs Reserve::load cannot have produced. It cannot detect a
// skipped intermediate version; adjacency is the caller's obligation.
func checkAccrualInputs(prev, next ReserveData, bstopRate uint32) error {
	if prev.Version != next.Version || (prev.Version != PoolV1 && prev.Version != PoolV2) {
		return fmt.Errorf("blend: accrual between pool versions %d and %d", prev.Version, next.Version)
	}
	if new(big.Int).SetUint64(uint64(bstopRate)).Cmp(scalar7) >= 0 {
		// config.rs refuses bstop_rate >= SCALAR_7.
		return fmt.Errorf("blend: accrual with bstop_rate %d >= 100%%", bstopRate)
	}
	for _, rd := range [2]ReserveData{prev, next} {
		for _, f := range [...]*big.Int{rd.DRate, rd.BRate, rd.BSupply, rd.DSupply, rd.BackstopCredit} {
			if f == nil || f.Sign() < 0 {
				return errors.New("blend: accrual over a missing or negative reserve field")
			}
		}
	}
	if next.LastTime < prev.LastTime {
		return fmt.Errorf("blend: accrual with last_time going backwards (%d → %d)", prev.LastTime, next.LastTime)
	}
	switch c := next.DRate.Cmp(prev.DRate); {
	case c < 0:
		return fmt.Errorf("blend: accrual with d_rate decreasing (%s → %s)", prev.DRate, next.DRate)
	case c == 0:
		return nil
	}
	// load() is the only writer of d_rate, and it short-circuits on an unchanged timestamp, zero
	// b_supply and zero utilization.
	if next.LastTime == prev.LastTime || prev.BSupply.Sign() == 0 || prev.DSupply.Sign() == 0 {
		return fmt.Errorf("blend: d_rate moved where the contract cannot accrue (last_time %d → %d, b_supply %s, d_supply %s)",
			prev.LastTime, next.LastTime, prev.BSupply, prev.DSupply)
	}
	return nil
}
