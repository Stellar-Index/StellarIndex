//go:build integration

package integration_test

import (
	"context"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/sources/soroswap"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// TestTradeWalkPool_WarmsAndServesTheLongRangeFallback: the serving store's walk pool warms against real Timescale, and a pair whose last
// trade is older than the recent window is still found through it, under the serving session's settings.
func TestTradeWalkPool_WarmsAndServesTheLongRangeFallback(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.OpenServing(ctx, dsn, 30*time.Second)
	if err != nil {
		t.Fatalf("OpenServing: %v", err)
	}
	defer func() { _ = store.Close() }()

	if err := store.WarmTradeWalkPool(ctx); err != nil {
		t.Fatalf("WarmTradeWalkPool: %v", err)
	}

	old := time.Now().UTC().Add(-30 * 24 * time.Hour).Truncate(time.Second)
	trade := canonical.Trade{
		Source:      soroswap.SourceName,
		Ledger:      61_000_001,
		TxHash:      hashLit("5a1c0ffee001"),
		OpIndex:     0,
		Timestamp:   old,
		Pair:        mustPair(mustSorobanAsset(t, 0x41), mustSorobanAsset(t, 0x42)),
		BaseAmount:  amountLit(t, "842000000"),
		QuoteAmount: amountLit(t, "1242000000"),
	}
	if err := store.InsertTrade(ctx, trade); err != nil {
		t.Fatalf("InsertTrade: %v", err)
	}

	since := time.Now().Add(-timescale.LatestVWAPWindow).UTC().Truncate(24 * time.Hour)
	got, err := store.LatestTradesForPair(ctx, trade.Pair, 1, since)
	if err != nil {
		t.Fatalf("LatestTradesForPair: %v", err)
	}
	if len(got) != 1 || got[0].TxHash != trade.TxHash || !got[0].Timestamp.Equal(old) {
		t.Fatalf("got %+v, want the 30-day-old trade %s at %s", got, trade.TxHash, old)
	}
}
