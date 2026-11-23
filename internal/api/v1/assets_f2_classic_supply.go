// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	"context"
	"math/big"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/supply"
)

// withLakeSupply is the detail's fallback for a missing or stale observer snapshot: a Soroban token's contract reading, or a
// classic asset outside the observer's watch-list taking the listing's reading. It sets detail.supplyFloor for the replacement.
// A watched classic asset keeps its stale snapshot: flagged stale, cap withheld, operator max-supply override intact.
func (s *Server) withLakeSupply(
	ctx context.Context, detail *AssetDetail, asset canonical.Asset, key string, keyErr error,
	snap supply.Supply, haveSnap, supplyFailed bool,
) (supply.Supply, bool, bool) {
	switch {
	case s.TokenSupply != nil && asset.Type == canonical.AssetSoroban && asset.ContractID != "":
		snap, haveSnap, supplyFailed, detail.supplyFloor = s.withContractSupply(ctx, asset.ContractID, snap, haveSnap, supplyFailed)
	case asset.Type == canonical.AssetClassic && keyErr == nil && !haveSnap:
		snap, haveSnap, supplyFailed, detail.supplyFloor = s.withClassicSupply(ctx, asset, key, snap, haveSnap, supplyFailed)
	}
	return snap, haveSnap, supplyFailed
}

// withClassicSupply replaces an unwatched classic asset's missing snapshot with the listing's reading ([classicSupplyReading]
// over the lake net /supply reads and the cached trustline sum). An arm that did not answer counts as a failed supply read;
// floor is the replacement's.
func (s *Server) withClassicSupply(
	ctx context.Context, asset canonical.Asset, key string, snap supply.Supply, haveSnap, supplyFailed bool,
) (out supply.Supply, have, failed, floor bool) {
	sctx, reads := withReadReport(ctx)
	assetID := asset.String()
	lake, lakeUnread := s.classicLakeNet(sctx, asset)
	broad, broadUnread := s.classicSupplyMap(sctx)
	failed = supplyFailed || lakeUnread || broadUnread || reads.stale.Load()
	figure, basis := classicSupplyReading(assetID, nil, lake, broad)
	total, ok := new(big.Int).SetString(figure, 10)
	if !ok {
		return snap, haveSnap, failed, false
	}
	out = supply.Supply{AssetKey: key, CirculatingSupply: total, Basis: basis}
	floor = basis.LowerBound()
	// total_supply carries no floor qualifier, so a trustline floor is left absent there, as on the contract arm.
	if !floor {
		out.TotalSupply = total
	}
	return out, true, failed, floor
}

// classicLakeNet is one classic asset's lake net, keyed for [classicSupplyReading], through the per-contract cached reader
// /supply uses (its own batch, so no wait on a bulk refresh summing other assets). No usable net is a miss, not unread.
func (s *Server) classicLakeNet(ctx context.Context, asset canonical.Asset) (map[string]string, bool) {
	sac, err := asset.SacContractID()
	if s.TokenSupply == nil || err != nil {
		return nil, false
	}
	rctx, cancel := context.WithTimeout(ctx, tokenMetadataReadTimeout)
	defer cancel()
	sup, err := s.TokenSupply.TokenSupply(rctx, sac)
	if err != nil {
		return nil, true
	}
	// The listing's bulk refresh admits the same nets ([Server.refreshClassicLakeSupply]).
	if sup.Total == nil || sup.Incomplete || sup.Total.Sign() <= 0 {
		return nil, false
	}
	return map[string]string{asset.String(): sup.Total.String()}, false
}

// classicNetBelowFloor reports whether a classic asset's trustline sum (a provable floor) exceeds its event net, which makes
// the net a lower bound, and whether the floor could not be read. A SAC id is checked as the classic asset it wraps; other
// contract ids and a deployment without the reader check nothing.
func (s *Server) classicNetBelowFloor(ctx context.Context, assetID string, net *big.Int) (below, unread bool) {
	if canonical.IsContractID(assetID) && s.Explorer != nil {
		wrapped, ok, err := s.sacWrappedAssetIDErr(ctx, assetID)
		if err != nil {
			return false, true
		}
		if ok {
			assetID = wrapped
		}
	}
	asset, err := canonical.ParseAsset(assetID)
	if err != nil || asset.Type != canonical.AssetClassic || net == nil {
		return false, false
	}
	broad, unread := s.classicSupplyMap(ctx)
	if unread {
		return false, true
	}
	floor, ok := new(big.Int).SetString(broad[asset.String()], 10)
	return ok && floor.Cmp(net) > 0, false
}

// anyLakeRowUnchecked reports a row serving the classic lake-flows arm, which a failed trustline read left unchecked against
// its floor: that floor could have been the higher, served figure.
func anyLakeRowUnchecked(rows []AssetDetail) bool {
	for i := range rows {
		if rows[i].SupplyBasis != nil && *rows[i].SupplyBasis == supply.BasisClassicLakeFlows.String() {
			return true
		}
	}
	return false
}
