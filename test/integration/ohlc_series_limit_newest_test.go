//go:build integration

package integration_test

import (
	"context"
	"testing"
	"time"

	c "github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// TestOHLCSeries_LimitKeepsNewestBucketsInWideWindow pins RLT-453: an
// explicit window wider than `limit` intervals must serve the NEWEST
// `limit` buckets, not the oldest. The previous query ended `ORDER BY
// bucket ASC` and then applied `LIMIT`, so a wide window silently
// returned a stale slice starting at `from` and never reaching `to` —
// exactly backwards from what a chart client sizing `limit` down for a
// wide window expects. Runs against a real TimescaleDB because the
// defect is in the SQL's ORDER BY/LIMIT interaction, not reproducible
// against a scripted driver that ignores the query text.
func TestOHLCSeries_LimitKeepsNewestBucketsInWideWindow(t *testing.T) {
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

	// Five distinct, closed 1-minute buckets, each with exactly one
	// trade so open == high == low == close == that bucket's price —
	// the buckets are trivially distinguishable by value. t0 sits 20
	// minutes back so every bucket in [t0, t0+5m) is closed under the
	// ADR-0015 `bucket + interval <= now()` guard.
	t0 := time.Now().UTC().Add(-20 * time.Minute).Truncate(time.Minute)
	const nBuckets = 5
	for i := 0; i < nBuckets; i++ {
		price := int64(i + 1) // 1, 2, 3, 4, 5
		if err := store.InsertTrade(ctx, mkAPITrade(
			i+1, t0.Add(time.Duration(i)*time.Minute), pair,
			1_000_000, price*1_000_000,
		)); err != nil {
			t.Fatalf("InsertTrade %d: %v", i, err)
		}
	}
	if _, err := store.DB().ExecContext(ctx,
		`CALL refresh_continuous_aggregate('prices_1m', NULL, NULL)`); err != nil {
		t.Fatalf("refresh prices_1m: %v", err)
	}

	const limit = 2
	bars, err := store.OHLCSeries(ctx, pair, timescale.Granularity1m,
		t0, t0.Add(nBuckets*time.Minute), limit)
	if err != nil {
		t.Fatalf("OHLCSeries: %v", err)
	}
	if len(bars) != limit {
		t.Fatalf("len(bars) = %d, want %d", len(bars), limit)
	}

	// Ascending order is the documented contract regardless of which
	// end got cut.
	if !bars[0].Bucket.Before(bars[1].Bucket) {
		t.Fatalf("bars not ascending: %v then %v", bars[0].Bucket, bars[1].Bucket)
	}

	wantBuckets := []time.Time{t0.Add(3 * time.Minute), t0.Add(4 * time.Minute)}
	wantOpen := []float64{4, 5}
	for i, want := range wantBuckets {
		if !bars[i].Bucket.Equal(want) {
			t.Errorf("bars[%d].Bucket = %v, want %v (the NEWEST %d buckets) — "+
				"got the OLDEST %d instead, the RLT-453 regression",
				i, bars[i].Bucket, want, limit, limit)
		}
		if got := mustFloat(t, bars[i].Open); got != wantOpen[i] {
			t.Errorf("bars[%d].Open = %v, want %v", i, got, wantOpen[i])
		}
	}
}

// TestOHLCSeriesReBucketed_LimitKeepsNewestOutBucketsInWideWindow is
// the folded-interval sibling of
// TestOHLCSeries_LimitKeepsNewestBucketsInWideWindow: OHLCSeriesReBucketed
// carries the identical ORDER BY/LIMIT defect (RLT-453), one layer up —
// re-bucketing prices_1h into 4-hour out-buckets.
func TestOHLCSeriesReBucketed_LimitKeepsNewestOutBucketsInWideWindow(t *testing.T) {
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

	const fold = 4 * time.Hour
	// Anchored 30h back (comfortably more than the 20h the 5 out-buckets
	// below span) so every out-bucket closes under the ADR-0015 guard.
	t0 := timeBucket(fold, time.Now().UTC().Add(-30*time.Hour))

	const nBuckets = 5
	for i := 0; i < nBuckets; i++ {
		price := int64(i + 1) // 1, 2, 3, 4, 5
		if err := store.InsertTrade(ctx, mkAPITrade(
			i+1, t0.Add(time.Duration(i)*fold), pair,
			1_000_000, price*1_000_000,
		)); err != nil {
			t.Fatalf("InsertTrade %d: %v", i, err)
		}
	}
	if _, err := store.DB().ExecContext(ctx,
		`CALL refresh_continuous_aggregate('prices_1h', NULL, NULL)`); err != nil {
		t.Fatalf("refresh prices_1h: %v", err)
	}

	const limit = 2
	bars, err := store.OHLCSeriesReBucketed(ctx, pair, timescale.Granularity1h, "4 hours",
		t0, t0.Add(nBuckets*fold), limit)
	if err != nil {
		t.Fatalf("OHLCSeriesReBucketed: %v", err)
	}
	if len(bars) != limit {
		t.Fatalf("len(bars) = %d, want %d", len(bars), limit)
	}
	if !bars[0].Bucket.Before(bars[1].Bucket) {
		t.Fatalf("bars not ascending: %v then %v", bars[0].Bucket, bars[1].Bucket)
	}

	wantBuckets := []time.Time{t0.Add(3 * fold), t0.Add(4 * fold)}
	wantOpen := []float64{4, 5}
	for i, want := range wantBuckets {
		if !bars[i].Bucket.Equal(want) {
			t.Errorf("bars[%d].Bucket = %v, want %v (the NEWEST %d out-buckets) — "+
				"got the OLDEST %d instead, the RLT-453 regression",
				i, bars[i].Bucket, want, limit, limit)
		}
		if got := mustFloat(t, bars[i].Open); got != wantOpen[i] {
			t.Errorf("bars[%d].Open = %v, want %v", i, got, wantOpen[i])
		}
	}
}
