//go:build integration

package integration_test

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Stellar-Index/StellarIndex/internal/platform"
	"github.com/Stellar-Index/StellarIndex/internal/platform/postgresstore"
)

// TestPriceAlertsCooldownFloorBackfill pins migration 0181 (GH #810): a
// stored cooldown below platform.MinAlertCooldownSeconds is raised to it
// (and its updated_at stamped), so a pre-existing 0 no longer re-fires
// every tick; values at or above the floor are untouched.
func TestPriceAlertsCooldownFloorBackfill(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrationsUpTo(t, dsn, 180)
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	acct := uniqueURLAccount(t, ctx, postgresstore.NewAccountStore(postgresstore.New(db)), "cooldown")

	stale := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	floor := platform.MinAlertCooldownSeconds
	want := map[int]int{0: floor, 60: floor, floor - 1: floor, floor: floor, 3600: 3600}
	ids := make(map[int]uuid.UUID, len(want))
	for before := range want {
		var id uuid.UUID
		if err := db.QueryRowContext(ctx, `
			INSERT INTO price_alerts
			    (account_id, base_asset, quote_asset, condition, threshold, cooldown_seconds, created_at, updated_at)
			VALUES ($1, 'native', 'fiat:USD', 'above', 0.15, $2, $3, $3)
			RETURNING id`, acct, before, stale).Scan(&id); err != nil {
			t.Fatalf("seed cooldown=%d: %v", before, err)
		}
		ids[before] = id
	}

	applyMigrations(t, dsn)

	for before, after := range want {
		var got int
		var updated time.Time
		if err := db.QueryRowContext(ctx,
			`SELECT cooldown_seconds, updated_at FROM price_alerts WHERE id = $1`, ids[before]).Scan(&got, &updated); err != nil {
			t.Fatalf("read cooldown=%d: %v", before, err)
		}
		if got != after {
			t.Errorf("cooldown %d after 0181 = %d, want %d", before, got, after)
		}
		if raised := before < floor; raised == updated.Equal(stale) {
			t.Errorf("cooldown %d: updated_at = %s, want stamped only when raised (%v)", before, updated, raised)
		}
	}
}
