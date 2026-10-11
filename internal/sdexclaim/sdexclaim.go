// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

// Package sdexclaim holds the shared helpers for interpreting SDEX
// ClaimAtoms (the per-fill records inside a ManageOffer/PathPayment result).
// Both the dispatcher's census walk and the ClickHouse structural extractor
// count "real" trades from the same xdr.ClaimAtom slices; this is their single
// canonical copy (they can't share via internal/sources/sdex — that would
// cycle back through internal/dispatcher).
package sdexclaim

import (
	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// (The former exported `Amounts` helper is gone: it had no callers left,
// and its "return (0, 0) for an unknown variant" contract is exactly the
// shape that let an un-decodable atom be counted as a zero-amount trade
// rather than rejected outright. Use [IsRealTrade]; `parts` below is the
// destructuring it needs.)

// parts destructures one ClaimAtom into the five fields every drop rule
// needs, plus `known` = whether the atom's discriminant is one of the
// three variants we understand. All zero values when known is false.
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

// BoughtSide returns one ClaimAtom's (AssetBought, AmountBought) — what
// the taker PAID into that offer or pool — and whether its discriminant is
// a known variant. Callers that need a leg rather than the trade predicate
// use this instead of switching on the variant themselves, so a new
// ClaimAtom variant is taught to [parts] once.
func BoughtSide(a xdr.ClaimAtom) (asset xdr.Asset, amount xdr.Int64, known bool) {
	_, bought, _, boughtAsset, known := parts(a)
	return boughtAsset, bought, known
}

// IsRealTrade reports whether internal/sources/sdex.decodeClaimAtom would
// return a Trade rather than an error for one ClaimAtom.
//
// It is the SINGLE definition of that predicate, applied by both count
// oracles (dispatcher.claimAtomCount for the ADR-0033 census and
// clickhouse.claimAtomCount for the lake's classic_trade_effect_count)
// so both equal the DECODER's trade output by construction. They do NOT
// equal COUNT(trades): the writer additionally drops one-side-zero fills
// (canonical.Trade.Validate, CHECK base_amount > 0), which rule 2 keeps.
// A served-tier oracle must re-derive through that filter
// (chops.sdexServedCensus).
//
// The four drop rules, in the decoder's order:
//
//  1. UNKNOWN ATOM TYPE: decodeClaimAtom returns ErrUnknownClaimAtomType.
//  2. BOTH LEGS ZERO: no-op claim atoms. ONE-side-zero fills are KEPT:
//     real trades where one leg rounded to 0.
//  3. AN ASSET THAT DOESN'T CONVERT: [canonical.AssetFromXDR] rejects
//     unsupported types, un-encodable issuers, and asset codes with
//     bytes outside [a-zA-Z0-9] (control-byte codes exist on chain).
//  4. A SELF-CROSS: canonical.NewPair rejects base == quote.
//
// All four must apply here: an oracle that counts rows the writer
// deterministically refuses can NEVER reconcile, hiding real loss.
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

// RealTradeCount counts the claims that will become `trades` rows — see
// [IsRealTrade] for the drop rules and why they must match the decoder's
// exactly.
func RealTradeCount(claims []xdr.ClaimAtom) int {
	n := 0
	for i := range claims {
		if IsRealTrade(claims[i]) {
			n++
		}
	}
	return n
}
