//go:build integration

package integration_test

import (
	"context"
	"testing"
	"time"

	c "github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// TestSorobanVolume24hUSD_XLMAnchored proves #37 end-to-end: a
// pure-Soroban SEP-41 token whose liquidity is quoted in XLM gets a REAL
// trailing-24h USD volume from Store.SorobanVolume24hUSDForAsset, while
// the plain Store.Volume24hUSDForAsset (which only sees the insert-time
// usd_volume) reports just the USD-pegged leg.
//
// Fixture (all in one closed 1-minute bucket ~2h back):
//   - native/USDC  vwap 0.5   → the XLM→USD anchor (1 XLM = 0.5 USD)
//   - token/XLM    20 XLM quoted → XLM-quote leg  → 20 * 0.5 = 10 USD
//   - XLM/token    10 XLM based  → XLM-base leg   → 10 * 0.5 =  5 USD
//   - token/USDC    7 USDC       → USD-pegged leg → usd_volume = 7 USD
//
// SorobanVolume24hUSDForAsset(token) = 10 + 5 + 7 = 22
// Volume24hUSDForAsset(token)        =            7   (USD-pegged leg only)
func TestSorobanVolume24hUSD_XLMAnchored(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	// Recognise classic USDC as a USD peg so the token/USDC leg lands a
	// non-null usd_volume, which the anchored reader takes as-is.
	spec, err := timescale.NewUSDVolumeQuoteSpec(
		[]string{"USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"}, nil)
	if err != nil {
		t.Fatalf("spec: %v", err)
	}
	store.SetUSDVolumeQuoteSpec(spec)

	xlm := c.NativeAsset()
	usdc, err := c.NewClassicAsset("USDC", "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN")
	if err != nil {
		t.Fatal(err)
	}
	token, err := c.NewSorobanAsset("CAFJZQWSED6YAWZU3GWRTOCNPPCGBN32L7QV43XX5LZLFTK6JLN34DLN")
	if err != nil {
		t.Fatal(err)
	}

	xlmUSDC, _ := c.NewPair(xlm, usdc)   // anchor: vwap = 0.5
	tokenXLM, _ := c.NewPair(token, xlm) // XLM-quote leg
	xlmToken, _ := c.NewPair(xlm, token) // XLM-base leg
	tokenUSDC, _ := c.NewPair(token, usdc)

	ts := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Minute)

	trades := []c.Trade{
		// Anchor: 100 XLM (stroops) traded for 50 USDC (stroops) → vwap 0.5.
		mkIntegrationTrade("soroswap", 1, ts, xlmUSDC, 1_000_000_000, 500_000_000),
		// token/XLM: 20 XLM on the quote side (usd_volume NULL — XLM quote).
		mkIntegrationTrade("soroswap", 2, ts, tokenXLM, 500, 200_000_000),
		// XLM/token: 10 XLM on the base side (usd_volume NULL — XLM base).
		mkIntegrationTrade("soroswap", 3, ts, xlmToken, 100_000_000, 300),
		// token/USDC: 7 USDC quote → usd_volume = 70_000_000 / 1e7 = 7.
		mkIntegrationTrade("soroswap", 4, ts, tokenUSDC, 900, 70_000_000),
	}
	for _, tr := range trades {
		if err := store.InsertTrade(ctx, tr); err != nil {
			t.Fatalf("InsertTrade %d: %v", tr.Ledger, err)
		}
	}
	if _, err := store.DB().ExecContext(ctx,
		`CALL refresh_continuous_aggregate('prices_1m', NULL, NULL)`); err != nil {
		t.Fatalf("refresh prices_1m: %v", err)
	}

	// Plain reader: only the USD-pegged token/USDC leg contributes.
	plain, err := store.Volume24hUSDForAsset(ctx, token.String())
	if err != nil {
		t.Fatalf("Volume24hUSDForAsset: %v", err)
	}
	if got := mustFloat(t, plain); got < 6.99 || got > 7.01 {
		t.Errorf("plain Volume24hUSDForAsset = %s (%.4f), want ~7 (USD-pegged leg only)", plain, got)
	}

	// XLM-anchored reader: USD-pegged leg (7) + XLM legs (10 + 5) = 22.
	anchored, err := store.SorobanVolume24hUSDForAsset(ctx, token.String())
	if err != nil {
		t.Fatalf("SorobanVolume24hUSDForAsset: %v", err)
	}
	if got := mustFloat(t, anchored); got < 21.99 || got > 22.01 {
		t.Errorf("SorobanVolume24hUSDForAsset = %s (%.4f), want ~22 (7 pegged + 10 + 5 XLM-anchored)", anchored, got)
	}
}

// TestSorobanVolume24hUSD_EmptyReturnsZero — an asset with no trades in the
// window returns "0" (not an error, not null) — same contract as the plain
// reader, so the asset-detail path can present a definite figure.
func TestSorobanVolume24hUSD_EmptyReturnsZero(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	token, err := c.NewSorobanAsset("CAFJZQWSED6YAWZU3GWRTOCNPPCGBN32L7QV43XX5LZLFTK6JLN34DLN")
	if err != nil {
		t.Fatal(err)
	}
	got, err := store.SorobanVolume24hUSDForAsset(ctx, token.String())
	if err != nil {
		t.Fatalf("SorobanVolume24hUSDForAsset: %v", err)
	}
	if got != "0" {
		t.Errorf("empty asset volume = %q, want \"0\"", got)
	}
}

// TestSorobanVolume24hUSD_PartlyValuedBucket pins per-trade valuation: a
// prices_1m bucket where one trade carries an insert-time usd_volume and a
// same-minute trade on the same pair does not (its FX lookup failed at
// insert) must value the second trade through the XLM leg, not report the
// first trade's figure for the whole bucket.
//
// Fixture (one closed 1-minute bucket ~2h back, anchor 1 XLM = 0.5 USD):
//   - token/XLM 10 XLM quoted, usd_volume = 5 (valued at insert)
//   - token/XLM 20 XLM quoted, usd_volume NULL → 20 * 0.5 = 10
//
// SorobanVolume24hUSDForAsset(token) = 5 + 10 = 15
func TestSorobanVolume24hUSD_PartlyValuedBucket(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	xlm := c.NativeAsset()
	usdc, err := c.NewClassicAsset("USDC", "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN")
	if err != nil {
		t.Fatal(err)
	}
	token, err := c.NewSorobanAsset("CAFJZQWSED6YAWZU3GWRTOCNPPCGBN32L7QV43XX5LZLFTK6JLN34DLN")
	if err != nil {
		t.Fatal(err)
	}
	xlmUSDC, _ := c.NewPair(xlm, usdc)
	tokenXLM, _ := c.NewPair(token, xlm)

	ts := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Minute)
	valued := mkIntegrationTrade("aquarius", 2, ts, tokenXLM, 400, 100_000_000)
	for _, tr := range []c.Trade{
		mkIntegrationTrade("soroswap", 1, ts, xlmUSDC, 1_000_000_000, 500_000_000), // anchor vwap 0.5
		valued,
		mkIntegrationTrade("soroswap", 3, ts.Add(20*time.Second), tokenXLM, 800, 200_000_000),
	} {
		if err := store.InsertTrade(ctx, tr); err != nil {
			t.Fatalf("InsertTrade %d: %v", tr.Ledger, err)
		}
	}
	// No FX resolver is wired, so every row lands NULL; value the one row
	// the way a successful insert-time tier would have.
	res, err := store.DB().ExecContext(ctx,
		`UPDATE trades SET usd_volume = 5 WHERE source = $1 AND ledger = $2`, valued.Source, valued.Ledger)
	if err != nil {
		t.Fatalf("value one trade: %v", err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		t.Fatalf("valued %d rows, want 1", n)
	}
	if _, err := store.DB().ExecContext(ctx,
		`CALL refresh_continuous_aggregate('prices_1m', NULL, NULL)`); err != nil {
		t.Fatalf("refresh prices_1m: %v", err)
	}

	got, err := store.SorobanVolume24hUSDForAsset(ctx, token.String())
	if err != nil {
		t.Fatalf("SorobanVolume24hUSDForAsset: %v", err)
	}
	if f := mustFloat(t, got); f < 14.99 || f > 15.01 {
		t.Errorf("SorobanVolume24hUSDForAsset = %s (%.4f), want 15 (5 valued + 20 XLM * 0.5)", got, f)
	}
}
