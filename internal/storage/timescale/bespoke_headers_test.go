// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package timescale

import "testing"

// typedBespokeHeaders is every typed table header the builders copy; a new typed table adds its header here.
var typedBespokeHeaders = map[string]BespokeTable{
	"defindexVaultFlowsHeader":  defindexVaultFlowsHeader,
	"defindexStrategyHeader":    defindexStrategyHeader,
	"lendingFlashLoansHeader":   lendingFlashLoansHeader,
	"blntBackstopFlowHeader":    blntBackstopFlowHeader,
	"creditSettlementsHeader":   creditSettlementsHeader,
	"aquariusReservesHeader":    aquariusReservesHeader,
	"aquariusClaimVolumeHeader": aquariusClaimVolumeHeader,
	"aquariusGovernanceHeader":  aquariusGovernanceHeader,
	"cometLiquidityHeader":      cometLiquidityHeader,
	"phoenixLiquidityHeader":    phoenixLiquidityHeader,
	"soroswapSkimPairHeader":    soroswapSkimPairHeader,
	"lendingNetFlowHeader":      lendingNetFlowHeader,
	"lendingPoolActivityHeader": lendingPoolActivityHeader,
	"lendingAuctionsHeader":     lendingAuctionsHeader,
}

func TestBespokeHeaders_KindsMatchColumns(t *testing.T) {
	for name, h := range typedBespokeHeaders {
		if n := len(h.ColumnKinds); n != 0 && n != len(h.Columns) {
			t.Errorf("%s: %d column kinds for %d columns", name, n, len(h.Columns))
			continue
		}
		for ci, k := range h.ColumnKinds {
			checkBespokeColumnKind(t, name, h, ci, k)
		}
	}
}

func checkBespokeColumnKind(t *testing.T, name string, h BespokeTable, ci int, k BespokeColumnKind) {
	t.Helper()
	assetCol := func(what string, c int) {
		if c < 0 || c >= len(h.ColumnKinds) {
			t.Errorf("%s column %d (%s): %s %d out of range", name, ci, h.Columns[ci], what, c)
			return
		}
		if got := h.ColumnKinds[c].Kind; got != BespokeKindAsset {
			t.Errorf("%s column %d (%s): %s %d is kind %q, want asset", name, ci, h.Columns[ci], what, c, got)
		}
	}
	if k.AssetColumn != nil {
		if k.Kind != BespokeKindAmount {
			t.Errorf("%s column %d: asset_column on a %q column", name, ci, k.Kind)
		}
		assetCol("asset_column", *k.AssetColumn)
	}
	for _, c := range k.PoolTokenColumns {
		assetCol("pool token column", c)
	}
}
