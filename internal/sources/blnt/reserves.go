package blnt

import (
	"math/big"

	"github.com/Stellar-Index/StellarIndex/internal/scval"
	"github.com/Stellar-Index/StellarIndex/internal/sources/blend"
)

// BLNT V3 pool storage uses the contract's "v2-compatible encoding" (ResData, ResConfig, instance
// Config) and a borrow curve byte-identical to Blend V2's, so this file delegates both to package blend.
// The supply rate is where BLNT differs (an immutable protocol fee on gross interest); a future
// divergence of the layout or curve is replaced here, in one file.

// Reserve-state types, shared with Blend V2 while the encodings match.
type (
	ReserveData    = blend.ReserveData
	ReserveConfig  = blend.ReserveConfig
	PoolConfig     = blend.PoolConfig
	ReserveMetrics = blend.ReserveMetrics
)

// ProtocolInterestFeeRate is the pool's PROTOCOL_INTEREST_FEE_RATE (0.2%, 7 decimals), taken from
// accrued borrower interest before the backstop's share.
const ProtocolInterestFeeRate = 20_000

var scalar7 = big.NewInt(10_000_000)

// DecodeReserveData decodes a ResData(asset) entry; BLNT rates are Blend V2-scaled (12-decimal b/d_rate).
func DecodeReserveData(v scval.ScVal) (ReserveData, error) {
	return blend.DecodeReserveData(v, blend.PoolV2)
}

// DecodeReserveConfig decodes a ResConfig(asset) entry.
func DecodeReserveConfig(v scval.ScVal) (ReserveConfig, error) { return blend.DecodeReserveConfig(v) }

// DecodePoolConfig decodes the instance-storage Config map.
func DecodePoolConfig(v scval.ScVal) (PoolConfig, error) { return blend.DecodePoolConfig(v) }

// ParseReserveConfigMetadata parses a queue_set_reserve event's attributes.metadata.
func ParseReserveConfigMetadata(b []byte) (ReserveConfig, error) {
	return blend.ParseReserveConfigMetadata(b)
}

// BaseMetrics derives supplied, borrowed and utilization from ReserveData alone.
func BaseMetrics(rd ReserveData) ReserveMetrics { return blend.BaseMetrics(rd) }

// SupplyRate returns the supplier rate in 7 decimals in the contract's accrual order: the protocol fee
// (floor) comes off gross interest first, then the backstop's floor share of the remainder.
func SupplyRate(borrowRate, util *big.Int, bstopRate uint32) *big.Int {
	gross := mulFloor(borrowRate, util)
	remaining := new(big.Int).Sub(gross, mulFloor(gross, big.NewInt(ProtocolInterestFeeRate)))
	if bstopRate > uint32(scalar7.Int64()) {
		bstopRate = uint32(scalar7.Int64())
	}
	return remaining.Sub(remaining, mulFloor(remaining, big.NewInt(int64(bstopRate))))
}

// Metrics derives the full per-reserve metrics: Blend V2's borrow APR with BLNT's fee-adjusted supply APR.
func Metrics(rd ReserveData, rc ReserveConfig, bstopRate uint32) ReserveMetrics {
	m := blend.Metrics(rd, rc, bstopRate)
	if rd.BSupply.Sign() == 0 {
		// With no b-tokens the contract credits all remaining interest to the backstop.
		m.SupplyAPR = 0
		return m
	}
	util := rd.Utilization()
	m.SupplyAPR = rate7ToFloat(SupplyRate(rc.BorrowRate(util, rd.IRMod), util, bstopRate))
	return m
}

func mulFloor(a, b *big.Int) *big.Int {
	n := new(big.Int).Mul(a, b)
	return n.Quo(n, scalar7)
}

func rate7ToFloat(rate *big.Int) float64 {
	f, _ := new(big.Rat).SetFrac(rate, scalar7).Float64() // i128:ok 7-decimal rate fraction, not an amount
	return f
}
