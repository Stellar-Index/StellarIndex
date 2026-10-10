//go:build integration

package integration_test

import (
	"context"
	"database/sql"
	"math"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// TestFXQuotesGenerationGuard_OperatorCorrectionIsDurable is the
// proven-red test for migration 0141. fx_quotes.rate_usd is
// the denominator of every fiat-quoted usd_volume, so its upsert needs the
// same generation guard as every other derived money-value writer: an
// unguarded `ON CONFLICT (ticker,bucket) DO UPDATE` is pure arrival-order
// last-writer-wins, so an operator correction written by fx-history-backfill
// (source='frankfurter-historical', gen>0) over a key the live worker owns
// (source='massive', gen 0) would be silently reverted by the next daily worker
// refresh.
//
// The fix threads derive_generation through InsertFXQuoteBatch and guards
// the upsert with `fx_quotes.derive_generation <= EXCLUDED.derive_generation`.
// This test exercises the real seam ([Store.SetDeriveGeneration] +
// InsertFXQuoteBatch, exactly what the worker and the tool call) and
// asserts:
//
//   - the operator correction (gen T>0) LANDS over the live gen-0 row (the
//     conflict guard permits 0 <= T), AND
//   - a subsequent live gen-0 worker refresh carrying the stale rate can
//     NEVER revert it (T <= 0 is false) — the correction is durable.
//
// To reproduce the red state: revert only the InsertFXQuoteBatch upsert to
// the unguarded `DO UPDATE SET ... source = EXCLUDED.source` (keep migration
// 0141) and the "correction survives" assertion goes red — the gen-0 replay
// overwrites the corrected rate.
func TestFXQuotesGenerationGuard_OperatorCorrectionIsDurable(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	const ticker = "EUR"
	bucket := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)

	const (
		wrongRate     = 0.85 // the transiently-wrong live value
		correctedRate = 0.92 // the operator correction
	)

	read := func() (rate float64, gen int64, source string) {
		const q = `SELECT rate_usd::float8, derive_generation, COALESCE(source, '')
		             FROM fx_quotes WHERE ticker = $1 AND bucket = $2`
		var src sql.NullString
		if err := store.DB().QueryRowContext(ctx, q, ticker, bucket).Scan(&rate, &gen, &src); err != nil {
			t.Fatalf("read fx_quotes (%s, %s): %v", ticker, bucket.Format("2006-01-02"), err)
		}
		return rate, gen, src.String
	}
	closeTo := func(got, want float64) bool { return math.Abs(got-want) < 1e-9 }

	// (1) Live forex worker writes the row at generation 0 (the wrong rate).
	store.SetDeriveGeneration(0)
	if err := store.InsertFXQuoteBatch(ctx, []timescale.FXQuote{{
		Bucket: bucket, Ticker: ticker, RateUSD: wrongRate, InverseUSD: 1.0 / wrongRate, Source: "massive",
	}}); err != nil {
		t.Fatalf("worker gen-0 write: %v", err)
	}

	// (2) Operator correction via fx-history-backfill: a POSITIVE generation
	// (what openBackfillStore stamps) carrying the corrected rate. It must
	// win the conflict (0 <= T).
	gen := time.Now().Unix()
	store.SetDeriveGeneration(gen)
	if err := store.InsertFXQuoteBatch(ctx, []timescale.FXQuote{{
		Bucket: bucket, Ticker: ticker, RateUSD: correctedRate, InverseUSD: 1.0 / correctedRate,
		Source: "frankfurter-historical",
	}}); err != nil {
		t.Fatalf("operator correction write: %v", err)
	}
	if rate, g, src := read(); !closeTo(rate, correctedRate) || g != gen || src != "frankfurter-historical" {
		t.Fatalf("after operator correction: rate=%v gen=%d source=%q, want %v/%d/frankfurter-historical "+
			"(the correction must land over the live gen-0 row)", rate, g, src, correctedRate, gen)
	}

	// (3) The next daily live worker refresh re-writes the row at generation
	// 0 with the stale rate. The guard must PRESERVE the operator correction
	// — this is the durability property regression guard (2) is about.
	store.SetDeriveGeneration(0)
	if err := store.InsertFXQuoteBatch(ctx, []timescale.FXQuote{{
		Bucket: bucket, Ticker: ticker, RateUSD: wrongRate, InverseUSD: 1.0 / wrongRate, Source: "massive",
	}}); err != nil {
		t.Fatalf("worker gen-0 refresh: %v", err)
	}
	if rate, g, src := read(); !closeTo(rate, correctedRate) || g != gen || src != "frankfurter-historical" {
		t.Errorf("after gen-0 worker refresh: rate=%v gen=%d source=%q, want %v/%d/frankfurter-historical "+
			"(the generation guard must make the operator correction durable — a live gen-0 replay "+
			"must NOT revert it)", rate, g, src, correctedRate, gen)
	}
}

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
		if len(hist) != 1 || hist[0].InverseUSDText != inverse || hist[0].RateUSDText != rate {
			t.Errorf("%s: ListFXHistory = %+v, want one row with InverseUSDText %q, RateUSDText %q", q.Ticker, hist, inverse, rate)
		}
	}
}

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
