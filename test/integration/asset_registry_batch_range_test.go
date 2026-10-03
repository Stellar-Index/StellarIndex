//go:build integration

package integration_test

import (
	"context"
	"math/big"
	"testing"
	"time"

	c "github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// Two batches inside the registry dedupe TTL, the second with a lower first
// ledger: first_* must take the overall minimum, last_* the overall maximum.
func TestAssetRegistry_BatchRangeKeepsBothEnds(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	usdc, err := c.NewClassicAsset("USDC", "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN")
	if err != nil {
		t.Fatalf("NewClassicAsset: %v", err)
	}
	pair, err := c.NewPair(c.NativeAsset(), usdc)
	if err != nil {
		t.Fatalf("NewPair: %v", err)
	}
	store.ResetAssetRegistryDedupeForTest()

	base := time.Now().UTC().Truncate(time.Second).Add(-time.Hour)
	mk := func(ledger uint32, tail string) c.Trade {
		return c.Trade{
			Source:      "test-range",
			Ledger:      ledger,
			TxHash:      "deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbe" + tail,
			Timestamp:   base.Add(time.Duration(ledger) * time.Second),
			Pair:        pair,
			BaseAmount:  c.NewAmount(big.NewInt(1_000_000_000)),
			QuoteAmount: c.NewAmount(big.NewInt(12_000_000)),
		}
	}
	if err := store.BatchInsertTrades(ctx, []c.Trade{mk(200, "a1"), mk(300, "a2")}); err != nil {
		t.Fatalf("batch 1: %v", err)
	}
	if err := store.BatchInsertTrades(ctx, []c.Trade{mk(100, "b1"), mk(150, "b2")}); err != nil {
		t.Fatalf("batch 2: %v", err)
	}

	var firstSeen, firstTrade, lastSeen, lastTrade uint32
	err = store.DB().QueryRowContext(ctx, `
		SELECT first_seen_ledger, first_trade_ledger, last_seen_ledger, last_trade_ledger
		  FROM classic_assets WHERE asset_id = $1`, usdc.String()).Scan(&firstSeen, &firstTrade, &lastSeen, &lastTrade)
	if err != nil {
		t.Fatalf("read classic_assets: %v", err)
	}
	if firstSeen != 100 || firstTrade != 100 {
		t.Errorf("first_seen/first_trade ledger = %d/%d, want 100/100", firstSeen, firstTrade)
	}
	if lastSeen != 300 || lastTrade != 300 {
		t.Errorf("last_seen/last_trade ledger = %d/%d, want 300/300", lastSeen, lastTrade)
	}
}
