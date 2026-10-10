//go:build integration

package integration_test

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/Stellar-Index/StellarIndex/internal/ops/chops"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// TestCHRebuildPreflight_AnswersWithoutTouchingTheLakeOrTheServedTier is
// the executing proof for `ch-rebuild -write -preflight`, driven
// through the real subcommand on real TimescaleDB.
//
// scripts/ops/ch-rebuild-projected.sh DELETEs a window and only then asks
// ch-rebuild to re-derive it. The refusals must not live inside that second
// step, or a guard doing its job leaves the window empty. The preflight lets
// the script ask first — which is only safe if the preflight:
//
//  1. really runs the guards — a live projector cursor below -to, and a
//     range over the buffered ceiling, are each refused here exactly as
//     the real run refuses them, with NO verdict line on stdout;
//  2. really stops before the lake — ClickHouse is unreachable in this
//     test, so the same command WITHOUT -preflight fails on the lake read
//     (the control), while the preflight succeeds;
//  3. names what the run would re-derive, in the form the script parses;
//  4. writes nothing.
func TestCHRebuildPreflight_AnswersWithoutTouchingTheLakeOrTheServedTier(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	cfgPath := filepath.Join(t.TempDir(), "stellarindex.toml")
	if err := os.WriteFile(cfgPath, []byte(fmt.Sprintf("[storage]\npostgres_dsn = %q\n", dsn)), 0o600); err != nil {
		t.Fatal(err)
	}

	const from, to = 61_000_000, 61_100_000
	// Nothing listens here: any lake read fails at once.
	const deadLake = "127.0.0.1:1"
	args := func(extra ...string) []string {
		return append([]string{
			"ch-rebuild", "-config", cfgPath, "-ch-addr", deadLake,
			"-from", fmt.Sprint(from), "-to", fmt.Sprint(to),
			"-sources", "aquarius,soroswap", "-write",
		}, extra...)
	}
	servedRows := func(t *testing.T) int {
		t.Helper()
		var n int
		const q = `SELECT (SELECT count(*) FROM trades) + (SELECT count(*) FROM soroswap_skim_events) + (SELECT count(*) FROM projection_dirty_windows)`
		if err := store.DB().QueryRowContext(ctx, q).Scan(&n); err != nil {
			t.Fatalf("count served rows: %v", err)
		}
		return n
	}

	// ── 1a. live cursor below -to: refused, no verdict ────────────────
	if err := store.UpsertCursor(ctx, "projector", "soroswap", to+1_000); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertCursor(ctx, "projector", "aquarius", to-50_000); err != nil {
		t.Fatal(err)
	}
	out, err := captureStdout(t, func() error { return chops.Run(args("-preflight")) })
	if err == nil || !strings.Contains(err.Error(), "live projector's cursor is below") || !strings.Contains(err.Error(), "aquarius") {
		t.Fatalf("preflight over a range the live projector is still inside was not refused: err=%v\n%s", err, out)
	}
	if strings.Contains(out, "preflight ok") {
		t.Fatalf("a REFUSED preflight still printed a verdict line — the script would delete on it:\n%s", out)
	}

	// ── 1b. range over the buffered ceiling: refused, no verdict ──────
	if err := store.UpsertCursor(ctx, "projector", "aquarius", 70_000_000); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertCursor(ctx, "projector", "soroswap", 70_000_000); err != nil {
		t.Fatal(err)
	}
	wide := []string{
		"ch-rebuild", "-config", cfgPath, "-ch-addr", deadLake,
		"-from", "55000000", "-to", "62894000", "-sources", "aquarius,soroswap", "-write", "-preflight",
	}
	out, err = captureStdout(t, func() error { return chops.Run(wide) })
	if err == nil || !strings.Contains(err.Error(), "window invocations") {
		t.Fatalf("preflight over a 7.9M-ledger range was not refused by the buffered-range guard: err=%v\n%s", err, out)
	}
	if strings.Contains(out, "preflight ok") {
		t.Fatalf("a REFUSED preflight still printed a verdict line:\n%s", out)
	}

	// ── 2. control: the real run reaches the lake, and the lake is dead ─
	// The first lake read is the per-WASM replay gate, so the failure
	// names the dead address, not the event stream.
	before := servedRows(t)
	if _, err = captureStdout(t, func() error { return chops.Run(args()) }); err == nil || !strings.Contains(err.Error(), "wasm replay gate") || !strings.Contains(err.Error(), deadLake) {
		t.Fatalf("control: the un-preflighted run should fail on the unreachable lake; got err=%v — "+
			"without this, a passing preflight proves nothing about WHERE it stopped", err)
	}

	// ── 2+3. the preflight passes the guards and stops short of it ─────
	out, err = captureStdout(t, func() error { return chops.Run(args("-preflight")) })
	if err != nil {
		t.Fatalf("preflight: %v\n%s", err, out)
	}
	// Catalogue order, not -sources order: it is the list the run decodes.
	want := fmt.Sprintf("ch-rebuild: preflight ok [%d,%d] rederive=soroswap,aquarius\n", from, to)
	if out != want {
		t.Fatalf("preflight stdout = %q, want exactly %q", out, want)
	}

	// ── 4. nothing was written ────────────────────────────────────────
	if after := servedRows(t); after != before {
		t.Fatalf("preflight changed the served tier: %d rows before, %d after", before, after)
	}
}

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
