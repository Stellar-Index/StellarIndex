//go:build integration

package integration_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/postgres"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	_ "github.com/jackc/pgx/v5/stdlib"
)

// 0019's stored comment, verbatim; 0173's down must restore exactly this.
const divergenceStatusComment0019 = `Whether |delta_pct| exceeded the per-(reference,pair) ` +
	`threshold at observation time. Distinct from the persistent ` +
	`flag returned by the API (which is "any reference firing").`

// TestDivergenceObservationsStatusComment pins the catalog comment on
// divergence_observations.status after migration 0173 up and down. The
// comment lives in pg_description, so only a real database can say what an
// operator reads through `\d+`.
func TestDivergenceObservationsStatusComment(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()

	got := divergenceStatusComment(t, ctx, db)
	// The flag is quorum + median/agreement + debounce; a row is one
	// reference against one threshold.
	for _, want := range []string{
		"NOT the API flag",
		"single threshold",
		"min_sources_for_warning",
		"median breach",
		"WarningPersistence",
		"div:<base>/<quote>",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("status comment missing %q; got %q", want, got)
		}
	}
	for _, bad := range []string{"any reference firing", "per-(reference,pair)"} {
		if strings.Contains(got, bad) {
			t.Errorf("status comment still claims %q; got %q", bad, got)
		}
	}

	_, thisFile, _, _ := runtime.Caller(0)
	migrationsDir := filepath.Join(filepath.Dir(thisFile), "..", "..", "migrations")
	m, err := migrate.New("file://"+migrationsDir, dsn)
	if err != nil {
		t.Fatalf("migrate.New: %v", err)
	}
	defer func() { _, _ = m.Close() }()
	if err := m.Migrate(172); err != nil {
		t.Fatalf("migrate down to 172: %v", err)
	}
	if got := divergenceStatusComment(t, ctx, db); got != divergenceStatusComment0019 {
		t.Errorf("0173 down did not restore 0019's comment verbatim:\n got %q\nwant %q", got, divergenceStatusComment0019)
	}
}

func divergenceStatusComment(t *testing.T, ctx context.Context, db *sql.DB) string {
	t.Helper()
	var c sql.NullString
	if err := db.QueryRowContext(ctx,
		`SELECT col_description('divergence_observations'::regclass, a.attnum)
		   FROM pg_attribute a
		  WHERE a.attrelid = 'divergence_observations'::regclass AND a.attname = 'status'`,
	).Scan(&c); err != nil {
		t.Fatalf("read divergence_observations.status comment: %v", err)
	}
	if !c.Valid {
		t.Fatal("divergence_observations.status has no catalog comment")
	}
	return c.String
}
