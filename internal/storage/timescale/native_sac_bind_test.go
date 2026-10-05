// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package timescale

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

const (
	testnetPassphraseForBind = "Test SDF Network ; September 2015"
	testnetNativeSACForBind  = "CDLZFC3SYJYDZT7K67VZ75HPJVIEUVNIXF47ZG2FB2RMQQVU2HHGCYSC"
)

func installTestnetForBind(t *testing.T) {
	t.Helper()
	canonical.InstallNetworkPassphrase(testnetPassphraseForBind)
	t.Cleanup(func() { canonical.InstallNetworkPassphrase("") })
	if got := canonical.NativeSACContractID(); got != testnetNativeSACForBind {
		t.Fatalf("NativeSACContractID() = %q, want the testnet SAC %q", got, testnetNativeSACForBind)
	}
}

// Every read path that treats the native XLM SAC as XLM binds
// canonical.NativeSACContractID() instead of the pubnet literal, at the
// placeholder the SQL names. Installed network is testnet, so a regression
// to the pubnet constant fails on the bound value.
func TestNativeSACIsBoundNotHardCoded(t *testing.T) {
	installTestnetForBind(t)
	ctx := context.Background()
	since := time.Now()

	for _, tc := range []struct {
		name string
		idx  int
		run  func(s *Store)
	}{
		{"TradesForArbScan", 3, func(s *Store) { _, _, _ = s.TradesForArbScan(ctx, since, 10) }},
		{"SorobanVolume24hUSDForAsset", 2, func(s *Store) { _, _ = s.SorobanVolume24hUSDForAsset(ctx, "C1") }},
		{"GetSourceStats", 1, func(s *Store) { _, _ = s.GetSourceStats(ctx) }},
		{"PairSourceStats", 3, func(s *Store) { _, _ = s.PairSourceStats(ctx, []string{"native"}, []string{"USDC-G"}) }},
		{"AssetSourceStats", 2, func(s *Store) { _, _ = s.AssetSourceStats(ctx, []string{"native"}) }},
		{"GetNetworkStats", 2, func(s *Store) { _, _ = s.GetNetworkStats(ctx) }},
		{"GetAssetMarketsCount", 2, func(s *Store) { _, _ = s.GetAssetMarketsCount(ctx, "native") }},
		{"GetAssetTopMarkets", 3, func(s *Store) { _, _ = s.GetAssetTopMarkets(ctx, "native", 5) }},
		{"GetAssetBySlug", 2, func(s *Store) { _, _ = s.GetAssetBySlug(ctx, "USDC") }},
		{"GetAssetPriceHistory24h", 2, func(s *Store) { _, _ = s.GetAssetPriceHistory24h(ctx, "native") }},
		{"GetAssetPriceHistory7d", 2, func(s *Store) { _, _ = s.GetAssetPriceHistory7d(ctx, "native") }},
		{"GetAssetsPriceHistory24hBatch", 4, func(s *Store) { _, _ = s.GetAssetsPriceHistory24hBatch(ctx, []string{"native"}) }},
		{"GetAssetsPriceHistory7dBatch", 4, func(s *Store) { _, _ = s.GetAssetsPriceHistory7dBatch(ctx, []string{"native"}) }},
		{"TransitiveUSDPriceCandidates", 3, func(s *Store) { _, _ = s.TransitiveUSDPriceCandidates(ctx, "C1") }},
		{"PopularPricelessCandidates", 1, func(s *Store) { _, _ = s.PopularPricelessCandidates(ctx) }},
		{"AssetIsPriced", 1, func(s *Store) { _, _ = s.AssetIsPriced(ctx, "C1") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, conn := newScriptedStore(t)
			tc.run(store)
			if len(conn.stmts) == 0 {
				t.Fatal("no statement issued")
			}
			st := conn.stmts[0]
			if got := st.arg(t, tc.idx); got != testnetNativeSACForBind {
				t.Errorf("$%d = %v, want the installed network's native SAC %s", tc.idx, got, testnetNativeSACForBind)
			}
			if !strings.Contains(st.sql, fmt.Sprintf("$%d::text", tc.idx)) {
				t.Errorf("SQL does not reference $%d::text", tc.idx)
			}
			if strings.Contains(st.sql, canonical.XLMSacContractID) {
				t.Errorf("SQL hard-codes the pubnet native SAC")
			}
		})
	}
}

func TestNativeSACIsBound_MarketQueryBuilders(t *testing.T) {
	installTestnetForBind(t)
	since := time.Now()
	for _, tc := range []struct {
		name  string
		idx   int
		build func() (string, []any)
	}{
		{"buildPoolsQuery", 10, func() (string, []any) {
			return buildPoolsQuery(since, PoolsFilter{}, "", 10, MarketsOrderVolume24hDesc)
		}},
		{"buildSourceMarketsQuery", 7, func() (string, []any) {
			return buildSourceMarketsQuery(since, "sdex", "", 10, MarketsOrderVolume24hDesc)
		}},
		{"buildDistinctPairsQuery", 8, func() (string, []any) {
			return buildDistinctPairsQuery(since, "", "native", "", 10, MarketsOrderVolume24hDesc)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			q, args := tc.build()
			if len(args) != tc.idx {
				t.Fatalf("args = %d, want the SAC last at $%d", len(args), tc.idx)
			}
			if args[tc.idx-1] != testnetNativeSACForBind {
				t.Errorf("$%d = %v, want %s", tc.idx, args[tc.idx-1], testnetNativeSACForBind)
			}
			if !strings.Contains(q, fmt.Sprintf("$%d::text", tc.idx)) {
				t.Errorf("SQL does not reference $%d::text", tc.idx)
			}
			if strings.Contains(q, canonical.XLMSacContractID) {
				t.Errorf("SQL hard-codes the pubnet native SAC")
			}
		})
	}
}
