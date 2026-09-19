//go:build integration

package integration_test

import (
	"context"
	"database/sql"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	c "github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/sources/cctp"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// TestChRebuildProjectedScript_DeleteSQLOnRealPostgres executes the SQL
// that scripts/ops/ch-rebuild-projected.sh ACTUALLY emits — captured by
// running the shipped script with a recording `psql` — against real
// TimescaleDB, and checks which rows survive (F075, RLT-380, RLT-381).
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
//     sushiswap_v3's (RLT-380) or sdex's.
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
	usdc, err := c.NewClassicAsset("USDC", "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN")
	if err != nil {
		t.Fatal(err)
	}
	pair, err := c.NewPair(c.NativeAsset(), usdc)
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
		{"sushiswap_v3", lo + 30, "not BackfillSafe: ch-rebuild refuses to rewrite it (RLT-380)"},
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
