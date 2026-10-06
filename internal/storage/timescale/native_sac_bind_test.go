// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package timescale

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
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
		{"BlendFillsForMEVScan", 0, func(s *Store) { _, _ = s.BlendFillsForMEVScan(ctx, since, 10) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, conn := newScriptedStore(t)
			tc.run(store)
			if len(conn.stmts) == 0 {
				t.Fatal("no statement issued")
			}
			st := conn.stmts[0]
			assertPlaceholdersMatchArgs(t, st.sql, len(st.args))
			if tc.idx == 0 {
				return
			}
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
			assertPlaceholdersMatchArgs(t, q, len(args))
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

var (
	sqlLineComment = regexp.MustCompile(`--[^\n]*`)
	sqlPlaceholder = regexp.MustCompile(`\$(\d+)`)
)

// pgx rejects a statement whose argument count differs from its highest
// placeholder, so an inserted bind must not leave an unused or missing arg.
func assertPlaceholdersMatchArgs(t *testing.T, sql string, nargs int) {
	t.Helper()
	seen := map[int]bool{}
	for _, m := range sqlPlaceholder.FindAllStringSubmatch(sqlLineComment.ReplaceAllString(sql, ""), -1) {
		n, _ := strconv.Atoi(m[1])
		seen[n] = true
	}
	if len(seen) != nargs {
		t.Fatalf("SQL uses %d distinct placeholders, statement binds %d args\nSQL: %s", len(seen), nargs, sql)
	}
	for i := 1; i <= nargs; i++ {
		if !seen[i] {
			t.Fatalf("SQL never references $%d of %d bound args\nSQL: %s", i, nargs, sql)
		}
	}
}

// The asset_price_snapshot writer prices asset-vs-XLM arms against the
// installed network's SAC too, not the pubnet literal.
func TestNativeSACIsBound_PriceSnapshotWriter(t *testing.T) {
	installTestnetForBind(t)
	store, conn := newScriptedStore(t,
		scriptedResult{}, scriptedResult{},
		scriptedResult{rowsAffected: 1}, scriptedResult{rowsAffected: 1},
		scriptedResult{rowsAffected: 1}, scriptedResult{rowsAffected: 1},
	)
	if err := store.RefreshAssetVolume24h(context.Background()); err != nil {
		t.Fatalf("RefreshAssetVolume24h: %v", err)
	}
	for _, st := range conn.stmts {
		if !strings.Contains(st.sql, "INSERT INTO asset_price_snapshot") {
			continue
		}
		assertPlaceholdersMatchArgs(t, st.sql, len(st.args))
		if got := st.arg(t, 1); got != testnetNativeSACForBind {
			t.Fatalf("snapshot writer $1 = %v, want the testnet SAC %s", got, testnetNativeSACForBind)
		}
		if strings.Contains(st.sql, canonical.XLMSacContractID) {
			t.Fatal("snapshot writer still embeds the pubnet XLM SAC literal")
		}
		return
	}
	t.Fatal("no asset_price_snapshot upsert issued")
}
