//go:build integration

package integration_test

import (
	"context"
	"errors"
	"math/big"
	"testing"
	"time"

	c "github.com/StellarIndex/stellar-index/internal/canonical"
	"github.com/StellarIndex/stellar-index/internal/sources/external"
	"github.com/StellarIndex/stellar-index/internal/storage/timescale"
)

// TestFXQuoteAtOrBefore proves the X2.5 forex-snap storage primitive
// returns the most recent FX-source observation at-or-before a cutoff.
// Drives the across-region determinism story: every region serving the
// same closed bucket queries the same hypertable and gets the same row.
func TestFXQuoteAtOrBefore(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	usd, err := c.NewFiatAsset("USD")
	if err != nil {
		t.Fatal(err)
	}
	eur, err := c.NewFiatAsset("EUR")
	if err != nil {
		t.Fatal(err)
	}
	pair, _ := c.NewPair(usd, eur)

	// Anchor in the past — deterministic windows regardless of clock.
	t0 := time.Now().UTC().Add(-3 * time.Hour).Truncate(time.Second)

	// FX sources publish at uniform 1e8 scale on each side per
	// CLAUDE.md — quote/base = price ratio with no scale adjustment.
	// 0.92 EUR per USD:    base=1e8,    quote=92_000_000
	// 0.93 EUR per USD:    base=1e8,    quote=93_000_000
	// 0.94 EUR per USD:    base=1e8,    quote=94_000_000
	trades := []c.Trade{
		mkIntegrationTrade("polygon-forex", 1, t0, pair, 100_000_000, 92_000_000),
		mkIntegrationTrade("exchangeratesapi", 2, t0.Add(15*time.Minute), pair, 100_000_000, 93_000_000),
		mkIntegrationTrade("polygon-forex", 3, t0.Add(30*time.Minute), pair, 100_000_000, 94_000_000),
	}
	for _, tr := range trades {
		if err := store.InsertTrade(ctx, tr); err != nil {
			t.Fatalf("InsertTrade: %v", err)
		}
	}

	fx := external.FXSources()
	if len(fx) < 2 {
		t.Fatalf("FXSources() returned %d, want at least 2", len(fx))
	}

	t.Run("hits second observation when cutoff between t1 and t2", func(t *testing.T) {
		cutoff := t0.Add(20 * time.Minute)
		price, obs, src, err := store.FXQuoteAtOrBefore(ctx, pair, cutoff, fx)
		if err != nil {
			t.Fatalf("FXQuoteAtOrBefore: %v", err)
		}
		if src != "exchangeratesapi" {
			t.Errorf("got source %q, want exchangeratesapi", src)
		}
		if !obs.Equal(t0.Add(15 * time.Minute)) {
			t.Errorf("got observed_at %v, want %v", obs, t0.Add(15*time.Minute))
		}
		want := new(big.Rat).SetFrac(big.NewInt(93_000_000), big.NewInt(100_000_000))
		if price.Cmp(want) != 0 {
			t.Errorf("got price %s, want %s", price.RatString(), want.RatString())
		}
	})

	t.Run("hits third observation at exact bucket-end timestamp", func(t *testing.T) {
		// Cutoff equals the 3rd trade's ts — `<=` semantics include it.
		cutoff := t0.Add(30 * time.Minute)
		_, obs, src, err := store.FXQuoteAtOrBefore(ctx, pair, cutoff, fx)
		if err != nil {
			t.Fatalf("FXQuoteAtOrBefore: %v", err)
		}
		if src != "polygon-forex" {
			t.Errorf("got source %q, want polygon-forex", src)
		}
		if !obs.Equal(t0.Add(30 * time.Minute)) {
			t.Errorf("got observed_at %v, want %v", obs, t0.Add(30*time.Minute))
		}
	})

	t.Run("hits first observation at exact first timestamp", func(t *testing.T) {
		_, _, src, err := store.FXQuoteAtOrBefore(ctx, pair, t0, fx)
		if err != nil {
			t.Fatalf("FXQuoteAtOrBefore: %v", err)
		}
		if src != "polygon-forex" {
			t.Errorf("got source %q, want polygon-forex", src)
		}
	})

	t.Run("returns ErrNoFXQuote when cutoff before first observation", func(t *testing.T) {
		_, _, _, err := store.FXQuoteAtOrBefore(ctx, pair, t0.Add(-1*time.Minute), fx)
		if !errors.Is(err, timescale.ErrNoFXQuote) {
			t.Fatalf("got err %v, want ErrNoFXQuote", err)
		}
	})

	t.Run("returns ErrNoFXQuote when fxSources is empty", func(t *testing.T) {
		_, _, _, err := store.FXQuoteAtOrBefore(ctx, pair, t0.Add(1*time.Hour), nil)
		if !errors.Is(err, timescale.ErrNoFXQuote) {
			t.Fatalf("got err %v, want ErrNoFXQuote", err)
		}
	})

	t.Run("source filter excludes non-FX trades", func(t *testing.T) {
		// Insert a same-pair trade from a non-FX source in the same
		// window. Querying with the FX-only filter must NOT return it.
		nonFX := mkIntegrationTrade("binance", 99, t0.Add(45*time.Minute), pair, 100_000_000, 200_000_000)
		if err := store.InsertTrade(ctx, nonFX); err != nil {
			t.Fatalf("InsertTrade non-FX: %v", err)
		}
		_, obs, src, err := store.FXQuoteAtOrBefore(ctx, pair, t0.Add(1*time.Hour), fx)
		if err != nil {
			t.Fatalf("FXQuoteAtOrBefore: %v", err)
		}
		if src == "binance" {
			t.Errorf("filter leaked non-FX source %q (observed_at=%v)", src, obs)
		}
		// Latest FX should still be the 30-min polygon-forex row.
		if !obs.Equal(t0.Add(30 * time.Minute)) {
			t.Errorf("got observed_at %v, want %v (latest FX row)", obs, t0.Add(30*time.Minute))
		}
	})

	// NOTE: every subtest above exercises the LEGACY trades fallback —
	// fx_quotes is empty in this test's database, so the fx_quotes-first
	// read (BACKLOG #42) misses and the connector-path trades rows win.
	// That IS the compatibility contract: re-enabled polygon-forex /
	// exchangeratesapi connectors keep working when the massive feed has
	// no rows. The fx_quotes-first behaviour is proven in
	// TestFXQuoteAtOrBeforeFXQuotesFirst below.

	t.Run("FXSources is deterministic and lex-ordered", func(t *testing.T) {
		got := external.FXSources()
		// massive (the forex worker's fx_quotes feed) was bridged into the
		// registry as a SubclassFX source (P0-7) — lex-sorted between the two.
		want := []string{"exchangeratesapi", "massive", "polygon-forex"}
		if len(got) != len(want) {
			t.Fatalf("FXSources len=%d, want %d (%v)", len(got), len(want), got)
		}
		for i, s := range want {
			if got[i] != s {
				t.Errorf("FXSources[%d]=%q, want %q", i, got[i], s)
			}
		}
	})
}

// TestFXQuoteAtOrBeforeFXQuotesFirst proves the unified FX read path
// (BACKLOG #42): when the active feed (`massive` → fx_quotes) has a row
// in the lookback, it wins over connector-path trades rows; when its
// newest row is older than the lookback, the snap falls back to the
// legacy trades path; and the fx_quotes rate converts to an EXACT
// *big.Rat (NUMERIC text → Rat, never a float — ADR-0003).
func TestFXQuoteAtOrBeforeFXQuotesFirst(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	usd, err := c.NewFiatAsset("USD")
	if err != nil {
		t.Fatal(err)
	}
	eur, err := c.NewFiatAsset("EUR")
	if err != nil {
		t.Fatal(err)
	}
	pair, _ := c.NewPair(usd, eur) // price = EUR per USD

	fx := external.FXSources()
	cutoff := time.Now().UTC().Truncate(time.Hour)
	day := cutoff.Truncate(24 * time.Hour)

	// Legacy connector-path row: 0.94 EUR per USD at cutoff-2h.
	// FX connector trades use the uniform 1e8 scale on each side.
	legacy := mkIntegrationTrade("polygon-forex", 11, cutoff.Add(-2*time.Hour), pair, 100_000_000, 94_000_000)
	if err := store.InsertTrade(ctx, legacy); err != nil {
		t.Fatalf("InsertTrade: %v", err)
	}

	t.Run("fx_quotes row wins over the trades row, exact scale", func(t *testing.T) {
		// Active-feed row: rate_usd(EUR) = 1.085 USD per EUR. float64
		// 1.085 round-trips through lib/pq as the shortest decimal
		// "1.085", so NUMERIC stores it exactly.
		if err := store.InsertFXQuoteBatch(ctx, []timescale.FXQuote{{
			Bucket: day, Ticker: "EUR", RateUSD: 1.085, InverseUSD: 1.0 / 1.085, Source: "massive",
		}}); err != nil {
			t.Fatalf("InsertFXQuoteBatch: %v", err)
		}

		price, obs, src, err := store.FXQuoteAtOrBefore(ctx, pair, cutoff, fx)
		if err != nil {
			t.Fatalf("FXQuoteAtOrBefore: %v", err)
		}
		if src != "massive" {
			t.Errorf("source = %q, want massive (fx_quotes must win over the polygon-forex trades row)", src)
		}
		if !obs.Equal(day) {
			t.Errorf("observedAt = %v, want the fx_quotes bucket %v", obs, day)
		}
		// USD/EUR price = 1 / rate_usd(EUR) = 200/217 — exact Rat
		// inversion, NOT the float-derived inverse_usd column.
		want := new(big.Rat).SetFrac(big.NewInt(200), big.NewInt(217))
		if price.Cmp(want) != 0 {
			t.Errorf("price = %s, want exactly %s", price.RatString(), want.RatString())
		}
	})

	t.Run("stale fx_quotes row falls back to the trades path", func(t *testing.T) {
		// A cutoff 10 days in the past puts the (today-bucketed)
		// fx_quotes row in the future relative to the query, and the
		// row planted below IS at-or-before the cutoff but 8 days old —
		// outside the 7-day snap lookback. Neither may serve: the
		// legacy trades read must. Anchor a dedicated trades row inside
		// the window.
		staleCutoff := cutoff.Add(-10 * 24 * time.Hour)
		if err := store.InsertFXQuoteBatch(ctx, []timescale.FXQuote{{
			Bucket: staleCutoff.Truncate(24 * time.Hour).Add(-8 * 24 * time.Hour),
			Ticker: "EUR", RateUSD: 2.0, InverseUSD: 0.5, Source: "massive",
		}}); err != nil {
			t.Fatalf("InsertFXQuoteBatch (stale row): %v", err)
		}
		old := mkIntegrationTrade("exchangeratesapi", 12, staleCutoff.Add(-30*time.Minute), pair, 100_000_000, 91_000_000)
		if err := store.InsertTrade(ctx, old); err != nil {
			t.Fatalf("InsertTrade: %v", err)
		}

		price, _, src, err := store.FXQuoteAtOrBefore(ctx, pair, staleCutoff, fx)
		if err != nil {
			t.Fatalf("FXQuoteAtOrBefore: %v", err)
		}
		if src != "exchangeratesapi" {
			t.Errorf("source = %q, want exchangeratesapi (trades fallback)", src)
		}
		want := new(big.Rat).SetFrac(big.NewInt(91_000_000), big.NewInt(100_000_000))
		if price.Cmp(want) != 0 {
			t.Errorf("price = %s, want %s", price.RatString(), want.RatString())
		}
	})

	t.Run("no row anywhere returns ErrNoFXQuote", func(t *testing.T) {
		mxn, err := c.NewFiatAsset("MXN")
		if err != nil {
			t.Fatal(err)
		}
		mxnPair, _ := c.NewPair(usd, mxn)
		_, _, _, err = store.FXQuoteAtOrBefore(ctx, mxnPair, cutoff, fx)
		if !errors.Is(err, timescale.ErrNoFXQuote) {
			t.Fatalf("err = %v, want ErrNoFXQuote", err)
		}
	})
}
