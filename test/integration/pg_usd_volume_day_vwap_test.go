// Copyright 2026 Stellar Index contributors
// SPDX-License-Identifier: Apache-2.0

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

// The verifier runs before prices_1d holds yesterday, so DayCloseVWAPXLMUSD rebuilds the day from prices_1h;
// the rebuilt value must equal prices_1d's own vwap, and a day without its 23:00 hour must not pass as whole.
func TestDayCloseVWAPXLMUSD_MatchesPrices1dFromHourly(t *testing.T) {
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
	usd, _ := c.NewFiatAsset("USD")
	xlmUSD, _ := c.NewPair(xlm, usd)

	whole := time.Now().UTC().Truncate(24*time.Hour).AddDate(0, 0, -3)
	partial := whole.AddDate(0, 0, 1)
	seed := []c.Trade{
		mkIntegrationTrade("binance", 1, whole.Add(10*time.Minute), xlmUSD, 100_000_000, 18_000_000),
		mkIntegrationTrade("binance", 2, whole.Add(5*time.Hour), xlmUSD, 300_000_000, 57_000_000),
		mkIntegrationTrade("binance", 3, whole.Add(23*time.Hour+30*time.Minute), xlmUSD, 200_000_000, 40_000_000),
		mkIntegrationTrade("binance", 4, partial.Add(time.Hour), xlmUSD, 100_000_000, 20_000_000),
	}
	for _, tr := range seed {
		if err := store.InsertTrade(ctx, tr); err != nil {
			t.Fatalf("InsertTrade: %v", err)
		}
	}
	refresh := func(views ...string) {
		t.Helper()
		for _, v := range views {
			if _, err := store.DB().ExecContext(ctx, `CALL refresh_continuous_aggregate('`+v+`', NULL, NULL)`); err != nil {
				t.Fatalf("refresh %s: %v", v, err)
			}
		}
	}
	// prices_1d stays empty here, as it is at the 05:12 UTC run.
	refresh("prices_1m", "prices_15m", "prices_1h")

	got, ok, err := store.DayCloseVWAPXLMUSD(ctx, whole)
	if err != nil || !ok {
		t.Fatalf("whole day: ok=%v err=%v", ok, err)
	}
	refresh("prices_4h", "prices_1d")
	var daily string
	if err := store.DB().QueryRowContext(ctx,
		`SELECT vwap::text FROM prices_1d WHERE base_asset = 'crypto:XLM' AND quote_asset = 'fiat:USD' AND bucket = $1`,
		whole).Scan(&daily); err != nil {
		t.Fatalf("read prices_1d: %v", err)
	}
	gotR, ok1 := new(big.Rat).SetString(got)
	dailyR, ok2 := new(big.Rat).SetString(daily)
	want := big.NewRat(115, 600) // (18+57+40) / (1+3+2); NUMERIC rounds the repeating tail
	off := new(big.Rat).Abs(new(big.Rat).Sub(gotR, want))
	if !ok1 || !ok2 || gotR.Cmp(dailyR) != 0 || off.Cmp(big.NewRat(1, 1e15)) > 0 {
		t.Fatalf("hourly-built vwap %s, prices_1d %s, want %s", got, daily, want.FloatString(6))
	}

	if v, ok, err := store.DayCloseVWAPXLMUSD(ctx, partial); err != nil || ok {
		t.Fatalf("day without its 23:00 hour passed as whole: vwap=%q ok=%v err=%v", v, ok, err)
	}
}
