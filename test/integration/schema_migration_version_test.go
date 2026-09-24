//go:build integration

package integration_test

import (
	"context"
	"testing"
	"time"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// TestStoreSchemaMigrationVersion runs the indexer/aggregator readiness
// reader (GH-1167) against a real migrated TimescaleDB: at the migrations
// head the schema check passes; with the applied head rolled back one
// version it fails, which is what makes their /readyz — and so the deploy
// gate — refuse a binary swapped ahead of its migrations.
func TestStoreSchemaMigrationVersion(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	version, dirty, err := store.SchemaMigrationVersion(ctx)
	if err != nil {
		t.Fatalf("SchemaMigrationVersion: %v", err)
	}
	if version != v1.ExpectedSchemaVersion || dirty {
		t.Fatalf("SchemaMigrationVersion = (%d, dirty=%v), want (%d, dirty=false)", version, dirty, v1.ExpectedSchemaVersion)
	}
	checker := v1.NewSchemaVersionChecker(store)
	if err := checker.Ping(ctx); err != nil {
		t.Fatalf("schema check at migrations head: %v", err)
	}

	if _, err := store.DB().ExecContext(ctx, `UPDATE schema_migrations SET version = $1`, v1.ExpectedSchemaVersion-1); err != nil {
		t.Fatalf("roll back schema_migrations: %v", err)
	}
	version, _, err = store.SchemaMigrationVersion(ctx)
	if err != nil {
		t.Fatalf("SchemaMigrationVersion after rollback: %v", err)
	}
	if version != v1.ExpectedSchemaVersion-1 {
		t.Fatalf("SchemaMigrationVersion after rollback = %d, want %d", version, v1.ExpectedSchemaVersion-1)
	}
	if err := checker.Ping(ctx); err == nil {
		t.Fatal("schema check passed with the applied head one behind the binary; want a mismatch error")
	}

	if _, err := store.DB().ExecContext(ctx, `DELETE FROM schema_migrations`); err != nil {
		t.Fatalf("empty schema_migrations: %v", err)
	}
	version, dirty, err = store.SchemaMigrationVersion(ctx)
	if err != nil || version != 0 || dirty {
		t.Fatalf("SchemaMigrationVersion on empty table = (%d, %v, %v), want (0, false, nil)", version, dirty, err)
	}
}
