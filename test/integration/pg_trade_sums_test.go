//go:build integration

package integration_test

import (
	"context"
	"testing"
	"time"

	c "github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// TradeSums1mInRange reads migration 0217's view: one row per (source, minute, stored direction), oriented to the asked pair,
// non-VWAP sources left out, with the view's watermark.
func TestTradeSums1mInRange_OrientsAndFilters(t *testing.T) {
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
		t.Fatal(err)
	}
	pair, err := c.NewPair(c.NativeAsset(), usdc)
	if err != nil {
		t.Fatal(err)
	}
	m0 := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)
	for i, tr := range []c.Trade{
		mkIntegrationTrade("sdex", 1, m0.Add(10*time.Second), pair, 1_000_000_000, 100_000_000),
		mkIntegrationTrade("sdex", 2, m0.Add(20*time.Second), pair, 1_000_000_000, 120_000_000),
		mkIntegrationTrade("sdex", 3, m0.Add(40*time.Second), pair.Flip(), 50_000_000, 400_000_000),
		mkIntegrationTrade("upshift", 4, m0.Add(30*time.Second), pair, 1_000_000_000, 999_000_000),
		mkIntegrationTrade("sdex", 5, m0.Add(65*time.Second), pair, 2_000_000_000, 220_000_000),
	} {
		if err := store.InsertTrade(ctx, tr); err != nil {
			t.Fatalf("InsertTrade %d: %v", i, err)
		}
	}
	if _, err := store.DB().ExecContext(ctx,
		`CALL refresh_continuous_aggregate('trade_sums_1m', $1::timestamptz, $2::timestamptz)`,
		m0, m0.Add(2*time.Minute)); err != nil {
		t.Fatalf("refresh trade_sums_1m: %v", err)
	}

	rows, watermark, err := store.TradeSums1mInRange(ctx, pair, m0, m0.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if watermark.Before(m0.Add(2 * time.Minute)) {
		t.Errorf("watermark %v, want >= %v after the refresh", watermark, m0.Add(2*time.Minute))
	}
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2 (sdex forward + sdex flipped; upshift and the 12:01 row excluded): %+v", len(rows), rows)
	}
	var fwd, rev *timescale.TradeMinuteSum
	for i := range rows {
		if rows[i].Source != "sdex" || !rows[i].Bucket.Equal(m0) {
			t.Fatalf("unexpected row %+v", rows[i])
		}
		if rows[i].Count == 2 {
			fwd = &rows[i]
		} else {
			rev = &rows[i]
		}
	}
	if fwd == nil || rev == nil {
		t.Fatalf("want one 2-trade forward row and one 1-trade flipped row: %+v", rows)
	}
	if fwd.BaseSum.String() != "2000000000" || fwd.QuoteSum.String() != "220000000" {
		t.Errorf("forward sums %s/%s, want 2000000000/220000000", fwd.BaseSum, fwd.QuoteSum)
	}
	if fwd.MinPrice.RatString() != "1/10" || fwd.MaxPrice.RatString() != "3/25" {
		t.Errorf("forward price bounds %s..%s, want 1/10..3/25", fwd.MinPrice.RatString(), fwd.MaxPrice.RatString())
	}
	if rev.BaseSum.String() != "400000000" || rev.QuoteSum.String() != "50000000" {
		t.Errorf("flipped row oriented %s/%s, want 400000000/50000000", rev.BaseSum, rev.QuoteSum)
	}
	if rev.MinPrice.RatString() != "1/8" || rev.MaxPrice.RatString() != "1/8" {
		t.Errorf("flipped price bounds %s..%s, want 1/8", rev.MinPrice.RatString(), rev.MaxPrice.RatString())
	}

	// A late insert below the watermark sits in the invalidation log until the next refresh; its minute must not read as materialised.
	if err := store.InsertTrade(ctx, mkIntegrationTrade("sdex", 6, m0.Add(50*time.Second), pair, 1_000_000_000, 110_000_000)); err != nil {
		t.Fatalf("late InsertTrade: %v", err)
	}
	if _, watermark, err = store.TradeSums1mInRange(ctx, pair, m0, m0.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if watermark.After(m0) {
		t.Errorf("watermark %v after a late insert at %v, want <= %v until the next refresh", watermark, m0.Add(50*time.Second), m0)
	}
	if _, err := store.DB().ExecContext(ctx,
		`CALL refresh_continuous_aggregate('trade_sums_1m', $1::timestamptz, $2::timestamptz)`,
		m0, m0.Add(2*time.Minute)); err != nil {
		t.Fatalf("second refresh trade_sums_1m: %v", err)
	}
	if rows, watermark, err = store.TradeSums1mInRange(ctx, pair, m0, m0.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if watermark.Before(m0.Add(2 * time.Minute)) {
		t.Errorf("watermark %v after the second refresh, want >= %v", watermark, m0.Add(2*time.Minute))
	}
	var forwardCount int64
	for _, r := range rows {
		if r.Count != 1 {
			forwardCount += r.Count
		}
	}
	if forwardCount != 3 {
		t.Errorf("forward minute count %d after the refresh, want 3 (the late trade included): %+v", forwardCount, rows)
	}
}
