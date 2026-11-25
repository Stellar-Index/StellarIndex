package blnt

import (
	"errors"
	"fmt"
	"math/big"

	"github.com/Stellar-Index/StellarIndex/internal/scval"
	"github.com/Stellar-Index/StellarIndex/internal/sources/blend"
)

// ProtocolFee is one reserve asset's entry in the pool's PoolDataKey::ProtocolFees map.
type ProtocolFee struct {
	Credit *big.Int // fee owed to the protocol, in the asset's base units
	// Carry is the fee's sub-unit remainder in 1e-7 base units, in [0, 1e7). The pool adds
	// gross·PROTOCOL_INTEREST_FEE_RATE to it on every accrual and moves whole units into Credit.
	Carry *big.Int
}

// ProtocolFees is the pool-wide ProtocolFees entry, keyed by reserve asset contract strkey.
type ProtocolFees map[string]ProtocolFee

// DecodeProtocolFees decodes the ProtocolFees entry: Map<Address, {carry: i128, credit: i128}>.
// Any other field, a duplicate asset or an out-of-range value is refused, not skipped.
func DecodeProtocolFees(v scval.ScVal) (ProtocolFees, error) {
	m, err := scval.AsMap(v)
	if err != nil {
		return nil, fmt.Errorf("blnt: protocol fees not a map: %w", err)
	}
	out := make(ProtocolFees, len(m))
	for _, e := range m {
		asset, err := scval.AsAddressStrkey(e.Key)
		if err != nil {
			return nil, fmt.Errorf("blnt: protocol fees key: %w", err)
		}
		if _, dup := out[asset]; dup {
			return nil, fmt.Errorf("blnt: protocol fees repeat asset %s", asset)
		}
		f, err := decodeProtocolFee(e.Val)
		if err != nil {
			return nil, fmt.Errorf("blnt: protocol fees for %s: %w", asset, err)
		}
		out[asset] = f
	}
	return out, nil
}

func decodeProtocolFee(v scval.ScVal) (ProtocolFee, error) {
	m, err := scval.AsMap(v)
	if err != nil {
		return ProtocolFee{}, err
	}
	if len(m) != 2 {
		return ProtocolFee{}, fmt.Errorf("%d fields, want carry and credit", len(m))
	}
	var f ProtocolFee
	for _, field := range [...]struct {
		name string
		dst  **big.Int
	}{{"carry", &f.Carry}, {"credit", &f.Credit}} {
		sv, err := scval.MustMapField(m, field.name)
		if err != nil {
			return ProtocolFee{}, err
		}
		amt, err := scval.AsAmountFromI128(sv)
		if err != nil {
			return ProtocolFee{}, fmt.Errorf("field %q: %w", field.name, err)
		}
		*field.dst = amt.BigInt()
	}
	return f, f.check()
}

func (f ProtocolFee) check() error {
	if f.Credit == nil || f.Credit.Sign() < 0 {
		return errors.New("missing or negative credit")
	}
	if f.Carry == nil || f.Carry.Sign() < 0 || f.Carry.Cmp(scalar7) >= 0 {
		return fmt.Errorf("carry %v outside [0, 1e7)", f.Carry)
	}
	return nil
}

// Accrual is blend's accrual with BLNT's protocol fee, and the fee entry it implies.
type Accrual struct {
	blend.Accrual
	ExpectedFees ProtocolFee // prevFee advanced by this accrual
	// FeeCreditResidual is nextFee.Credit − ExpectedFees.Credit. Non-zero means a fee withdrawal,
	// or a prevFee that was not the entry in force before next's transaction.
	FeeCreditResidual *big.Int
	FeesMatch         bool // nextFee equals ExpectedFees in both credit and carry
}

// AccrueBetween recomputes the accrual between stored versions prev → next of one BLNT reserve, under
// the same adjacency and bstop_rate obligations as blend.AccrueBetween. prevFee and nextFee are the
// reserve asset's ProtocolFees entry before and after next's transaction (pass prevFee twice when
// the transaction did not write ProtocolFees). The fee is floor((carry + gross·rate)/1e7) with the
// remainder carried, and the backstop's share is taken from gross less that fee.
func AccrueBetween(prev, next ReserveData, prevFee, nextFee ProtocolFee, bstopRate uint32) (Accrual, error) {
	for _, f := range [2]ProtocolFee{prevFee, nextFee} {
		if err := f.check(); err != nil {
			return Accrual{}, fmt.Errorf("blnt: accrual over protocol fee entry: %w", err)
		}
	}
	carry := new(big.Int).Set(prevFee.Carry)
	a, err := blend.AccrueBetweenWithFee(prev, next, bstopRate, func(gross *big.Int) *big.Int {
		acc := new(big.Int).Mul(gross, big.NewInt(ProtocolInterestFeeRate))
		acc.Add(acc, carry)
		fee := new(big.Int)
		fee.QuoRem(acc, scalar7, carry) // non-negative operands: truncation is floor
		return fee
	})
	if err != nil {
		return Accrual{}, err
	}
	expected := ProtocolFee{Credit: new(big.Int).Add(prevFee.Credit, a.ProtocolFee), Carry: carry}
	return Accrual{
		Accrual:           a,
		ExpectedFees:      expected,
		FeeCreditResidual: new(big.Int).Sub(nextFee.Credit, expected.Credit),
		FeesMatch:         nextFee.Credit.Cmp(expected.Credit) == 0 && nextFee.Carry.Cmp(expected.Carry) == 0,
	}, nil
}
