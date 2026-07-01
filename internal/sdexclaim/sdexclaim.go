// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

// Package sdexclaim holds the shared helpers for interpreting SDEX
// ClaimAtoms (the per-fill records inside a ManageOffer/PathPayment result).
// Both the dispatcher's census walk and the ClickHouse structural extractor
// count "real" trades from the same xdr.ClaimAtom slices; this is their single
// canonical copy (they can't share via internal/sources/sdex — that would
// cycle back through internal/dispatcher).
package sdexclaim

import "github.com/stellar/go-stellar-sdk/xdr"

// Amounts returns the (sold, bought) amounts for one ClaimAtom across all
// three atom variants (OrderBook / LiquidityPool / V0). Returns (0, 0) for an
// unknown variant.
func Amounts(a xdr.ClaimAtom) (sold, bought xdr.Int64) {
	switch a.Type {
	case xdr.ClaimAtomTypeClaimAtomTypeOrderBook:
		ob := a.MustOrderBook()
		return ob.AmountSold, ob.AmountBought
	case xdr.ClaimAtomTypeClaimAtomTypeLiquidityPool:
		lp := a.MustLiquidityPool()
		return lp.AmountSold, lp.AmountBought
	case xdr.ClaimAtomTypeClaimAtomTypeV0:
		v0 := a.MustV0()
		return v0.AmountSold, v0.AmountBought
	}
	return 0, 0
}

// RealTradeCount counts the claims that moved a non-zero amount on at least
// one side — the "real" trades, excluding dust/self-fills that clear at zero.
func RealTradeCount(claims []xdr.ClaimAtom) int {
	n := 0
	for i := range claims {
		if s, b := Amounts(claims[i]); s > 0 || b > 0 {
			n++
		}
	}
	return n
}
