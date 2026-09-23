//go:build integration

package integration_test

import (
	"context"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// TestInsertPriceSourceContributions_BatchIsAtomic pins that one
// bucket's contribution rows commit together or not at all. The rows'
// weights only mean anything as a set (they sum to 1); a per-row
// autocommit left the prefix before a failing row persisted, so a
// reader would see a bucket whose weights sum to less than 1.
func TestInsertPriceSourceContributions_BatchIsAtomic(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	bucket := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	countRows := func(asset string) int {
		t.Helper()
		var n int
		if err := store.DB().QueryRowContext(ctx,
			`SELECT count(*) FROM price_source_contributions WHERE asset_id = $1 AND bucket = $2`,
			asset, bucket,
		).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", asset, err)
		}
		return n
	}

	// Second row violates CHECK (trade_count >= 0), after the first
	// row has already been sent.
	failing := []timescale.PriceSourceContribution{
		{AssetID: "native", QuoteID: "fiat:USD", Bucket: bucket, Source: "sdex", Weight: 0.6, TradeCount: 3},
		{AssetID: "native", QuoteID: "fiat:USD", Bucket: bucket, Source: "binance", Weight: 0.4, TradeCount: -1},
	}
	if err := store.InsertPriceSourceContributions(ctx, failing); err == nil {
		t.Fatal("InsertPriceSourceContributions accepted a trade_count of -1")
	}
	if n := countRows("native"); n != 0 {
		t.Fatalf("failed batch left %d committed row(s); want 0 (partial bucket)", n)
	}

	vol := 1234.5
	ok := []timescale.PriceSourceContribution{
		{AssetID: "crypto:BTC", QuoteID: "fiat:USD", Bucket: bucket, Source: "sdex", Weight: 0.25, VolumeUSD: &vol, TradeCount: 2},
		{AssetID: "crypto:BTC", QuoteID: "fiat:USD", Bucket: bucket, Source: "kraken", Weight: 0.75, TradeCount: 9},
	}
	if err := store.InsertPriceSourceContributions(ctx, ok); err != nil {
		t.Fatalf("InsertPriceSourceContributions: %v", err)
	}
	var (
		n      int
		sumW   string
		sumVol string
	)
	if err := store.DB().QueryRowContext(ctx,
		`SELECT count(*), sum(weight)::text, sum(volume_usd)::text
		   FROM price_source_contributions WHERE asset_id = 'crypto:BTC' AND bucket = $1`, bucket,
	).Scan(&n, &sumW, &sumVol); err != nil {
		t.Fatalf("read committed batch: %v", err)
	}
	if n != 2 || sumW != "1.00" || sumVol != "1234.5" {
		t.Errorf("committed batch = %d rows, Σweight %s, Σvolume_usd %s; want 2, 1.00, 1234.5", n, sumW, sumVol)
	}
}
