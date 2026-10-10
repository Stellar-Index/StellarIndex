//go:build integration

package integration_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/postgres"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/canonical/discovery"
	"github.com/Stellar-Index/StellarIndex/internal/consumer"
	"github.com/Stellar-Index/StellarIndex/internal/domain"
	"github.com/Stellar-Index/StellarIndex/internal/pipeline"
	"github.com/Stellar-Index/StellarIndex/internal/sources/blend"
	"github.com/Stellar-Index/StellarIndex/internal/sources/cctp"
	"github.com/Stellar-Index/StellarIndex/internal/sources/sdex"
	"github.com/Stellar-Index/StellarIndex/internal/sources/soroswap_router"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
	"github.com/Stellar-Index/StellarIndex/internal/supply"
)

// TestChRebuildProjectedScript_DeleteSQLOnRealPostgres executes the SQL
// that scripts/ops/ch-rebuild-projected.sh ACTUALLY emits — captured by
// running the shipped script with a recording `psql` — against real
// TimescaleDB, and checks which rows survive.
//
// internal/ops/chops/ch_rebuild_projected_script_*_test.go hold the script
// to its rules by reading that SQL as text. Text is not proof: only the
// database says which rows `source = ANY (string_to_array(...))` matches,
// and whether a failed batch really leaves the earlier tables alone.
//
//  1. SRC=soroswap deletes soroswap's in-window trades and NOTHING else:
//     not another source's trades, not a whole-table source's rows, not
//     soroswap's own rows outside the window.
//  2. A batch that fails on a LATER table leaves the EARLIER ones intact.
//     The statements are replayed the way psql sends them — one at a time,
//     stopping at the first error, then disconnecting — because a single
//     multi-statement Exec is implicitly one transaction and would pass
//     this even without the script's BEGIN/COMMIT.
//  3. The default SRC deletes every listed source's rows, and still not
//     sushiswap_v3's or sdex's.
func TestChRebuildProjectedScript_DeleteSQLOnRealPostgres(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the ops script is bash; r1 and CI are Linux")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	// ── fixture: one window, rows inside and outside it ───────────────
	const lo, hi = 61_000_000, 61_999_999
	usdc, err := canonical.NewClassicAsset("USDC", "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN")
	if err != nil {
		t.Fatal(err)
	}
	pair, err := canonical.NewPair(canonical.NativeAsset(), usdc)
	if err != nil {
		t.Fatal(err)
	}
	ts := time.Date(2026, 6, 10, 10, 0, 0, 0, time.UTC)
	seedTrade := func(source string, nonce int, ledger uint32) {
		t.Helper()
		tr := mkIntegrationTrade(source, nonce, ts.Add(time.Duration(nonce)*time.Second), pair, 1_000_000_000, 500_000_000)
		tr.Ledger = ledger
		if err := store.InsertTrade(ctx, tr); err != nil {
			t.Fatalf("InsertTrade %s@%d: %v", source, ledger, err)
		}
	}
	seedTrade("soroswap", 1, lo+10)
	seedTrade("aquarius", 2, lo+20)
	seedTrade("sushiswap_v3", 3, lo+30)
	seedTrade("sdex", 4, lo+40)
	seedTrade("soroswap", 5, hi+10) // the next window's
	if err := store.InsertCCTPEvent(ctx, timescale.CCTPEvent{
		ContractID: cctp.MainnetMessageTransmitter,
		Ledger:     lo + 50,
		TxHash:     strings.Repeat("ab", 32),
		ObservedAt: ts,
		EventType:  timescale.CCTPAttesterEnabled,
		Attributes: map[string]any{"attester": "att-0"},
	}); err != nil {
		t.Fatalf("InsertCCTPEvent: %v", err)
	}

	count := func(t *testing.T, q string, args ...any) int {
		t.Helper()
		var n int
		if err := store.DB().QueryRowContext(ctx, q, args...).Scan(&n); err != nil {
			t.Fatalf("%q: %v", q, err)
		}
		return n
	}
	trades := func(t *testing.T, source string, ledger uint32) int {
		t.Helper()
		return count(t, `SELECT count(*) FROM trades WHERE source = $1 AND ledger = $2`, source, int64(ledger))
	}
	cctpRows := func(t *testing.T) int { t.Helper(); return count(t, `SELECT count(*) FROM cctp_events`) }
	for _, f := range []struct {
		source string
		ledger uint32
	}{{"soroswap", lo + 10}, {"aquarius", lo + 20}, {"sushiswap_v3", lo + 30}, {"sdex", lo + 40}, {"soroswap", hi + 10}} {
		if trades(t, f.source, f.ledger) != 1 {
			t.Fatalf("fixture: %s@%d did not land", f.source, f.ledger)
		}
	}
	if cctpRows(t) != 1 {
		t.Fatal("fixture: the cctp row did not land")
	}

	// ── 0. the occupancy probe names exactly the sources holding rows ──
	var occupied []string
	for _, stmt := range strings.Split(emittedDeleteSQL(t, ""), ";\n") {
		if stmt = strings.TrimSpace(stmt); !strings.HasPrefix(stmt, "SELECT 'occupied=") {
			continue
		}
		var tag string
		switch err := store.DB().QueryRowContext(ctx, stmt).Scan(&tag); {
		case errors.Is(err, sql.ErrNoRows):
		case err != nil:
			t.Fatalf("%q: %v", stmt, err)
		default:
			occupied = append(occupied, strings.TrimPrefix(tag, "occupied="))
		}
	}
	sort.Strings(occupied)
	if got, want := strings.Join(occupied, ","), "aquarius,cctp,soroswap"; got != want {
		t.Errorf("occupancy probe = %q, want %q — the -write would not be required to restore what the DELETE removes", got, want)
	}

	// ── 1. the narrowed run ───────────────────────────────────────────
	narrowed := emittedDeleteSQL(t, "soroswap")
	if err := replayLikePsql(ctx, store.DB(), narrowed); err != nil {
		t.Fatalf("SRC=soroswap batch failed on real Postgres: %v\n%s", err, narrowed)
	}
	if trades(t, "soroswap", lo+10) != 0 {
		t.Errorf("SRC=soroswap did not delete soroswap's in-window trade — the clean-slate repair is a no-op\n%s", narrowed)
	}
	for _, keep := range []struct {
		source string
		ledger uint32
		why    string
	}{
		{"aquarius", lo + 20, "another source's trades — this run never re-derives them"},
		{"sushiswap_v3", lo + 30, "not in the script's default SRC: it has no DELETE map for it (RLT-380)"},
		{"sdex", lo + 40, "op-derived, never in scope"},
		{"soroswap", hi + 10, "outside the window"},
	} {
		if trades(t, keep.source, keep.ledger) != 1 {
			t.Errorf("SRC=soroswap deleted %s@%d (%s)\n%s", keep.source, keep.ledger, keep.why, narrowed)
		}
	}
	if cctpRows(t) != 1 {
		t.Errorf("SRC=soroswap emptied cctp_events, a table this run never re-derives (F075)\n%s", narrowed)
	}

	// ── 2. a batch that fails part-way deletes nothing ────────────────
	full := emittedDeleteSQL(t, "")
	if !strings.Contains(full, "blend_admin") || strings.Index(full, "DELETE FROM trades") > strings.Index(full, "blend_admin") {
		t.Fatalf("fixture: want the trades DELETE ahead of blend_admin in the default batch\n%s", full)
	}
	if _, err := store.DB().ExecContext(ctx, `ALTER TABLE blend_admin RENAME TO blend_admin_moved`); err != nil {
		t.Fatal(err)
	}
	if err := replayLikePsql(ctx, store.DB(), full); err == nil {
		t.Fatal("fixture: the batch succeeded although blend_admin does not exist")
	}
	if _, err := store.DB().ExecContext(ctx, `ALTER TABLE blend_admin_moved RENAME TO blend_admin`); err != nil {
		t.Fatal(err)
	}
	if trades(t, "aquarius", lo+20) != 1 || cctpRows(t) != 1 {
		t.Errorf("a batch that FAILED on blend_admin still emptied the tables ahead of it (aquarius trades=%d, cctp=%d) — "+
			"the DELETE is not one transaction (RLT-381)", trades(t, "aquarius", lo+20), cctpRows(t))
	}

	// ── 3. the default run ────────────────────────────────────────────
	if err := replayLikePsql(ctx, store.DB(), full); err != nil {
		t.Fatalf("default batch failed on real Postgres: %v\n%s", err, full)
	}
	if trades(t, "aquarius", lo+20) != 0 || cctpRows(t) != 0 {
		t.Errorf("the default run left rows it re-derives (aquarius trades=%d, cctp=%d)", trades(t, "aquarius", lo+20), cctpRows(t))
	}
	if trades(t, "sushiswap_v3", lo+30) != 1 || trades(t, "sdex", lo+40) != 1 || trades(t, "soroswap", hi+10) != 1 {
		t.Errorf("the default run deleted rows nothing re-derives: sushiswap_v3=%d sdex=%d next-window soroswap=%d (want 1 each)",
			trades(t, "sushiswap_v3", lo+30), trades(t, "sdex", lo+40), trades(t, "soroswap", hi+10))
	}
}

// replayLikePsql runs a psql script the way `psql -v ON_ERROR_STOP=1` does:
// each statement on its own, stop at the first error, then disconnect — so
// a transaction the script opened and never committed is rolled back.
func replayLikePsql(ctx context.Context, db *sql.DB, script string) error {
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	// Raw + driver.ErrBadConn would be the literal disconnect; a ROLLBACK
	// before handing the connection back to the pool has the same effect.
	defer func() {
		_, _ = conn.ExecContext(ctx, `ROLLBACK`)
		_ = conn.Close()
	}()
	for _, stmt := range strings.Split(script, ";\n") {
		if stmt = strings.TrimSpace(stmt); stmt == "" {
			continue
		}
		if _, err := conn.ExecContext(ctx, stmt); err != nil {
			return err
		}
	}
	return nil
}

// emittedDeleteSQL runs the SHIPPED script for one window with a recording
// psql and an ops binary that says yes, and returns the SQL it fed psql.
// src "" leaves SRC at the script's default.
func emittedDeleteSQL(t *testing.T, src string) string {
	t.Helper()
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Fatalf("bash not found — this test must execute the script: %v", err)
	}
	_, thisFile, _, _ := runtime.Caller(0)
	script := filepath.Join(filepath.Dir(thisFile), "..", "..", "scripts", "ops", "ch-rebuild-projected.sh")
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil { //nolint:gosec // test temp dir
		t.Fatal(err)
	}
	sqlPath := filepath.Join(dir, "emitted.sql")
	stubs := map[string]string{
		"psql": "#!/usr/bin/env bash\ncat >> \"$STUB_SQL\"\n",
		"stellarindex-ops-ch": "#!/usr/bin/env bash\nfrom=; to=; srcs=; pre=0\n" +
			"while [ $# -gt 0 ]; do case \"$1\" in -from) from=$2; shift;; -to) to=$2; shift;; -sources) srcs=$2; shift;; -preflight) pre=1;; esac; shift; done\n" +
			// Byte-for-byte the line ch_rebuild_preflight_test.go pins on the real binary.
			"[ \"$pre\" = 1 ] && echo \"ch-rebuild: preflight ok [$from,$to] rederive=$srcs\"\nexit 0\n",
	}
	for name, body := range stubs {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(body), 0o755); err != nil { //nolint:gosec // an executable test stub
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, bash, script) //nolint:gosec // fixed script path, test-controlled env
	cmd.Env = []string{
		"PATH=" + bin + ":/usr/bin:/bin:/usr/local/bin:/opt/homebrew/bin",
		"STUB_SQL=" + sqlPath,
		"OPS=" + filepath.Join(bin, "stellarindex-ops-ch"),
		"CFG=" + filepath.Join(dir, "stellarindex.toml"),
		"STATE=" + filepath.Join(dir, "state", "done.txt"),
		"LOG=" + filepath.Join(dir, "rebuild.log"),
		"STELLARINDEX_POSTGRES_DSN=postgres://stub-host/stubdb",
		"FROM=61000000", "TO=61999999", "WIN=1000000",
	}
	if src != "" {
		cmd.Env = append(cmd.Env, "SRC="+src)
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		log, _ := os.ReadFile(filepath.Join(dir, "rebuild.log")) //nolint:gosec // test temp file
		t.Fatalf("script failed: %v\n%s\n%s", err, out, log)
	}
	b, err := os.ReadFile(sqlPath) //nolint:gosec // test temp file
	if err != nil {
		t.Fatalf("the script fed psql nothing: %v", err)
	}
	return string(b)
}

// Migration 0201 + the verdict write: computed_at is restamped by every run,
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
			t.Fatalf("seed pre-0201 %s: %v", r.source, err)
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
		t.Errorf("0201 down left %d evidence column(s) in place", cols)
	}
	applyMigrations(t, dsn)
}

// TestCompletenessTargetFloorMonotonicity exercises the durable projection
// floor (migration 0116) against a real TimescaleDB.
//
// The floor exists because compute-completeness otherwise derives its
// reconcile floor from MIN(ledger) of the target it is checking, which makes
// the check blind to the one event it exists to catch: delete the oldest
// served rows and MIN(ledger) rises with the loss, so the range follows it
// up, the surviving rows reconcile perfectly, and the verdict reads
// complete. "Rows were dropped" and "we never projected below there" produce
// identical output.
//
// The single property that makes the remembered floor trustworthy is that it
// only ever FALLS. If a run starting higher could overwrite it, a partial or
// deliberately-narrowed run would ratchet the floor up to the post-loss MIN
// and silently restore the original bug. That is what this test pins —
// against the real database, because LEAST() semantics and the ON CONFLICT
// clause are SQL behaviour, not Go behaviour, and a mock would only prove I
// can write down what I already believe.
func TestCompletenessTargetFloorMonotonicity(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	const (
		src   = "soroswap"
		table = "trades"
		filt  = "source = 'soroswap'"
	)
	key := timescale.TargetFloorKey(src, table, filt)

	// 1. Absent target must be ABSENT, not zero. A caller that read a
	//    missing row as floor=0 would report "loss below 0" on the first
	//    run after the migration, for every target at once.
	floors, err := store.CompletenessTargetFloors(ctx)
	if err != nil {
		t.Fatalf("CompletenessTargetFloors (empty): %v", err)
	}
	if _, ok := floors[key]; ok {
		t.Fatalf("expected no floor recorded before the first upsert, got %+v", floors[key])
	}

	// 2. First write establishes the floor.
	if err := store.UpsertCompletenessTargetFloor(ctx, timescale.CompletenessTargetFloor{
		Source: src, Table: table, Filter: filt, VerifiedFrom: 61_500_000,
	}); err != nil {
		t.Fatalf("first upsert: %v", err)
	}
	if got := mustFloor(t, ctx, store, key); got != 61_500_000 {
		t.Errorf("after first upsert = %d, want 61500000", got)
	}

	// 3. A HIGHER value must NOT raise the floor. This is the regression
	//    that matters: a rising MIN(ledger) is exactly what loss looks
	//    like, so recording it would erase the evidence.
	if err := store.UpsertCompletenessTargetFloor(ctx, timescale.CompletenessTargetFloor{
		Source: src, Table: table, Filter: filt, VerifiedFrom: 71_000_000,
	}); err != nil {
		t.Fatalf("higher upsert: %v", err)
	}
	if got := mustFloor(t, ctx, store, key); got != 61_500_000 {
		t.Errorf("after a HIGHER upsert = %d, want the floor to stay at 61500000 — "+
			"a rising floor lets a partial run ratchet past real loss", got)
	}

	// 4. A LOWER value legitimately lowers it: new ground was verified.
	if err := store.UpsertCompletenessTargetFloor(ctx, timescale.CompletenessTargetFloor{
		Source: src, Table: table, Filter: filt, VerifiedFrom: 2,
	}); err != nil {
		t.Fatalf("lower upsert: %v", err)
	}
	if got := mustFloor(t, ctx, store, key); got != 2 {
		t.Errorf("after a LOWER upsert = %d, want 2", got)
	}

	// 5. Targets are isolated by filter, not just by table. `trades` holds
	//    several sources' rows, and their true floors genuinely differ —
	//    if the filter were dropped from the key, the earliest source
	//    would drag every other source's floor down with it and mask loss
	//    in all of them.
	const otherFilt = "source = 'sdex'"
	otherKey := timescale.TargetFloorKey(src, table, otherFilt)
	if err := store.UpsertCompletenessTargetFloor(ctx, timescale.CompletenessTargetFloor{
		Source: src, Table: table, Filter: otherFilt, VerifiedFrom: 55_000_000,
	}); err != nil {
		t.Fatalf("other-filter upsert: %v", err)
	}
	if got := mustFloor(t, ctx, store, otherKey); got != 55_000_000 {
		t.Errorf("other-filter floor = %d, want 55000000", got)
	}
	if got := mustFloor(t, ctx, store, key); got != 2 {
		t.Errorf("original floor = %d after writing a different filter, want 2 — "+
			"whereFilter must be part of the target identity", got)
	}

	// 6. Same table+filter under a DIFFERENT source is a different target.
	otherSrcKey := timescale.TargetFloorKey("sdex", table, filt)
	if err := store.UpsertCompletenessTargetFloor(ctx, timescale.CompletenessTargetFloor{
		Source: "sdex", Table: table, Filter: filt, VerifiedFrom: 40_000_000,
	}); err != nil {
		t.Fatalf("other-source upsert: %v", err)
	}
	if got := mustFloor(t, ctx, store, otherSrcKey); got != 40_000_000 {
		t.Errorf("other-source floor = %d, want 40000000", got)
	}
	if got := mustFloor(t, ctx, store, key); got != 2 {
		t.Errorf("original floor = %d after writing a different source, want 2", got)
	}
}

func mustFloor(t *testing.T, ctx context.Context, store *timescale.Store, key string) uint32 {
	t.Helper()
	floors, err := store.CompletenessTargetFloors(ctx)
	if err != nil {
		t.Fatalf("CompletenessTargetFloors: %v", err)
	}
	f, ok := floors[key]
	if !ok {
		t.Fatalf("no floor recorded for key %q", key)
	}
	return f.VerifiedFrom
}

// TestDeriveGenerationGuardProtocol_CorrectiveReDerive is the proven-red test
// for the money re-derive trap extended to the PROTOCOL projector tables
// (migration 0110). Without the guard these writers use
// `ON CONFLICT (natural key) DO NOTHING` with the derived value (an i128 amount
// scaled by token decimals, a reserve/supply/shares figure, …) OUTSIDE the
// conflict key, so a corrected re-derive of a wrong money value silently
// no-op'd — the ONLY way to fix it was a destructive DELETE + full re-backfill.
//
// The fix is the same generation-guarded idempotent-corrective upsert 0109
// shipped for the core tables. This test exercises the real seam
// ([Store.SetDeriveGeneration] + the writers) end-to-end against a live
// Postgres for two REPRESENTATIVE targeted tables — blend_positions
// (single-row) and sep41_supply_events (batch) — and asserts:
//
//   - a re-derive at a HIGHER generation (N>0) UPDATEs the wrong value in place
//     — the correction lands. This assertion FAILS on the unfixed DO-NOTHING
//     writers (they keep the original V1).
//   - a subsequent LOWER-generation write (a live gen-0 replay carrying a
//     different value) can NEVER revert the correction — the guard
//     (`derive_generation <= EXCLUDED.derive_generation`) preserves it.
//   - the batch writer dedupes an intra-batch duplicate conflict key (last
//     wins) rather than erroring on Postgres's "cannot affect row a second
//     time" (which the old DO NOTHING absorbed).
//
// scripts/ci/lint-derive-generation-guard is the static, PR-time complement: it enumerates every derive_generation-carrying table from
// migrations/ and every hand-written ON CONFLICT DO UPDATE writer under
// internal/storage/timescale/, so a NEW writer that forgets the guard fails
// CI without needing its own representative-table integration test here.
//
// To reproduce the red state: revert one writer's SQL to
// `ON CONFLICT ... DO NOTHING` (keep migration 0110 + SetDeriveGeneration) and
// that table's "correction lands" assertion goes red.
func TestDeriveGenerationGuardProtocol_CorrectiveReDerive(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	// The wrong original value (V1), the corrected re-derive (V2), and a
	// different value carried by a stale live gen-0 replay (V3, must be ignored).
	const (
		v1 = 12_000_000
		v2 = 99_000_000
		v3 = 55_000_000
	)

	// ── blend_positions: single-row generation-guarded upsert ──────────────
	t.Run("BlendPositions", func(t *testing.T) {
		const (
			pool   = "CDLZFC3SYJYDZT7K67VZ75HPJVIEUVNIXF47ZG2FB2RMQQVU2HHGCYSC"
			asset  = "CC4WPS7HRSPRZAXBVUDYLRXLZRHPLA6VTZARKZJTNVNECAS5IDRXRUB6"
			user   = "GABCDEFGHIJKLMNOPQRSTUVWXYZ234567ABCDEFGHIJKLMNOPQRSTU56K"
			ledger = uint32(60_100_001)
		)
		ts := time.Date(2026, 5, 2, 12, 0, 0, 0, time.UTC)
		txHash := pad64("b", 1)
		// One fixed PK (pool, ledger, tx_hash, op_index, event_kind,
		// event_index, ledger_close_time) so V1/V2/V3 re-derive ONE row.
		mk := func(tokenAmt int64) domain.BlendPositionEvent {
			return domain.BlendPositionEvent{
				Pool:        pool,
				Kind:        domain.BlendEventSupply,
				Asset:       asset,
				User:        user,
				TokenAmount: big.NewInt(tokenAmt),
				BOrDAmount:  big.NewInt(tokenAmt - 1_000),
				Ledger:      ledger,
				TxHash:      txHash,
				OpIndex:     0,
				EventIndex:  0,
				Timestamp:   ts,
			}
		}
		read := func() string {
			var got string
			const q = `SELECT token_amount::text FROM blend_positions WHERE pool = $1 AND ledger = $2`
			if err := store.DB().QueryRowContext(ctx, q, pool, int(ledger)).Scan(&got); err != nil {
				t.Fatalf("read blend_positions: %v", err)
			}
			return got
		}

		// gen 1 — the original (wrong) value lands.
		store.SetDeriveGeneration(1)
		if err := store.InsertBlendPositionEvent(ctx, mk(v1)); err != nil {
			t.Fatalf("InsertBlendPositionEvent V1: %v", err)
		}

		// gen 2 — a corrected re-derive of the SAME PK must UPDATE in place.
		// The unfixed DO-NOTHING writer keeps V1 → this goes red.
		store.SetDeriveGeneration(2)
		if err := store.InsertBlendPositionEvent(ctx, mk(v2)); err != nil {
			t.Fatalf("InsertBlendPositionEvent V2: %v", err)
		}
		if got := read(); got != "99000000" {
			t.Errorf("after gen-2 corrective re-derive: token_amount=%s, want 99000000 "+
				"(INV-3: the old DO NOTHING keeps 12000000)", got)
		}

		// gen 0 — a stale live replay carrying a DIFFERENT value must not revert.
		store.SetDeriveGeneration(0)
		if err := store.InsertBlendPositionEvent(ctx, mk(v3)); err != nil {
			t.Fatalf("InsertBlendPositionEvent V3 (gen 0 replay): %v", err)
		}
		if got := read(); got != "99000000" {
			t.Errorf("after gen-0 replay: token_amount=%s, want 99000000 "+
				"(the generation guard must preserve the correction)", got)
		}
	})

	// ── sep41_supply_events: BATCH generation-guarded upsert + dedup ───────
	t.Run("SEP41SupplyEventBatch", func(t *testing.T) {
		const (
			contractID = "CDLZFC3SYJYDZT7K67VZ75HPJVIEUVNIXF47ZG2FB2RMQQVU2HHGCYSC"
			ledger     = uint32(70_100_001)
		)
		obs := time.Date(2026, 5, 2, 0, 0, 0, 0, time.UTC)
		txHash := pad64("2", 7)
		// One fixed PK (contract_id, ledger, tx_hash, op_index, observed_at,
		// event_kind, event_index) so V1/V2/V3 re-derive ONE row.
		mk := func(amt int64) timescale.SEP41SupplyEvent {
			return timescale.SEP41SupplyEvent{
				ContractID: contractID,
				Ledger:     ledger,
				TxHash:     txHash,
				OpIndex:    0,
				EventIndex: 0,
				ObservedAt: obs,
				Kind:       timescale.SEP41EventMint,
				Amount:     big.NewInt(amt),
			}
		}
		read := func() string {
			var got string
			const q = `SELECT amount::text FROM sep41_supply_events WHERE contract_id = $1 AND ledger = $2`
			if err := store.DB().QueryRowContext(ctx, q, contractID, int(ledger)).Scan(&got); err != nil {
				t.Fatalf("read sep41_supply_events: %v", err)
			}
			return got
		}

		// gen 1 — original (wrong) value.
		store.SetDeriveGeneration(1)
		if err := store.InsertSEP41SupplyEventBatch(ctx, []timescale.SEP41SupplyEvent{mk(v1)}); err != nil {
			t.Fatalf("InsertSEP41SupplyEventBatch V1: %v", err)
		}

		// gen 2 — corrected re-derive of the SAME PK must UPDATE in place.
		store.SetDeriveGeneration(2)
		if err := store.InsertSEP41SupplyEventBatch(ctx, []timescale.SEP41SupplyEvent{mk(v2)}); err != nil {
			t.Fatalf("InsertSEP41SupplyEventBatch V2: %v", err)
		}
		if got := read(); got != "99000000" {
			t.Errorf("after gen-2 batch corrective re-derive: amount=%s, want 99000000 "+
				"(INV-3: the old DO NOTHING keeps 12000000)", got)
		}

		// gen 0 — stale live replay must not revert.
		store.SetDeriveGeneration(0)
		if err := store.InsertSEP41SupplyEventBatch(ctx, []timescale.SEP41SupplyEvent{mk(v3)}); err != nil {
			t.Fatalf("InsertSEP41SupplyEventBatch V3 (gen 0 replay): %v", err)
		}
		if got := read(); got != "99000000" {
			t.Errorf("after gen-0 batch replay: amount=%s, want 99000000 "+
				"(guard must preserve the correction)", got)
		}

		// The DO UPDATE upsert rejects an intra-statement duplicate conflict
		// key ("cannot affect row a second time"), which the old DO NOTHING
		// tolerated. A batch carrying the SAME PK twice must be deduped
		// in-writer, not error — and the last copy must win.
		store.SetDeriveGeneration(3)
		dupA := mk(77_000_000)
		dupB := mk(88_000_000)
		if err := store.InsertSEP41SupplyEventBatch(ctx, []timescale.SEP41SupplyEvent{dupA, dupB}); err != nil {
			t.Fatalf("batch with an intra-batch duplicate PK must dedupe, not error: %v", err)
		}
		if got := read(); got != "88000000" {
			t.Errorf("intra-batch duplicate PK: amount=%s, want 88000000 (last copy wins)", got)
		}
	})
}

// TestDeriveGenerationGuard_CorrectiveReDerive is the proven-red test for
// the money re-derive trap (migration 0109). Without
// the guard the served-tier writers use `ON CONFLICT (...) DO NOTHING` with
// the derived value OUTSIDE the conflict key, so a corrected re-derive of
// a wrong money value silently no-op'd — the ONLY way to fix it was a
// destructive DELETE + full re-backfill.
//
// The fix is a generation-guarded idempotent-corrective upsert. This test
// exercises the real seam ([Store.SetDeriveGeneration] + the writers) and
// asserts, for trades (single + batch) and supply:
//
//   - a re-derive at a HIGHER generation (N>0) UPDATEs the wrong value in
//     place — the correction lands. This assertion FAILS on the unfixed
//     DO-NOTHING writers (they keep the original V1).
//   - a subsequent LOWER-generation write (a live gen-0 replay carrying a
//     different value) can NEVER revert the correction — the guard
//     (`derive_generation <= EXCLUDED.derive_generation`) preserves it.
//
// To reproduce the red state: revert only the writers' SQL to
// `ON CONFLICT ... DO NOTHING` (keep migration 0109 + SetDeriveGeneration)
// and the "correction lands" assertions go red.
func TestDeriveGenerationGuard_CorrectiveReDerive(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	usd, err := canonical.NewFiatAsset("USD")
	if err != nil {
		t.Fatal(err)
	}
	xlm, err := canonical.NewCryptoAsset("XLM")
	if err != nil {
		t.Fatal(err)
	}
	xlmUSD, _ := canonical.NewPair(xlm, usd)

	// One fixed instant so every (source, ledger, tx_hash, op_index, ts)
	// across V1/V2/V3 is the SAME trades PK — a re-derive of one row, not
	// a new row.
	ts := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)

	// The wrong original value (V1), the corrected value (V2), and a
	// different value carried by a stale live replay (V3). Binance +
	// fiat:USD auto-populates usd_volume = quote_amount / 1e8, so the
	// derived money column tracks the quote correction too.
	const (
		v1 = 12_000_000 // wrong original       → usd_volume 0.12
		v2 = 99_000_000 // corrected re-derive   → usd_volume 0.99
		v3 = 55_000_000 // stale live gen-0 value → must be ignored
	)

	readTrade := func(source string, ledger uint32) (quote string, usdVol sql.NullString) {
		const q = `SELECT quote_amount::text, usd_volume::text FROM trades WHERE source = $1 AND ledger = $2`
		if err := store.DB().QueryRowContext(ctx, q, source, ledger).Scan(&quote, &usdVol); err != nil {
			t.Fatalf("read trade (%s, %d): %v", source, ledger, err)
		}
		return quote, usdVol
	}
	wantUSD := func(t *testing.T, uv sql.NullString) {
		t.Helper()
		if !uv.Valid || (uv.String != "0.99000000" && uv.String != "0.99") {
			t.Errorf("usd_volume = %v, want 0.99 (the corrected money value)", uv)
		}
	}

	t.Run("InsertTrade", func(t *testing.T) {
		// gen 1 — the original (wrong) value lands.
		store.SetDeriveGeneration(1)
		tr := mkIntegrationTrade("binance", 7, ts, xlmUSD, 100_000_000, v1)
		if err := store.InsertTrade(ctx, tr); err != nil {
			t.Fatalf("InsertTrade V1: %v", err)
		}

		// gen 2 — a corrected re-derive of the SAME PK must UPDATE in
		// place. Unfixed DO-NOTHING writers keep V1 → this goes red.
		store.SetDeriveGeneration(2)
		trV2 := mkIntegrationTrade("binance", 7, ts, xlmUSD, 100_000_000, v2)
		if err := store.InsertTrade(ctx, trV2); err != nil {
			t.Fatalf("InsertTrade V2: %v", err)
		}
		if q, uv := readTrade("binance", trV2.Ledger); q != "99000000" {
			t.Errorf("after gen-2 corrective re-derive: quote_amount = %s, want 99000000 "+
				"(INV-3: the old DO NOTHING keeps 12000000)", q)
		} else {
			wantUSD(t, uv)
		}

		// gen 0 — a stale live replay carrying a DIFFERENT value must not
		// revert the gen-2 correction.
		store.SetDeriveGeneration(0)
		trV3 := mkIntegrationTrade("binance", 7, ts, xlmUSD, 100_000_000, v3)
		if err := store.InsertTrade(ctx, trV3); err != nil {
			t.Fatalf("InsertTrade V3 (gen 0 replay): %v", err)
		}
		if q, uv := readTrade("binance", trV3.Ledger); q != "99000000" {
			t.Errorf("after gen-0 replay: quote_amount = %s, want 99000000 "+
				"(the generation guard must preserve the correction)", q)
		} else {
			wantUSD(t, uv)
		}
	})

	t.Run("BatchInsertTrades", func(t *testing.T) {
		store.SetDeriveGeneration(1)
		b1 := mkIntegrationTrade("binance", 8, ts, xlmUSD, 100_000_000, v1)
		if err := store.BatchInsertTrades(ctx, []canonical.Trade{b1}); err != nil {
			t.Fatalf("BatchInsertTrades V1: %v", err)
		}

		store.SetDeriveGeneration(2)
		b2 := mkIntegrationTrade("binance", 8, ts, xlmUSD, 100_000_000, v2)
		if err := store.BatchInsertTrades(ctx, []canonical.Trade{b2}); err != nil {
			t.Fatalf("BatchInsertTrades V2: %v", err)
		}
		if q, uv := readTrade("binance", b2.Ledger); q != "99000000" {
			t.Errorf("after gen-2 batch corrective re-derive: quote_amount = %s, want 99000000", q)
		} else {
			wantUSD(t, uv)
		}

		store.SetDeriveGeneration(0)
		b3 := mkIntegrationTrade("binance", 8, ts, xlmUSD, 100_000_000, v3)
		if err := store.BatchInsertTrades(ctx, []canonical.Trade{b3}); err != nil {
			t.Fatalf("BatchInsertTrades V3 (gen 0 replay): %v", err)
		}
		if q, _ := readTrade("binance", b3.Ledger); q != "99000000" {
			t.Errorf("after gen-0 batch replay: quote_amount = %s, want 99000000 "+
				"(guard must preserve the correction)", q)
		}

		// The DO UPDATE upsert rejects an intra-statement duplicate
		// conflict key ("cannot affect row a second time"), which the old
		// DO NOTHING tolerated. A batch carrying the SAME PK twice (CEX WS
		// redelivery) must be deduped in-writer, not error — and the last
		// copy must win.
		store.SetDeriveGeneration(3)
		dupA := mkIntegrationTrade("binance", 8, ts, xlmUSD, 100_000_000, 77_000_000)
		dupB := mkIntegrationTrade("binance", 8, ts, xlmUSD, 100_000_000, 88_000_000)
		if err := store.BatchInsertTrades(ctx, []canonical.Trade{dupA, dupB}); err != nil {
			t.Fatalf("batch with an intra-batch duplicate PK must dedupe, not error: %v", err)
		}
		if q, _ := readTrade("binance", dupB.Ledger); q != "88000000" {
			t.Errorf("intra-batch duplicate PK: quote_amount = %s, want 88000000 (last copy wins)", q)
		}
	})

	t.Run("InsertSupply", func(t *testing.T) {
		const assetKey = "XLM"
		const ledgerSeq = 60_000_000
		obs := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
		mkSupply := func(total, circ int64) supply.Supply {
			return supply.Supply{
				AssetKey:          assetKey,
				TotalSupply:       big.NewInt(total),
				CirculatingSupply: big.NewInt(circ),
				MaxSupply:         big.NewInt(total),
				Basis:             supply.BasisXLMSDFReserveExclusion,
				LedgerSequence:    ledgerSeq,
				ObservedAt:        obs,
			}
		}
		readSupply := func() (total, circ string) {
			const q = `SELECT total_supply::text, circulating_supply::text
			             FROM asset_supply_history WHERE asset_key = $1 AND ledger_sequence = $2`
			if err := store.DB().QueryRowContext(ctx, q, assetKey, int64(ledgerSeq)).Scan(&total, &circ); err != nil {
				t.Fatalf("read supply: %v", err)
			}
			return total, circ
		}

		// gen 1 — original (wrong) supply.
		store.SetDeriveGeneration(1)
		if err := store.InsertSupply(ctx, mkSupply(1000, 900)); err != nil {
			t.Fatalf("InsertSupply V1: %v", err)
		}

		// gen 2 — corrected re-derive of the SAME (asset, ledger, time)
		// must UPDATE both value columns. Unfixed DO NOTHING keeps V1.
		store.SetDeriveGeneration(2)
		if err := store.InsertSupply(ctx, mkSupply(2000, 1800)); err != nil {
			t.Fatalf("InsertSupply V2: %v", err)
		}
		if total, circ := readSupply(); total != "2000" || circ != "1800" {
			t.Errorf("after gen-2 corrective re-derive: total=%s circulating=%s, want 2000/1800 "+
				"(INV-3: the old DO NOTHING keeps 1000/900)", total, circ)
		}

		// gen 0 — stale live replay with different values must not revert.
		store.SetDeriveGeneration(0)
		if err := store.InsertSupply(ctx, mkSupply(3000, 2700)); err != nil {
			t.Fatalf("InsertSupply V3 (gen 0 replay): %v", err)
		}
		if total, circ := readSupply(); total != "2000" || circ != "1800" {
			t.Errorf("after gen-0 replay: total=%s circulating=%s, want 2000/1800 "+
				"(guard must preserve the correction)", total, circ)
		}
	})
}

// TestDiscoveryRoundTrip exercises the SEP-41 auto-discovery
// storage layer end-to-end: RecordDiscovered → IsKnownDiscovered →
// ListDiscovered, including the ON CONFLICT update path that
// preserves first_seen_* and bumps event_count.
func TestDiscoveryRoundTrip(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	const contractA = "CAS3J7GYLGXMF6TDJBBYYSE3HQ6BBSMLNUQ34T6TZMYMW2EVH34XOWMA"
	const contractB = "CBCZGGNOEUZG4CAAE7TGTQQHETZMKUT4OIPFHHPKEUX46U4KXBBZ3GLH"

	// Empty table: IsKnownDiscovered=false, ListDiscovered returns []
	known, err := store.IsKnownDiscovered(ctx, contractA)
	if err != nil {
		t.Fatalf("IsKnownDiscovered (empty): %v", err)
	}
	if known {
		t.Error("IsKnownDiscovered=true for empty table")
	}

	if rows, err := store.ListDiscovered(ctx, 0); err != nil || len(rows) != 0 {
		t.Errorf("ListDiscovered (empty): %d rows, err=%v", len(rows), err)
	}

	// First Record for contract A — mint event at ledger 50_000_000.
	hitA1 := discovery.Hit{
		ContractID:        contractA,
		EventType:         discovery.EventMint,
		Ledger:            50_000_000,
		ObservedAtRFC3339: "2026-04-01T12:00:00Z",
	}
	if err := store.RecordDiscovered(ctx, hitA1); err != nil {
		t.Fatalf("RecordDiscovered (first): %v", err)
	}

	// Second Record for SAME contract — transfer event at later
	// ledger. Must update last_seen_*, increment event_count, NOT
	// overwrite first_seen_event.
	hitA2 := hitA1
	hitA2.EventType = discovery.EventTransfer
	hitA2.Ledger = 50_000_500
	hitA2.ObservedAtRFC3339 = "2026-04-02T12:00:00Z"
	if err := store.RecordDiscovered(ctx, hitA2); err != nil {
		t.Fatalf("RecordDiscovered (second): %v", err)
	}

	// IsKnownDiscovered now true for A.
	known, _ = store.IsKnownDiscovered(ctx, contractA)
	if !known {
		t.Error("IsKnownDiscovered=false after Record")
	}

	// Record contract B — different contract.
	hitB := discovery.Hit{
		ContractID:        contractB,
		EventType:         discovery.EventBurn,
		Ledger:            50_001_000,
		ObservedAtRFC3339: "2026-04-03T12:00:00Z",
	}
	if err := store.RecordDiscovered(ctx, hitB); err != nil {
		t.Fatalf("RecordDiscovered (B): %v", err)
	}

	// ListDiscovered: 2 rows, B before A (newest first_seen_at first).
	rows, err := store.ListDiscovered(ctx, 0)
	if err != nil {
		t.Fatalf("ListDiscovered: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("ListDiscovered returned %d rows, want 2", len(rows))
	}
	if rows[0].ContractID != contractB {
		t.Errorf("rows[0] = %q, want contract B (newest first)", rows[0].ContractID)
	}

	// Find the row for A and verify ON CONFLICT semantics.
	var rowA *timescale.DiscoveredAsset
	for i := range rows {
		if rows[i].ContractID == contractA {
			rowA = &rows[i]
		}
	}
	if rowA == nil {
		t.Fatal("contract A missing from ListDiscovered")
	}
	// First-write-wins on first_seen_*.
	if rowA.FirstSeenEvent != discovery.EventMint {
		t.Errorf("FirstSeenEvent = %q, want %q (first-write-wins)", rowA.FirstSeenEvent, discovery.EventMint)
	}
	if rowA.FirstSeenLedger != 50_000_000 {
		t.Errorf("FirstSeenLedger = %d, want 50_000_000", rowA.FirstSeenLedger)
	}
	// Last-write-wins on last_seen_*.
	if rowA.LastSeenLedger != 50_000_500 {
		t.Errorf("LastSeenLedger = %d, want 50_000_500", rowA.LastSeenLedger)
	}
	// event_count incremented to 2 (two Records).
	if rowA.EventCount != 2 {
		t.Errorf("EventCount = %d, want 2", rowA.EventCount)
	}

	// Limit clamps the result.
	rows, err = store.ListDiscovered(ctx, 1)
	if err != nil || len(rows) != 1 {
		t.Errorf("ListDiscovered(limit=1): %d rows, err=%v", len(rows), err)
	}
}

// TestDiscoveryRoundTrip_EventCountDelta is the CA2-A10-correct-4
// regression at the storage layer: a Hit with Count set (as
// AsyncSink.flushPending produces when it flushes an accumulated
// in-process-dedup delta) must increment event_count by that many,
// not by a flat 1 — otherwise event_count/last_seen_ledger only ever
// reflect the first observation per process lifetime, not the true
// event volume.
func TestDiscoveryRoundTrip_EventCountDelta(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	const contractC = "CCCZGGNOEUZG4CAAE7TGTQQHETZMKUT4OIPFHHPKEUX46U4KXBBZ3GLH"

	first := discovery.Hit{
		ContractID:        contractC,
		EventType:         discovery.EventTransfer,
		Ledger:            60_000_000,
		ObservedAtRFC3339: "2026-05-01T00:00:00Z",
	}
	if err := store.RecordDiscovered(ctx, first); err != nil {
		t.Fatalf("RecordDiscovered (first): %v", err)
	}

	// Simulates AsyncSink.flushPending: one call carrying the true
	// accumulated count of 999 skipped repeat observations, stamped
	// with the LATEST ledger/time, not the first.
	delta := discovery.Hit{
		ContractID:        contractC,
		EventType:         discovery.EventTransfer,
		Ledger:            60_004_999,
		ObservedAtRFC3339: "2026-05-01T02:00:00Z",
		Count:             999,
	}
	if err := store.RecordDiscovered(ctx, delta); err != nil {
		t.Fatalf("RecordDiscovered (delta): %v", err)
	}

	rows, err := store.ListDiscovered(ctx, 0)
	if err != nil {
		t.Fatalf("ListDiscovered: %v", err)
	}
	var row *timescale.DiscoveredAsset
	for i := range rows {
		if rows[i].ContractID == contractC {
			row = &rows[i]
		}
	}
	if row == nil {
		t.Fatal("contract C missing from ListDiscovered")
	}
	if row.EventCount != 1000 {
		t.Errorf("EventCount = %d, want 1000 (1 + delta of 999, not a flat +1)", row.EventCount)
	}
	if row.LastSeenLedger != 60_004_999 {
		t.Errorf("LastSeenLedger = %d, want 60_004_999 (the delta's true last-observed ledger)", row.LastSeenLedger)
	}
}

// TestCountDistinctLedgersSorobanEventsReadsCensus is the DB-backed
// proof that the soroban-events density
// numerator is answered by the ledger_ingest_log census (PK range scan)
// and NOT by a scan of soroban_events. The fixture leaves soroban_events
// EMPTY and writes a census with a known number of event-carrying
// ledgers in the window; a scan of observed rows would count 0, and the
// census count is the right one. A non-overridden target over the same
// table proves the generic path is untouched.
func TestCountDistinctLedgersSorobanEventsReadsCensus(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	// Ledgers 1000..1009: seven carry Soroban events, three are quiet.
	// Plus one event-carrying ledger OUTSIDE the window (1010) that a
	// correct BETWEEN must exclude.
	quiet := map[uint32]bool{1002: true, 1005: true, 1008: true}
	hash := func(seq uint32) []byte {
		h := make([]byte, 32)
		h[0], h[1], h[2], h[3] = byte(seq>>24), byte(seq>>16), byte(seq>>8), byte(seq)
		return h
	}
	for seq := uint32(1000); seq <= 1010; seq++ {
		n := 3
		if quiet[seq] {
			n = 0
		}
		if err := store.UpsertLedgerIngestLog(ctx, timescale.LedgerIngestRow{
			LedgerSeq:         seq,
			LedgerCloseTime:   time.Date(2026, 8, 28, 18, 0, int(seq-1000)*5, 0, time.UTC),
			LedgerHash:        hash(seq),
			PrevLedgerHash:    hash(seq - 1),
			SorobanEventCount: n,
		}); err != nil {
			t.Fatalf("UpsertLedgerIngestLog(%d): %v", seq, err)
		}
	}

	var sorobanTarget timescale.GapDetectorTarget
	for _, target := range timescale.DefaultGapDetectorTargets {
		if target.Source == "soroban-events" {
			sorobanTarget = target
		}
	}
	if sorobanTarget.Table != "soroban_events" {
		t.Fatalf("soroban-events target not registered: %+v", sorobanTarget)
	}

	got, err := store.CountDistinctLedgers(ctx, sorobanTarget, 1000, 1009)
	if err != nil {
		t.Fatalf("CountDistinctLedgers(soroban-events): %v", err)
	}
	if want := int64(7); got != want {
		t.Errorf("soroban-events distinct ledgers = %d; want %d (ledger_ingest_log census: 10 in window, 3 quiet). "+
			"0 means the count still reads the (empty) soroban_events hypertable", got, want)
	}

	// Differential: the generic path over the same table, no override —
	// counts distinct ledger_seq rows in the window regardless of census.
	generic := timescale.GapDetectorTarget{Source: "census-rows", Table: "ledger_ingest_log", LedgerColumn: "ledger_seq"}
	got, err = store.CountDistinctLedgers(ctx, generic, 1000, 1009)
	if err != nil {
		t.Fatalf("CountDistinctLedgers(generic): %v", err)
	}
	if want := int64(10); got != want {
		t.Errorf("generic COUNT(DISTINCT) = %d; want %d", got, want)
	}
}

// TestFindPerSourceLedgerGapsSeedClosesWindowBoundary is the DB-backed
// proof that a gap detector cycle only scans
// [from, tip], so a writer that halts and resumes later can leave its
// last pre-halt row in one cycle's window and its first post-resume row
// in the NEXT cycle's window — two disjoint LAG-over-DISTINCT scans,
// neither of which ever sees both endpoints, so the gap is never
// reported at all. Seeding the scan with the previous cycle's
// highest-observed ledger restores the pairing across that boundary.
//
// Fixture: a writer active at ledgers 1000-1005, then silent, then
// active again at 81005-81010 (an 80,000-ledger halt). Cycle A scans
// [1000, 41000] and only sees the first burst — nothing to pair, no
// gap, correctly so (nothing is missing INSIDE that window). Cycle B
// scans [41001, 81010] and only sees the second burst: with no seed,
// the resumed rows never pair with the pre-halt rows and the 80,000-
// ledger gap is silently dropped forever, exactly as CA2-A10's
// blend_positions walkthrough describes. With B seeded from A's
// highest-observed ledger (1005, persisted as the "last present"
// cursor), the same window pairs the seed against 81005 and reports
// the gap.
func TestFindPerSourceLedgerGapsSeedClosesWindowBoundary(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	hash := func(seq uint32) []byte {
		h := make([]byte, 32)
		h[0], h[1], h[2], h[3] = byte(seq>>24), byte(seq>>16), byte(seq>>8), byte(seq)
		return h
	}
	writeLedger := func(seq uint32) {
		t.Helper()
		if err := store.UpsertLedgerIngestLog(ctx, timescale.LedgerIngestRow{
			LedgerSeq:         seq,
			LedgerCloseTime:   time.Date(2026, 9, 1, 0, 0, int(seq), 0, time.UTC),
			LedgerHash:        hash(seq),
			PrevLedgerHash:    hash(seq - 1),
			SorobanEventCount: 1,
		}); err != nil {
			t.Fatalf("UpsertLedgerIngestLog(%d): %v", seq, err)
		}
	}
	for _, seq := range []uint32{1000, 1001, 1002, 1003, 1004, 1005} {
		writeLedger(seq)
	}
	for _, seq := range []uint32{81005, 81006, 81007, 81008, 81009, 81010} {
		writeLedger(seq)
	}

	target := timescale.GapDetectorTarget{Source: "test-seed-src", Table: "ledger_ingest_log", LedgerColumn: "ledger_seq"}
	const minGapSize = int64(50000)

	// Cycle A: [1000, 41000]. Only the first burst is in range — no gap.
	gapsA, err := store.FindPerSourceLedgerGaps(ctx, target, 1000, 41000, minGapSize, 0)
	if err != nil {
		t.Fatalf("cycle A: %v", err)
	}
	if len(gapsA) != 0 {
		t.Fatalf("cycle A found %d gaps; want 0 (nothing missing inside [1000,41000])", len(gapsA))
	}

	// Cycle B, UNSEEDED (seed=0): reproduces today's behaviour. Only the
	// resumed burst is in range, so there is no pairing at all and the
	// 80,000-ledger halt is invisible.
	gapsBUnseeded, err := store.FindPerSourceLedgerGaps(ctx, target, 41001, 81010, minGapSize, 0)
	if err != nil {
		t.Fatalf("cycle B unseeded: %v", err)
	}
	if len(gapsBUnseeded) != 0 {
		t.Fatalf("cycle B unseeded found %d gaps; want 0 — this pins the DEFECT (a real 80,000-ledger halt reads as clean)", len(gapsBUnseeded))
	}

	// Cycle B, SEEDED with the highest ledger cycle A actually observed
	// (1005 — obtainable via MaxLedgerInWindow, exactly what the gap
	// detector persists between cycles).
	maxA, ok, err := store.MaxLedgerInWindow(ctx, target, 1000, 41000)
	if err != nil {
		t.Fatalf("MaxLedgerInWindow: %v", err)
	}
	if !ok || maxA != 1005 {
		t.Fatalf("MaxLedgerInWindow(cycle A window) = (%d,%v); want (1005,true)", maxA, ok)
	}

	gapsBSeeded, err := store.FindPerSourceLedgerGaps(ctx, target, 41001, 81010, minGapSize, maxA)
	if err != nil {
		t.Fatalf("cycle B seeded: %v", err)
	}
	if len(gapsBSeeded) != 1 {
		t.Fatalf("cycle B seeded found %d gaps; want exactly 1 (the fix must recover the boundary-spanning halt)", len(gapsBSeeded))
	}
	g := gapsBSeeded[0]
	if g.Start != 1006 || g.End != 81004 {
		t.Errorf("gap = [%d,%d]; want [1006,81004]", g.Start, g.End)
	}
	if want := int64(81004 - 1006 + 1); g.Size != want {
		t.Errorf("gap size = %d; want %d", g.Size, want)
	}
}

// TestFindPerSourceLedgerGapsSorobanEventsCloseTimeBound is the DB-backed
// proof that soroban_events is partitioned by ledger_close_time,
// so the gap scan bounds that column by the close times ledger_ingest_log
// records around [from, to]. The bound must stay correct when the census
// has holes (the scan runs exactly when coverage may be broken): anchors
// are the nearest logged ledgers outside the window, and a missing anchor
// leaves that side open.
func TestFindPerSourceLedgerGapsSorobanEventsCloseTimeBound(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	// 5 s per ledger: 400,000 ledgers span ~23 days, i.e. several of
	// soroban_events' 7-day chunks.
	genesis := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	closeTime := func(seq uint32) time.Time { return genesis.Add(time.Duration(seq) * 5 * time.Second) }
	hash := func(seq uint32) []byte {
		h := make([]byte, 32)
		h[0], h[1], h[2], h[3] = byte(seq>>24), byte(seq>>16), byte(seq>>8), byte(seq)
		return h
	}
	event := func(seq uint32, at time.Time) domain.SorobanEventRow {
		return domain.SorobanEventRow{
			Ledger: seq, LedgerCloseTime: at, TxHash: hash(seq),
			ContractID: "CTEST", ContractIDHex: hash(1), TopicCount: 1,
			Topic0XDR: []byte{0}, TopicsXDR: [][]byte{{0}}, BodyXDR: []byte{0},
		}
	}

	// Events at 1000 and 201005-201006: one 200,004-ledger gap [1001, 201004].
	if err := store.InsertSorobanEventsBatch(ctx, []domain.SorobanEventRow{
		event(1000, closeTime(1000)), event(201005, closeTime(201005)), event(201006, closeTime(201006)),
	}); err != nil {
		t.Fatalf("InsertSorobanEventsBatch: %v", err)
	}
	// Sparse census: nothing below 1000, a hole across the whole gap.
	for _, seq := range []uint32{1000, 201005, 400000} {
		if err := store.UpsertLedgerIngestLog(ctx, timescale.LedgerIngestRow{
			LedgerSeq: seq, LedgerCloseTime: closeTime(seq),
			LedgerHash: hash(seq), PrevLedgerHash: hash(seq - 1), SorobanEventCount: 1,
		}); err != nil {
			t.Fatalf("UpsertLedgerIngestLog(%d): %v", seq, err)
		}
	}

	var target timescale.GapDetectorTarget
	for _, tg := range timescale.DefaultGapDetectorTargets {
		if tg.Table == "soroban_events" {
			target = tg
		}
	}
	if target.CloseTimeColumn == "" {
		t.Fatalf("soroban_events target has no CloseTimeColumn: %+v", target)
	}
	const minGap = int64(100000)

	assertOneGap := func(name string, from, to int64, wantStart, wantEnd int64) {
		t.Helper()
		gaps, err := store.FindPerSourceLedgerGaps(ctx, target, from, to, minGap, 0)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(gaps) != 1 || gaps[0].Start != wantStart || gaps[0].End != wantEnd {
			t.Fatalf("%s [%d,%d]: gaps = %+v; want exactly [%d,%d]", name, from, to, gaps, wantStart, wantEnd)
		}
	}

	// Anchors exactly at the window edges: the bound is inclusive, so the
	// edge rows that bracket the gap stay in the scan.
	assertOneGap("exact anchors", 1000, 201005, 1001, 201004)
	// No logged ledger at `to` (the tip is not yet in the census): the
	// next logged ledger above it anchors the upper side.
	assertOneGap("upper anchor beyond window", 1000, 300000, 1001, 201004)
	// No logged ledger at or below `from`: lower side unbounded.
	assertOneGap("no lower anchor", 500, 201006, 1001, 201004)

	// The scan reads only rows inside the enclosing close-time window. A
	// row at ledger 100000 stamped years before its anchors is outside it;
	// were it read, it would split the gap into [1001,99999] (below
	// minGap) and [100001,201004].
	if err := store.InsertSorobanEventsBatch(ctx, []domain.SorobanEventRow{
		event(100000, time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)),
	}); err != nil {
		t.Fatalf("InsertSorobanEventsBatch(out-of-window row): %v", err)
	}
	assertOneGap("close-time bound applied", 1000, 201005, 1001, 201004)
}

// TestInstanceLock_OneHolderPerDatabase drives the instance lock against
// real Postgres, two stores standing in for two processes: the second is
// refused while the first holds it, a different name is independent,
// and both a release and the holder's session dying (a crash) free it.
func TestInstanceLock_OneHolderPerDatabase(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	dsn := startTimescale(t, ctx)
	open := func() *timescale.Store {
		s, err := timescale.Open(ctx, dsn)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = s.Close() })
		return s
	}
	first, second := open(), open()
	logger := slog.New(slog.DiscardHandler)
	hold := func(s *timescale.Store, name string) (*timescale.InstanceLock, error) {
		return s.HoldInstanceLock(ctx, name, func() {}, logger)
	}

	held, err := hold(first, timescale.IndexerInstanceLockName)
	if err != nil {
		t.Fatalf("first indexer: %v", err)
	}
	if _, err := hold(second, timescale.IndexerInstanceLockName); !errors.Is(err, timescale.ErrInstanceLockHeld) {
		t.Fatalf("second indexer while the first runs: err = %v, want ErrInstanceLockHeld", err)
	}
	agg, err := hold(second, timescale.AggregatorInstanceLockName)
	if err != nil {
		t.Fatalf("aggregator beside the indexer: %v (names must be independent)", err)
	}
	if err := agg.Release(ctx); err != nil {
		t.Fatalf("aggregator release: %v", err)
	}

	if err := held.Release(ctx); err != nil {
		t.Fatalf("release: %v", err)
	}
	held, err = hold(second, timescale.IndexerInstanceLockName)
	if err != nil {
		t.Fatalf("after release: %v", err)
	}

	// The holder crashes: its session ends and the lock must go with it.
	var terminated int
	if err := first.DB().QueryRowContext(ctx, `
		SELECT count(pg_terminate_backend(pid)) FROM pg_locks
		WHERE locktype = 'advisory' AND granted AND pid <> pg_backend_pid()
		  AND pid IN (SELECT pid FROM pg_stat_activity WHERE backend_type = 'client backend')`).Scan(&terminated); err != nil || terminated != 1 {
		t.Fatalf("fixture: terminate the holding session: terminated=%d err=%v", terminated, err)
	}
	var took *timescale.InstanceLock
	deadline := time.Now().Add(10 * time.Second)
	for {
		took, err = hold(first, timescale.IndexerInstanceLockName)
		if err == nil || !errors.Is(err, timescale.ErrInstanceLockHeld) || time.Now().After(deadline) {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("after the holder's session died: %v", err)
	}
	if err := took.Release(ctx); err != nil {
		t.Fatalf("release: %v", err)
	}
	_ = held.Release(ctx) // its session is gone; the unlock error is expected
}

// 0051's stored table comment, verbatim; 0182's down must restore exactly this.
const ledgerIngestLogComment0051 = `Substrate-continuity record (ADR-0033). One row per fully-` +
	`processed ledger, written post-persist. soroban_event_count / ` +
	`classic_trade_effect_count are LCM-derived checksums reconciled ` +
	`against soroban_events / trades. Contiguity + hash-chain over ` +
	`this table is Claim 1 of the completeness model.`

// TestLedgerIngestLogComment pins the catalog comments on ledger_ingest_log
// after migration 0182 up and down: the indexer writes the row
// after ENQUEUE to the async sink, so `\d+` must not call it post-persist.
func TestLedgerIngestLogComment(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()

	table, col := ledgerIngestLogComments(t, ctx, db)
	for name, got := range map[string]sql.NullString{"table": table, "persisted_at": col} {
		if !got.Valid {
			t.Fatalf("ledger_ingest_log %s has no catalog comment", name)
		}
		if !strings.Contains(strings.ToLower(got.String), "enqueued") || !strings.Contains(got.String, "census-backfill") {
			t.Errorf("%s comment does not name the enqueue/census-backfill writers; got %q", name, got.String)
		}
		if strings.Contains(got.String, "post-persist") {
			t.Errorf("%s comment still claims post-persist; got %q", name, got.String)
		}
	}
	if !strings.Contains(table.String, "NOT a persistence marker") {
		t.Errorf("table comment does not disclaim persistence; got %q", table.String)
	}

	_, thisFile, _, _ := runtime.Caller(0)
	migrationsDir := filepath.Join(filepath.Dir(thisFile), "..", "..", "migrations")
	m, err := migrate.New("file://"+migrationsDir, dsn)
	if err != nil {
		t.Fatalf("migrate.New: %v", err)
	}
	defer func() { _, _ = m.Close() }()
	if err := m.Migrate(181); err != nil {
		t.Fatalf("migrate down to 181: %v", err)
	}
	table, col = ledgerIngestLogComments(t, ctx, db)
	if table.String != ledgerIngestLogComment0051 {
		t.Errorf("0182 down did not restore 0051's table comment verbatim:\n got %q\nwant %q", table.String, ledgerIngestLogComment0051)
	}
	if col.Valid {
		t.Errorf("0182 down left a persisted_at comment 0051 never set: %q", col.String)
	}
}

func ledgerIngestLogComments(t *testing.T, ctx context.Context, db *sql.DB) (table, col sql.NullString) {
	t.Helper()
	if err := db.QueryRowContext(ctx,
		`SELECT obj_description('ledger_ingest_log'::regclass, 'pg_class'),
		        col_description('ledger_ingest_log'::regclass, a.attnum)
		   FROM pg_attribute a
		  WHERE a.attrelid = 'ledger_ingest_log'::regclass AND a.attname = 'persisted_at'`,
	).Scan(&table, &col); err != nil {
		t.Fatalf("read ledger_ingest_log comments: %v", err)
	}
	return table, col
}

// ledgerHashFor builds a deterministic 32-byte hash for a sequence so
// a contiguous run forms a valid chain: row[seq].prev_ledger_hash ==
// ledgerHashFor(seq-1) == row[seq-1].ledger_hash.
func ledgerHashFor(seq uint32) []byte {
	h := make([]byte, 32)
	h[0] = byte(seq)
	h[1] = byte(seq >> 8)
	h[2] = byte(seq >> 16)
	h[3] = byte(seq >> 24)
	h[31] = 0xC0
	return h
}

// TestLedgerIngestLog exercises the ADR-0033 Phase 2 substrate
// queries against real TimescaleDB: upsert (insert + update path),
// gap detection (interior + leading + trailing boundaries), hash-chain
// verification (clean + injected break), and extent.
func TestLedgerIngestLog(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	t0 := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	insert := func(seq uint32) {
		t.Helper()
		row := timescale.LedgerIngestRow{
			LedgerSeq:               seq,
			LedgerCloseTime:         t0.Add(time.Duration(seq) * time.Second),
			LedgerHash:              ledgerHashFor(seq),
			PrevLedgerHash:          ledgerHashFor(seq - 1),
			SorobanEventCount:       int(seq % 7),
			ClassicTradeEffectCount: int(seq % 3),
		}
		if err := store.UpsertLedgerIngestLog(ctx, row); err != nil {
			t.Fatalf("UpsertLedgerIngestLog(%d): %v", seq, err)
		}
	}

	// Present: 100..109 and 113..115 — interior gap [110,112].
	for s := uint32(100); s <= 109; s++ {
		insert(s)
	}
	for s := uint32(113); s <= 115; s++ {
		insert(s)
	}

	// ─── Gaps over [100,120]: interior [110,112], trailing [116,120].
	gaps, err := store.FindLedgerIngestGaps(ctx, 100, 120)
	if err != nil {
		t.Fatalf("FindLedgerIngestGaps([100,120]): %v", err)
	}
	wantGaps := []timescale.LedgerGap{
		{Start: 110, End: 112, Size: 3},
		{Start: 116, End: 120, Size: 5},
	}
	assertGaps(t, "[100,120]", gaps, wantGaps)

	// ─── Gaps over [98,115]: leading [98,99], interior [110,112].
	gaps, err = store.FindLedgerIngestGaps(ctx, 98, 115)
	if err != nil {
		t.Fatalf("FindLedgerIngestGaps([98,115]): %v", err)
	}
	assertGaps(t, "[98,115]", gaps, []timescale.LedgerGap{
		{Start: 98, End: 99, Size: 2},
		{Start: 110, End: 112, Size: 3},
	})

	// ─── Hash chain over the present runs: clean (only adjacent pairs
	// both present are checked, so 109→110 and 112→113 boundaries are
	// not chain-checked here — that's FindLedgerIngestGaps's job).
	breaks, err := store.VerifyLedgerHashChain(ctx, 100, 115)
	if err != nil {
		t.Fatalf("VerifyLedgerHashChain: %v", err)
	}
	if len(breaks) != 0 {
		t.Errorf("clean chain: got %d breaks, want 0: %+v", len(breaks), breaks)
	}

	// ─── Inject a break by UPDATING 105's prev to a wrong value
	// (also exercises the ON CONFLICT DO UPDATE path).
	if err := store.UpsertLedgerIngestLog(ctx, timescale.LedgerIngestRow{
		LedgerSeq:       105,
		LedgerCloseTime: t0.Add(105 * time.Second),
		LedgerHash:      ledgerHashFor(105),
		PrevLedgerHash:  ledgerHashFor(999), // wrong — does not match 104's hash
	}); err != nil {
		t.Fatalf("UpsertLedgerIngestLog(105 update): %v", err)
	}
	breaks, err = store.VerifyLedgerHashChain(ctx, 100, 115)
	if err != nil {
		t.Fatalf("VerifyLedgerHashChain (after break): %v", err)
	}
	if len(breaks) != 1 || breaks[0].LedgerSeq != 105 {
		t.Fatalf("expected exactly one break at 105, got %+v", breaks)
	}

	// ─── Classic-trade-effect census (SDEX reconciliation, Phase 5).
	// Inserted rows carry ClassicTradeEffectCount = seq%3; only >0 are
	// returned. 105's update above left its count at 0 (already absent
	// since 105%3==0), so it doesn't affect this.
	census, err := store.ClassicTradeEffectCountsByLedger(ctx, 100, 115)
	if err != nil {
		t.Fatalf("ClassicTradeEffectCountsByLedger: %v", err)
	}
	if census[100] != 1 || census[101] != 2 || census[113] != 2 || census[115] != 1 {
		t.Errorf("census sample wrong: 100=%d(want1) 101=%d(want2) 113=%d(want2) 115=%d(want1)",
			census[100], census[101], census[113], census[115])
	}
	if _, present := census[102]; present { // 102%3==0 → omitted
		t.Errorf("census should omit ledger 102 (zero trade effects), got %d", census[102])
	}
	// 100,101,103,104,106,107,109,113,115 have seq%3>0 → 9 entries.
	if len(census) != 9 {
		t.Errorf("census has %d entries, want 9", len(census))
	}

	// ─── Extent.
	lo, hi, ok, err := store.LedgerIngestExtent(ctx)
	if err != nil {
		t.Fatalf("LedgerIngestExtent: %v", err)
	}
	if !ok || lo != 100 || hi != 115 {
		t.Errorf("LedgerIngestExtent = (%d,%d,%v), want (100,115,true)", lo, hi, ok)
	}

	// ─── SorobanEventsTimeBound (chunk-pruning helper): fully-covered
	// contiguous range reports covered=true with the exact close-time
	// span; a range with gaps reports covered=false.
	lo2, hi2, covered, err := store.SorobanEventsTimeBound(ctx, 100, 109)
	if err != nil {
		t.Fatalf("SorobanEventsTimeBound [100,109]: %v", err)
	}
	if !covered {
		t.Errorf("SorobanEventsTimeBound [100,109]: covered=false, want true (contiguous)")
	}
	if !lo2.Equal(t0.Add(100*time.Second)) || !hi2.Equal(t0.Add(109*time.Second)) {
		t.Errorf("SorobanEventsTimeBound [100,109] span = [%s,%s], want [+100s,+109s]", lo2, hi2)
	}
	if _, _, covered2, err := store.SorobanEventsTimeBound(ctx, 100, 120); err != nil || covered2 {
		t.Errorf("SorobanEventsTimeBound [100,120]: covered=%v err=%v, want covered=false (has gaps)", covered2, err)
	}

	// ─── Completeness snapshot round-trip (Phase 6): insert, update
	// (idempotent), list.
	if err := store.UpsertCompletenessSnapshot(ctx, timescale.CompletenessSnapshot{
		Source: "soroswap", Genesis: 100, Tip: 200, Watermark: 175,
		CoveragePct: 0.75, Complete: false, FirstProblem: 176,
		SubstrateOK: true, RecognitionOK: true, ProjectionOK: false, Detail: "projection: 1 mismatch",
	}); err != nil {
		t.Fatalf("UpsertCompletenessSnapshot: %v", err)
	}
	if err := store.UpsertCompletenessSnapshot(ctx, timescale.CompletenessSnapshot{
		Source: "soroswap", Genesis: 100, Tip: 200, Watermark: 180,
		CoveragePct: 0.80, Complete: false, FirstProblem: 181,
		ProjectionVerifiedFrom: 150,
		SubstrateOK:            true, RecognitionOK: true, ProjectionOK: false, Detail: "projection: 1 mismatch",
	}); err != nil {
		t.Fatalf("UpsertCompletenessSnapshot (update): %v", err)
	}
	snaps, err := store.ListCompletenessSnapshots(ctx)
	if err != nil {
		t.Fatalf("ListCompletenessSnapshots: %v", err)
	}
	var found bool
	for _, sn := range snaps {
		if sn.Source != "soroswap" {
			continue
		}
		found = true
		// ProjectionVerifiedFrom (migration 0155) round-trips: the
		// projection axis's floor is the bottom of the range
		// ProjectionOK is a claim about, and Genesis (100 here) is the
		// LAKE axis's floor — reading the second for the first is the
		// overstatement the column exists to close.
		if sn.Watermark != 180 || sn.CoveragePct != 0.80 || sn.ProjectionOK || sn.FirstProblem != 181 || sn.ProjectionVerifiedFrom != 150 {
			t.Errorf("snapshot = %+v, want Watermark=180 CoveragePct=0.80 ProjectionOK=false FirstProblem=181 ProjectionVerifiedFrom=150", sn)
		}
		if sn.ComputedAt.IsZero() {
			t.Error("ComputedAt is zero, want now()")
		}
	}
	if !found {
		t.Error("ListCompletenessSnapshots missing soroswap row")
	}
}

func assertGaps(t *testing.T, label string, got, want []timescale.LedgerGap) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: got %d gaps %+v, want %d %+v", label, len(got), got, len(want), want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("%s: gap[%d] = %+v, want %+v", label, i, got[i], want[i])
		}
	}
}

// TestSinkShutdownDrain_PersistsAllInFlight proves that a shutdown
// (parent ctx cancelled) with N events already in flight on the sink channel
// must persist ALL N — none dropped. A persistWorker select without the drain could
// pick the shutdown arm over draining a buffered event, and the drain-timeout
// path counted-and-dropped the remainder instead of persisting it.
//
// This buffers N distinct trades, cancels the parent ctx, then runs
// PersistEvents against a REAL store and asserts every trade landed.
func TestSinkShutdownDrain_PersistsAllInFlight(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	usdc, err := canonical.NewClassicAsset("USDC", "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN")
	if err != nil {
		t.Fatalf("NewClassicAsset: %v", err)
	}
	pair, err := canonical.NewPair(canonical.NativeAsset(), usdc)
	if err != nil {
		t.Fatalf("NewPair: %v", err)
	}

	const n = 200
	ts := time.Now().UTC().Truncate(time.Second).Add(-time.Hour)

	// Buffer all N trades on the channel BEFORE starting the sink, then cancel
	// the parent ctx, so every worker's first select sees ctx.Done ready with
	// the channel full — the exact race the fix must survive.
	in := make(chan consumer.Event, n)
	for i := 0; i < n; i++ {
		txHash := fmt.Sprintf("%064x", i) // 64 hex chars, distinct per trade
		in <- sdex.TradeEvent{Trade: canonical.Trade{
			Source:      "test-shutdown-drain",
			Ledger:      uint32(70_000_000 + i),
			TxHash:      txHash,
			OpIndex:     0,
			Timestamp:   ts,
			Pair:        pair,
			BaseAmount:  canonical.NewAmount(big.NewInt(1_000_000_000)),
			QuoteAmount: canonical.NewAmount(big.NewInt(12_000_000)),
		}}
	}

	sinkCtx, sinkCancel := context.WithCancel(ctx)
	sinkCancel() // shutdown requested with N events already buffered
	close(in)    // producer done — lets the blocking drain exit cleanly

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	done := make(chan struct{})
	go func() {
		defer close(done)
		pipeline.PersistEvents(sinkCtx, logger, store, in, pipeline.SinkModeAll, nil)
	}()

	select {
	case <-done:
	case <-time.After(90 * time.Second):
		t.Fatal("PersistEvents did not return within 90s after ctx cancel + channel close")
	}

	got := countTrades(t, store, "test-shutdown-drain")
	if got != n {
		t.Fatalf("persisted trades = %d, want %d — shutdown dropped %d in-flight events (C2-17 regression)", got, n, n-got)
	}
}

// TestSourceEntryCounts_RouterBumpFollowsTheLandedInsert is the DB-backed
// twin of pipeline.TestHandleEvent_EntryCountFollowsTheLandedInsert: the
// soroswap-router `entries` tally moves only when a row LANDS. A row the
// store rejects contributes nothing, and a landed row contributes exactly
// one — the bump must not run ahead of the insert, or a rejected row (or
// every infra-retry attempt of one event) inflates the tally.
func TestSourceEntryCounts_RouterBumpFollowsTheLandedInsert(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	count := func() int64 {
		m, err := store.SourceEntryCounts(ctx)
		if err != nil {
			t.Fatalf("SourceEntryCounts: %v", err)
		}
		return m[soroswap_router.SourceName]
	}

	const (
		pathHopA  = "CAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA1"
		pathHopB  = "CAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA2"
		recipient = "GAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA3"
	)
	swap := func(txHash string, path []string) soroswap_router.Event {
		return soroswap_router.Event{Swap: soroswap_router.RouterSwap{
			Source:     soroswap_router.SourceName,
			Ledger:     1000,
			ClosedAt:   time.Now().UTC().Truncate(time.Second),
			TxHash:     txHash,
			OpIndex:    0,
			OpSource:   recipient,
			TxSource:   recipient,
			ContractID: soroswap_router.MainnetRouter,
			Function:   soroswap_router.FnSwapExactTokensForTokens,
			Recipient:  recipient,
			Path:       path,
			AmountIn:   canonical.NewAmount(big.NewInt(1_000_000)),
			AmountOut:  canonical.NewAmount(big.NewInt(990_000)),
			CallPath:   []string{soroswap_router.MainnetRouter},
			CallDepth:  0,
			CallKind:   "top_level",
		}}
	}

	// A single-hop path is rejected by the writer before any SQL runs
	// (Path must have >= 2 hops): no row, so no entry.
	if err := pipeline.HandleEvent(ctx, logger, store, swap("aa01", []string{pathHopA})); err == nil {
		t.Fatal("HandleEvent landed a single-hop router swap; the store must reject it")
	}
	if got := count(); got != 0 {
		t.Fatalf("after a rejected row: soroswap-router entries = %d, want 0 (the bump must follow the landed insert)", got)
	}

	// A landed row counts exactly once.
	if err := pipeline.HandleEvent(ctx, logger, store, swap("aa02", []string{pathHopA, pathHopB})); err != nil {
		t.Fatalf("HandleEvent: %v", err)
	}
	if got := count(); got != 1 {
		t.Fatalf("after a landed row: soroswap-router entries = %d, want 1", got)
	}
}

// TestSourceEntryCounts_AtomicIdempotentBump is the correctness core
// of the always-on entry tally (migration 0035): the writers bump
// source_entry_counts ATOMICALLY and IDEMPOTENTLY. A backfill
// re-walk that re-inserts already-stored rows (ON CONFLICT DO
// NOTHING → 0 rows) must NOT inflate the tally — otherwise every
// `-resume` / parallel-chunk replay would drift the count upward,
// re-creating exactly the "legacy data" class of bug this design
// exists to avoid.
func TestSourceEntryCounts_AtomicIdempotentBump(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	xlm, err := canonical.NewCryptoAsset("XLM")
	if err != nil {
		t.Fatal(err)
	}
	usd, err := canonical.NewFiatAsset("USD")
	if err != nil {
		t.Fatal(err)
	}
	xlmUSD, _ := canonical.NewPair(xlm, usd)
	ts := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Second)

	count := func(source string) int64 {
		m, err := store.SourceEntryCounts(ctx)
		if err != nil {
			t.Fatalf("SourceEntryCounts: %v", err)
		}
		return m[source]
	}

	tr1 := mkIntegrationTrade("sdex", 1, ts, xlmUSD, 100_000_000, 12_000_000)
	tr2 := mkIntegrationTrade("sdex", 2, ts, xlmUSD, 100_000_000, 12_000_000)

	// First insert → tally 1.
	if err := store.InsertTrade(ctx, tr1); err != nil {
		t.Fatalf("InsertTrade tr1: %v", err)
	}
	if got := count("sdex"); got != 1 {
		t.Fatalf("after first insert: sdex entries = %d, want 1", got)
	}

	// Re-insert the SAME trade (backfill re-walk). PK conflict →
	// DO NOTHING → the HAVING-gated counter upsert must be a no-op.
	if err := store.InsertTrade(ctx, tr1); err != nil {
		t.Fatalf("InsertTrade tr1 (replay): %v", err)
	}
	if got := count("sdex"); got != 1 {
		t.Fatalf("after replay: sdex entries = %d, want 1 (idempotent)", got)
	}

	// A genuinely new trade for the same source → tally 2.
	if err := store.InsertTrade(ctx, tr2); err != nil {
		t.Fatalf("InsertTrade tr2: %v", err)
	}
	if got := count("sdex"); got != 2 {
		t.Fatalf("after second distinct insert: sdex entries = %d, want 2", got)
	}

	// Oracle updates feed the SAME tally (the whole point of the
	// rename: "entries", not "trades"). Same idempotency contract.
	ou := canonical.OracleUpdate{
		Source:    "reflector-dex",
		Ledger:    50_000_123,
		TxHash:    strings.Repeat("ab", 32),
		OpIndex:   0,
		Timestamp: ts,
		Asset:     xlm,
		Quote:     usd,
		Price:     canonical.NewAmount(big.NewInt(1_2345678901234)),
		Decimals:  14,
	}
	if err := store.InsertOracleUpdate(ctx, ou); err != nil {
		t.Fatalf("InsertOracleUpdate: %v", err)
	}
	if err := store.InsertOracleUpdate(ctx, ou); err != nil {
		t.Fatalf("InsertOracleUpdate (replay): %v", err)
	}
	if got := count("reflector-dex"); got != 1 {
		t.Fatalf("oracle entries = %d, want 1 (idempotent across oracle_updates)", got)
	}
	// Trade tally untouched by oracle ingest.
	if got := count("sdex"); got != 2 {
		t.Fatalf("sdex entries drifted to %d after oracle insert, want 2", got)
	}

	// SeedSourceEntryCounts is the authoritative reconcile: it must
	// CORRECT drift (SET, not ADD). Poison the tally, reseed, verify
	// it snaps back to the real table totals.
	if _, err := store.DB().ExecContext(ctx,
		`UPDATE source_entry_counts SET entry_count = 99999 WHERE source = 'sdex'`); err != nil {
		t.Fatalf("poison: %v", err)
	}
	if _, err := store.SeedSourceEntryCounts(ctx); err != nil {
		t.Fatalf("SeedSourceEntryCounts: %v", err)
	}
	if got := count("sdex"); got != 2 {
		t.Fatalf("after reseed: sdex entries = %d, want 2 (authoritative recount)", got)
	}
	if got := count("reflector-dex"); got != 1 {
		t.Fatalf("after reseed: reflector-dex entries = %d, want 1", got)
	}
}

// TestSourceEntryCounts_LogOnlySourcesReconcile closes the reconcile gap
// for the "log-only" sinks (soroswap-router, defindex): the sink bumps
// source_entry_counts by 1 per decoded event (NON-idempotently), so a
// replay / re-derive that re-drives the sink double-counts them
// permanently — a KALE-class trap for the `entries` diagnostics column.
//
// Both sinks now ALSO persist one idempotent row per event to a countable
// hypertable (soroswap_router_swaps / defindex_flows). This test proves
// SeedSourceEntryCounts folds those tables in and SET-resets the two
// sources authoritatively — so the drift a replay introduces is CORRECTED
// (the seed recomputes both from their tables, so operators may seed-reset them).
func TestSourceEntryCounts_LogOnlySourcesReconcile(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	count := func(source string) int64 {
		m, err := store.SourceEntryCounts(ctx)
		if err != nil {
			t.Fatalf("SourceEntryCounts: %v", err)
		}
		return m[source]
	}
	ts := time.Now().UTC().Add(-3 * time.Hour).Truncate(time.Second)
	txh := func(n int) string { return fmt.Sprintf("%064x", n) }

	// insertRouter mimics the sink: persist one idempotent row + bump the
	// counter (the non-idempotent per-event bump).
	insertRouter := func(nonce int) {
		t.Helper()
		row := timescale.SoroswapRouterSwap{
			Ledger:          uint32(1000 + nonce),
			LedgerCloseTime: ts.Add(time.Duration(nonce) * time.Second),
			TxHash:          txh(nonce),
			OpIndex:         0,
			ContractID:      "CC4WPS7HRSPRZAXBVUDYLRXLZRHPLA6VTZARKZJTNVNECAS5IDRXRUB6",
			FunctionName:    "swap_exact_tokens_for_tokens",
			Recipient:       "GA1IF6WRUM4NRJIF7SDBEK4HXQFLA33MB47AR33YHV5EDJKC742OCLEV",
			Path:            []string{"native", "CC4WPS7HRSPRZAXBVUDYLRXLZRHPLA6VTZARKZJTNVNECAS5IDRXRUB6"},
			AmountIn:        "1000",
			AmountOut:       "990",
			CallSig:         txh(nonce), // per-call PK discriminator
		}
		if err := store.InsertSoroswapRouterSwap(ctx, row); err != nil {
			t.Fatalf("InsertSoroswapRouterSwap[%d]: %v", nonce, err)
		}
		if err := store.BumpSourceEntryCount(ctx, "soroswap-router", 1); err != nil {
			t.Fatalf("bump router[%d]: %v", nonce, err)
		}
	}
	// insertDefindex mimics the sink for one flow (strategy or vault layer).
	insertDefindex := func(nonce int, layer timescale.DefindexLayer) {
		t.Helper()
		row := timescale.DefindexFlow{
			Ledger:          uint32(2000 + nonce),
			LedgerCloseTime: ts.Add(time.Duration(nonce) * time.Second),
			TxHash:          txh(1000 + nonce),
			OpIndex:         0,
			EventIndex:      0,
			ContractID:      "CDLZFC3SYJYDZT7K67VZ75HPJVIEUVNIXF47ZG2FB2RMQQVU2HHGCYSC",
			Layer:           layer,
			Direction:       timescale.DefindexDeposit,
			Actor:           "GA1IF6WRUM4NRJIF7SDBEK4HXQFLA33MB47AR33YHV5EDJKC742OCLEV",
		}
		switch layer {
		case timescale.DefindexLayerStrategy:
			row.Amount = "5000"
		case timescale.DefindexLayerVault:
			row.AmountsVec = []string{"5000"}
			row.DfTokens = "4998"
		}
		if err := store.InsertDefindexFlow(ctx, row); err != nil {
			t.Fatalf("InsertDefindexFlow[%d]: %v", nonce, err)
		}
		if err := store.BumpSourceEntryCount(ctx, "defindex", 1); err != nil {
			t.Fatalf("bump defindex[%d]: %v", nonce, err)
		}
	}

	// ─── Steady-state ingest: 3 router swaps, 2 defindex flows ───────────
	for i := 1; i <= 3; i++ {
		insertRouter(i)
	}
	insertDefindex(1, timescale.DefindexLayerStrategy)
	insertDefindex(2, timescale.DefindexLayerVault)
	if got := count("soroswap-router"); got != 3 {
		t.Fatalf("router entries = %d, want 3", got)
	}
	if got := count("defindex"); got != 2 {
		t.Fatalf("defindex entries = %d, want 2", got)
	}

	// ─── Replay the SAME range: table inserts DO NOTHING (idempotent),
	//     but the per-event bump still ADDs → the counter double-counts.
	for i := 1; i <= 3; i++ {
		insertRouter(i)
	}
	insertDefindex(1, timescale.DefindexLayerStrategy)
	insertDefindex(2, timescale.DefindexLayerVault)
	if got := count("soroswap-router"); got != 6 {
		t.Fatalf("router entries after replay = %d, want 6 (bump is not replay-safe — the drift this fix reconciles)", got)
	}
	if got := count("defindex"); got != 4 {
		t.Fatalf("defindex entries after replay = %d, want 4 (bump double-counted)", got)
	}

	// ─── Authoritative reconcile: SET-reset from the countable tables ────
	if _, err := store.SeedSourceEntryCounts(ctx); err != nil {
		t.Fatalf("SeedSourceEntryCounts: %v", err)
	}
	if got := count("soroswap-router"); got != 3 {
		t.Fatalf("router entries after reseed = %d, want 3 (snapped back to soroswap_router_swaps COUNT)", got)
	}
	if got := count("defindex"); got != 2 {
		t.Fatalf("defindex entries after reseed = %d, want 2 (snapped back to defindex_flows COUNT — both layers)", got)
	}
}

// TestSourceEntryCounts_GappedNonTradeSinksReconcile extends the
// log-only reconcile guarantee to EVERY remaining source that bumps
// source_entry_counts through pipeline/sink.go::bumpEntryCount (a
// non-idempotent +1 per decoded event): comet liquidity, soroswap
// skim, phoenix liquidity/stake, blend positions/emissions/admin,
// blend-backstop, cctp, rozo, sep41_transfers.
//
// Each writes one idempotent (ON CONFLICT DO NOTHING) row per event to
// a countable hypertable, so its COUNT is replay-stable and equals the
// bump total. This test drives each sink twice over the SAME range —
// the second pass is a replay: the table INSERT is a no-op but the bump
// ADDs, double-counting — then proves SeedSourceEntryCounts folds every
// table in and SET-resets each source back to the honest total.
//
// comet / soroswap / phoenix ALSO write a swap to `trades` (bumped
// idempotently INSIDE InsertTrade, so replay-safe). Their steady-state
// entries therefore = #swaps + #non-swap-events; the test asserts the
// seed sums the trades count with the folded non-trade table WITHOUT
// double-counting (disjoint event sets).
func TestSourceEntryCounts_GappedNonTradeSinksReconcile(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	count := func(source string) int64 {
		m, err := store.SourceEntryCounts(ctx)
		if err != nil {
			t.Fatalf("SourceEntryCounts: %v", err)
		}
		return m[source]
	}
	bump := func(source string) {
		t.Helper()
		if err := store.BumpSourceEntryCount(ctx, source, 1); err != nil {
			t.Fatalf("bump %s: %v", source, err)
		}
	}

	xlm, err := canonical.NewCryptoAsset("XLM")
	if err != nil {
		t.Fatal(err)
	}
	usd, err := canonical.NewFiatAsset("USD")
	if err != nil {
		t.Fatal(err)
	}
	xlmUSD, _ := canonical.NewPair(xlm, usd)
	ts := time.Now().UTC().Add(-3 * time.Hour).Truncate(time.Second)

	const (
		contractID = "CC4WPS7HRSPRZAXBVUDYLRXLZRHPLA6VTZARKZJTNVNECAS5IDRXRUB6"
		poolID     = "CDLZFC3SYJYDZT7K67VZ75HPJVIEUVNIXF47ZG2FB2RMQQVU2HHGCYSC"
		userAddr   = "GA1IF6WRUM4NRJIF7SDBEK4HXQFLA33MB47AR33YHV5EDJKC742OCLEV"
	)

	// Each closure below inserts a FIXED row — calling it twice is a
	// replay (same PK → ON CONFLICT DO NOTHING). Non-trade closures also
	// call bump(...) to mirror the sink's NON-idempotent bumpEntryCount.
	// The trade closures rely on InsertTrade's idempotent internal bump.

	insertCometSwap := func() {
		t.Helper()
		if err := store.InsertTrade(ctx, mkIntegrationTrade("comet", 1, ts, xlmUSD, 100_000_000, 12_000_000)); err != nil {
			t.Fatalf("InsertTrade comet: %v", err)
		}
	}
	insertCometLiquidity := func() {
		t.Helper()
		if err := store.InsertCometLiquidity(ctx, timescale.CometLiquidityEvent{
			ContractID: contractID, Ledger: 1001, LedgerCloseTime: ts,
			TxHash: strings.Repeat("a1", 32), OpIndex: 0, EventIndex: 0,
			Kind: timescale.CometLiquidityJoinPool, Caller: userAddr, Token: contractID,
			Amount: canonical.NewAmount(big.NewInt(5_000)), PoolAmountIn: canonical.NewAmount(big.NewInt(0)),
		}); err != nil {
			t.Fatalf("InsertCometLiquidity: %v", err)
		}
		bump("comet")
	}

	insertSoroswapSwap := func() {
		t.Helper()
		if err := store.InsertTrade(ctx, mkIntegrationTrade("soroswap", 1, ts, xlmUSD, 100_000_000, 12_000_000)); err != nil {
			t.Fatalf("InsertTrade soroswap: %v", err)
		}
	}
	insertSoroswapSkim := func() {
		t.Helper()
		if err := store.InsertSoroswapSkimEvent(ctx, timescale.SoroswapSkimEvent{
			ContractID: contractID, Ledger: 1002, LedgerCloseTime: ts,
			TxHash: []byte(strings.Repeat("s", 32)), OpIndex: 0, EventIndex: 0,
			To: userAddr, Amount0: "1000", Amount1: "990",
		}); err != nil {
			t.Fatalf("InsertSoroswapSkimEvent: %v", err)
		}
		bump("soroswap")
	}

	insertPhoenixSwap := func() {
		t.Helper()
		if err := store.InsertTrade(ctx, mkIntegrationTrade("phoenix", 1, ts, xlmUSD, 100_000_000, 12_000_000)); err != nil {
			t.Fatalf("InsertTrade phoenix: %v", err)
		}
	}
	insertPhoenixLiquidity := func() {
		t.Helper()
		if err := store.InsertPhoenixLiquidityChange(ctx, timescale.PhoenixLiquidityChange{
			Pool: poolID, Ledger: 1003, ObservedAt: ts, TxHash: strings.Repeat("b2", 32),
			OpIndex: 0, EventIndex: 0, Action: timescale.PhoenixProvideLiquidity,
			Sender: userAddr, TokenA: contractID, TokenB: poolID, AmountA: "1000", AmountB: "2000",
		}); err != nil {
			t.Fatalf("InsertPhoenixLiquidityChange: %v", err)
		}
		bump("phoenix")
	}
	insertPhoenixStake := func() {
		t.Helper()
		if err := store.InsertPhoenixStakeEvent(ctx, timescale.PhoenixStakeEvent{
			StakeContract: contractID, Ledger: 1004, ObservedAt: ts, TxHash: strings.Repeat("c3", 32),
			OpIndex: 0, EventIndex: 0, Action: timescale.PhoenixBond, User: userAddr,
			LPToken: poolID, Amount: "1000",
		}); err != nil {
			t.Fatalf("InsertPhoenixStakeEvent: %v", err)
		}
		bump("phoenix")
	}

	insertBlendPosition := func() {
		t.Helper()
		if err := store.InsertBlendPositionEvent(ctx, domain.BlendPositionEvent{
			Pool: poolID, Kind: blend.EventSupply, Asset: contractID, User: userAddr,
			TokenAmount: big.NewInt(1_000_000), BOrDAmount: big.NewInt(990_000),
			Ledger: 2001, TxHash: pad64("e", 1), OpIndex: 0, Timestamp: ts,
		}); err != nil {
			t.Fatalf("InsertBlendPositionEvent: %v", err)
		}
		bump("blend")
	}
	insertBlendEmission := func() {
		t.Helper()
		if err := store.InsertBlendEmissionEvent(ctx, domain.BlendEmissionEvent{
			Pool: poolID, Kind: blend.EventGulp, Asset: contractID, Amount: big.NewInt(100),
			Ledger: 2002, TxHash: pad64("f", 2), OpIndex: 0, Timestamp: ts,
		}); err != nil {
			t.Fatalf("InsertBlendEmissionEvent: %v", err)
		}
		bump("blend")
	}
	insertBlendAdmin := func() {
		t.Helper()
		if err := store.InsertBlendAdminEvent(ctx, domain.BlendAdminEvent{
			ContractID: poolID, Kind: blend.EventSetAdmin, Admin: userAddr, Target: userAddr,
			Ledger: 2003, TxHash: pad64("a", 3), OpIndex: 0, Timestamp: ts,
		}); err != nil {
			t.Fatalf("InsertBlendAdminEvent: %v", err)
		}
		bump("blend")
	}

	insertBackstop := func() {
		t.Helper()
		if err := store.InsertBlendBackstopEvent(ctx, timescale.BlendBackstopEvent{
			ContractID: contractID, Ledger: 3001, TxHash: strings.Repeat("d4", 32),
			OpIndex: 0, EventIndex: 0, ObservedAt: ts, EventType: timescale.BackstopDeposit,
			Pool: poolID, UserAddress: userAddr, Amount: "1000",
		}); err != nil {
			t.Fatalf("InsertBlendBackstopEvent: %v", err)
		}
		bump("blend_backstop")
	}
	insertCCTP := func() {
		t.Helper()
		if err := store.InsertCCTPEvent(ctx, timescale.CCTPEvent{
			ContractID: contractID, Ledger: 3002, TxHash: strings.Repeat("e5", 32),
			OpIndex: 0, ObservedAt: ts, EventType: timescale.CCTPDepositForBurn, Amount: "1000",
		}); err != nil {
			t.Fatalf("InsertCCTPEvent: %v", err)
		}
		bump("cctp")
	}
	insertRozo := func() {
		t.Helper()
		if err := store.InsertRozoEvent(ctx, timescale.RozoEvent{
			ContractID: contractID, Ledger: 3003, TxHash: strings.Repeat("f6", 32),
			OpIndex: 0, ObservedAt: ts, EventType: timescale.RozoPayment,
			Amount: "1000", Destination: userAddr,
		}); err != nil {
			t.Fatalf("InsertRozoEvent: %v", err)
		}
		bump("rozo")
	}
	insertSEP41Transfer := func() {
		t.Helper()
		if err := store.InsertSEP41Transfer(ctx, timescale.SEP41TransferRow{
			ContractID: contractID, Ledger: 3004, TxHash: strings.Repeat("07", 32),
			OpIndex: 0, EventIndex: 0, ObservedAt: ts, Kind: timescale.SEP41Transfer,
			FromAddr: userAddr, ToAddr: userAddr, Amount: big.NewInt(1000),
		}); err != nil {
			t.Fatalf("InsertSEP41Transfer: %v", err)
		}
		bump("sep41_transfers")
	}

	drive := func() {
		insertCometSwap()
		insertCometLiquidity()
		insertSoroswapSwap()
		insertSoroswapSkim()
		insertPhoenixSwap()
		insertPhoenixLiquidity()
		insertPhoenixStake()
		insertBlendPosition()
		insertBlendEmission()
		insertBlendAdmin()
		insertBackstop()
		insertCCTP()
		insertRozo()
		insertSEP41Transfer()
	}

	// steady = the correct entry total once the fold is in place:
	//   comet   = 1 swap + 1 liquidity          = 2
	//   soroswap= 1 swap + 1 skim               = 2
	//   phoenix = 1 swap + 1 liquidity + 1 stake= 3
	//   blend   = 1 position + 1 emission + 1 admin = 3
	//   others  = 1 each
	steady := map[string]int64{
		"comet": 2, "soroswap": 2, "phoenix": 3, "blend": 3,
		"blend_backstop": 1, "cctp": 1, "rozo": 1, "sep41_transfers": 1,
	}

	// ─── Steady-state ingest ─────────────────────────────────────────
	drive()
	for src, want := range steady {
		if got := count(src); got != want {
			t.Fatalf("steady: %s entries = %d, want %d", src, got, want)
		}
	}

	// ─── Replay the SAME range: idempotent rows DO NOTHING, but the
	//     per-event bumpEntryCount still ADDs. Trade swaps are bumped
	//     inside InsertTrade (HAVING-gated) so they DON'T double; only
	//     the bumpEntryCount portion drifts.
	drive()
	drifted := map[string]int64{
		"comet": 3, "soroswap": 3, "phoenix": 5, "blend": 6,
		"blend_backstop": 2, "cctp": 2, "rozo": 2, "sep41_transfers": 2,
	}
	for src, want := range drifted {
		if got := count(src); got != want {
			t.Fatalf("after replay: %s entries = %d, want %d (bumpEntryCount is not replay-safe — the drift this fix reconciles)", src, got, want)
		}
	}

	// ─── Authoritative reconcile: SET-reset from the folded tables ───
	if _, err := store.SeedSourceEntryCounts(ctx); err != nil {
		t.Fatalf("SeedSourceEntryCounts: %v", err)
	}
	for src, want := range steady {
		if got := count(src); got != want {
			t.Fatalf("after reseed: %s entries = %d, want %d (should snap back to the folded table COUNT — for comet/soroswap/phoenix, trades + non-trade table, no double-count)", src, got, want)
		}
	}
}

// TestSourceEntryCounts_FXQuotesBumpInlineAndReconcile pins the third
// inline-bump path BumpSourceEntryCount's contract names: the fx_quotes
// insert (the `massive` forex worker's only write) must bump the
// per-source tally exactly once per row that actually lands — not on a
// same-generation re-run, not on an in-place rate correction — and the
// seed's fx_quotes fold must reconcile to the same number. Before the
// bump existed the active fiat-FX feed had NO entries at all until an
// operator re-seeded, indistinguishable from a dead connector.
func TestSourceEntryCounts_FXQuotesBumpInlineAndReconcile(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	count := func(source string) int64 {
		m, err := store.SourceEntryCounts(ctx)
		if err != nil {
			t.Fatalf("SourceEntryCounts: %v", err)
		}
		return m[source]
	}

	day := time.Now().UTC().Truncate(24 * time.Hour)
	batch := []timescale.FXQuote{
		{Bucket: day, Ticker: "EUR", RateUSD: 0.92, InverseUSD: 1 / 0.92, Source: "massive"},
		{Bucket: day.AddDate(0, 0, -1), Ticker: "EUR", RateUSD: 0.91, InverseUSD: 1 / 0.91, Source: "massive"},
		{Bucket: day, Ticker: "GBP", RateUSD: 0.79, InverseUSD: 1 / 0.79, Source: "massive"},
	}
	if err := store.InsertFXQuoteBatch(ctx, batch); err != nil {
		t.Fatalf("InsertFXQuoteBatch: %v", err)
	}
	if got := count("massive"); got != 3 {
		t.Fatalf("after first batch: massive entries = %d, want 3 (one per landed fx_quotes row)", got)
	}

	// Same batch again: the worker's hourly refresh re-writes today's
	// row and the trailing history at the same generation. No new row
	// lands, so the tally must not move.
	if err := store.InsertFXQuoteBatch(ctx, batch); err != nil {
		t.Fatalf("InsertFXQuoteBatch replay: %v", err)
	}
	if got := count("massive"); got != 3 {
		t.Fatalf("after replay: massive entries = %d, want 3 (re-run must not inflate)", got)
	}

	// An in-place correction updates a row; still not a new entry.
	corrected := []timescale.FXQuote{{Bucket: day, Ticker: "EUR", RateUSD: 0.93, InverseUSD: 1 / 0.93, Source: "massive"}}
	if err := store.InsertFXQuoteBatch(ctx, corrected); err != nil {
		t.Fatalf("InsertFXQuoteBatch correction: %v", err)
	}
	if got := count("massive"); got != 3 {
		t.Fatalf("after correction: massive entries = %d, want 3 (update is not an entry)", got)
	}

	// The seed reconciles from the table and must agree with the bumps.
	if _, err := store.SeedSourceEntryCounts(ctx); err != nil {
		t.Fatalf("SeedSourceEntryCounts: %v", err)
	}
	if got := count("massive"); got != 3 {
		t.Fatalf("after seed: massive entries = %d, want 3 (seed fold must equal the inline bumps)", got)
	}
}

// TestSourceEntryCounts_UnfoldedSinksReconcile is the reconciliation
// invariant for the sinks the seed must fold: aquarius (whose
// non-swap streams were dropped in favour of its trades count),
// blend_emitter, sorocredit and upshift (absent outright). Each source
// gets one idempotent row plus a replay over-count via the sink's
// non-idempotent bumpEntryCount; the seed must SET-reset every one of
// them to the honest row count instead of overwriting (aquarius) or
// ignoring (the rest) it.
func TestSourceEntryCounts_UnfoldedSinksReconcile(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	count := func(source string) int64 {
		m, err := store.SourceEntryCounts(ctx)
		if err != nil {
			t.Fatalf("SourceEntryCounts: %v", err)
		}
		return m[source]
	}
	bump := func(source string) {
		t.Helper()
		if err := store.BumpSourceEntryCount(ctx, source, 1); err != nil {
			t.Fatalf("bump %s: %v", source, err)
		}
	}

	xlm, err := canonical.NewCryptoAsset("XLM")
	if err != nil {
		t.Fatal(err)
	}
	usd, err := canonical.NewFiatAsset("USD")
	if err != nil {
		t.Fatal(err)
	}
	xlmUSD, _ := canonical.NewPair(xlm, usd)
	ts := time.Now().UTC().Add(-3 * time.Hour).Truncate(time.Second)

	const (
		contractID = "CC4WPS7HRSPRZAXBVUDYLRXLZRHPLA6VTZARKZJTNVNECAS5IDRXRUB6"
		backstopID = "CDLZFC3SYJYDZT7K67VZ75HPJVIEUVNIXF47ZG2FB2RMQQVU2HHGCYSC"
		userAddr   = "GA1IF6WRUM4NRJIF7SDBEK4HXQFLA33MB47AR33YHV5EDJKC742OCLEV"
	)

	// aquarius: one swap (trades, idempotent inline bump) + one liquidity
	// row, bumped as the sink does. The seed must sum both streams.
	if err := store.InsertTrade(ctx, mkIntegrationTrade("aquarius", 1, ts, xlmUSD, 100_000_000, 12_000_000)); err != nil {
		t.Fatalf("InsertTrade aquarius: %v", err)
	}
	if err := store.InsertAquariusLiquidity(ctx, timescale.AquariusLiquidityEvent{
		ContractID: contractID, Ledger: 1001, LedgerCloseTime: ts,
		TxHash: strings.Repeat("a1", 32), OpIndex: 0, EventIndex: 0,
		Action: timescale.AquariusLiquidityDeposit,
		Tokens: []string{contractID}, Amounts: []canonical.Amount{canonical.NewAmount(big.NewInt(5_000))},
		Shares: canonical.NewAmount(big.NewInt(100)),
	}); err != nil {
		t.Fatalf("InsertAquariusLiquidity: %v", err)
	}
	bump("aquarius")

	if err := store.InsertBlendEmitterDistribute(ctx, timescale.BlendEmitterDistributeEvent{
		ContractID: contractID, Ledger: 1002, LedgerCloseTime: ts,
		TxHash: strings.Repeat("b2", 32), OpIndex: 0, EventIndex: 0,
		BackstopID: backstopID, Amount: canonical.NewAmount(big.NewInt(7_000)),
	}); err != nil {
		t.Fatalf("InsertBlendEmitterDistribute: %v", err)
	}
	bump("blend_emitter")

	if err := store.InsertCreditEvent(ctx, timescale.CreditEvent{
		EventType: "withdrawal", CollateralContract: contractID, Account: userAddr, Amount: "1000",
		Ledger: 1003, LedgerCloseTime: ts, TxHash: strings.Repeat("c3", 32), OpIndex: 0, EventIndex: 0,
	}); err != nil {
		t.Fatalf("InsertCreditEvent: %v", err)
	}
	bump("sorocredit")

	if err := store.InsertUpshiftVaultEvent(ctx, timescale.UpshiftVaultEvent{
		ContractID: contractID, Ledger: 1004, LedgerCloseTime: ts,
		TxHash: strings.Repeat("d4", 32), OpIndex: 0, EventIndex: 0,
		Kind: timescale.UpshiftDeposit, Caller: userAddr, Receiver: userAddr, Owner: userAddr,
		Assets: canonical.NewAmount(big.NewInt(9_000)), Shares: canonical.NewAmount(big.NewInt(9_000)),
	}); err != nil {
		t.Fatalf("InsertUpshiftVaultEvent: %v", err)
	}
	bump("upshift")

	// A replay re-drives the sink over the same events: the rows are
	// idempotent, the bumps are not.
	for _, src := range []string{"aquarius", "blend_emitter", "sorocredit", "upshift"} {
		bump(src)
	}
	want := map[string]int64{"aquarius": 2, "blend_emitter": 1, "sorocredit": 1, "upshift": 1}
	for src, w := range want {
		if got := count(src); got != w+1 {
			t.Fatalf("precondition: %s drifted tally = %d, want %d", src, got, w+1)
		}
	}

	if _, err := store.SeedSourceEntryCounts(ctx); err != nil {
		t.Fatalf("SeedSourceEntryCounts: %v", err)
	}
	for src, w := range want {
		if got := count(src); got != w {
			t.Errorf("after seed: %s entries = %d, want %d (the seed must fold every table the sink bumps for it)", src, got, w)
		}
	}
}
