//go:build integration

package integration_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/golang-migrate/migrate/v4"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/redis/go-redis/v9"

	"github.com/Stellar-Index/StellarIndex/internal/aggregate/anomaly"
	"github.com/Stellar-Index/StellarIndex/internal/aggregate/freeze"
	c "github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// TestFreezeWindowLadders_Migration0163Down_KeepsTheLiveFreeze runs the 0163 down beside a compressed chunk and an open
// freeze with per-window ladders: per-window ownership is lost (as migrations/README.md promises) but the pair-level
// summary, the 0119 columns kept as the fail-closed fold of the windows, survives, so the freeze itself does.
func TestFreezeWindowLadders_Migration0163Down_KeepsTheLiveFreeze(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	seedAndCompressFreezeChunk(t, ctx, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	sink := timescale.NewFreezeEventSink(store)
	asset, _ := c.NewClassicAsset("USDC", "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN")
	quote, _ := c.NewFiatAsset("USD")
	if err := sink.RecordFreeze(ctx, asset, quote, "0.874500000000", anomaly.Decision{
		Action: anomaly.ActionFreeze, Class: anomaly.ClassStablecoin, Reason: "phase2:3_signal_AND",
	}); err != nil {
		t.Fatalf("RecordFreeze: %v", err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	escalated := freeze.State{
		FiredAt: now.Add(-2 * time.Hour), HoldUntil: now.Add(25 * time.Minute),
		ExtensionsUsed: freeze.DefaultMaxExtensions, Escalated: true,
	}
	fresh := freeze.State{FiredAt: now.Add(-time.Minute), HoldUntil: now.Add(9 * time.Minute)}
	if err := sink.SaveWindowLadder(ctx, asset, quote, time.Hour, escalated); err != nil {
		t.Fatalf("SaveWindowLadder(1h): %v", err)
	}
	if err := sink.SaveWindowLadder(ctx, asset, quote, 5*time.Minute, fresh); err != nil {
		t.Fatalf("SaveWindowLadder(5m): %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("store close: %v", err)
	}

	applyMigrationsUpTo(t, dsn, 162) // executes the 0163 down
	assertFreezeChunkStillCompressed(t, ctx, dsn)

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("sql open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	var cols int
	if err := db.QueryRowContext(ctx, `
		SELECT count(*) FROM information_schema.columns
		 WHERE table_name = 'freeze_events' AND column_name = 'window_ladders'`).Scan(&cols); err != nil {
		t.Fatalf("column lookup: %v", err)
	}
	if cols != 0 {
		t.Errorf("window_ladders still present after the down (%d)", cols)
	}
	if n := countOpenFreezeRows(t, ctx, dsn, asset.String(), quote.String()); n != 1 {
		t.Fatalf("open freeze rows after the down = %d, want 1", n)
	}
	var (
		hold sql.NullTime
		esc  bool
		ext  int
	)
	if err := db.QueryRowContext(ctx, `
		SELECT hold_until, escalated, extensions_used
		  FROM freeze_events
		 WHERE asset_id = $1 AND quote_id = $2 AND recovered_at IS NULL`,
		asset.String(), quote.String()).Scan(&hold, &esc, &ext); err != nil {
		t.Fatalf("read open freeze row: %v", err)
	}
	if !hold.Valid || !hold.Time.Equal(escalated.HoldUntil) || !esc || ext != freeze.DefaultMaxExtensions {
		t.Errorf("pair-level ladder after the down = (hold_until=%v valid=%v, escalated=%v, extensions_used=%d), "+
			"want (%v, true, %d): the down lost more than the new column",
			hold.Time.UTC(), hold.Valid, esc, ext, escalated.HoldUntil, freeze.DefaultMaxExtensions)
	}
}

// TestFreezeWindowLadders_Migration0163 pins what only Postgres answers: - 0163 applies to a freeze_events hypertable
// already holding a compressed chunk and an open freeze (a fresh DB has zero chunks and would prove only that the SQL
// parses); - a pre-0163 open row (window_ladders NULL) answers for EVERY window (fail-closed: its owner is unknowable)
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
	if err := sink.MarkRecovered(ctx, asset, quote, "operator:test"); err != nil {
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

// windowLadderExportRecipe is the command the header of migrations/0163_freeze_events_window_ladders.down.sql tells an
// operator to run before reverting. Held verbatim so the header and this test cannot drift apart.
const windowLadderExportRecipe = `SELECT asset_id, quote_id, frozen_at, hold_until, window_ladders
     FROM freeze_events
    WHERE recovered_at IS NULL;`

// assertWindowLadderExportRecipe runs the recipe against the live schema and asserts it returns the pair's open row with
// both windows' ladders; a recipe naming a missing column or matching no open row would print nothing and exit 0.
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

// window_ladders beside a binary that does not know it: migrations/README.md rule 9 keeps the schema at 0163 across a
// rollback, so the previous binary advances only the four 0119 pair-level columns and leaves the map stale. A reader
// that prefers the stale map discards that binary's escalation, which is the under-hold direction.

// rollbackFixture is a production-wired writer over real TimescaleDB plus a
// raw handle for playing the previous binary's SQL.
type rollbackFixture struct {
	w            *freeze.Writer
	sink         *timescale.FreezeEventSink
	db           *sql.DB
	loseRedis    func()
	asset, quote c.Asset
	decision     anomaly.Decision
}

func newRollbackFixture(t *testing.T, ctx context.Context, dsn string) *rollbackFixture {
	t.Helper()
	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	sink := timescale.NewFreezeEventSink(store)
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	w, err := freeze.NewWriter(rdb, 0, freeze.WithEventSink(sink), freeze.WithLadderStore(sink, 0))
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("sql open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	asset, _ := c.NewClassicAsset("USDC", "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN")
	quote, _ := c.NewFiatAsset("USD")
	return &rollbackFixture{
		w: w, sink: sink, db: db, loseRedis: mr.FlushAll, asset: asset, quote: quote,
		decision: anomaly.Decision{Action: anomaly.ActionFreeze, Class: anomaly.ClassStablecoin, Reason: "phase2:3_signal_AND"},
	}
}

// previousBinarySaveLadder is the released binary's SaveLadder, verbatim:
// the four 0119 columns, and nothing about window_ladders.
func (f *rollbackFixture) previousBinarySaveLadder(t *testing.T, ctx context.Context, st freeze.State) {
	t.Helper()
	res, err := f.db.ExecContext(ctx, `
		UPDATE freeze_events
		   SET hold_until = $3::timestamptz, extensions_used = $4, escalated = $5, corroborated = $6
		 WHERE asset_id = $1 AND quote_id = $2 AND recovered_at IS NULL`,
		f.asset.String(), f.quote.String(), st.HoldUntil.UTC(), st.ExtensionsUsed, st.Escalated, st.Corroborated)
	if err != nil {
		t.Fatalf("previous-binary SaveLadder: %v", err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		t.Fatalf("previous-binary SaveLadder matched %d rows, want 1", n)
	}
}

// TestFreezeWindowLadders_PreviousBinaryEscalationSurvivesRollForward: the
// new binary leaves the 1h window on the last rung; the rolled-back binary
// escalates the freeze; the new binary comes back and Redis is lost.
func TestFreezeWindowLadders_PreviousBinaryEscalationSurvivesRollForward(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	f := newRollbackFixture(t, ctx, dsn)
	now := time.Now().UTC().Truncate(time.Microsecond)

	climbing := freeze.State{
		FiredAt: now.Add(-100 * time.Minute), HoldUntil: now.Add(4 * time.Minute),
		ExtensionsUsed: freeze.DefaultMaxExtensions, Corroborated: true,
	}
	if err := f.w.MarkHoldForWindow(ctx, f.asset, f.quote, time.Hour, "0.874500000000", f.decision, climbing, 9*time.Minute); err != nil {
		t.Fatalf("MarkHoldForWindow(1h): %v", err)
	}
	escalatedHold := now.Add(30 * time.Minute)
	f.previousBinarySaveLadder(t, ctx, freeze.State{
		HoldUntil: escalatedHold, ExtensionsUsed: freeze.DefaultMaxExtensions, Escalated: true, Corroborated: true,
	})

	f.loseRedis()
	got, ok, err := f.w.LoadStateForWindow(ctx, f.asset, f.quote, time.Hour)
	if err != nil || !ok {
		t.Fatalf("LoadStateForWindow(1h) = (ok=%v, err=%v), want present", ok, err)
	}
	if !got.Escalated || !got.HoldUntil.Equal(escalatedHold) || got.ExtensionsUsed != freeze.DefaultMaxExtensions {
		t.Errorf("1h window rehydrated %+v, want escalated with hold_until %v: the pair-level columns "+
			"say ESCALATED, the stale window_ladders entry won, and the freeze resumes auto-unfreezing",
			got, escalatedHold)
	}
	// A window the map never named still answers to the ownerless ladder,
	// as it does for any row whose ladder has no recorded owner.
	if day, _, _ := f.w.LoadStateForWindow(ctx, f.asset, f.quote, 24*time.Hour); !day.Escalated {
		t.Errorf("24h window rehydrated %+v, want the ownerless escalated ladder", day)
	}

	// The write path reads the row through the same rule, so the window's
	// next tick PERSISTS what it rehydrated. The record must converge on
	// the escalation, not write the stale map back over the columns.
	if err := f.w.MarkHoldForWindow(ctx, f.asset, f.quote, time.Hour, "0.874500000000", f.decision, got, 35*time.Minute); err != nil {
		t.Fatalf("MarkHoldForWindow(1h) after the roll-forward: %v", err)
	}
	pair, ok, err := f.sink.LoadLadder(ctx, f.asset, f.quote)
	if err != nil || !ok || !pair.Escalated || !pair.HoldUntil.Equal(escalatedHold) {
		t.Errorf("pair-level ladder after the window's next tick = (%+v, ok=%v, err=%v), want it still "+
			"escalated to %v", pair, ok, err, escalatedHold)
	}
	f.loseRedis()
	if again, _, _ := f.w.LoadStateForWindow(ctx, f.asset, f.quote, time.Hour); !again.Escalated {
		t.Errorf("1h window after its next tick and a second Redis loss = %+v, want it escalated", again)
	}
}

// TestFreezeWindowLadders_StaleMapKeepsItsOwnEscalation is the other stale direction, and why a stale map is folded, not
// discarded: the map holds the 1h escalation, then the rolled-back binary's 5m window overwrote the pair-level columns
// (last writer wins) with a later hold, so the columns are ahead on hold and behind on escalation.
func TestFreezeWindowLadders_StaleMapKeepsItsOwnEscalation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	f := newRollbackFixture(t, ctx, dsn)
	now := time.Now().UTC().Truncate(time.Microsecond)

	escalated := freeze.State{
		FiredAt: now.Add(-2*time.Hour - 5*time.Minute), HoldUntil: now.Add(10 * time.Minute),
		ExtensionsUsed: freeze.DefaultMaxExtensions, Escalated: true,
	}
	if err := f.w.MarkHoldForWindow(ctx, f.asset, f.quote, time.Hour, "0.874500000000", f.decision, escalated, 15*time.Minute); err != nil {
		t.Fatalf("MarkHoldForWindow(1h): %v", err)
	}
	laterHold := now.Add(40 * time.Minute)
	f.previousBinarySaveLadder(t, ctx, freeze.State{HoldUntil: laterHold}) // fresh: rung 0, not escalated

	f.loseRedis()
	got, ok, err := f.w.LoadStateForWindow(ctx, f.asset, f.quote, time.Hour)
	if err != nil || !ok {
		t.Fatalf("LoadStateForWindow(1h) = (ok=%v, err=%v), want present", ok, err)
	}
	if !got.Escalated || got.ExtensionsUsed != freeze.DefaultMaxExtensions || got.HoldUntil.Before(laterHold) {
		t.Errorf("1h window rehydrated %+v, want it still escalated at rung %d and held to at least %v",
			got, freeze.DefaultMaxExtensions, laterHold)
	}
}

// TestFreezeWindowLadders_SteadyStateNeverReadsAsStale: on a row only this binary wrote, the staleness rule must not
// fire, or it would mint an ownerless ladder for every freeze and rehydrate it onto never-frozen windows.
func TestFreezeWindowLadders_SteadyStateNeverReadsAsStale(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	f := newRollbackFixture(t, ctx, dsn)
	// 1600ns past the second: stored as 1µs in the column, 1.6µs in the map.
	base := time.Now().UTC().Truncate(time.Second)
	escalated := freeze.State{
		FiredAt: base.Add(-2 * time.Hour), HoldUntil: base.Add(25*time.Minute + 1600*time.Nanosecond),
		ExtensionsUsed: freeze.DefaultMaxExtensions, Escalated: true,
	}
	fresh := freeze.State{FiredAt: base.Add(-time.Minute), HoldUntil: base.Add(9*time.Minute + 700*time.Nanosecond)}
	if err := f.w.MarkHoldForWindow(ctx, f.asset, f.quote, time.Hour, "0.874500000000", f.decision, escalated, 30*time.Minute); err != nil {
		t.Fatalf("MarkHoldForWindow(1h): %v", err)
	}
	if err := f.w.MarkHoldForWindow(ctx, f.asset, f.quote, 5*time.Minute, "0.874500000000", f.decision, fresh, 14*time.Minute); err != nil {
		t.Fatalf("MarkHoldForWindow(5m): %v", err)
	}

	ladders, unowned, ok, err := f.sink.LoadWindowLadders(ctx, f.asset, f.quote)
	if err != nil || !ok {
		t.Fatalf("LoadWindowLadders = (ok=%v, err=%v), want present", ok, err)
	}
	if unowned.Active() {
		t.Errorf("a row only this binary wrote produced an ownerless ladder %+v — sub-microsecond "+
			"rounding read as staleness", unowned)
	}
	// Byte-identical round trip of what each window wrote.
	if got := ladders[time.Hour]; !got.HoldUntil.Equal(escalated.HoldUntil) || !got.Escalated || got.ExtensionsUsed != escalated.ExtensionsUsed {
		t.Errorf("1h entry read back %+v, want %+v", got, escalated)
	}
	if got := ladders[5*time.Minute]; !got.HoldUntil.Equal(fresh.HoldUntil) || got.Escalated || got.ExtensionsUsed != 0 {
		t.Errorf("5m entry read back %+v, want %+v — a sibling's escalation leaked into it", got, fresh)
	}
	f.loseRedis()
	if day, _, _ := f.w.LoadStateForWindow(ctx, f.asset, f.quote, 24*time.Hour); day.Active() {
		t.Errorf("24h window rehydrated %+v — it was never frozen", day)
	}
}

// TestFreezeWindowLadders_EscalationSurvivesSiblingWrite: DB-backed regression for the pair-keyed durable ladder, driven
// through freeze.Writer on the real FreezeEventSink so it fails against a non-laddered build for the right reason.
func TestFreezeWindowLadders_EscalationSurvivesSiblingWrite(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	sink := timescale.NewFreezeEventSink(store)

	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	newWriter := func() *freeze.Writer {
		t.Helper()
		w, werr := freeze.NewWriter(rdb, 0,
			freeze.WithEventSink(sink),
			freeze.WithLadderStore(sink, 0))
		if werr != nil {
			t.Fatalf("NewWriter: %v", werr)
		}
		return w
	}

	asset, _ := c.NewClassicAsset("USDC", "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN")
	quote, _ := c.NewFiatAsset("USD")
	decision := anomaly.Decision{
		Action:       anomaly.ActionFreeze,
		Class:        anomaly.ClassStablecoin,
		DeviationPct: 14.2,
		Reason:       "phase2:3_signal_AND confidence=0.121 z=8.44 sources=1",
	}

	const (
		short = 5 * time.Minute
		long  = time.Hour
		day   = 24 * time.Hour
	)
	now := time.Now().UTC().Truncate(time.Microsecond)
	escalated := freeze.State{
		FiredAt:        now.Add(-2*time.Hour - 5*time.Minute),
		HoldUntil:      now.Add(25 * time.Minute),
		ExtensionsUsed: freeze.DefaultMaxExtensions,
		Escalated:      true,
		Corroborated:   true,
	}
	fresh := freeze.State{
		FiredAt:   now.Add(-time.Minute),
		HoldUntil: now.Add(9 * time.Minute),
	}

	w := newWriter()
	if err := w.MarkHoldForWindow(ctx, asset, quote, long, "0.874500000000",
		decision, escalated, 30*time.Minute); err != nil {
		t.Fatalf("MarkHoldForWindow(1h): %v", err)
	}
	if err := w.MarkHoldForWindow(ctx, asset, quote, short, "0.874500000000",
		decision, fresh, 14*time.Minute); err != nil {
		t.Fatalf("MarkHoldForWindow(5m): %v", err)
	}
	// One market event is still ONE freeze_events row: the window dimension
	// belongs to the ladder, not to the /v1/anomalies timeline.
	if n := countOpenFreezeRows(t, ctx, dsn, asset.String(), quote.String()); n != 1 {
		t.Fatalf("two frozen windows opened %d freeze_events rows, want 1", n)
	}

	mr.FlushAll()            // Redis is lost…
	restarted := newWriter() // …and the aggregator restarts.

	gotLong, ok, err := restarted.LoadStateForWindow(ctx, asset, quote, long)
	if err != nil || !ok {
		t.Fatalf("LoadStateForWindow(1h) = (ok=%v, err=%v), want present", ok, err)
	}
	if !gotLong.Escalated || gotLong.ExtensionsUsed != freeze.DefaultMaxExtensions ||
		!gotLong.HoldUntil.Equal(escalated.HoldUntil) || !gotLong.FiredAt.Equal(escalated.FiredAt) {
		t.Errorf("1h window rehydrated %+v, want its own escalated ladder %+v — a sibling "+
			"window's later durable write replaced it, so an ESCALATED freeze resumes "+
			"auto-unfreezing after a Redis loss", gotLong, escalated)
	}

	gotShort, _, err := restarted.LoadStateForWindow(ctx, asset, quote, short)
	if err != nil {
		t.Fatalf("LoadStateForWindow(5m): %v", err)
	}
	if gotShort.Escalated || gotShort.ExtensionsUsed != 0 ||
		!gotShort.HoldUntil.Equal(fresh.HoldUntil) || !gotShort.FiredAt.Equal(fresh.FiredAt) {
		t.Errorf("5m window rehydrated %+v, want its own fresh ladder %+v", gotShort, fresh)
	}

	gotDay, present, err := restarted.LoadStateForWindow(ctx, asset, quote, day)
	if err != nil {
		t.Fatalf("LoadStateForWindow(24h): %v", err)
	}
	if gotDay.Active() {
		t.Errorf("24h window rehydrated %+v — it was never frozen", gotDay)
	}
	if !present {
		t.Error("the pair is frozen, so a window with no ladder of its own must still read " +
			"PRESENT: absent under a live freeze is the operator override")
	}

	// The pair-level view is the fail-closed summary, because the recovery
	// worker and `freeze-unfreeze -list` read it with no window to name:
	// the furthest hold, the highest rung, escalated if ANY window is.
	pair, ok, err := sink.LoadLadder(ctx, asset, quote)
	if err != nil || !ok {
		t.Fatalf("LoadLadder = (ok=%v, err=%v), want the pair-level summary", ok, err)
	}
	if !pair.Escalated || pair.ExtensionsUsed != freeze.DefaultMaxExtensions ||
		!pair.HoldUntil.Equal(escalated.HoldUntil) {
		t.Errorf("pair-level summary = %+v, want escalated / %d extensions / hold %v — "+
			"it must never under-state the pair's worst window",
			pair, freeze.DefaultMaxExtensions, escalated.HoldUntil)
	}

	// The 5m window auto-releases while the 1h stays frozen. Its durable
	// entry must go with it; the 1h window's must not move.
	if err := restarted.RetireWindowLadder(ctx, asset, quote, short); err != nil {
		t.Fatalf("RetireWindowLadder(5m): %v", err)
	}
	mr.FlushAll()
	if got, _, lerr := restarted.LoadStateForWindow(ctx, asset, quote, short); lerr != nil || got.Active() {
		t.Errorf("5m window after its release = (%+v, err=%v), want no ladder — the durable "+
			"record kept a freeze that had already ended", got, lerr)
	}
	if got, ok, lerr := restarted.LoadStateForWindow(ctx, asset, quote, long); lerr != nil || !ok || !got.Escalated {
		t.Errorf("1h window after its sibling's release = (%+v, ok=%v, err=%v), want its "+
			"escalated ladder untouched", got, ok, lerr)
	}

	// The operator override still ends EVERY window: Clear retires the
	// ladder, MarkRecovered closes the row (freeze-unfreeze does both).
	if err := restarted.Clear(ctx, asset, quote); err != nil {
		t.Fatalf("Clear: %v", err)
	}
	if err := sink.MarkRecovered(ctx, asset, quote, "operator:test"); err != nil {
		t.Fatalf("MarkRecovered: %v", err)
	}
	for _, window := range []time.Duration{short, long, day} {
		got, ok, lerr := restarted.LoadStateForWindow(ctx, asset, quote, window)
		if lerr != nil || ok || got.Active() {
			t.Errorf("window %s after the operator override = (%+v, ok=%v, err=%v), want absent — "+
				"a human could not end an escalated freeze", window, got, ok, lerr)
		}
	}
}

// TestFreezeLadder_DurableAcrossRedisLoss pins what only Postgres answers: 0119 applies to a freeze_events chunk compressed
// before migrating (a fresh DB has none), SaveLadder/LoadLadder round-trip the exact ladder on the open row, LoadLadder
// honours recovered_at IS NULL, and a pre-0119 row (NULL hold_until) reads as no durable ladder, not a zero one.
func TestFreezeLadder_DurableAcrossRedisLoss(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	// Migrate to 0118 first, seed a row, COMPRESS its chunk, and only then
	// apply 0119 — otherwise "ADD COLUMN on a compressed hypertable" is an
	// untested claim, since a freshly-migrated database has no chunks at all.
	applyMigrationsUpTo(t, dsn, 118)
	seedAndCompressFreezeChunk(t, ctx, dsn)
	applyMigrations(t, dsn)
	assertFreezeChunkStillCompressed(t, ctx, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	sink := timescale.NewFreezeEventSink(store)

	asset, _ := c.NewClassicAsset("USDC", "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN")
	quote, _ := c.NewFiatAsset("USD")

	decision := anomaly.Decision{
		Action:       anomaly.ActionFreeze,
		Class:        anomaly.ClassStablecoin,
		DeviationPct: 14.2,
		Reason:       "phase2:3_signal_AND confidence=0.121 z=8.44 sources=1",
	}

	// ── no row yet: SaveLadder must not create one, LoadLadder must
	//    report absent. RecordFreeze owns row creation; a SaveLadder that
	//    could insert would race it into two open rows for one pair.
	// ErrNotFound, not a silent nil: "no open row matched" is exactly what
	// "migration 0119 has not been applied while the new binary is running"
	// looks like from the caller's side, and the Writer counts it on
	// stellarindex_anomaly_freeze_ladder_write_failures_total. Swallowing it
	// made that whole failure mode invisible.
	if err := sink.SaveLadder(ctx, asset, quote, freeze.State{
		FiredAt: time.Now().UTC(), HoldUntil: time.Now().UTC().Add(time.Hour),
	}); !errors.Is(err, timescale.ErrNotFound) {
		t.Fatalf("SaveLadder with no open row = %v, want ErrNotFound", err)
	}
	if _, ok, lerr := sink.LoadLadder(ctx, asset, quote); lerr != nil || ok {
		t.Fatalf("LoadLadder before any freeze = (ok=%v, err=%v), want (false, nil)", ok, lerr)
	}
	if n := countFreezeRows(t, ctx, dsn, asset.String(), quote.String()); n != 0 {
		t.Fatalf("SaveLadder created %d freeze_events row(s); it must only ever UPDATE", n)
	}

	// ── the freeze fires ────────────────────────────────────────────
	if err := sink.RecordFreeze(ctx, asset, quote, "0.874500000000", decision); err != nil {
		t.Fatalf("RecordFreeze: %v", err)
	}

	// A pre-0119 row: the ladder columns are NULL and must read as "no durable ladder"; restoring a zero ladder ({FiredAt:
	// frozen_at, ExtensionsUsed: 0, Escalated: false}) would silently reset the 2-hour escalation clock.
	if _, ok, lerr := sink.LoadLadder(ctx, asset, quote); lerr != nil || ok {
		t.Fatalf("LoadLadder on a NULL-ladder (pre-0119) row = (ok=%v, err=%v), want (false, nil)", ok, lerr)
	}

	// ── the pair climbs the whole ladder and escalates ──────────────
	now := time.Now().UTC().Truncate(time.Microsecond) // pg timestamptz resolution
	want := freeze.State{
		FiredAt:        now.Add(-2*time.Hour - 5*time.Minute),
		HoldUntil:      now.Add(25 * time.Minute),
		ExtensionsUsed: freeze.DefaultMaxExtensions,
		Escalated:      true,
		Corroborated:   true,
	}
	if err := sink.SaveLadder(ctx, asset, quote, want); err != nil {
		t.Fatalf("SaveLadder: %v", err)
	}

	got, ok, err := sink.LoadLadder(ctx, asset, quote)
	if err != nil {
		t.Fatalf("LoadLadder: %v", err)
	}
	if !ok {
		t.Fatal("LoadLadder reported no durable ladder right after SaveLadder — " +
			"a Redis flush would release this ESCALATED freeze")
	}
	if !got.Escalated {
		t.Error("Escalated did not round-trip; an escalated freeze would resume auto-unfreezing")
	}
	if got.ExtensionsUsed != freeze.DefaultMaxExtensions {
		t.Errorf("ExtensionsUsed = %d, want %d", got.ExtensionsUsed, freeze.DefaultMaxExtensions)
	}
	if !got.Corroborated {
		t.Error("Corroborated did not round-trip")
	}
	if !got.HoldUntil.Equal(want.HoldUntil) {
		t.Errorf("HoldUntil = %v, want %v", got.HoldUntil, want.HoldUntil)
	}
	// FiredAt comes from the row's own frozen_at (stamped by RecordFreeze),
	// NOT from the State we saved — the column is deliberately not
	// duplicated. Assert it is the freeze's real age, i.e. very recent.
	if age := time.Since(got.FiredAt); age > time.Minute || age < 0 {
		t.Errorf("FiredAt = %v (age %v); want the row's own frozen_at, not the saved State's", got.FiredAt, age)
	}

	// Only ONE row exists — repeated SaveLadder is an UPDATE, never an insert.
	if n := countFreezeRows(t, ctx, dsn, asset.String(), quote.String()); n != 1 {
		t.Fatalf("freeze_events holds %d row(s) for the pair, want 1", n)
	}

	// Clear's ladder retirement: freeze.Writer.Clear writes a zero State on auto-release, which must NULL hold_until so
	// LoadLadder reports no ladder while the row is still open (the recovery worker sweeps every 60s). Otherwise a
	// restarting aggregator re-freezes a pair whose anomaly already cleared.
	if err := sink.SaveLadder(ctx, asset, quote, freeze.State{}); err != nil {
		t.Fatalf("SaveLadder(zero): %v", err)
	}
	if _, ok, lerr := sink.LoadLadder(ctx, asset, quote); lerr != nil || ok {
		t.Fatalf("LoadLadder after a retiring zero-State save = (ok=%v, err=%v), want (false, nil)", ok, lerr)
	}
	if n := countOpenFreezeRows(t, ctx, dsn, asset.String(), quote.String()); n != 1 {
		t.Fatalf("retiring the ladder closed the row (%d open); recovered_at is the recovery worker's job", n)
	}
	// The retire must keep the escalation history the timeline reports.
	rows, lerr := store.ListFreezeEvents(ctx, true, 10)
	if lerr != nil || len(rows) != 1 {
		t.Fatalf("ListFreezeEvents after retire = (%d rows, err=%v), want 1 row", len(rows), lerr)
	}
	if r := rows[0]; r.Escalated == nil || !*r.Escalated || r.ExtensionsUsed == nil ||
		*r.ExtensionsUsed != freeze.DefaultMaxExtensions || r.Corroborated == nil || !*r.Corroborated {
		t.Fatalf("retire erased the escalation history: escalated=%v extensions_used=%v corroborated=%v",
			r.Escalated, r.ExtensionsUsed, r.Corroborated)
	}
	// Restore the escalated ladder for the override assertions below.
	if err := sink.SaveLadder(ctx, asset, quote, want); err != nil {
		t.Fatalf("SaveLadder(restore): %v", err)
	}

	// The operator override's durable half: freeze-unfreeze stamps recovered_at, after which the ladder must read absent, or
	// a human could not end a freeze that by construction never ends on its own.
	if err := sink.MarkRecovered(ctx, asset, quote, "operator:test"); err != nil {
		t.Fatalf("MarkRecovered: %v", err)
	}
	if _, ok, lerr := sink.LoadLadder(ctx, asset, quote); lerr != nil || ok {
		t.Fatalf("LoadLadder after MarkRecovered = (ok=%v, err=%v), want (false, nil) — "+
			"the operator override must still stick", ok, lerr)
	}
	// And a save against a closed row must not resurrect it — reported as
	// ErrNotFound for the same reason as above.
	if err := sink.SaveLadder(ctx, asset, quote, want); !errors.Is(err, timescale.ErrNotFound) {
		t.Fatalf("SaveLadder after recovery = %v, want ErrNotFound", err)
	}
	if _, ok, _ := sink.LoadLadder(ctx, asset, quote); ok {
		t.Fatal("SaveLadder re-opened a recovered freeze")
	}
}

func countFreezeRows(t *testing.T, ctx context.Context, dsn, assetID, quoteID string) int {
	t.Helper()
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer func() { _ = db.Close() }()
	var n int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM freeze_events WHERE asset_id = $1 AND quote_id = $2`,
		assetID, quoteID).Scan(&n); err != nil {
		t.Fatalf("count freeze_events: %v", err)
	}
	return n
}

func countOpenFreezeRows(t *testing.T, ctx context.Context, dsn, assetID, quoteID string) int {
	t.Helper()
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer func() { _ = db.Close() }()
	var n int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM freeze_events
		  WHERE asset_id = $1 AND quote_id = $2 AND recovered_at IS NULL`,
		assetID, quoteID).Scan(&n); err != nil {
		t.Fatalf("count open freeze_events: %v", err)
	}
	return n
}

// seedAndCompressFreezeChunk inserts one historical freeze_events row and compresses its chunk so migration 0119's ALTER
// TABLE runs against genuinely compressed data. The chunk interval is 30 days (migration 0018), so a well-past row lands
// in its own chunk and cannot collide with later rows.
func seedAndCompressFreezeChunk(t *testing.T, ctx context.Context, dsn string) {
	t.Helper()
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer func() { _ = db.Close() }()

	if _, err := db.ExecContext(ctx, `
		INSERT INTO freeze_events (asset_id, quote_id, frozen_at, frozen_at_ledger,
		                           reason, frozen_value, recovered_at, recovered_at_ledger)
		VALUES ('native', 'fiat:USD', TIMESTAMPTZ '2025-01-15 00:00:00Z', 1000,
		        'outlier_storm', 0.5, TIMESTAMPTZ '2025-01-15 01:00:00Z', 1100)`); err != nil {
		t.Fatalf("seed historical freeze row: %v", err)
	}
	var compressed int
	if err := db.QueryRowContext(ctx, `
		SELECT count(*) FROM (
			SELECT compress_chunk(c) FROM show_chunks('freeze_events') c
		) s`).Scan(&compressed); err != nil {
		t.Fatalf("compress_chunk: %v", err)
	}
	if compressed == 0 {
		t.Fatal("no freeze_events chunk was compressed — the ADD COLUMN-on-compressed claim would be vacuous")
	}
}

// assertFreezeChunkStillCompressed proves the precondition held through the migration; if 0119 silently decompressed the
// chunk (or the seed never compressed one), the headline claim would be false while every other assertion passed.
func assertFreezeChunkStillCompressed(t *testing.T, ctx context.Context, dsn string) {
	t.Helper()
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer func() { _ = db.Close() }()

	var n int
	if err := db.QueryRowContext(ctx, `
		SELECT count(*) FROM timescaledb_information.chunks
		 WHERE hypertable_name = 'freeze_events' AND is_compressed`).Scan(&n); err != nil {
		t.Fatalf("read chunk compression state: %v", err)
	}
	if n == 0 {
		t.Fatal("no compressed freeze_events chunk after migrating — 0119 was NOT exercised against compressed data")
	}
}

// applyMigrationsUpTo runs migrations up to and including `version`, like applyMigrations (explorer_test.go) but
// stopping short so a test can act on the schema before a given migration lands.
func applyMigrationsUpTo(t *testing.T, dsn string, version uint) {
	t.Helper()
	if err := applyMigrationsUpToErr(dsn, version); err != nil {
		t.Fatalf("migrate to %d: %v", version, err)
	}
}

// applyMigrationsUpToErr returns the migration's error instead of failing.
func applyMigrationsUpToErr(dsn string, version uint) error {
	_, thisFile, _, _ := runtime.Caller(0)
	migrationsDir := filepath.Join(filepath.Dir(thisFile), "..", "..", "migrations")
	m, err := migrate.New("file://"+migrationsDir, dsn)
	if err != nil {
		return fmt.Errorf("migrate.New: %w", err)
	}
	defer func() { _, _ = m.Close() }()
	return m.Migrate(version)
}

// TestFreezeReleaseWindow_RedisLossKeepsEscalatedSibling: freeze.Writer wired as cmd/stellarindex-aggregator wires it,
// over the real FreezeEventSink. One pair with 1h escalated and 5m about to recover; Redis loses the marker and 5m
// releases.
func TestFreezeReleaseWindow_RedisLossKeepsEscalatedSibling(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	sink := timescale.NewFreezeEventSink(store)

	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	w, err := freeze.NewWriter(rdb, 0, freeze.WithEventSink(sink), freeze.WithLadderStore(sink, 0))
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}

	asset, _ := c.NewClassicAsset("USDC", "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN")
	quote, _ := c.NewFiatAsset("USD")
	decision := anomaly.Decision{
		Action: anomaly.ActionFreeze, Class: anomaly.ClassStablecoin, Reason: "phase2:3_signal_AND",
	}
	const (
		short = 5 * time.Minute
		long  = time.Hour
	)
	now := time.Now().UTC().Truncate(time.Microsecond)
	escalated := freeze.State{
		FiredAt: now.Add(-2*time.Hour - 5*time.Minute), HoldUntil: now.Add(25 * time.Minute),
		ExtensionsUsed: freeze.DefaultMaxExtensions, Escalated: true, Corroborated: true,
	}
	recovering := freeze.State{FiredAt: now.Add(-11 * time.Minute), HoldUntil: now.Add(9 * time.Minute), UnfreezeStreak: 1}
	if err := w.MarkHoldForWindow(ctx, asset, quote, long, "0.874500000000", decision, escalated, 30*time.Minute); err != nil {
		t.Fatalf("MarkHoldForWindow(1h): %v", err)
	}
	if err := w.MarkHoldForWindow(ctx, asset, quote, short, "0.874500000000", decision, recovering, 14*time.Minute); err != nil {
		t.Fatalf("MarkHoldForWindow(5m): %v", err)
	}

	mr.FlushAll() // Redis loses the marker while both windows are frozen.
	kept, err := w.ReleaseWindow(ctx, asset, quote, short)
	if err != nil {
		t.Fatalf("ReleaseWindow(5m): %v", err)
	}
	if !kept {
		t.Error("ReleaseWindow reported the freeze cleared while the durable record held the 1h window's escalated ladder")
	}

	// The row itself: still one open row, still carrying the pair-level
	// escalation, and window_ladders holding the 1h entry and ONLY it.
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("sql open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	var (
		hold sql.NullTime
		esc  sql.NullBool
		ext  sql.NullInt64
		raw  sql.NullString
	)
	if err := db.QueryRowContext(ctx, `
		SELECT hold_until, escalated, extensions_used, window_ladders::text
		  FROM freeze_events
		 WHERE asset_id = $1 AND quote_id = $2 AND recovered_at IS NULL`,
		asset.String(), quote.String()).Scan(&hold, &esc, &ext, &raw); err != nil {
		t.Fatalf("read open freeze row: %v", err)
	}
	if !hold.Valid || !hold.Time.Equal(escalated.HoldUntil) || !esc.Bool || ext.Int64 != int64(freeze.DefaultMaxExtensions) {
		t.Errorf("pair-level ladder after the 5m release = (hold_until=%v valid=%v, escalated=%v, extensions_used=%d), "+
			"want the 1h window's (%v, true, %d): the release retired the whole durable record",
			hold.Time.UTC(), hold.Valid, esc.Bool, ext.Int64, escalated.HoldUntil, freeze.DefaultMaxExtensions)
	}
	ladders := map[string]freeze.State{}
	if raw.Valid {
		if err := json.Unmarshal([]byte(raw.String), &ladders); err != nil {
			t.Fatalf("decode window_ladders %q: %v", raw.String, err)
		}
	}
	if got, held := ladders["3600"]; !held || !got.Escalated || len(ladders) != 1 {
		t.Errorf("window_ladders after the 5m release = %s, want exactly the 1h window's escalated entry", raw.String)
	}

	// And what the 1h window's next cold read gets from it.
	got, ok, err := w.LoadStateForWindow(ctx, asset, quote, long)
	if err != nil || !ok || !got.Escalated || got.ExtensionsUsed != freeze.DefaultMaxExtensions ||
		!got.HoldUntil.Equal(escalated.HoldUntil) {
		t.Errorf("LoadStateForWindow(1h) = (%+v, ok=%v, err=%v), want its escalated ladder %+v",
			got, ok, err, escalated)
	}
	if got, _, _ := w.LoadStateForWindow(ctx, asset, quote, short); got.Active() {
		t.Errorf("the released 5m window's durable ladder survived: %+v", got)
	}

	// The last window's release is still a clear: nothing left to protect.
	// (The operator override reaches the same end through Writer.Clear.)
	kept, err = w.ReleaseWindow(ctx, asset, quote, long)
	if err != nil || kept {
		t.Errorf("ReleaseWindow(1h) with no sibling left = (kept=%v, err=%v), want (false, nil)", kept, err)
	}
	if _, ok, _ := w.LoadStateForWindow(ctx, asset, quote, long); ok {
		t.Error("the durable ladder outlived the pair's last release")
	}
}
