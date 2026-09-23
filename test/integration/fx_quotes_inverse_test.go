//go:build integration

package integration_test

import (
	"context"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// TestFXQuotes_InverseUSDIsTheExactNumericReciprocal pins ADR-0003 on
// the fx_quotes reciprocal: inverse_usd must equal 1 / rate_usd computed
// in NUMERIC, not the worker's float64 quotient. A rate of 3 makes the
// two visibly diverge — the float64 reciprocal is 0.3333333333333333
// (16 digits, then rounded), the NUMERIC one carries the division's own
// precision — so any float-derived value fails the equality.
func TestFXQuotes_InverseUSDIsTheExactNumericReciprocal(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	day := time.Now().UTC().Truncate(24 * time.Hour)
	quotes := []timescale.FXQuote{
		{Bucket: day, Ticker: "XTS", RateUSD: 3, InverseUSD: 1.0 / 3, Source: "massive"},
		{Bucket: day, Ticker: "EUR", RateUSD: 0.92, InverseUSD: 1.0 / 0.92, Source: "massive"},
	}
	if err := store.InsertFXQuoteBatch(ctx, quotes); err != nil {
		t.Fatalf("InsertFXQuoteBatch: %v", err)
	}

	for _, q := range quotes {
		var (
			exact   bool
			inverse string
			rate    string
		)
		if err := store.DB().QueryRowContext(ctx,
			`SELECT inverse_usd = 1::numeric / rate_usd, inverse_usd::text, rate_usd::text
			   FROM fx_quotes WHERE ticker = $1 AND bucket = $2`, q.Ticker, day,
		).Scan(&exact, &inverse, &rate); err != nil {
			t.Fatalf("read %s: %v", q.Ticker, err)
		}
		if !exact {
			t.Errorf("%s: stored inverse_usd = %s is not the NUMERIC reciprocal of rate_usd = %s "+
				"(a float64 1/rate reached the money column)", q.Ticker, inverse, rate)
		}

		// The read path hands the served-price surfaces that exact text.
		hist, err := store.ListFXHistory(ctx, q.Ticker, day, day)
		if err != nil {
			t.Fatalf("ListFXHistory %s: %v", q.Ticker, err)
		}
		if len(hist) != 1 || hist[0].InverseUSDText != inverse {
			t.Errorf("%s: ListFXHistory = %+v, want one row with InverseUSDText %q", q.Ticker, hist, inverse)
		}
	}
}
