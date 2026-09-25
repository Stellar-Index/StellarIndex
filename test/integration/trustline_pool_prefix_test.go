//go:build integration

package integration_test

import (
	"context"
	"testing"
	"time"

	chstore "github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// TestTrustlineAssetsAfter_PoolPrefixExcludesOnlyRealPoolShares is the
// executing proof of CA2-A14-correct-5: TrustlineAssetsAfter's pool-share
// exclusion must match only the two spellings TrustLineAssetID emits
// ("pool:<hex>" and the bare "pool"), never a real credit-asset string that
// merely starts with the substring "pool" — asset codes are case-sensitive
// and "poolX" etc. are valid Stellar asset codes (internal/canonical/asset.go
// validateClassicAssetCode).
//
// Pre-fix, `NOT startsWith(asset, 'pool')` drops "poolX-GISSUER..." silently
// alongside the real pool-share rows; this test goes RED on that predicate.
func TestTrustlineAssetsAfter_PoolPrefixExcludesOnlyRealPoolShares(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)

	const (
		ledger    = uint32(70_100_000)
		realAsset = "poolX-GCA14POOLPREFIXTESTISSUERAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	)
	closeTime := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)

	rows := []chstore.LedgerEntryChangeRow{
		{
			LedgerSeq: ledger, CloseTime: closeTime, TxHash: "c24-1", OpIndex: 0, ChangeIndex: 0,
			IntraLedgerSeq: 1, ChangeType: "updated", EntryType: "trustline",
			KeyXDR: "c24-key-real", Asset: realAsset,
		},
		{
			LedgerSeq: ledger, CloseTime: closeTime, TxHash: "c24-2", OpIndex: 0, ChangeIndex: 0,
			IntraLedgerSeq: 2, ChangeType: "updated", EntryType: "trustline",
			KeyXDR: "c24-key-poolhex", Asset: "pool:c24deadbeef",
		},
		{
			LedgerSeq: ledger, CloseTime: closeTime, TxHash: "c24-3", OpIndex: 0, ChangeIndex: 0,
			IntraLedgerSeq: 3, ChangeType: "updated", EntryType: "trustline",
			KeyXDR: "c24-key-poolbare", Asset: "pool",
		},
	}
	if _, err := chstore.InsertEntryChanges(ctx, addr, rows, 0); err != nil {
		t.Fatalf("InsertEntryChanges: %v", err)
	}

	scanner, err := chstore.NewHoldingsScanner(ctx, addr)
	if err != nil {
		t.Fatalf("NewHoldingsScanner: %v", err)
	}
	defer func() { _ = scanner.Close() }()

	// "ooo" sorts after every uppercase-coded asset other tests seed
	// (ASCII uppercase < lowercase) but strictly before "pool", "pool:..."
	// and "poolX...", so this page captures all three fixture rows without
	// depending on how many unrelated rows the shared container holds.
	seeds, err := scanner.TrustlineAssetsAfter(ctx, "ooo", 50)
	if err != nil {
		t.Fatalf("TrustlineAssetsAfter: %v", err)
	}

	var gotReal, gotPoolHex, gotPoolBare bool
	for _, s := range seeds {
		switch s.Asset {
		case realAsset:
			gotReal = true
		case "pool:c24deadbeef":
			gotPoolHex = true
		case "pool":
			gotPoolBare = true
		}
	}

	if !gotReal {
		t.Errorf("TrustlineAssetsAfter dropped real asset %q — the pool-prefix exclusion over-matched a credit asset code starting with %q", realAsset, "pool")
	}
	if gotPoolHex {
		t.Errorf("TrustlineAssetsAfter returned the pool:<hex> share row %q — pool-share exclusion regressed", "pool:c24deadbeef")
	}
	if gotPoolBare {
		t.Errorf("TrustlineAssetsAfter returned the bare pool fallback row — pool-share exclusion regressed")
	}
}
