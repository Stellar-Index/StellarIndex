//go:build integration

package integration_test

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/Stellar-Index/StellarIndex/internal/aggregate/anomaly"
	"github.com/Stellar-Index/StellarIndex/internal/aggregate/freeze"
	c "github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// TestFreezeLadder_DurableAcrossRedisLoss is the DB-backed half of the
// migration-0119 fix, run against real TimescaleDB.
//
// The unit test in internal/aggregate/freeze pins the Writer's policy
// against a fake store. This pins the part only Postgres can answer:
//
//   - migration 0119 actually applies to a `freeze_events` hypertable with
//     a COMPRESSED CHUNK in it. That is the risky bit and it is NOT
//     exercised by simply running the migrations on a fresh database: a
//     fresh DB has zero chunks, so ADD COLUMN touches nothing and the test
//     would prove only that the SQL parses. The fixture below therefore
//     inserts a row into an OLD chunk and calls compress_chunk() BEFORE
//     migrating, so the ALTER really does run against compressed data. A
//     failure here is a failed deploy, and the pipeline rolls back the
//     binary but never the schema (CS-099);
//   - SaveLadder's UPDATE lands on the open row and LoadLadder reads back
//     the EXACT ladder, through real timestamptz round-tripping;
//   - LoadLadder honours `recovered_at IS NULL`, which is what makes
//     `stellarindex-ops freeze-unfreeze`'s override still stick;
//   - a pre-0119 row (NULL hold_until) reports "no durable ladder" rather
//     than a zero one, so it degrades to the old behaviour instead of
//     restoring a freeze that reads as "fired just now".
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

	// A pre-0119 row shape: the ladder columns are still NULL. It must
	// read as "no durable ladder" rather than a zero one — restoring
	// {FiredAt: <frozen_at>, ExtensionsUsed: 0, Escalated: false} would
	// silently reset the 2-hour escalation clock.
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

	// ── Clear's ladder retirement, on real SQL ─────────────────────
	// freeze.Writer.Clear writes back a zero State on auto-release, which
	// must NULL hold_until and so make LoadLadder report no ladder even
	// though the ROW is still open (the recovery worker sweeps every 60s).
	// Without this the pre-sweep window lets a restarting aggregator
	// re-freeze a pair whose anomaly had already cleared.
	if err := sink.SaveLadder(ctx, asset, quote, freeze.State{}); err != nil {
		t.Fatalf("SaveLadder(zero): %v", err)
	}
	if _, ok, lerr := sink.LoadLadder(ctx, asset, quote); lerr != nil || ok {
		t.Fatalf("LoadLadder after a retiring zero-State save = (ok=%v, err=%v), want (false, nil)", ok, lerr)
	}
	if n := countOpenFreezeRows(t, ctx, dsn, asset.String(), quote.String()); n != 1 {
		t.Fatalf("retiring the ladder closed the row (%d open); recovered_at is the recovery worker's job", n)
	}
	// Restore the escalated ladder for the override assertions below.
	if err := sink.SaveLadder(ctx, asset, quote, want); err != nil {
		t.Fatalf("SaveLadder(restore): %v", err)
	}

	// ── the operator override's durable half ───────────────────────
	// `stellarindex-ops freeze-unfreeze` stamps recovered_at. After that
	// the ladder must read absent, or a human could not end a freeze that
	// by construction never ends on its own.
	if err := sink.MarkRecovered(ctx, asset, quote); err != nil {
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

// seedAndCompressFreezeChunk inserts one historical freeze_events row and
// compresses the chunk that holds it, so migration 0119's ALTER TABLE runs
// against genuinely compressed data.
//
// TimescaleDB's chunk interval for this hypertable is 30 days (migration
// 0018), so a row dated well in the past lands in its own chunk and cannot
// collide with the rows the test writes later.
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

// assertFreezeChunkStillCompressed proves the precondition held THROUGH the
// migration: if 0119 had silently decompressed the chunk (or if the seed had
// not compressed one), the test's headline claim would be false while every
// other assertion still passed.
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

// applyMigrationsUpTo runs migrations from the repo tree up to and including
// `version`. Mirrors applyMigrations (storage_test.go) but stops short, so a
// test can act on the schema BEFORE a specific migration lands — here, to
// create a compressed chunk that 0119's ALTER TABLE then has to survive.
func applyMigrationsUpTo(t *testing.T, dsn string, version uint) {
	t.Helper()
	_, thisFile, _, _ := runtime.Caller(0)
	migrationsDir := filepath.Join(filepath.Dir(thisFile), "..", "..", "migrations")
	m, err := migrate.New("file://"+migrationsDir, dsn)
	if err != nil {
		t.Fatalf("migrate.New: %v", err)
	}
	defer func() { _, _ = m.Close() }()
	if err := m.Migrate(version); err != nil {
		t.Fatalf("migrate to %d: %v", version, err)
	}
}
