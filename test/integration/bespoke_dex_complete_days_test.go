//go:build integration

package integration_test

import (
	"context"
	"strings"
	"testing"
	"time"

	c "github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// TestBespokeDEX7dCoversSevenCompleteDays proves every "(7d)" figure in the
// DEX block covers the same 7 complete UTC days (D-7..D-1) against a real
// TimescaleDB, CAGG-backed and raw-trade-backed alike.
//
// Fixture: one priced soroswap trade at 12:00 UTC on each of D-8..D-1 plus
// one just after 00:00 today, with USD volumes that are distinct powers of
// two so every window mis-cut produces a different sum:
//
//	D-8 256 (outside)   D-7..D-1  1,2,4,8,16,32,64   D (today) 128 (partial)
//
// Want: USD volume 127.00, 7 trades, avg 127/7 = 18.14, top trade 64.00.
// A rolling `bucket > now() - '7 days'` cut drops the D-7 bucket (126),
// and a rolling `ts > now() - '7 days'` cut on raw trades takes today's 128.
func TestBespokeDEX7dCoversSevenCompleteDays(t *testing.T) {
	now := time.Now().UTC()
	today := now.Truncate(24 * time.Hour)
	if now.Sub(today) < 10*time.Minute || today.Add(24*time.Hour).Sub(now) < 10*time.Minute {
		t.Skip("within 10 minutes of a UTC day boundary: the Go and DB clocks may straddle it")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	seedCompleteDaysFixture(t, ctx, store, today)

	blk, err := store.BuildProtocolBespoke(ctx, "soroswap", "dex", 7)
	if err != nil {
		t.Fatalf("BuildProtocolBespoke: %v", err)
	}
	if blk == nil {
		t.Fatal("BuildProtocolBespoke returned no block for a source with trades in the window")
	}

	kpis := map[string]string{}
	for _, k := range blk.KPIs {
		kpis[k.Label] = k.Value
	}
	for label, want := range map[string]string{
		"USD volume (7d)":     "127.00",
		"Trades (7d)":         "7",
		"Avg trade size (7d)": "18.14",
	} {
		if got := kpis[label]; got != want {
			t.Errorf("%s = %q, want %q (the 7 complete UTC days D-7..D-1)", label, got, want)
		}
	}

	var largest *timescale.BespokeTable
	for i := range blk.Tables {
		if blk.Tables[i].Title == "Largest trades" {
			largest = &blk.Tables[i]
		}
	}
	if largest == nil || len(largest.Rows) != 7 || largest.Rows[0][3] != "64.00" {
		t.Errorf("largest trades must list the 7 window trades topped by 64.00 (not today's 128), got %+v", largest)
	}

	var vol *timescale.BespokeSeries
	for i := range blk.Series {
		if blk.Series[i].Name == "USD volume" {
			vol = &blk.Series[i]
		}
	}
	if vol == nil || len(vol.Points) != 7 {
		t.Fatalf("daily USD volume series must carry one point per complete day D-7..D-1, got %+v", vol)
	}
	if first, want := vol.Points[0].Date, today.AddDate(0, 0, -7).Format("2006-01-02"); first != want {
		t.Errorf("daily series starts %s, want %s", first, want)
	}

	if !strings.Contains(strings.Join(blk.Notes, "\n"), "7 complete UTC days before today") {
		t.Errorf("7d block must disclose its complete-day window, notes: %q", blk.Notes)
	}
}

// seedCompleteDaysFixture inserts the D-8..D trades described above and
// materializes the daily pair CAGG over them.
func seedCompleteDaysFixture(t *testing.T, ctx context.Context, store *timescale.Store, today time.Time) {
	t.Helper()
	spec, err := timescale.NewUSDVolumeQuoteSpec(
		[]string{"USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"}, nil)
	if err != nil {
		t.Fatalf("spec: %v", err)
	}
	store.SetUSDVolumeQuoteSpec(spec)
	usdc, err := c.NewClassicAsset("USDC", "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN")
	if err != nil {
		t.Fatal(err)
	}
	sorobanContract, err := c.NewSorobanAsset("CAFJZQWSED6YAWZU3GWRTOCNPPCGBN32L7QV43XX5LZLFTK6JLN34DLN")
	if err != nil {
		t.Fatal(err)
	}
	pair, _ := c.NewPair(sorobanContract, usdc)

	const usd = 10_000_000 // USDC quote stroops per USD
	var trades []c.Trade
	for daysAgo := 8; daysAgo >= 1; daysAgo-- {
		ts := today.AddDate(0, 0, -daysAgo).Add(12 * time.Hour)
		vol := int64(256)
		if daysAgo <= 7 {
			vol = int64(1) << (7 - daysAgo) // D-7 → 1 … D-1 → 64
		}
		trades = append(trades, mkIntegrationTrade("soroswap", daysAgo, ts, pair, 1_000, vol*usd))
	}
	trades = append(trades, mkIntegrationTrade("soroswap", 20, today.Add(time.Minute), pair, 1_000, 128*usd))
	for _, tr := range trades {
		if err := store.InsertTrade(ctx, tr); err != nil {
			t.Fatalf("InsertTrade %d: %v", tr.Ledger, err)
		}
	}
	if _, err := store.DB().ExecContext(ctx,
		`CALL refresh_continuous_aggregate('dex_volume_by_pair_1d', NULL, NULL)`); err != nil {
		t.Fatalf("refresh dex_volume_by_pair_1d: %v", err)
	}
}
