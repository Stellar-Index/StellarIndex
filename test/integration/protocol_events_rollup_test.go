//go:build integration

package integration_test

import (
	"context"
	"testing"
	"time"

	c "github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// TestProtocolEventRollup_RoundTrip proves the #43 rollup end-to-end:
// RefreshProtocolEventCounts folds the trailing-24h census into
// protocol_events_24h, CountRecentEventsBySource reads it back, only
// recent trades count, the refresh is idempotent, and a source that
// ages out of the 24h window is pruned on the next pass.
func TestProtocolEventRollup_RoundTrip(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	xlm, _ := c.NewCryptoAsset("XLM")
	usdc, _ := c.NewCryptoAsset("USDC")
	pair, _ := c.NewPair(xlm, usdc)

	recent := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Second)
	old := time.Now().UTC().Add(-30 * time.Hour).Truncate(time.Second)

	// 3 recent sdex trades + 1 recent soroswap trade + 1 old aquarius
	// trade (outside the 24h window → must not count).
	seed := []c.Trade{
		mkIntegrationTrade("sdex", 1, recent, pair, 100, 100),
		mkIntegrationTrade("sdex", 2, recent, pair, 100, 100),
		mkIntegrationTrade("sdex", 3, recent, pair, 100, 100),
		mkIntegrationTrade("soroswap", 4, recent, pair, 100, 100),
		mkIntegrationTrade("aquarius", 5, old, pair, 100, 100),
	}
	for _, tr := range seed {
		if err := store.InsertTrade(ctx, tr); err != nil {
			t.Fatalf("InsertTrade %s: %v", tr.Source, err)
		}
	}

	if err := store.RefreshProtocolEventCounts(ctx); err != nil {
		t.Fatalf("RefreshProtocolEventCounts: %v", err)
	}

	counts, err := store.CountRecentEventsBySource(ctx)
	if err != nil {
		t.Fatalf("CountRecentEventsBySource: %v", err)
	}
	if counts["sdex"] != 3 {
		t.Errorf("sdex = %d, want 3", counts["sdex"])
	}
	if counts["soroswap"] != 1 {
		t.Errorf("soroswap = %d, want 1", counts["soroswap"])
	}
	if _, ok := counts["aquarius"]; ok {
		t.Errorf("aquarius = %d present, want absent (trade is > 24h old)", counts["aquarius"])
	}

	// Idempotent: a second refresh yields identical counts.
	if err := store.RefreshProtocolEventCounts(ctx); err != nil {
		t.Fatalf("RefreshProtocolEventCounts (2nd): %v", err)
	}
	counts2, err := store.CountRecentEventsBySource(ctx)
	if err != nil {
		t.Fatalf("CountRecentEventsBySource (2nd): %v", err)
	}
	if counts2["sdex"] != 3 || counts2["soroswap"] != 1 {
		t.Errorf("after 2nd refresh sdex=%d soroswap=%d, want 3/1", counts2["sdex"], counts2["soroswap"])
	}

	// Prune: a stale sentinel row (a source no longer counted this pass)
	// must be dropped by the next refresh, while live sources survive.
	if _, err := store.DB().ExecContext(ctx,
		`INSERT INTO protocol_events_24h (source, events_24h, computed_at)
		 VALUES ('zzz_stale_source', 999, now() - interval '1 hour')`); err != nil {
		t.Fatalf("insert stale sentinel: %v", err)
	}
	if err := store.RefreshProtocolEventCounts(ctx); err != nil {
		t.Fatalf("RefreshProtocolEventCounts (3rd): %v", err)
	}
	counts3, err := store.CountRecentEventsBySource(ctx)
	if err != nil {
		t.Fatalf("CountRecentEventsBySource (3rd): %v", err)
	}
	if _, ok := counts3["zzz_stale_source"]; ok {
		t.Errorf("stale sentinel still present after refresh, want pruned")
	}
	if counts3["sdex"] != 3 || counts3["soroswap"] != 1 {
		t.Errorf("live sources changed across prune: sdex=%d soroswap=%d, want 3/1", counts3["sdex"], counts3["soroswap"])
	}
}
