//go:build integration

package integration_test

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"
)

func openMigrated(t *testing.T) (context.Context, *sql.DB) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	t.Cleanup(cancel)
	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return ctx, db
}

// The stored comment must tell a \d+ reader that rows drive the price correction, not a decline.
func TestNonstandardDecimalsAssetsComment(t *testing.T) {
	ctx, db := openMigrated(t)
	var c string
	if err := db.QueryRowContext(ctx,
		`SELECT obj_description('nonstandard_decimals_assets'::regclass, 'pg_class')`).Scan(&c); err != nil {
		t.Fatalf("read comment: %v", err)
	}
	if !strings.Contains(c, "AdjustPrice") || strings.Contains(c, "declines") {
		t.Fatalf("nonstandard_decimals_assets comment still describes a decline guard: %q", c)
	}
}
