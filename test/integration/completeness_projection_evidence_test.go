//go:build integration

package integration_test

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// Migration 0199 + the verdict write: computed_at is restamped by every run,
// including one that only carried the projection claim forward, so the
// evidence time must be stored apart from it and survive a carry.
func TestCompletenessProjectionEvidence(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	dsn := startTimescale(t, ctx)
	applyMigrationsUpTo(t, dsn, 198)
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	// Rows as the previous binary leaves them: one whose last run reconciled
	// the full served range, one whose last run carried the prefix.
	provenAt := time.Date(2026, 9, 20, 5, 41, 0, 0, time.UTC)
	for _, r := range []struct{ source, detail string }{
		{"blend", "projection: verified [51499546,64000000] — the full range the served tier holds"},
		{"sdex", "projection: verified [63990000,64000000]; [61609957,63989999] carried from the prior clean verdict (tip=63989999), not re-verified this run"},
	} {
		if _, err := db.ExecContext(ctx, `
			INSERT INTO completeness_snapshots (
			    source, genesis_ledger, tip_ledger, watermark_ledger, coverage_pct, complete,
			    lake_complete, first_problem_ledger, projection_verified_from,
			    substrate_ok, recognition_ok, projection_ok, detail, computed_at)
			VALUES ($1, 2, 64000000, 64000000, 1, true, true, 0, 51499546, true, true, true, $2, $3)`,
			r.source, r.detail, provenAt); err != nil {
			t.Fatalf("seed pre-0199 %s: %v", r.source, err)
		}
	}
	applyMigrations(t, dsn)
	store := openVerdictStore(t, ctx, dsn)

	byName := func() map[string]timescale.CompletenessSnapshot {
		t.Helper()
		snaps, err := store.ListCompletenessSnapshots(ctx)
		if err != nil {
			t.Fatalf("list snapshots: %v", err)
		}
		out := make(map[string]timescale.CompletenessSnapshot, len(snaps))
		for _, s := range snaps {
			out[s.Source] = s
		}
		return out
	}
	got := byName()
	if !got["blend"].ProjectionEvidencedAt.Equal(provenAt) {
		t.Errorf("backfill: blend evidenced_at = %v, want its full-range run's computed_at %v", got["blend"].ProjectionEvidencedAt, provenAt)
	}
	if !got["sdex"].ProjectionEvidencedAt.IsZero() {
		t.Errorf("backfill: sdex evidenced_at = %v, want NULL — a carried claim's proof time is unknown", got["sdex"].ProjectionEvidencedAt)
	}

	// A fresh full reconcile stamps the evidence with the write's own now().
	full := verdictSnap(64_000_000)
	full.ProjectionReconciledFrom = 50_000_000
	full.ProjectionEvidencedNow = true
	if err := store.UpsertCompletenessSnapshot(ctx, full); err != nil {
		t.Fatalf("full-range write: %v", err)
	}
	first := byName()[verdictPublishSource]
	if first.ProjectionEvidencedAt.IsZero() || !first.ProjectionEvidencedAt.Equal(first.ComputedAt) {
		t.Fatalf("full-range write: evidenced_at = %v, computed_at = %v, want equal and set", first.ProjectionEvidencedAt, first.ComputedAt)
	}

	// The next run carries the prefix: computed_at advances, the evidence does not.
	time.Sleep(20 * time.Millisecond)
	carry := verdictSnap(64_010_000)
	carry.ProjectionReconciledFrom = 64_000_001
	carry.ProjectionEvidencedAt = first.ProjectionEvidencedAt
	if err := store.UpsertCompletenessSnapshot(ctx, carry); err != nil {
		t.Fatalf("carry write: %v", err)
	}
	second := byName()[verdictPublishSource]
	if !second.ComputedAt.After(first.ComputedAt) {
		t.Errorf("carry write: computed_at = %v, want after %v", second.ComputedAt, first.ComputedAt)
	}
	if !second.ProjectionEvidencedAt.Equal(first.ProjectionEvidencedAt) {
		t.Errorf("carry write: evidenced_at = %v, want the full run's %v — a carry is not new evidence", second.ProjectionEvidencedAt, first.ProjectionEvidencedAt)
	}
	if second.ProjectionReconciledFrom != 64_000_001 {
		t.Errorf("carry write: reconciled_from = %d, want 64000001", second.ProjectionReconciledFrom)
	}

	// A failing claim has no evidence behind it.
	failing := verdictSnap(64_020_000)
	failing.Complete, failing.ProjectionOK = false, false
	if err := store.UpsertCompletenessSnapshot(ctx, failing); err != nil {
		t.Fatalf("failing write: %v", err)
	}
	if at := byName()[verdictPublishSource].ProjectionEvidencedAt; !at.IsZero() {
		t.Errorf("failing write: evidenced_at = %v, want NULL", at)
	}

	applyMigrationsUpTo(t, dsn, 198)
	var cols int
	if err := db.QueryRowContext(ctx, `
		SELECT count(*) FROM information_schema.columns
		 WHERE table_name = 'completeness_snapshots'
		   AND column_name IN ('projection_reconciled_from', 'projection_evidenced_at')`).Scan(&cols); err != nil {
		t.Fatalf("read columns: %v", err)
	}
	if cols != 0 {
		t.Errorf("0199 down left %d evidence column(s) in place", cols)
	}
	applyMigrations(t, dsn)
}
