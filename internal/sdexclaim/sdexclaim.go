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

// IsRealTrade reports whether one ClaimAtom will become a `trades` row —
// i.e. whether internal/sources/sdex.decodeClaimAtom would return a
// Trade rather than an error for it.
//
// It is the SINGLE definition of that predicate, applied by the two
// count oracles (dispatcher.claimAtomCount for the ADR-0033 census and
// clickhouse.claimAtomCount for the lake's classic_trade_effect_count)
// so both equal COUNT(trades) BY CONSTRUCTION rather than by two
// comments asking future editors to keep three sites in step.
//
// The four drop rules, in the decoder's own order:
//
//  1. UNKNOWN ATOM TYPE. A future/unrecognised ClaimAtom discriminant is
//     not decodable, so decodeClaimAtom returns ErrUnknownClaimAtomType.
//  2. BOTH LEGS ZERO. stellar-core occasionally emits no-op claim atoms
//     that clear at zero on both sides; Hubble drops them too.
//     ONE-side-zero fills are KEPT — they are real trades where one leg
//     rounded to 0, and the aggregator/OHLC paths already skip zero legs.
//  3. AN ASSET THAT DOESN'T CONVERT. [canonical.AssetFromXDR] rejects
//     unsupported asset types, un-encodable issuers, and — the case that
//     actually bites on pubnet — asset codes carrying bytes outside
//     [a-zA-Z0-9]. stellar-core does not enforce the character rule, so
//     control-byte codes exist on chain and the decoder drops those fills.
//  4. A SELF-CROSS. canonical.NewPair rejects base == quote, so a claim
//     atom whose sold and bought assets are the same asset is not a
//     trade.
//
// C2-010 (audit-2026-07-23): pre-fix this predicate was rule 2 alone.
// Rules 1, 3 and 4 were applied only inside the decoder, so both census
// counters over-reported every self-cross and every non-alphanumeric-code
// fill. The census is the ADR-0033 oracle that is supposed to prove the
// decoders lost nothing — an oracle that counts rows the writer
// deterministically refuses can NEVER reconcile, so a genuine future
// trade-loss bug would have been indistinguishable from this permanent
// baseline discrepancy.
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
