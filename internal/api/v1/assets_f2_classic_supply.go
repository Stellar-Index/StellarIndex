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
func (s *Server) withLakeSupply(
	ctx context.Context, detail *AssetDetail, asset canonical.Asset, key string, keyErr error,
	snap supply.Supply, haveSnap, supplyFailed bool,
) (supply.Supply, bool, bool) {
	switch {
	case s.TokenSupply != nil && asset.Type == canonical.AssetSoroban && asset.ContractID != "":
		snap, haveSnap, supplyFailed, detail.supplyFloor = s.withContractSupply(ctx, asset.ContractID, snap, haveSnap, supplyFailed)
	case asset.Type == canonical.AssetClassic && keyErr == nil:
		snap, haveSnap, supplyFailed, detail.supplyFloor = s.withClassicSupply(ctx, asset, key, snap, haveSnap, supplyFailed)
	}
	return snap, haveSnap, supplyFailed
}

// withClassicSupply replaces a classic asset's missing or stale observer snapshot with the listing's own reading
// ([classicSupplyReading] over the same cached lake-flows and trustline readers), so /v1/assets and /v1/assets/{id} serve one
// figure under one basis. An arm that did not answer counts as a failed supply read; floor is the replacement's.
func (s *Server) withClassicSupply(
	ctx context.Context, asset canonical.Asset, key string, snap supply.Supply, haveSnap, supplyFailed bool,
) (out supply.Supply, have, failed, floor bool) {
	sctx, reads := withReadReport(ctx)
	assetID := asset.String()
	lake, lakeMissing := s.classicLakeSupplyRead(sctx, []AssetDetail{{AssetID: assetID}})
	broad, broadUnread := s.classicSupplyMap(sctx)
	failed = supplyFailed || len(lakeMissing) > 0 || broadUnread || reads.stale.Load()
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

// classicNetBelowFloor reports whether a classic asset's trustline sum (a provable floor) exceeds its event net, which makes
// the net a lower bound, and whether the floor could not be read. Contract ids and a deployment without the reader check nothing.
func (s *Server) classicNetBelowFloor(ctx context.Context, assetID string, net *big.Int) (below, unread bool) {
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
