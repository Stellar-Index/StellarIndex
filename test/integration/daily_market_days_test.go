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

// TestStoreDailyMarketDays_ReadsWholeLastDay executes DailyMarketDays
// against real TimescaleDB with `to` at the start of the last day, as
// the /v1/rwa/premium caller passes it. Trades at 00:30 and 15:30 on
// that day must BOTH count: two hours, a 15-hour span, and the whole
// day's VWAP. A trade on the following day must not.
//
// Day D-1 VWAP = (10 + 30) / (100 + 100) = 0.2; the 00:30 hour alone
// would read 0.1 with Hours = 1.
func TestStoreDailyMarketDays_ReadsWholeLastDay(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	const issuerAccount = "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
	usdc, err := c.NewClassicAsset("USDC", issuerAccount)
	if err != nil {
		t.Fatal(err)
	}
	ustry, err := c.NewClassicAsset("USTRY", issuerAccount)
	if err != nil {
		t.Fatal(err)
	}
	pair, err := c.NewPair(ustry, usdc)
	if err != nil {
		t.Fatal(err)
	}

	lastDay := time.Date(2026, 5, 10, 0, 0, 0, 0, time.UTC)
	for i, tr := range []c.Trade{
		mkAPITrade(1, lastDay.Add(30*time.Minute), pair, 1_000_000_000, 100_000_000),              // 100 for 10 → 0.10
		mkAPITrade(2, lastDay.Add(15*time.Hour+30*time.Minute), pair, 1_000_000_000, 300_000_000), // 100 for 30 → 0.30
		mkAPITrade(3, lastDay.Add(24*time.Hour+2*time.Hour), pair, 1_000_000_000, 1_000_000_000),  // next day: must not be read
		mkAPITrade(4, lastDay.Add(-24*time.Hour+3*time.Hour), pair, 1_000_000_000, 500_000_000),   // day before: 0.50
	} {
		if err := store.InsertTrade(ctx, tr); err != nil {
			t.Fatalf("InsertTrade[%d]: %v", i, err)
		}
	}
	if _, err := store.DB().ExecContext(ctx,
		`CALL refresh_continuous_aggregate('prices_1h', NULL, NULL)`); err != nil {
		t.Fatalf("refresh prices_1h: %v", err)
	}

	days, err := store.DailyMarketDays(ctx,
		[]c.Asset{ustry}, []c.Asset{usdc}, lastDay.Add(-24*time.Hour), lastDay)
	if err != nil {
		t.Fatalf("DailyMarketDays: %v", err)
	}
	if len(days) != 2 {
		t.Fatalf("days = %d, want 2 (the day before and the whole last day, not the day after): %+v", len(days), days)
	}
	if !days[0].Day.Equal(lastDay.Add(-24 * time.Hour)) {
		t.Errorf("first day = %s, want %s", days[0].Day, lastDay.Add(-24*time.Hour))
	}
	d := days[1]
	if !d.Day.Equal(lastDay) {
		t.Fatalf("last day = %s, want %s", d.Day, lastDay)
	}
	if d.Hours != 2 {
		t.Errorf("last day Hours = %d, want 2 (00:30 and 15:30 hours)", d.Hours)
	}
	if d.SpanSeconds != 15*3600 {
		t.Errorf("last day SpanSeconds = %d, want %d", d.SpanSeconds, 15*3600)
	}
	if d.Trades != 2 {
		t.Errorf("last day Trades = %d, want 2", d.Trades)
	}
	got, ok := new(big.Rat).SetString(d.VWAP)
	if !ok || got.Cmp(big.NewRat(1, 5)) != 0 {
		t.Errorf("last day VWAP = %s, want 0.2 (the whole day, not its 00:00 hour)", d.VWAP)
	}
}

// TestStoreDailyMarketDays_FoldsSACSpellingOfBase pins CA2-A07-correct-2:
// a token with a configured SAC wrapper trades under both spellings, and
// the live substance gate counts both. The historical reader must fold
// the SAC-form rows onto the requested classic asset, and measure hours
// and span over the UNION: the 00:00 hour traded under both spellings
// counts once.
//
// Classic: 00:10 (100 for 10), 05:30 (100 for 20). SAC: 00:40 (100 for
// 30), 20:30 (100 for 40). Union: hours {00,05,20} = 3, span 20h, 4
// trades, VWAP = 100/400 = 0.25. Classic alone reads 2 hours, 5h, 0.15.
func TestStoreDailyMarketDays_FoldsSACSpellingOfBase(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	const (
		issuerAccount   = "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
		sorobanContract = "CAUIKL3IYGMERDRUN6YSCLWVAKIFG5Q4YJHUKM4S4NJZQIA3BAS6OJPK"
	)
	reg, err := c.NewAliasRegistry(map[string]string{sorobanContract: "USTRY:" + issuerAccount})
	if err != nil {
		t.Fatalf("NewAliasRegistry: %v", err)
	}
	c.InstallAliasRegistry(reg)
	t.Cleanup(func() { c.InstallAliasRegistry(nil) })

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	usdc, err := c.NewClassicAsset("USDC", issuerAccount)
	if err != nil {
		t.Fatal(err)
	}
	ustry, err := c.NewClassicAsset("USTRY", issuerAccount)
	if err != nil {
		t.Fatal(err)
	}
	ustrySAC, err := c.NewSorobanAsset(sorobanContract)
	if err != nil {
		t.Fatal(err)
	}
	classicPair, err := c.NewPair(ustry, usdc)
	if err != nil {
		t.Fatal(err)
	}
	sacPair, err := c.NewPair(ustrySAC, usdc)
	if err != nil {
		t.Fatal(err)
	}

	day := time.Date(2026, 5, 10, 0, 0, 0, 0, time.UTC)
	for i, tr := range []c.Trade{
		mkAPITrade(11, day.Add(10*time.Minute), classicPair, 1_000_000_000, 100_000_000),
		mkAPITrade(12, day.Add(5*time.Hour+30*time.Minute), classicPair, 1_000_000_000, 200_000_000),
		mkAPITrade(13, day.Add(40*time.Minute), sacPair, 1_000_000_000, 300_000_000),
		mkAPITrade(14, day.Add(20*time.Hour+30*time.Minute), sacPair, 1_000_000_000, 400_000_000),
	} {
		if err := store.InsertTrade(ctx, tr); err != nil {
			t.Fatalf("InsertTrade[%d]: %v", i, err)
		}
	}
	if _, err := store.DB().ExecContext(ctx,
		`CALL refresh_continuous_aggregate('prices_1h', NULL, NULL)`); err != nil {
		t.Fatalf("refresh prices_1h: %v", err)
	}

	days, err := store.DailyMarketDays(ctx, []c.Asset{ustry}, []c.Asset{usdc}, day, day)
	if err != nil {
		t.Fatalf("DailyMarketDays: %v", err)
	}
	if len(days) != 1 {
		t.Fatalf("days = %d, want 1 row folded onto the classic member: %+v", len(days), days)
	}
	d := days[0]
	if d.AssetID != ustry.String() {
		t.Errorf("AssetID = %q, want the requested classic %q", d.AssetID, ustry.String())
	}
	if d.Hours != 3 {
		t.Errorf("Hours = %d, want 3 (00, 05, 20; the 00 hour traded on both spellings counts once)", d.Hours)
	}
	if d.SpanSeconds != 20*3600 {
		t.Errorf("SpanSeconds = %d, want %d (min/max over the union)", d.SpanSeconds, 20*3600)
	}
	if d.Trades != 4 {
		t.Errorf("Trades = %d, want 4", d.Trades)
	}
	got, ok := new(big.Rat).SetString(d.VWAP)
	if !ok || got.Cmp(big.NewRat(1, 4)) != 0 {
		t.Errorf("VWAP = %s, want 0.25 (sum(quote)/sum(base) over both spellings)", d.VWAP)
	}
}
