//go:build integration

package integration_test

import (
	"context"
	"fmt"
	"math/big"
	"net/http/httptest"
	"testing"
	"time"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	c "github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// TestZeroLegAdmission drives an SDEX zero-quote fill through the real
// migrated schema and InsertTrade: the row is stored, /v1/history serves it
// with price null, prices_1m counts it without pricing it, and the
// /v1/price last-trade fallback (LatestTradesForPair) never returns it.
func TestZeroLegAdmission(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	usdc, err := c.NewClassicAsset("USDC", priceableIssuer)
	if err != nil {
		t.Fatalf("NewClassicAsset: %v", err)
	}
	pair, err := c.NewPair(c.NativeAsset(), usdc)
	if err != nil {
		t.Fatalf("NewPair: %v", err)
	}
	t0 := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Minute)
	mk := func(ledger uint32, off time.Duration, base, quote int64) c.Trade {
		return c.Trade{
			Source: "sdex", Ledger: ledger, TxHash: fmt.Sprintf("%064x", ledger),
			Timestamp: t0.Add(off), Pair: pair,
			BaseAmount:  c.NewAmount(big.NewInt(base)),
			QuoteAmount: c.NewAmount(big.NewInt(quote)),
		}
	}
	priced := mk(61_000_000, 5*time.Second, 1_000_000_000, 120_000_000) // 0.12
	zeroQuote := mk(61_000_001, 50*time.Second, 5_000_000_000, 0)       // newest
	for _, tr := range []c.Trade{priced, zeroQuote} {
		if err := store.InsertTrade(ctx, tr); err != nil {
			t.Fatalf("InsertTrade(ledger %d): %v", tr.Ledger, err)
		}
	}

	t.Run("stored", func(t *testing.T) {
		var quote string
		if err := store.DB().QueryRowContext(ctx,
			`SELECT quote_amount::text FROM trades WHERE ledger = $1 AND source = 'sdex'`,
			zeroQuote.Ledger).Scan(&quote); err != nil {
			t.Fatalf("zero-quote row not stored: %v", err)
		}
		if quote != "0" {
			t.Errorf("stored quote_amount = %s, want 0", quote)
		}
	})

	t.Run("history price null", func(t *testing.T) {
		srv := v1.New(v1.Options{History: apiHistoryAdapter{s: store}})
		ts := httptest.NewServer(srv.Handler())
		t.Cleanup(ts.Close)
		var env struct {
			Data []v1.TradeRow `json:"data"`
		}
		getJSON(t, ts.URL+"/v1/history?base=native&quote="+usdc.String()+
			"&from="+t0.Add(-time.Minute).Format(time.RFC3339)+
			"&to="+t0.Add(2*time.Minute).Format(time.RFC3339), &env)
		if len(env.Data) != 2 {
			t.Fatalf("history returned %d rows, want 2", len(env.Data))
		}
		for _, r := range env.Data {
			switch r.Ledger {
			case zeroQuote.Ledger:
				if r.Price != nil {
					t.Errorf("zero-quote row price = %q, want null", *r.Price)
				}
			case priced.Ledger:
				if r.Price == nil || *r.Price != "0.1200000000" {
					t.Errorf("priced row price = %v, want 0.1200000000", r.Price)
				}
			default:
				t.Errorf("unexpected ledger %d", r.Ledger)
			}
		}
	})

	t.Run("prices_1m counts but does not price", func(t *testing.T) {
		if _, err := store.DB().ExecContext(ctx,
			`CALL refresh_continuous_aggregate('prices_1m', $1::timestamptz, $2::timestamptz)`,
			t0.Add(-time.Hour), t0.Add(time.Hour)); err != nil {
			t.Fatalf("refresh prices_1m: %v", err)
		}
		got := readPriceableRow(t, ctx, store.DB(), "1m",
			ohlcDustPair{base: pair.Base.String(), quote: pair.Quote.String()}, t0)
		assertNumeric(t, "vwap", got.vwap, "0.12")
		assertNumeric(t, "last_price", got.last, "0.12")
		assertNumeric(t, "low_price", got.low, "0.12")
		if got.tradeCount != 2 {
			t.Errorf("trade_count = %d, want 2", got.tradeCount)
		}
	})

	t.Run("LatestTradesForPair skips the zero leg", func(t *testing.T) {
		got, err := store.LatestTradesForPair(ctx, pair, 1)
		if err != nil {
			t.Fatalf("LatestTradesForPair: %v", err)
		}
		if len(got) != 1 || got[0].Ledger != priced.Ledger {
			t.Fatalf("LatestTradesForPair = %+v, want only ledger %d (the newest priceable trade)", got, priced.Ledger)
		}
		snap, ok := v1.LastTradeToSnapshot(got[0], 7)
		if !ok || snap.Price != "0.1200000" {
			t.Errorf("LastTradeToSnapshot = (%q, %v), want (0.1200000, true)", snap.Price, ok)
		}
	})
}
