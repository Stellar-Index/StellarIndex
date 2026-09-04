// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package aggregate

import (
	"context"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// The thin-pool third-alias shape (launch-plan D7, C4-012/13) on the
// headline tier: TestComputeGlobalPrice_VWAPTierReachesSACOnlyLast pins
// the ordering for XLM's compile-time family; these pin it for the
// families the operator declares in `[supply].sac_wrappers`, which is
// where every OTHER SAC-wrapped classic gets its second spelling (the
// ansible template codifies 38 wrapper families — USDC, AQUA, yXLM, BLND
// among them). The registry is process-wide, so these are NOT parallel.

const (
	thinPoolAquaIssuer  = "GBNZILSTVQZ4R7IKQDGHYGY2QXL5QOFJYQMXPKWRRM5PAV7Y4M67AQUA"
	thinPoolAquaClassic = "AQUA-" + thinPoolAquaIssuer
	thinPoolAquaSAC     = "CAUIKL3IYGMERDRUN6YSCLWVAKIFG5Q4YJHUKM4S4NJZQIA3BAS6OJPK"
)

func installThinPoolRegistry(t *testing.T) canonical.Asset {
	t.Helper()
	reg, err := canonical.NewAliasRegistry(map[string]string{
		thinPoolAquaSAC: "AQUA:" + thinPoolAquaIssuer,
	})
	if err != nil {
		t.Fatalf("NewAliasRegistry: %v", err)
	}
	canonical.InstallAliasRegistry(reg)
	t.Cleanup(func() { canonical.InstallAliasRegistry(nil) })
	classic, err := canonical.NewClassicAsset("AQUA", thinPoolAquaIssuer)
	if err != nil {
		t.Fatalf("NewClassicAsset: %v", err)
	}
	return classic
}

func assertVWAPCalls(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("vwap query order = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("vwap query[%d] = %q, want %q (full order %v)", i, got[i], want[i], got)
		}
	}
}

// TestComputeGlobalPrice_VWAPTierConfiguredWrapperSACLast: a configured
// classic↔SAC family walks classic FIRST. When the classic book clears
// the trade-count floor the SAC pool is never queried — the tier has no
// freshness preference, so a quiet-but-deep classic bucket beats a fresh
// thin pool by ORDER alone — and when the classic misses the SAC form is
// reached, second, so a Soroban-only market still prices.
func TestComputeGlobalPrice_VWAPTierConfiguredWrapperSACLast(t *testing.T) {
	classic := installThinPoolRegistry(t)
	quote, err := canonical.NewFiatAsset("USD")
	if err != nil {
		t.Fatalf("fiat: %v", err)
	}

	// (1) the classic book has depth — the pool must not be consulted,
	// however many trades it claims.
	deep := &aliasAwareReader{byBase: map[string]int64{
		thinPoolAquaClassic: 50,
		thinPoolAquaSAC:     9_999,
	}}
	res, err := ComputeGlobalPrice(context.Background(), classic, quote, deep, DefaultGlobalPriceOptions())
	if err != nil {
		t.Fatalf("ComputeGlobalPrice (deep classic): %v", err)
	}
	if res.Authority != AuthorityVWAPNative || res.TradeCount != 50 {
		t.Errorf("authority/trade_count = %q/%d, want vwap_native/50 (the classic book, not the SAC pool)", res.Authority, res.TradeCount)
	}
	assertVWAPCalls(t, deep.vwapCalls, []string{thinPoolAquaClassic})

	// (2) the classic form misses — the SAC form is the last resort and
	// is reached, in second position.
	sacOnly := &aliasAwareReader{byBase: map[string]int64{thinPoolAquaSAC: 12}}
	res, err = ComputeGlobalPrice(context.Background(), classic, quote, sacOnly, DefaultGlobalPriceOptions())
	if err != nil {
		t.Fatalf("ComputeGlobalPrice (SAC only): %v", err)
	}
	if res.Authority != AuthorityVWAPNative || res.TradeCount != 12 {
		t.Errorf("authority/trade_count = %q/%d, want vwap_native/12", res.Authority, res.TradeCount)
	}
	assertVWAPCalls(t, sacOnly.vwapCalls, []string{thinPoolAquaClassic, thinPoolAquaSAC})
}

// TestComputeGlobalPrice_VWAPTierBelowFloorSACDoesNotRescue: a classic
// bucket UNDER the trade-count floor falls through to the SAC form — the
// floor is per-form, and the SAC pool can then answer. That is the one
// arrangement in which a thin pool prices a wrapped classic on this tier,
// and it is bounded by the same floor: the pool must itself clear
// VWAPMinTradeCount, and the reader behind it (globalPriceReader in the
// API binary) withholds the pair when the alias-union market is below the
// substance floor. Pinned so the boundary is explicit rather than
// implied.
func TestComputeGlobalPrice_VWAPTierBelowFloorSACDoesNotRescue(t *testing.T) {
	classic := installThinPoolRegistry(t)
	quote, err := canonical.NewFiatAsset("USD")
	if err != nil {
		t.Fatalf("fiat: %v", err)
	}
	opts := DefaultGlobalPriceOptions() // VWAPMinTradeCount = 5

	// Classic under the floor, SAC also under the floor: NO tier-1 price.
	both := &aliasAwareReader{byBase: map[string]int64{
		thinPoolAquaClassic: 2,
		thinPoolAquaSAC:     3,
	}}
	if _, err := ComputeGlobalPrice(context.Background(), classic, quote, both, opts); err == nil {
		t.Errorf("ComputeGlobalPrice served a tier-1 price from two sub-floor buckets; want ErrNoPrice")
	}
	assertVWAPCalls(t, both.vwapCalls, []string{thinPoolAquaClassic, thinPoolAquaSAC})
}
