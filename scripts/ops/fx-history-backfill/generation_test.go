//go:build integration

package main

import (
	"context"
	"database/sql"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
)

// TestOpenBackfillStore_StampsPositiveGeneration is the MR-1 regression (2)
// proven-red test (audit-2026-08-14). fx-history-backfill is an INV-3
// corrective entry point that writes historical fx_quotes rows over the key
// the live forex worker owns. Unlike every other corrective tool
// (backfill.go, backfill_external.go, supply.go, ch_rebuild.go) it never
// called store.SetDeriveGeneration, so it wrote at generation 0 and its
// operator corrections were silently reverted by the next daily gen-0 worker
// refresh (last-writer-wins).
//
// openBackfillStore is the fix: it stamps time.Now().Unix() (a POSITIVE
// generation) on the store before any write, so the fx_quotes generation
// guard (migration 0141) makes corrections durable. This test opens a real
// store through openBackfillStore — the exact seam main() uses — and asserts
// the store is in a POSITIVE-generation corrective mode.
//
// Reverting openBackfillStore to a plain timescale.Open (dropping the
// SetDeriveGeneration call) turns this red: DeriveGeneration() == 0.
func TestOpenBackfillStore_StampsPositiveGeneration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	dsn := startTimescaleForTool(t, ctx)

	before := time.Now().Unix()
	store, err := openBackfillStore(ctx, dsn)
	if err != nil {
		t.Fatalf("openBackfillStore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	gen := store.DeriveGeneration()
	if gen <= 0 {
		t.Fatalf("openBackfillStore stamped derive_generation=%d, want a POSITIVE value — "+
			"a gen-0 operator correction is silently reverted by the next live worker "+
			"refresh (MR-1 regression 2)", gen)
	}
	if gen < before {
		t.Errorf("derive_generation=%d predates the call (%d); expected ~time.Now().Unix()", gen, before)
	}
}

// startTimescaleForTool boots a throwaway TimescaleDB container and returns
// its DSN. openBackfillStore only pings (no schema needed), so migrations are
// not applied. Mirrors test/integration/storage_test.go's startTimescale;
// duplicated here because that helper lives in the integration_test package,
// which package main cannot import.
func startTimescaleForTool(t *testing.T, ctx context.Context) string {
	t.Helper()
	pg, err := tcpostgres.Run(ctx,
		"timescale/timescaledb:2.26.4-pg15",
		tcpostgres.WithDatabase("stellarindex"),
		tcpostgres.WithUsername("stellarindex"),
		tcpostgres.WithPassword("stellarindex-test"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).WithStartupTimeout(60*time.Second),
		),
	)
	if err != nil {
		t.Fatalf("start timescale: %v", err)
	}
	t.Cleanup(func() { _ = pg.Terminate(context.Background()) })

	dsn, err := pg.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("conn str: %v", err)
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.ExecContext(ctx, "CREATE EXTENSION IF NOT EXISTS timescaledb"); err != nil {
		t.Fatalf("create extension: %v", err)
	}
	return dsn
}
