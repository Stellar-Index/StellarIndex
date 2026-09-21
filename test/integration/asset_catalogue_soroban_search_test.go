//go:build integration

package integration_test

import (
	"context"
	"testing"
	"time"

	c "github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/canonical/discovery"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// TestAssetsListing_SorobanTypeAndQ pins RLT-023: a Soroban-native
// contract asset has NULL code/issuer_g_strkey/slug (contract assets
// have no SEP-1 code or issuer account — see listAssetsBaseSelect's
// discovered-contract arm), so the q predicate's original
// `COALESCE(ca.slug, ca.code)` was NULL for every such row and
// `type=soroban&q=<anything>` matched zero rows unconditionally,
// regardless of whether the contract id itself matched. The fix folds
// ca.asset_id into that COALESCE, mirroring the base SELECT's own
// "slug" column, so a query on the contract id (or a prefix of it)
// finds the row.
func TestAssetsListing_SorobanTypeAndQ(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	c.InstallAliasRegistry(nil)
	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	const sorobanSearchContractID = "CHBRPOIGF3CBFNOBM2O4RAK3VRJNVGFYGWWQC5HYFSXMECOSFOGYR5XK"

	now := time.Now().UTC().Truncate(time.Minute)
	if err := store.RecordDiscovered(ctx, discovery.Hit{
		ContractID:        sorobanSearchContractID,
		Kind:              discovery.KindSEP41,
		EventType:         discovery.EventTransfer,
		Ledger:            50_000_000,
		ObservedAtRFC3339: now.Add(-24 * time.Hour).Format(time.RFC3339),
	}); err != nil {
		t.Fatalf("RecordDiscovered: %v", err)
	}
	// listAssetsBaseSelect's discovered-contract arm requires an
	// asset_volume_24h row (bounded by TRADED volume, not discovery).
	seedRawVolume(t, ctx, store.DB(), sorobanSearchContractID, "1000")

	rows, err := store.ListAssetsExt(ctx, timescale.ListAssetsOptions{
		Type:  "soroban",
		Q:     "CHBRPOIGF3", // contract-id prefix; there is no code/slug/issuer to match on
		Limit: 10,
	})
	if err != nil {
		t.Fatalf("ListAssetsExt: %v", err)
	}
	if len(rows) != 1 || rows[0].AssetID != sorobanSearchContractID {
		t.Fatalf("ListAssetsExt(type=soroban, q=contract-id-prefix) = %+v, want exactly the seeded contract row", rows)
	}

	// A query that matches nothing must still return the empty page, not
	// error — the predicate should still be selective.
	empty, err := store.ListAssetsExt(ctx, timescale.ListAssetsOptions{
		Type:  "soroban",
		Q:     "NOSUCHCONTRACTPREFIX",
		Limit: 10,
	})
	if err != nil {
		t.Fatalf("ListAssetsExt (no match): %v", err)
	}
	if len(empty) != 0 {
		t.Errorf("ListAssetsExt(type=soroban, q=no-match) = %+v, want empty", empty)
	}
}
