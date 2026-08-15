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

// TestFXQuotesGenerationGuard_OperatorCorrectionIsDurable is the MR-1
// proven-red test (audit-2026-08-14, migration 0141). fx_quotes.rate_usd is
// the denominator of every fiat-quoted usd_volume, yet its upsert was the
// ONLY money-value derived writer the INV-3 fix skipped: an unguarded
// `ON CONFLICT (ticker,bucket) DO UPDATE`, pure arrival-order
// last-writer-wins. So an operator correction written by fx-history-backfill
// (source='frankfurter-historical', gen>0) over a key the live worker owns
// (source='massive', gen 0) was silently reverted by the next daily worker
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
	// — this is the durability property MR-1 regression (2) is about.
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
