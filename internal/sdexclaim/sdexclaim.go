// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

// Package sdexclaim holds the shared helpers for interpreting SDEX
// ClaimAtoms (the per-fill records inside a ManageOffer/PathPayment result).
// The dispatcher's census walk and the ClickHouse structural extractor both count "real" trades
// from the same slices; this is their single copy, since sharing via internal/sources/sdex would
// cycle back through internal/dispatcher.
package sdexclaim

import (
	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// parts destructures one ClaimAtom into the fields every drop rule needs, plus `known` for whether
// the discriminant is one of the three variants we understand (all zero values when false). A
// zero-returning helper for unknown variants would count an un-decodable atom as a zero-amount
// trade, so callers must check known.
func parts(a xdr.ClaimAtom) (sold, bought xdr.Int64, soldAsset, boughtAsset xdr.Asset, known bool) {
	switch a.Type {
	case xdr.ClaimAtomTypeClaimAtomTypeOrderBook:
		ob := a.MustOrderBook()
		return ob.AmountSold, ob.AmountBought, ob.AssetSold, ob.AssetBought, true
	case xdr.ClaimAtomTypeClaimAtomTypeLiquidityPool:
		lp := a.MustLiquidityPool()
		return lp.AmountSold, lp.AmountBought, lp.AssetSold, lp.AssetBought, true
	case xdr.ClaimAtomTypeClaimAtomTypeV0:
		v0 := a.MustV0()
		return v0.AmountSold, v0.AmountBought, v0.AssetSold, v0.AssetBought, true
	}
	return 0, 0, xdr.Asset{}, xdr.Asset{}, false
}

// BoughtSide returns one ClaimAtom's (AssetBought, AmountBought), what the taker PAID into that offer or pool, and whether its discriminant is a known variant; callers needing a leg use it so a new ClaimAtom variant is taught to [parts] once.
func BoughtSide(a xdr.ClaimAtom) (asset xdr.Asset, amount xdr.Int64, known bool) {
	_, bought, _, boughtAsset, known := parts(a)
	return boughtAsset, bought, known
}

// IsRealTrade reports whether sdex.decodeClaimAtom would return a Trade for a ClaimAtom: the single predicate both count oracles share, so they equal the decoder's output.
// It drops unknown atom types, both-legs-zero (one-side-zero is kept), assets AssetFromXDR rejects, and self-crosses; the writer's Validate drops more (see chops.sdexServedCensus).
func IsRealTrade(a xdr.ClaimAtom) bool {
	sold, bought, soldAsset, boughtAsset, known := parts(a)
	if !known {
		return false
	}
	if sold <= 0 && bought <= 0 {
		return false
	}
	base, err := canonical.AssetFromXDR(soldAsset)
	if err != nil {
		return false
	}
	quote, err := canonical.AssetFromXDR(boughtAsset)
	if err != nil {
		return false
	}
	if _, err := canonical.NewPair(base, quote); err != nil {
		return false
	}
	return true
}

// RealTradeCount counts the claims that will become `trades` rows; see [IsRealTrade] for the drop rules, which must match the decoder's exactly.
func RealTradeCount(claims []xdr.ClaimAtom) int {
	n := 0
	for i := range claims {
		if IsRealTrade(claims[i]) {
			n++
		}
	}
	return n
}
