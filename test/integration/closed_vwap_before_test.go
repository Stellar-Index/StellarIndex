//go:build integration

package integration_test

import (
	"context"
	"strconv"
	"testing"
	"time"

	c "github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// TestClosedVWAP1mCombinedBefore_AnchorsAtTheInstant executes the
// point-in-time guard's baseline read against the real schema: it must
// return the buckets immediately BEFORE the anchor (never the anchor's own
// bucket, never newer ones), newest-first, with a flipped-only bucket
// combined into the requested orientation.
func TestClosedVWAP1mCombinedBefore_AnchorsAtTheInstant(t *testing.T) {
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
	xlmUSDC, _ := c.NewPair(c.NativeAsset(), usdc)
	usdcXLM, _ := c.NewPair(usdc, c.NativeAsset())

	// Six one-minute buckets three hours back. Bucket 2 is stored ONLY in
	// the flipped direction (2.0 XLM/USDC = 0.5 USDC/XLM once inverted).
	t0 := time.Now().UTC().Add(-3 * time.Hour).Truncate(time.Minute)
	bucket := func(i int) time.Time { return t0.Add(time.Duration(i) * time.Minute) }
	for i := 0; i < 6; i++ {
		tr := mkAPITrade(i+1, bucket(i).Add(10*time.Second), xlmUSDC, 1_000_000, int64(100_000*(i+1)))
		if i == 2 {
			tr = mkAPITrade(i+1, bucket(i).Add(10*time.Second), usdcXLM, 1_000_000, 2_000_000)
		}
		if err := store.InsertTrade(ctx, tr); err != nil {
			t.Fatalf("InsertTrade: %v", err)
		}
	}
	if _, err := store.DB().ExecContext(ctx,
		`CALL refresh_continuous_aggregate('prices_1m', NULL, NULL)`); err != nil {
		t.Fatalf("refresh prices_1m: %v", err)
	}

	rows, err := store.ClosedVWAP1mCombinedBefore(ctx, xlmUSDC, bucket(4), 3)
	if err != nil {
		t.Fatalf("ClosedVWAP1mCombinedBefore: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("got %d rows, want 3 (buckets 3, 2, 1): %+v", len(rows), rows)
	}
	for i, want := range []int{3, 2, 1} {
		if !rows[i].Bucket.Equal(bucket(want)) {
			t.Errorf("rows[%d].Bucket = %v, want bucket %d (%v) — the read must start strictly before the anchor",
				i, rows[i].Bucket, want, bucket(want))
		}
	}
	if got, err := strconv.ParseFloat(rows[1].VWAP, 64); err != nil || got < 0.49 || got > 0.51 {
		t.Errorf("flipped-only bucket VWAP = %q, want ~0.5 (inverted into the requested orientation)", rows[1].VWAP)
	}

	none, err := store.ClosedVWAP1mCombinedBefore(ctx, xlmUSDC, bucket(0), 3)
	if err != nil {
		t.Fatalf("ClosedVWAP1mCombinedBefore before history: %v", err)
	}
	if len(none) != 0 {
		t.Errorf("anchor at the first bucket returned %d rows, want none: %+v", len(none), none)
	}
}
