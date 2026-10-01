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
)

// 0085's stored comments, restored verbatim by 0194 down.
const (
	sep41RollupTableComment0085 = "Incremental per-contract mint/burn/clawback checkpoint for SEP-41 " +
		"Algorithm-3 supply. Advanced by the aggregator rollup worker; read " +
		"as rollup + sargable delta by SEP41KindTotalsAtOrBefore. Prevents " +
		"the full-history per-tick aggregate over sep41_supply_events " +
		"(incident 2026-07-06)."
	sep41RollupLastLedgerComment0085 = "Highest SETTLED ledger folded into the totals; the reader adds the " +
		"live delta above it up to the request ledger."
)

// TestSEP41SupplyRollupComment executes 0194 up and down against a real
// database: only pg_description can show whether the stored text an operator
// reads in \d+ still describes a single writer and a TRUNCATE recovery.
func TestSEP41SupplyRollupComment(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()

	table, lastLedger := sep41RollupComments(t, ctx, db)
	for _, want := range []string{"seed-sep41-genesis", "ResetSEP41SupplyRollupFold", "Never TRUNCATE"} {
		if !strings.Contains(table, want) {
			t.Errorf("sep41_supply_rollup comment missing %q; got %q", want, table)
		}
	}
	if strings.Contains(table, "disjoint") {
		t.Errorf("sep41_supply_rollup comment claims disjoint writer columns; the fold columns are shared: %q", table)
	}
	if !strings.Contains(lastLedger, "reset") {
		t.Errorf("last_ledger comment does not explain a reset row; got %q", lastLedger)
	}

	_, thisFile, _, _ := runtime.Caller(0)
	migrationsDir := filepath.Join(filepath.Dir(thisFile), "..", "..", "migrations")
	m, err := migrate.New("file://"+migrationsDir, dsn)
	if err != nil {
		t.Fatalf("migrate.New: %v", err)
	}
	defer func() { _, _ = m.Close() }()
	if err := m.Steps(-1); err != nil {
		t.Fatalf("0194 down: %v", err)
	}
	table, lastLedger = sep41RollupComments(t, ctx, db)
	if table != sep41RollupTableComment0085 {
		t.Errorf("0194 down did not restore 0085's table comment verbatim:\n got %q\nwant %q", table, sep41RollupTableComment0085)
	}
	if lastLedger != sep41RollupLastLedgerComment0085 {
		t.Errorf("0194 down did not restore 0085's last_ledger comment verbatim:\n got %q\nwant %q", lastLedger, sep41RollupLastLedgerComment0085)
	}
}

func sep41RollupComments(t *testing.T, ctx context.Context, db *sql.DB) (table, lastLedger string) {
	t.Helper()
	var tc, lc sql.NullString
	if err := db.QueryRowContext(ctx,
		`SELECT obj_description('sep41_supply_rollup'::regclass, 'pg_class'),
		        col_description('sep41_supply_rollup'::regclass,
		            (SELECT attnum FROM pg_attribute WHERE attrelid = 'sep41_supply_rollup'::regclass AND attname = 'last_ledger'))`,
	).Scan(&tc, &lc); err != nil {
		t.Fatalf("read sep41_supply_rollup comments: %v", err)
	}
	if !tc.Valid || !lc.Valid {
		t.Fatal("sep41_supply_rollup or last_ledger has no catalog comment")
	}
	return tc.String, lc.String
}
