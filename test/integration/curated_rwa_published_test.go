//go:build integration

package integration_test

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// curated_rwa_published_series (migration 0162) through real Timescale:
// the sync's replace-per-series transaction and the reader that serves
// the curator's latest published total, its split and the whole series.
//
// What only a database can prove here:
//
//  1. REPLACE is per series and whole: a second run's total series
//     evicts every month the first run wrote, including months the
//     second run no longer prints, while a series the run did NOT name
//     is left exactly as it was.
//  2. The reader's recognition bound is in the SQL: rows whose
//     observed_at is past 48h are an absence, not a stale figure.
//  3. NUMERIC round-trip: the curator's printed decimal comes back as the
//     same literal (ADR-0003), and `date` comes back as the same day.
func TestCuratedRWAPublished_ReplacePerSeriesAndRead(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("sql open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	const curator = "dune:stellar"
	executed := time.Date(2026, 9, 17, 4, 58, 0, 0, time.UTC)
	month := func(y int, m time.Month, d int) time.Time { return time.Date(y, m, d, 0, 0, 0, 0, time.UTC) }
	total := func(mo time.Time, v string) timescale.CuratedRWAPublishedRow {
		return timescale.CuratedRWAPublishedRow{
			Series: timescale.CuratedRWASeriesMonthlyTotal, MonthEnd: mo, ValueUSD: v,
			SourceQuery: 6961845, ExecutedAt: executed,
		}
	}
	split := func(mo time.Time, sub, v string) timescale.CuratedRWAPublishedRow {
		return timescale.CuratedRWAPublishedRow{
			Series: timescale.CuratedRWASeriesMonthlyBySubclass, MonthEnd: mo, Subclass: sub, ValueUSD: v,
			SourceQuery: 6961847, ExecutedAt: executed,
		}
	}

	// ── run 1: two months of total, a split for each ──
	n, err := store.ReplaceCuratedRWAPublished(ctx, curator, []timescale.CuratedRWAPublishedRow{
		total(month(2025, 7, 31), "3900000000.10"),
		total(month(2025, 8, 31), "4004795860.00"),
		split(month(2025, 7, 31), "US Treasuries", "3000000000.10"),
		split(month(2025, 7, 31), "Private Credit", "900000000.00"),
		split(month(2025, 8, 31), "US Treasuries", "3100000000.00"),
		split(month(2025, 8, 31), "Private Credit", "904795860.00"),
	})
	if err != nil {
		t.Fatalf("replace run 1: %v", err)
	}
	if n != 6 {
		t.Errorf("run 1 inserted %d rows, want 6", n)
	}

	got, err := store.LatestCuratedPublished(ctx, curator)
	if err != nil {
		t.Fatalf("read after run 1: %v", err)
	}
	if got == nil {
		t.Fatal("read after run 1 returned nil — nothing recognised")
	}
	if !got.MonthEnd.Equal(month(2025, 8, 31)) || got.TotalUSD != "4004795860.00" {
		t.Errorf("latest = %s %s, want 2025-08-31 4004795860.00 (the literal, not a float rendering)",
			got.MonthEnd.Format("2006-01-02"), got.TotalUSD)
	}
	if got.SourceQuery != 6961845 || got.SplitSourceQuery != 6961847 || !got.ExecutedAt.Equal(executed) {
		t.Errorf("provenance = query %d / %d at %v, want 6961845 / 6961847 at %v",
			got.SourceQuery, got.SplitSourceQuery, got.ExecutedAt, executed)
	}
	if got.ObservedAt.IsZero() || time.Since(got.ObservedAt) > time.Minute {
		t.Errorf("observed_at = %v, want this run's clock", got.ObservedAt)
	}
	if len(got.Series) != 2 || !got.Series[0].MonthEnd.Equal(month(2025, 7, 31)) || got.Series[1].ValueUSD != "4004795860.00" {
		t.Errorf("series = %+v, want two points oldest first", got.Series)
	}
	if len(got.BySubclass) != 2 || got.BySubclass[0].Subclass != "US Treasuries" || got.BySubclass[0].ValueUSD != "3100000000.00" ||
		got.BySubclass[1].Subclass != "Private Credit" || got.BySubclass[1].ValueUSD != "904795860.00" {
		t.Errorf("split = %+v, want the LATEST month's two rows largest first", got.BySubclass)
	}

	// ── run 2: the total series alone, one month dropped, one added ──
	// A replace is per series and whole: July must be gone from the
	// total series (the curator no longer prints it), September must be
	// present, and the split series — not named by this run — must be
	// exactly what run 1 wrote.
	if _, err := store.ReplaceCuratedRWAPublished(ctx, curator, []timescale.CuratedRWAPublishedRow{
		total(month(2025, 8, 31), "4004795860.00"),
		total(month(2025, 9, 30), "4100000000.00"),
	}); err != nil {
		t.Fatalf("replace run 2: %v", err)
	}
	got, err = store.LatestCuratedPublished(ctx, curator)
	if err != nil || got == nil {
		t.Fatalf("read after run 2: %v, %v", got, err)
	}
	if !got.MonthEnd.Equal(month(2025, 9, 30)) || got.TotalUSD != "4100000000.00" {
		t.Errorf("latest after run 2 = %s %s, want 2025-09-30 4100000000.00", got.MonthEnd.Format("2006-01-02"), got.TotalUSD)
	}
	if len(got.Series) != 2 || !got.Series[0].MonthEnd.Equal(month(2025, 8, 31)) {
		t.Errorf("series after run 2 = %+v, want Aug+Sep only (July evicted with its series)", got.Series)
	}
	// September has no split rows — the split series was not replaced —
	// so the latest month's split is empty, never July's or August's.
	if len(got.BySubclass) != 0 {
		t.Errorf("split for a month the split series does not carry = %+v, want none", got.BySubclass)
	}
	var splitRows int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM curated_rwa_published_series
		WHERE curator = $1 AND series = $2`, curator, timescale.CuratedRWASeriesMonthlyBySubclass).Scan(&splitRows); err != nil {
		t.Fatal(err)
	}
	if splitRows != 4 {
		t.Errorf("split series has %d rows after a total-only run, want run 1's 4 untouched", splitRows)
	}

	// ── another curator is invisible to this one ──
	if _, err := store.ReplaceCuratedRWAPublished(ctx, "other:curator", []timescale.CuratedRWAPublishedRow{
		total(month(2025, 10, 31), "1.00"),
	}); err != nil {
		t.Fatalf("replace other curator: %v", err)
	}
	got, err = store.LatestCuratedPublished(ctx, curator)
	if err != nil || got == nil || !got.MonthEnd.Equal(month(2025, 9, 30)) {
		t.Errorf("another curator's rows leaked into %s: %+v, %v", curator, got, err)
	}

	// ── the recognition bound: 49h-old rows are an absence ──
	if _, err := db.ExecContext(ctx, `UPDATE curated_rwa_published_series
		SET observed_at = now() - INTERVAL '49 hours' WHERE curator = $1`, curator); err != nil {
		t.Fatal(err)
	}
	got, err = store.LatestCuratedPublished(ctx, curator)
	if err != nil {
		t.Fatalf("read past the bound: %v", err)
	}
	if got != nil {
		t.Errorf("rows past the 48h recognition bound were served: %+v", got)
	}

	// ── a duplicate bucket in one run does not trip the primary key ──
	if _, err := store.ReplaceCuratedRWAPublished(ctx, curator, []timescale.CuratedRWAPublishedRow{
		total(month(2025, 9, 30), "1.00"),
		total(month(2025, 9, 30), "2.00"),
	}); err != nil {
		t.Fatalf("replace with a duplicate bucket: %v", err)
	}
	got, err = store.LatestCuratedPublished(ctx, curator)
	if err != nil || got == nil || got.TotalUSD != "2.00" {
		t.Errorf("duplicate bucket: got %+v, %v; want the LAST printed value 2.00", got, err)
	}
}
