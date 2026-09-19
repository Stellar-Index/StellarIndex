//go:build integration

package integration_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/ops/chops"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// TestCHRebuildPreflight_AnswersWithoutTouchingTheLakeOrTheServedTier is
// the executing proof for `ch-rebuild -write -preflight` (RLT-381), driven
// through the real subcommand on real TimescaleDB.
//
// scripts/ops/ch-rebuild-projected.sh DELETEs a window and only then asks
// ch-rebuild to re-derive it. The refusals used to live inside that second
// step, so a guard doing its job left the window empty. The preflight lets
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
	before := servedRows(t)
	if _, err = captureStdout(t, func() error { return chops.Run(args()) }); err == nil || !strings.Contains(err.Error(), "event stream") {
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
