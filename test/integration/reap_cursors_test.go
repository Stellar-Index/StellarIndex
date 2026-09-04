//go:build integration

package integration_test

import (
	"context"
	"database/sql"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// ReapCursors is the only irreversible statement in the cursor-cleanup
// path. Its safety rests on three SQL predicates — the cutoff, the
// -source scope, and `source <> ALL($3)` — and the shape test in
// internal/storage/timescale pins the text of all three but cannot
// prove Postgres agrees with the reading. In particular `<> ALL(...)`
// over a text[] bound as a Go []string is a pgx encode path, and this
// repo has shipped a param-typing bug before (see
// divergence_observations_test.go). So: a real table, real rows, a real
// DELETE, and the live cursor still there afterwards.
func TestReapCursorsProtectsLiveNamespaces(t *testing.T) {
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
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	now := time.Now().UTC()
	cutoff := now.Add(-7 * 24 * time.Hour)

	// The r1 population in miniature: two live cursors stuck for a
	// month, two dead one-shot shards, and one shard still walking.
	seedCursor := func(source, sub string, ledger int64, age time.Duration) {
		t.Helper()
		const q = `
            INSERT INTO ingestion_cursors (source, sub_source, first_ledger, last_ledger, last_updated)
            VALUES ($1, $2, $3, $3, $4)
        `
		if _, err := db.ExecContext(ctx, q, source, sub, ledger, now.Add(-age)); err != nil {
			t.Fatalf("seed %s/%s: %v", source, sub, err)
		}
	}
	seed := func() {
		t.Helper()
		if _, err := db.ExecContext(ctx, `DELETE FROM ingestion_cursors`); err != nil {
			t.Fatalf("clear cursors: %v", err)
		}
		seedCursor("ledgerstream", "", 63302110, 30*24*time.Hour)
		seedCursor("projector", "soroswap", 63302110, 30*24*time.Hour)
		seedCursor("backfill", "11474999-15299997:sdex", 15299997, 112*24*time.Hour)
		seedCursor("projected-rebuild", "shard-1", 40000000, 40*24*time.Hour)
		seedCursor("backfill", "still-walking", 61000000, 20*time.Minute)
	}
	remaining := func() map[string]bool {
		t.Helper()
		rows, lerr := store.ListCursors(ctx)
		if lerr != nil {
			t.Fatalf("ListCursors: %v", lerr)
		}
		out := map[string]bool{}
		for _, c := range rows {
			out[c.Source+"/"+c.Sub] = true
		}
		return out
	}

	seed()
	deleted, err := store.ReapCursors(ctx, cutoff, "", timescale.LiveCursorSources())
	if err != nil {
		t.Fatalf("ReapCursors: %v", err)
	}
	if deleted != 2 {
		t.Errorf("deleted = %d, want 2 (the sdex shard + the projected-rebuild shard)", deleted)
	}
	left := remaining()
	for _, want := range []string{"ledgerstream/", "projector/soroswap"} {
		if !left[want] {
			t.Errorf("%s was DELETED — a stuck live cursor is an incident to investigate, and losing its resume point restarts ingest from the configured start ledger", want)
		}
	}
	if !left["backfill/still-walking"] {
		t.Error("backfill/still-walking was deleted — it is inside the cutoff")
	}
	if left["backfill/11474999-15299997:sdex"] || left["projected-rebuild/shard-1"] {
		t.Errorf("an abandoned shard survived the reap: %v", left)
	}

	// -source narrows to one job without weakening either other clause.
	seed()
	deleted, err = store.ReapCursors(ctx, cutoff, "projected-rebuild", timescale.LiveCursorSources())
	if err != nil {
		t.Fatalf("ReapCursors(-source): %v", err)
	}
	if deleted != 1 {
		t.Errorf("scoped delete = %d, want 1", deleted)
	}
	left = remaining()
	if !left["backfill/11474999-15299997:sdex"] {
		t.Error("-source projected-rebuild deleted a backfill row")
	}
	if !left["ledgerstream/"] || !left["projector/soroswap"] {
		t.Errorf("a scoped run deleted a live cursor: %v", left)
	}

	// An empty protected list is what a caller passing nil would get:
	// the guard must be the only thing that was holding those rows, so
	// this run proves the earlier survival came from the predicate and
	// not from the rows being out of range anyway.
	seed()
	deleted, err = store.ReapCursors(ctx, cutoff, "", nil)
	if err != nil {
		t.Fatalf("ReapCursors(nil protected): %v", err)
	}
	if deleted != 4 {
		t.Errorf("unprotected delete = %d, want 4 — the two live rows ARE past the cutoff, so only `source <> ALL($3)` was sparing them", deleted)
	}
}
