//go:build integration

package integration_test

import (
	"context"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// TestLatestFXQuotes_NewestRowPerTickerInWindow pins the read the forex
// worker seeds its held rates from on cold start: one row per ticker, the
// newest bucket at or after since, rate as exact NUMERIC text.
func TestLatestFXQuotes_NewestRowPerTickerInWindow(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	today := time.Now().UTC().Truncate(24 * time.Hour)
	yesterday := today.AddDate(0, 0, -1)
	if err := store.InsertFXQuoteBatch(ctx, []timescale.FXQuote{
		{Bucket: yesterday.AddDate(0, 0, -1), Ticker: "AED", RateUSD: 3.6, Source: "massive"},
		{Bucket: yesterday, Ticker: "AED", RateUSD: 3.6725, Source: "massive"},
		{Bucket: today, Ticker: "EUR", RateUSD: 0.92, Source: "ecb"},
		{Bucket: today.AddDate(0, 0, -10), Ticker: "CNY", RateUSD: 7.1, Source: "massive"},
	}); err != nil {
		t.Fatalf("InsertFXQuoteBatch: %v", err)
	}

	got, err := store.LatestFXQuotes(ctx, today.AddDate(0, 0, -7))
	if err != nil {
		t.Fatalf("LatestFXQuotes: %v", err)
	}
	byTicker := map[string]timescale.FXQuote{}
	for _, q := range got {
		if _, dup := byTicker[q.Ticker]; dup {
			t.Fatalf("ticker %s returned twice", q.Ticker)
		}
		byTicker[q.Ticker] = q
	}
	if len(byTicker) != 2 {
		t.Fatalf("got %d tickers %v, want AED and EUR (CNY is outside the window)", len(byTicker), got)
	}
	aed := byTicker["AED"]
	if !aed.Bucket.Equal(yesterday) || aed.RateUSDText != "3.6725" || aed.Source != "massive" {
		t.Errorf("AED = %+v, want bucket %v, rate 3.6725, source massive", aed, yesterday)
	}
	if eur := byTicker["EUR"]; !eur.Bucket.Equal(today) || eur.RateUSDText != "0.92" || eur.Source != "ecb" {
		t.Errorf("EUR = %+v, want bucket %v, rate 0.92, source ecb", eur, today)
	}
}
