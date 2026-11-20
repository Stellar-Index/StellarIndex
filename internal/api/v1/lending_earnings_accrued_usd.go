package v1

import "math/big"

// earningsAccruedUnavailable: the leg is valued but its accrued interest is unread (not reconciled), so it has no accrued_usd.
const earningsAccruedUnavailable = "accrued_unavailable"

// LendingAccruedUSD sums the legs' accrued_usd at the reserves' current prices: exact sums rounded to 2 dp.
type LendingAccruedUSD struct {
	AccruedSupplyUSD string `json:"accrued_supply_usd"` // interest earned on supply legs
	AccruedBorrowUSD string `json:"accrued_borrow_usd"` // interest paid on borrow legs
	// AccruedUSD is accrued_supply_usd - accrued_borrow_usd, null with AccruedUSDReason when any leg is left out: an omitted
	// borrow would make it too high, so it cannot be a lower bound.
	AccruedUSD       *string `json:"accrued_usd"`
	AccruedUSDReason *string `json:"accrued_usd_reason"`
	// AccruedPartial marks the two sums as partial sums over the valued legs, not floors: a leg's accrued can be negative
	// (rounding, a BLNT reconcile_loss). AccruedExcluded names each leg left out.
	AccruedPartial  bool                    `json:"accrued_partial"`
	AccruedExcluded []LendingStatsExclusion `json:"accrued_excluded"`
}

type accruedTally struct {
	supply, borrow big.Rat
	excluded       []LendingStatsExclusion
}

func (a *accruedTally) exclude(ex LendingStatsExclusion) { a.excluded = append(a.excluded, ex) }

// add counts a valued leg's accrued USD, or names it when that is nil.
func (a *accruedTally) add(leg *LendingEarningsLeg, accrued *big.Rat) {
	if accrued == nil {
		reason := earningsAccruedUnavailable
		if leg.UsdReason != nil {
			reason = *leg.UsdReason
		}
		pool := leg.Pool
		a.exclude(LendingStatsExclusion{Kind: leg.Side, Pool: &pool, Asset: leg.Asset, Reason: reason})
		return
	}
	sum := &a.supply
	if leg.Side == earningsSideBorrow {
		sum = &a.borrow
	}
	sum.Add(sum, accrued)
}

func (a *accruedTally) view() LendingAccruedUSD {
	out := LendingAccruedUSD{
		AccruedSupplyUSD: ratString(&a.supply, 2), AccruedBorrowUSD: ratString(&a.borrow, 2),
		AccruedPartial: len(a.excluded) > 0, AccruedExcluded: append([]LendingStatsExclusion{}, a.excluded...),
	}
	if out.AccruedPartial {
		out.AccruedUSDReason = strRef(netInputsPartial)
		return out
	}
	out.AccruedUSD = strRef(ratString(new(big.Rat).Sub(&a.supply, &a.borrow), 2))
	return out
}
