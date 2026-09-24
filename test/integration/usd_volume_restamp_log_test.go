//go:build integration

package integration_test

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/jackc/pgx/v5/stdlib"
)

// restampLogRow is one usd_volume_restamp_log row, NUMERICs as text.
type restampLogRow struct {
	prior    *string
	priorGen int64
	written  string
	gen      int64
}

// readRestampLog returns every before-image logged for one trade, oldest
// first.
func readRestampLog(t *testing.T, ctx context.Context, db *sql.DB, source string, ledger uint32) []restampLogRow {
	t.Helper()
	rows, err := db.QueryContext(ctx, `
		SELECT prior_usd_volume::text, prior_derive_generation, usd_volume::text, derive_generation
		  FROM usd_volume_restamp_log
		 WHERE source = $1 AND ledger = $2
		 ORDER BY id`, source, int64(ledger))
	if err != nil {
		t.Fatalf("read usd_volume_restamp_log: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var out []restampLogRow
	for rows.Next() {
		var (
			r     restampLogRow
			prior sql.NullString
		)
		if err := rows.Scan(&prior, &r.priorGen, &r.written, &r.gen); err != nil {
			t.Fatalf("scan usd_volume_restamp_log: %v", err)
		}
		if prior.Valid {
			r.prior = &prior.String
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate usd_volume_restamp_log: %v", err)
	}
	return out
}

// restampLogCount is how many before-images a run's generation logged.
func restampLogCount(t *testing.T, ctx context.Context, db *sql.DB, gen int64) int64 {
	t.Helper()
	var n int64
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM usd_volume_restamp_log WHERE derive_generation = $1`, gen).Scan(&n); err != nil {
		t.Fatalf("count usd_volume_restamp_log: %v", err)
	}
	return n
}

// restampLogMigration documents the undo statement an operator runs.
const restampLogMigration = "0173_usd_volume_restamp_log.up.sql"

// undoRecipeFromMigration lifts the undo statement out of 0173's header
// comment, `$gen` bound as `$1`, so the test executes exactly what the
// operator is told to run.
func undoRecipeFromMigration(t *testing.T) string {
	t.Helper()
	_, thisFile, _, _ := runtime.Caller(0)
	src, err := os.ReadFile(filepath.Join(filepath.Dir(thisFile), "..", "..", "migrations", restampLogMigration))
	if err != nil {
		t.Fatalf("read %s: %v", restampLogMigration, err)
	}
	var stmt []string
	for _, line := range strings.Split(string(src), "\n") {
		body, ok := strings.CutPrefix(line, "--")
		if !ok {
			continue
		}
		body = strings.TrimSpace(body)
		if len(stmt) == 0 && body != "UPDATE trades t" {
			continue
		}
		stmt = append(stmt, body)
		if strings.HasSuffix(body, ";") {
			break
		}
	}
	recipe := strings.ReplaceAll(strings.TrimSuffix(strings.Join(stmt, "\n"), ";"), "$gen", "$1")
	for _, want := range []string{
		"SET usd_volume = l.prior_usd_volume,",
		"FROM (SELECT DISTINCT ON (source, ledger, tx_hash, op_index, ts) *",
		"ORDER BY source, ledger, tx_hash, op_index, ts, id) l",
		"AND t.derive_generation = $1",
	} {
		if !strings.Contains(recipe, want) {
			t.Fatalf("%s header undo statement lost %q:\n%s", restampLogMigration, want, recipe)
		}
	}
	return recipe
}

// undoRestampRun runs 0173's documented undo for one run's generation.
func undoRestampRun(t *testing.T, ctx context.Context, db *sql.DB, gen int64) int64 {
	t.Helper()
	res, err := db.ExecContext(ctx, undoRecipeFromMigration(t), gen)
	if err != nil {
		t.Fatalf("undo restamp run %d: %v", gen, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func sameNumeric(a *string, b *string) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

// TestUSDVolumeRestampLog_Migration0173DownRefusesWhileHoldingBeforeImages:
// the log is the only record of what a restamp overwrote, so 0173's down
// must refuse while it holds a row and drop the table once it is empty.
func TestUSDVolumeRestampLog_Migration0173DownRefusesWhileHoldingBeforeImages(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	if _, err := db.ExecContext(ctx, `
		INSERT INTO usd_volume_restamp_log
		       (source, ledger, tx_hash, op_index, ts, prior_usd_volume, prior_derive_generation, usd_volume, derive_generation)
		VALUES ('sdex', 1, $1, 0, now(), 1.25, 0, 1.00000000, 1756400000)`, strings.Repeat("ab", 32)); err != nil {
		t.Fatalf("seed log row: %v", err)
	}

	_, thisFile, _, _ := runtime.Caller(0)
	migrationsDir := "file://" + filepath.Join(filepath.Dir(thisFile), "..", "..", "migrations")

	// The RAISE EXCEPTION aborts the down's transaction mid-statement and
	// golang-migrate's postgres driver never issues a ROLLBACK on that
	// connection, so a later command on the same *Migrate instance (Force,
	// or another Migrate) fails to take its advisory lock ("database
	// locked") rather than surfacing the refusal. assertDownRefused in
	// migrations_test.go hits the same hazard: close the instance that ran
	// the failing down immediately, then use a fresh one per step.
	m, err := migrate.New(migrationsDir, dsn)
	if err != nil {
		t.Fatalf("migrate.New: %v", err)
	}
	err = m.Migrate(172)
	_, _ = m.Close()
	if err == nil || !strings.Contains(err.Error(), "still holds before-images") {
		t.Fatalf("0173 down with a logged before-image: err = %v, want the RAISE refusal", err)
	}
	assertTableExists(t, db, ctx, "usd_volume_restamp_log")

	// The failed down rolled back inside its own transaction; clear
	// golang-migrate's dirty flag at the version the schema is really at.
	f, err := migrate.New(migrationsDir, dsn)
	if err != nil {
		t.Fatalf("migrate.New: %v", err)
	}
	err = f.Force(173)
	_, _ = f.Close()
	if err != nil {
		t.Fatalf("force 173: %v", err)
	}

	if _, err := db.ExecContext(ctx, `DELETE FROM usd_volume_restamp_log`); err != nil {
		t.Fatal(err)
	}

	d, err := migrate.New(migrationsDir, dsn)
	if err != nil {
		t.Fatalf("migrate.New: %v", err)
	}
	defer func() { _, _ = d.Close() }()
	if err := d.Migrate(172); err != nil {
		t.Fatalf("0173 down on an empty log: %v", err)
	}
	assertTableAbsent(t, db, ctx, "usd_volume_restamp_log")
}
