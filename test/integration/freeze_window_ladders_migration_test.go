//go:build integration

package integration_test

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/Stellar-Index/StellarIndex/internal/aggregate/anomaly"
	"github.com/Stellar-Index/StellarIndex/internal/aggregate/freeze"
	c "github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// TestFreezeWindowLadders_Migration0163 pins the parts of the per-window
// durable ladder only Postgres can answer, against real TimescaleDB:
//
//   - migration 0163 applies to a `freeze_events` hypertable that already
//     holds a COMPRESSED chunk and a live, OPEN freeze written by the
//     previous binary. A freshly-migrated database has zero chunks, so
//     simply running the migrations would prove only that the SQL parses;
//   - that pre-0163 open row — a pair-level ladder, window_ladders NULL —
//     keeps answering for EVERY window (fail-closed: its owner is
//     unknowable, and dropping it releases a freeze that is still running),
//     and the first window-aware write preserves it as the unowned entry
//     instead of narrowing the pair to one window;
//   - SaveWindowLadder's read-merge-write executes, round-trips each
//     window's state through jsonb exactly, and keeps the 0119 columns as
//     the fail-closed summary;
//   - SaveLadder's changed UPDATE executes (the `$3::timestamptz` retire
//     CASE), and a retire clears the per-window entries so a later freeze
//     on the same still-open row does not inherit them;
//   - the operator override's durable half (recovered_at) still makes every
//     read absent, and a write against a closed row is ErrNotFound.
func TestFreezeWindowLadders_Migration0163(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrationsUpTo(t, dsn, 162)
	seedAndCompressFreezeChunk(t, ctx, dsn)

	asset, _ := c.NewClassicAsset("USDC", "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN")
	quote, _ := c.NewFiatAsset("USD")
	now := time.Now().UTC().Truncate(time.Microsecond)
	legacyHold := now.Add(20 * time.Minute)
	seedPreWindowOpenFreeze(t, ctx, dsn, asset.String(), quote.String(), now.Add(-3*time.Hour), legacyHold)

	applyMigrations(t, dsn)
	assertFreezeChunkStillCompressed(t, ctx, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	sink := timescale.NewFreezeEventSink(store)

	const (
		short = 5 * time.Minute
		long  = time.Hour
	)

	// ── the pre-0163 open row: owner unknown, so it answers pair-wide ──
	ladders, unowned, ok, err := sink.LoadWindowLadders(ctx, asset, quote)
	if err != nil || !ok {
		t.Fatalf("LoadWindowLadders on a pre-0163 open row = (ok=%v, err=%v), want present", ok, err)
	}
	if len(ladders) != 0 {
		t.Errorf("a pre-0163 row reported owned window ladders %+v; it records none", ladders)
	}
	if !unowned.Escalated || unowned.ExtensionsUsed != freeze.DefaultMaxExtensions || !unowned.HoldUntil.Equal(legacyHold) {
		t.Errorf("unowned ladder = %+v, want the row's escalated pair-level ladder (hold %v) — "+
			"reading it as absent would release a live escalated freeze on upgrade", unowned, legacyHold)
	}

	// ── the first window-aware write must not narrow the pair ──────────
	fresh := freeze.State{FiredAt: now.Add(-time.Minute), HoldUntil: now.Add(9 * time.Minute)}
	if err := sink.SaveWindowLadder(ctx, asset, quote, short, fresh); err != nil {
		t.Fatalf("SaveWindowLadder(5m): %v", err)
	}
	ladders, unowned, ok, err = sink.LoadWindowLadders(ctx, asset, quote)
	if err != nil || !ok {
		t.Fatalf("LoadWindowLadders = (ok=%v, err=%v)", ok, err)
	}
	if got := ladders[short]; !got.FiredAt.Equal(fresh.FiredAt) || !got.HoldUntil.Equal(fresh.HoldUntil) || got.Escalated {
		t.Errorf("5m ladder = %+v, want %+v", got, fresh)
	}
	if !unowned.Escalated || !unowned.HoldUntil.Equal(legacyHold) {
		t.Errorf("unowned ladder after the first window-aware write = %+v, want the escalated "+
			"pre-0163 ladder preserved: the 5m window's write narrowed a pair-wide freeze "+
			"to a single window and dropped the rest", unowned)
	}
	assertPairSummary(t, ctx, sink, asset, quote, true, freeze.DefaultMaxExtensions, legacyHold)

	// ── per-window round trip, exact ───────────────────────────────────
	extended := freeze.State{
		FiredAt:        now.Add(-70 * time.Minute),
		HoldUntil:      now.Add(28 * time.Minute),
		ExtensionsUsed: 2,
		UnfreezeStreak: 1,
		Corroborated:   true,
	}
	if err := sink.SaveWindowLadder(ctx, asset, quote, long, extended); err != nil {
		t.Fatalf("SaveWindowLadder(1h): %v", err)
	}
	ladders, _, _, err = sink.LoadWindowLadders(ctx, asset, quote)
	if err != nil {
		t.Fatalf("LoadWindowLadders: %v", err)
	}
	if got := ladders[long]; !got.FiredAt.Equal(extended.FiredAt) || !got.HoldUntil.Equal(extended.HoldUntil) ||
		got.ExtensionsUsed != extended.ExtensionsUsed || got.UnfreezeStreak != extended.UnfreezeStreak ||
		got.Escalated != extended.Escalated || got.Corroborated != extended.Corroborated {
		t.Errorf("1h ladder = %+v, want %+v (exact jsonb round trip)", got, extended)
	}
	if got := ladders[short]; !got.HoldUntil.Equal(fresh.HoldUntil) {
		t.Errorf("writing the 1h ladder disturbed the 5m one: %+v", got)
	}
	if n := countFreezeRows(t, ctx, dsn, asset.String(), quote.String()); n != 1 {
		t.Fatalf("window-aware writes left %d freeze_events rows for the pair, want 1 — "+
			"SaveWindowLadder must only ever UPDATE", n)
	}

	// ── the down migration's export recipe, EXECUTED ───────────────────
	assertWindowLadderExportRecipe(t, ctx, dsn, asset.String(), quote.String())

	// ── retiring a window recomputes the summary from what is left ─────
	if err := sink.SaveWindowLadder(ctx, asset, quote, long, freeze.State{}); err != nil {
		t.Fatalf("SaveWindowLadder(1h, retire): %v", err)
	}
	ladders, _, _, err = sink.LoadWindowLadders(ctx, asset, quote)
	if err != nil {
		t.Fatalf("LoadWindowLadders: %v", err)
	}
	if _, held := ladders[long]; held {
		t.Error("the 1h ladder survived its own retire")
	}

	// ── SaveLadder's retire (freeze.Writer.Clear) clears the windows ───
	if err := sink.SaveLadder(ctx, asset, quote, freeze.State{}); err != nil {
		t.Fatalf("SaveLadder(zero): %v", err)
	}
	if _, _, ok, lerr := sink.LoadWindowLadders(ctx, asset, quote); lerr != nil || ok {
		t.Fatalf("LoadWindowLadders after a retire = (ok=%v, err=%v), want (false, nil)", ok, lerr)
	}
	if _, ok, lerr := sink.LoadLadder(ctx, asset, quote); lerr != nil || ok {
		t.Fatalf("LoadLadder after a retire = (ok=%v, err=%v), want (false, nil)", ok, lerr)
	}
	// A new freeze on the SAME still-open row (the recovery worker closes
	// it up to 60s later) starts clean: no unowned ladder, no old windows.
	if err := sink.SaveWindowLadder(ctx, asset, quote, long, extended); err != nil {
		t.Fatalf("SaveWindowLadder(1h) after a retire: %v", err)
	}
	ladders, unowned, ok, err = sink.LoadWindowLadders(ctx, asset, quote)
	if err != nil || !ok {
		t.Fatalf("LoadWindowLadders = (ok=%v, err=%v)", ok, err)
	}
	if len(ladders) != 1 || unowned.Active() {
		t.Errorf("a freeze after a retire inherited the ended one: ladders=%+v unowned=%+v", ladders, unowned)
	}
	assertPairSummary(t, ctx, sink, asset, quote, false, extended.ExtensionsUsed, extended.HoldUntil)

	// ── a window must be a real window ─────────────────────────────────
	if err := sink.SaveWindowLadder(ctx, asset, quote, 0, extended); err == nil {
		t.Error("SaveWindowLadder accepted a zero window; key \"0\" is the unowned ladder")
	}

	// ── the operator override's durable half still sticks ──────────────
	if err := sink.MarkRecovered(ctx, asset, quote); err != nil {
		t.Fatalf("MarkRecovered: %v", err)
	}
	if _, _, ok, lerr := sink.LoadWindowLadders(ctx, asset, quote); lerr != nil || ok {
		t.Fatalf("LoadWindowLadders after MarkRecovered = (ok=%v, err=%v), want (false, nil)", ok, lerr)
	}
	if err := sink.SaveWindowLadder(ctx, asset, quote, long, extended); !errors.Is(err, timescale.ErrNotFound) {
		t.Fatalf("SaveWindowLadder against a closed row = %v, want ErrNotFound", err)
	}

	// RecordFreeze still opens exactly one row for the next event.
	if err := sink.RecordFreeze(ctx, asset, quote, "0.874500000000", anomaly.Decision{
		Action: anomaly.ActionFreeze, Class: anomaly.ClassStablecoin, Reason: "phase2:3_signal_AND",
	}); err != nil {
		t.Fatalf("RecordFreeze: %v", err)
	}
	if n := countOpenFreezeRows(t, ctx, dsn, asset.String(), quote.String()); n != 1 {
		t.Fatalf("%d open rows after a new freeze, want 1", n)
	}
}

// windowLadderExportRecipe is the command the header of
// migrations/0163_freeze_events_window_ladders.down.sql tells an operator
// to run BEFORE reverting, to keep the per-window ladders the down drops.
// Held verbatim so the header and this test cannot drift apart silently.
const windowLadderExportRecipe = `SELECT asset_id, quote_id, frozen_at, hold_until, window_ladders
     FROM freeze_events
    WHERE recovered_at IS NULL;`

// assertWindowLadderExportRecipe runs that recipe against the live schema
// and asserts it returns what the header promises: the pair's open row,
// with both windows' ladders in it. A recipe that named a column the
// migration does not create, or a predicate matching no open row, would
// print nothing and exit 0 on the day an operator needed it.
func assertWindowLadderExportRecipe(t *testing.T, ctx context.Context, dsn, assetID, quoteID string) {
	t.Helper()
	_, thisFile, _, _ := runtime.Caller(0)
	downPath := filepath.Join(filepath.Dir(thisFile), "..", "..", "migrations",
		"0163_freeze_events_window_ladders.down.sql")
	header, err := os.ReadFile(downPath)
	if err != nil {
		t.Fatalf("read %s: %v", downPath, err)
	}
	normalise := func(s string) string { return strings.Join(strings.Fields(strings.ReplaceAll(s, "--", " ")), " ") }
	if !strings.Contains(normalise(string(header)), normalise(windowLadderExportRecipe)) {
		t.Fatalf("the export recipe in %s no longer matches the one this test executes:\n%s",
			downPath, windowLadderExportRecipe)
	}

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer func() { _ = db.Close() }()
	rows, err := db.QueryContext(ctx, strings.TrimSuffix(windowLadderExportRecipe, ";"))
	if err != nil {
		t.Fatalf("the down migration's export recipe does not execute: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var exported int
	for rows.Next() {
		var (
			gotAsset, gotQuote string
			frozenAt           time.Time
			holdUntil          sql.NullTime
			ladders            sql.NullString
		)
		if err := rows.Scan(&gotAsset, &gotQuote, &frozenAt, &holdUntil, &ladders); err != nil {
			t.Fatalf("scan export row: %v", err)
		}
		if gotAsset != assetID || gotQuote != quoteID {
			continue
		}
		exported++
		if !holdUntil.Valid || !ladders.Valid ||
			!strings.Contains(ladders.String, `"300"`) || !strings.Contains(ladders.String, `"3600"`) {
			t.Errorf("export recipe returned hold_until=%v window_ladders=%q, want a live hold and "+
				"both the 5m (\"300\") and 1h (\"3600\") ladders", holdUntil, ladders.String)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("export rows: %v", err)
	}
	if exported != 1 {
		t.Errorf("export recipe returned %d row(s) for the frozen pair, want 1", exported)
	}
}

// assertPairSummary checks the 0119 pair-level columns — what the recovery
// worker and `freeze-unfreeze -list` read — against the expected fail-closed
// fold of the per-window entries.
func assertPairSummary(
	t *testing.T, ctx context.Context, sink *timescale.FreezeEventSink,
	asset, quote c.Asset, escalated bool, extensions int, holdUntil time.Time,
) {
	t.Helper()
	pair, ok, err := sink.LoadLadder(ctx, asset, quote)
	if err != nil || !ok {
		t.Fatalf("LoadLadder = (ok=%v, err=%v), want the pair-level summary", ok, err)
	}
	if pair.Escalated != escalated || pair.ExtensionsUsed != extensions || !pair.HoldUntil.Equal(holdUntil) {
		t.Errorf("pair-level summary = %+v, want escalated=%v extensions=%d hold=%v",
			pair, escalated, extensions, holdUntil)
	}
}

// seedPreWindowOpenFreeze writes the row shape the PREVIOUS binary leaves
// behind: an OPEN freeze carrying an escalated pair-level ladder (migration
// 0119 columns), on a schema that has no `window_ladders` column yet.
func seedPreWindowOpenFreeze(t *testing.T, ctx context.Context, dsn, assetID, quoteID string, frozenAt, holdUntil time.Time) {
	t.Helper()
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.ExecContext(ctx, `
		INSERT INTO freeze_events (asset_id, quote_id, frozen_at, frozen_at_ledger,
		                           reason, frozen_value,
		                           hold_until, extensions_used, escalated, corroborated)
		VALUES ($1, $2, $3::timestamptz, 0, 'outlier_storm', 0.8745,
		        $4::timestamptz, $5, true, true)`,
		assetID, quoteID, frozenAt, holdUntil, freeze.DefaultMaxExtensions); err != nil {
		t.Fatalf("seed pre-0163 open freeze: %v", err)
	}
}
