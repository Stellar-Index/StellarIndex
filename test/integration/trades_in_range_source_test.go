//go:build integration

package integration_test

import (
	"context"
	"testing"
	"time"

	c "github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// TestTradesInRangeAfterFromSource executes the single-source page read:
// only the named source's rows come back, the full-PK cursor still pages
// through them, and an empty source is the unfiltered read.
func TestTradesInRangeAfterFromSource(t *testing.T) {
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
	pair, _ := c.NewPair(c.NativeAsset(), usdc)

	t0 := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Second)
	for i, src := range []string{"sdex", "soroswap", "sdex", "soroswap", "sdex"} {
		tr := mkIntegrationTrade(src, i+1, t0.Add(time.Duration(i)*time.Minute), pair, 1_000_000_000, 12_000_000)
		if err := store.InsertTrade(ctx, tr); err != nil {
			t.Fatalf("InsertTrade: %v", err)
		}
	}
	from, to := t0.Add(-time.Minute), t0.Add(time.Hour)

	got, err := store.TradesInRangeAfterFromSource(ctx, pair, "sdex", from, to, time.Time{}, 0, "", "", 0, 100)
	if err != nil {
		t.Fatalf("filtered read: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("source=sdex returned %d rows, want 3", len(got))
	}
	for _, tr := range got {
		if tr.Source != "sdex" {
			t.Errorf("source=sdex returned a %q row", tr.Source)
		}
	}

	// Cursor pagination over the filtered stream.
	first, err := store.TradesInRangeAfterFromSource(ctx, pair, "sdex", from, to, time.Time{}, 0, "", "", 0, 1)
	if err != nil || len(first) != 1 {
		t.Fatalf("first page: rows=%d err=%v", len(first), err)
	}
	f := first[0]
	next, err := store.TradesInRangeAfterFromSource(ctx, pair, "sdex", from, to,
		f.Timestamp, f.Ledger, f.TxHash, f.Source, f.OpIndex, 100)
	if err != nil {
		t.Fatalf("second page: %v", err)
	}
	if len(next) != 2 || next[0].Ledger != f.Ledger+2 {
		t.Fatalf("second page = %d rows (first ledger %d), want the remaining 2 starting two ledgers after the first page", len(next), firstLedger(next))
	}

	none, err := store.TradesInRangeAfterFromSource(ctx, pair, "no-such-source", from, to, time.Time{}, 0, "", "", 0, 100)
	if err != nil || len(none) != 0 {
		t.Fatalf("unknown source: rows=%d err=%v, want empty", len(none), err)
	}

	all, err := store.TradesInRangeAfterFromSource(ctx, pair, "", from, to, time.Time{}, 0, "", "", 0, 100)
	if err != nil || len(all) != 5 {
		t.Fatalf("empty source: rows=%d err=%v, want the unfiltered 5", len(all), err)
	}
}

func firstLedger(ts []c.Trade) uint32 {
	if len(ts) == 0 {
		return 0
	}
	return ts[0].Ledger
}
