package timescale

import (
	"strings"
	"testing"
)

// TestListAssets_readsAssetVolumeRollup asserts the listing's
// per_asset_24h_vol CTE reads the asset_volume_24h rollup and does not
// inline the trailing-24h SUM(volume_usd), which the aggregator worker
// computes. If this regresses (someone
// re-inlines the per-asset SUM) the ~4.8s cold /v1/assets scan returns.
func TestListAssets_readsAssetVolumeRollup(t *testing.T) {
	if !strings.Contains(listAssetsBaseSelect, "FROM asset_volume_24h") {
		t.Errorf("per_asset_24h_vol CTE must read FROM asset_volume_24h")
	}
	// The old inline per-asset SUM must be gone from the listing (the
	// only SUM(volume_usd) in the query was per_asset_24h_vol).
	if strings.Contains(listAssetsBaseSelect, "SUM(volume_usd)") {
		t.Errorf("listing must not inline SUM(volume_usd) — that is the rollup worker's job")
	}
}

// TestListAssetsBaseSelectSQL_rendersForBothOrders guards the one
// remaining render-time substitution: every ordering must produce a
// query that reads the volume rollup and carries no leftover marker.
// (The only marker is /*RANK_TIER*/, whose absence
// listAssetsBaseSelectSQL panics on rather than shipping a syntax error.)
func TestListAssetsBaseSelectSQL_rendersForBothOrders(t *testing.T) {
	for _, order := range []AssetsOrder{AssetsOrderVolume24hUSDDesc, AssetsOrderObservationCountDesc} {
		sql := listAssetsBaseSelectSQL(order)
		if !strings.Contains(sql, "FROM asset_volume_24h") {
			t.Errorf("order %v: render lost the volume rollup read", order)
		}
		if strings.Contains(sql, "/*PUSHDOWN_") || strings.Contains(sql, "chosen_assets") {
			t.Errorf("order %v: pushdown machinery is back — it has nothing left to narrow", order)
		}
		if strings.Contains(sql, rankTierMarker) {
			t.Errorf("order %v: unsubstituted rank-tier marker would ship as a syntax error", order)
		}
	}
}

// TestRefreshAssetVolumeUpsert_shape asserts the writer sums both sides
// (base OR quote) over prices_1m and upserts idempotently.
func TestRefreshAssetVolumeUpsert_shape(t *testing.T) {
	for _, want := range []string{
		"INSERT INTO asset_volume_24h",
		"SUM(volume_usd)",         // the per-asset aggregate
		"base_asset  AS asset_id", // base side
		"quote_asset AS asset_id", // quote side
		"UNION ALL",               // single-sided union
		"GROUP BY asset_id",       // one row per asset
		"ON CONFLICT (asset_id)",  // idempotent replace
	} {
		if !strings.Contains(refreshAssetVolumeUpsert, want) {
			t.Errorf("upsert query missing %q:\n%s", want, refreshAssetVolumeUpsert)
		}
	}
}

// TestAssetVolumeUnpricedTrades_shape pins the lower-bound plumbing: the
// refresh counts usd_volume IS NULL trades onto existing volume rows only
// (LEFT JOIN from vol, so it never admits a contract to the Soroban spine),
// on a bare ts window, and the listing emits the flag from that count.
func TestAssetVolumeUnpricedTrades_shape(t *testing.T) {
	for _, want := range []string{
		"(asset_id, vol_usd, unpriced_trades, computed_at)",
		"tr.usd_volume IS NULL",
		"tr.ts >= now() - INTERVAL '24 hours'",
		"FROM vol\n  LEFT JOIN unpriced",
		"unpriced_trades = EXCLUDED.unpriced_trades",
	} {
		if !strings.Contains(refreshAssetVolumeUpsert, want) {
			t.Errorf("upsert missing %q:\n%s", want, refreshAssetVolumeUpsert)
		}
	}
	if !strings.Contains(listAssetsBaseSelect, "COALESCE(vol.unpriced_trades, 0) > 0  AS volume_lower_bound") {
		t.Error("listing must emit volume_lower_bound from asset_volume_24h.unpriced_trades")
	}
	for name, q := range map[string]string{"slug": getAssetBySlugSQL, "native": getNativeAssetSQL} {
		if !strings.Contains(q, "u.unpriced_trades > 0) AS volume_lower_bound") {
			t.Errorf("%s query must emit volume_lower_bound", name)
		}
	}
}

// TestRefreshAssetVolumeUpsert_sargable asserts the window predicate is
// a bare `bucket >= … AND bucket <= now() - 1 minute` comparison — no function
// wrapped around the indexed `bucket` column (the class of bug the
// price-latency-sargable incident fixed). A function on bucket would
// defeat chunk pruning and re-introduce the full-history scan.
func TestRefreshAssetVolumeUpsert_sargable(t *testing.T) {
	if !strings.Contains(refreshAssetVolumeUpsert, "bucket >= now() - INTERVAL '24 hours'") {
		t.Errorf("upsert must use a bare `bucket >= now() - INTERVAL` floor:\n%s", refreshAssetVolumeUpsert)
	}
	if !strings.Contains(refreshAssetVolumeUpsert, "bucket <= now() - INTERVAL '1 minute'") {
		t.Errorf("upsert must use a bare closed-bucket `bucket <= now() - INTERVAL '1 minute'` ceiling:\n%s", refreshAssetVolumeUpsert)
	}
	for _, banned := range []string{"date_trunc(", "time_bucket(", "bucket + INTERVAL", "bucket - INTERVAL"} {
		if strings.Contains(refreshAssetVolumeUpsert, banned) {
			t.Errorf("upsert WHERE must not wrap/offset the indexed bucket column (%q):\n%s", banned, refreshAssetVolumeUpsert)
		}
	}
}
