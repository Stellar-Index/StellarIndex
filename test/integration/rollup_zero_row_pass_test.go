//go:build integration

package integration_test

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// TestRollupRefresh_ZeroRowPassKeepsLastGood runs the three rollup refreshes
// against a database whose upstream (prices_1m, trades) is EMPTY — the
// state a CAGG rebuilt WITH NO DATA or a stalled refresh policy leaves. The
// pass must keep every last-good row still inside its rollup's validity
// bound and drop only the rows past it, instead of deleting the whole
// served table on its now()-stamped prune.
func TestRollupRefresh_ZeroRowPassKeepsLastGood(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	db := store.DB()

	for _, q := range []string{
		`INSERT INTO asset_volume_24h (asset_id, vol_usd, computed_at) VALUES
		   ('ZZZ-FRESH', 12345, now() - interval '5 minutes'),
		   ('ZZZ-EXPIRED', 999, now() - interval '25 hours')`,
		`INSERT INTO asset_price_snapshot (asset_id, price_usd, computed_at) VALUES
		   ('ZZZ-FRESH', 0.1234567, now() - interval '5 minutes'),
		   ('ZZZ-EXPIRED', 7.5, now() - interval '16 minutes')`,
		`INSERT INTO asset_volume_character
		 (asset_id, window_days, volume_usd, distinct_makers, distinct_takers,
		  top_account_pair_vol_share, self_cross_share, issuer_side_share,
		  market_styled_share, is_market_styled, character, computed_at)
		 VALUES
		   ('ZZZ-FRESH', 14, 1, 0, 0, 0, 0, 0, 0, false, 'market', now() - interval '1 hour'),
		   ('ZZZ-EXPIRED', 14, 1, 0, 0, 0, 0, 0, 0, false, 'market', now() - interval '15 days')`,
	} {
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatalf("seed rollup: %v", err)
		}
	}

	if err := store.RefreshAssetListingRollups(ctx); err != nil {
		t.Fatalf("RefreshAssetListingRollups: %v", err)
	}
	if err := store.RefreshAssetVolumeCharacter(ctx); err != nil {
		t.Fatalf("RefreshAssetVolumeCharacter: %v", err)
	}

	for _, tc := range []struct{ table, idsQ, valueQ, want string }{
		{
			"asset_volume_24h",
			`SELECT asset_id FROM asset_volume_24h ORDER BY asset_id`,
			`SELECT vol_usd::text FROM asset_volume_24h WHERE asset_id = 'ZZZ-FRESH'`,
			"12345",
		},
		{
			"asset_price_snapshot",
			`SELECT asset_id FROM asset_price_snapshot ORDER BY asset_id`,
			`SELECT price_usd::text FROM asset_price_snapshot WHERE asset_id = 'ZZZ-FRESH'`,
			"0.1234567",
		},
		{
			"asset_volume_character",
			`SELECT asset_id FROM asset_volume_character ORDER BY asset_id`,
			`SELECT volume_usd::text FROM asset_volume_character WHERE asset_id = 'ZZZ-FRESH'`,
			"1",
		},
	} {
		assetIDs := rollupAssetIDs(ctx, t, db, tc.idsQ)
		if len(assetIDs) != 1 || assetIDs[0] != "ZZZ-FRESH" {
			t.Errorf("%s after a zero-row pass = %v, want only the in-bound last-good row [ZZZ-FRESH]", tc.table, assetIDs)
			continue
		}
		var got string
		if err := db.QueryRowContext(ctx, tc.valueQ).Scan(&got); err != nil {
			t.Fatalf("read %s: %v", tc.table, err)
		}
		if got != tc.want {
			t.Errorf("%s last-good value = %q, want %q unchanged", tc.table, got, tc.want)
		}
	}
}

func rollupAssetIDs(ctx context.Context, t *testing.T, db *sql.DB, q string) []string {
	t.Helper()
	rows, err := db.QueryContext(ctx, q)
	if err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("%s scan: %v", q, err)
		}
		out = append(out, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("%s rows: %v", q, err)
	}
	return out
}
