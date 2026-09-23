//go:build integration

package integration_test

import (
	"context"
	"fmt"
	"math/big"
	"testing"
	"time"

	c "github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// Store.EarliestTradeInWindow is the probe backfill-external refuses an
// overlapping window on. It must be scoped to exactly (source, pair) and
// the half-open [from, to): a row for another venue, another pair, or
// outside the window must neither refuse the run nor be reported as the
// -to bound.
func TestEarliestTradeInWindow_ScopedToSourcePairAndWindow(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("timescale.Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	xlmUSD := earliestPair(t, "crypto:XLM", "fiat:USD")
	xlmEUR := earliestPair(t, "crypto:XLM", "fiat:EUR")
	from := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	to := from.Add(24 * time.Hour)
	want := from.Add(3*time.Hour + 250*time.Microsecond)

	seed := []struct {
		source string
		pair   c.Pair
		ts     time.Time
	}{
		{"kraken", xlmUSD, from.Add(-time.Second)},  // before the window
		{"kraken", xlmUSD, to},                      // `to` is exclusive
		{"bitstamp", xlmUSD, from.Add(time.Hour)},   // another venue
		{"kraken", xlmEUR, from.Add(time.Hour)},     // another pair
		{"kraken", xlmUSD, want.Add(2 * time.Hour)}, // later in window
		{"kraken", xlmUSD, want},                    // the answer
	}
	for i, s := range seed {
		tr := c.Trade{
			Source:      s.source,
			TxHash:      fmt.Sprintf("%064x", i+1),
			Timestamp:   s.ts,
			Pair:        s.pair,
			BaseAmount:  c.NewAmount(big.NewInt(100_000_000)),
			QuoteAmount: c.NewAmount(big.NewInt(17_000_000)),
		}
		if err := store.InsertTrade(ctx, tr); err != nil {
			t.Fatalf("seed %d InsertTrade: %v", i, err)
		}
	}

	got, found, err := store.EarliestTradeInWindow(ctx, "kraken", xlmUSD, from, to)
	if err != nil {
		t.Fatalf("EarliestTradeInWindow: %v", err)
	}
	if !found || !got.Equal(want) {
		t.Fatalf("EarliestTradeInWindow = (%v, %v), want (%v, true)", got, found, want)
	}

	// A window holding only other venues' and other pairs' rows is empty.
	_, found, err = store.EarliestTradeInWindow(ctx, "kraken", xlmUSD, from, from.Add(2*time.Hour))
	if err != nil {
		t.Fatalf("EarliestTradeInWindow (narrow): %v", err)
	}
	if found {
		t.Fatal("window with only another venue's and another pair's rows reported as occupied")
	}
}

func earliestPair(t *testing.T, base, quote string) c.Pair {
	t.Helper()
	b, err := c.ParseAsset(base)
	if err != nil {
		t.Fatalf("ParseAsset(%s): %v", base, err)
	}
	q, err := c.ParseAsset(quote)
	if err != nil {
		t.Fatalf("ParseAsset(%s): %v", quote, err)
	}
	p, err := c.NewPair(b, q)
	if err != nil {
		t.Fatalf("NewPair: %v", err)
	}
	return p
}
