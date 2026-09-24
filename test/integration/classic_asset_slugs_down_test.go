//go:build integration

package integration_test

import (
	"context"
	"database/sql"
	"testing"
	"time"
)

// TestClassicAssetSlugDownsKeepForeignSlugs executes 0135 down and 0134 down
// over rows the migrations wrote and one they did not. 0134's up fills only
// NULL slugs and 0135's writer emits asset_id, so each down may clear only
// its own forms: slug is a UNIQUE public URL key, and an unconditional
// `SET slug = NULL` erases one no migration wrote.
func TestClassicAssetSlugDownsKeepForeignSlugs(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()

	applyMigrationsUpTo(t, dsn, 135)
	const (
		ownedID   = "USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
		foreignID = "AQUA-GBNZILSTVQZ4R7IKQDGHYGY2QXL5QOFJYQMXPKWRRM5PAV7Y4M67AQUA"
	)
	if _, err := db.ExecContext(ctx, `
        INSERT INTO classic_assets
            (asset_id, code, issuer_g_strkey, slug,
             first_seen_at, first_seen_ledger, last_seen_at, last_seen_ledger)
        VALUES ($1, 'USDC', 'GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN', $1,
                now(), 1, now(), 1),
               ($2, 'AQUA', 'GBNZILSTVQZ4R7IKQDGHYGY2QXL5QOFJYQMXPKWRRM5PAV7Y4M67AQUA', 'aqua',
                now(), 1, now(), 1)`, ownedID, foreignID); err != nil {
		t.Fatalf("seed classic_assets: %v", err)
	}

	applyMigrationsUpTo(t, dsn, 134)
	if got := classicAssetSlug(t, ctx, db, ownedID); got != "usdc-ga5zsejy" {
		t.Errorf("after 0135 down, 0135-written slug = %q, want 0134's tier-1 form %q", got, "usdc-ga5zsejy")
	}
	if got := classicAssetSlug(t, ctx, db, foreignID); got != "aqua" {
		t.Errorf("after 0135 down, foreign slug = %q, want it kept as %q", got, "aqua")
	}

	applyMigrationsUpTo(t, dsn, 133)
	if got := classicAssetSlug(t, ctx, db, ownedID); got != "" {
		t.Errorf("after 0134 down, 0134-written slug = %q, want NULL", got)
	}
	if got := classicAssetSlug(t, ctx, db, foreignID); got != "aqua" {
		t.Errorf("after 0134 down, foreign slug = %q, want it kept as %q", got, "aqua")
	}
}

func classicAssetSlug(t *testing.T, ctx context.Context, db *sql.DB, assetID string) string {
	t.Helper()
	var slug sql.NullString
	if err := db.QueryRowContext(ctx,
		`SELECT slug FROM classic_assets WHERE asset_id = $1`, assetID).Scan(&slug); err != nil {
		t.Fatalf("read slug for %s: %v", assetID, err)
	}
	return slug.String
}
