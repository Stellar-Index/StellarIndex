// Copyright (c) 2026 Stellar Index contributors.
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

// TestStorage_LatestOracleUpdatesForAssets_KeepsBothLiveQuotes (Q095).
//
// Redstone publishes EUROC as two independent live feeds from the same
// source: EUROC/EUR and EUROC/USD. Before this fix,
// LatestOracleUpdatesForAssets's `DISTINCT ON (source)` collapsed both
// into one row — whichever quote had the higher (ts, ledger) silently
// won, discarding the other. This proves both survive, and that
// `?quote=`-shaped filtering (done by the caller) can recover exactly
// one.
func TestStorage_LatestOracleUpdatesForAssets_KeepsBothLiveQuotes(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	euroc, err := c.NewCryptoAsset("EUROC")
	if err != nil {
		t.Fatalf("NewCryptoAsset(EUROC): %v", err)
	}
	eur, _ := c.NewFiatAsset("EUR")
	usd, _ := c.NewFiatAsset("USD")

	price, _ := new(big.Int).SetString("100030000", 10)    // 1.0003 EUR
	priceUSD, _ := new(big.Int).SetString("113980000", 10) // 1.1398 USD
	ts := time.Now().UTC().Truncate(time.Second)

	seeds := []c.OracleUpdate{
		{
			Source: "redstone", Ledger: 60_000_001,
			TxHash:  "3333333333333333333333333333333333333333333333333333333333333333",
			OpIndex: 0, Timestamp: ts.Add(-time.Minute), // older
			Asset: euroc, Quote: eur,
			Price: c.NewAmount(price), Decimals: 8,
		},
		{
			Source: "redstone", Ledger: 60_000_002,
			TxHash:  "4444444444444444444444444444444444444444444444444444444444444444",
			OpIndex: 0, Timestamp: ts, // newer
			Asset: euroc, Quote: usd,
			Price: c.NewAmount(priceUSD), Decimals: 8,
		},
	}
	for _, u := range seeds {
		if err := store.InsertOracleUpdate(ctx, u); err != nil {
			t.Fatalf("InsertOracleUpdate(%s): %v", u.Asset, err)
		}
	}

	got, err := store.LatestOracleUpdatesForAssets(ctx, []c.Asset{euroc}, "redstone")
	if err != nil {
		t.Fatalf("LatestOracleUpdatesForAssets: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("LatestOracleUpdatesForAssets(EUROC, redstone) returned %d row(s), want 2 (one per live quote): %+v", len(got), got)
	}
	byQuote := map[string]c.OracleUpdate{}
	for _, u := range got {
		byQuote[u.Quote.String()] = u
	}
	eurRow, ok := byQuote[eur.String()]
	if !ok {
		t.Fatalf("EUROC/EUR row missing from %+v", got)
	}
	usdRow, ok := byQuote[usd.String()]
	if !ok {
		t.Fatalf("EUROC/USD row missing from %+v", got)
	}
	if eurRow.Ledger != 60_000_001 {
		t.Errorf("EUROC/EUR ledger = %d, want 60000001 (the older, EUR-quoted observation)", eurRow.Ledger)
	}
	if usdRow.Ledger != 60_000_002 {
		t.Errorf("EUROC/USD ledger = %d, want 60000002 (the newer, USD-quoted observation)", usdRow.Ledger)
	}
}
