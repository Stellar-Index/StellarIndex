//go:build integration

package integration_test

import (
	"context"
	"database/sql"
	"testing"
	"time"
)

// TestRouterRegistry0072DownRestoresNotes executes 0072 up, down and up
// again. The up appends a rename sentence to routers.notes; a down that
// restores only the name lets every re-up append the sentence again.
func TestRouterRegistry0072DownRestoresNotes(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()

	applyMigrationsUpTo(t, dsn, 71)
	seedName, seedNotes := soroswapRouterRow(t, ctx, db)

	applyMigrationsUpTo(t, dsn, 72)
	upName, upNotes := soroswapRouterRow(t, ctx, db)
	if upName != "soroswap-router" || upNotes == seedNotes {
		t.Fatalf("0072 up = (%q, %q), want the renamed row with the rename note appended", upName, upNotes)
	}

	applyMigrationsUpTo(t, dsn, 71)
	if name, notes := soroswapRouterRow(t, ctx, db); name != seedName || notes != seedNotes {
		t.Errorf("0072 down = (%q, %q), want the pre-0072 row (%q, %q)", name, notes, seedName, seedNotes)
	}

	applyMigrationsUpTo(t, dsn, 72)
	if name, notes := soroswapRouterRow(t, ctx, db); name != upName || notes != upNotes {
		t.Errorf("0072 up after a down = (%q, %q), want the first up's row (%q, %q)", name, notes, upName, upNotes)
	}
}

func soroswapRouterRow(t *testing.T, ctx context.Context, db *sql.DB) (name, notes string) {
	t.Helper()
	var n sql.NullString
	if err := db.QueryRowContext(ctx, `
        SELECT name, notes FROM routers
         WHERE contract_id = 'CAG5LRYQ5JVEUI5TEID72EYOVX44TTUJT5BQR2J6J77FH65PCCFAJDDH'`).Scan(&name, &n); err != nil {
		t.Fatalf("read soroswap router row: %v", err)
	}
	return name, n.String
}
