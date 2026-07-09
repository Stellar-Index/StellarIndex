package timescale

import (
	"strings"
	"testing"
)

// TestListAssets_readsAssetVolumeRollup asserts the listing's
// per_asset_24h_vol CTE reads the asset_volume_24h rollup and no longer
// inlines the trailing-24h SUM(volume_usd) that the 2026-07-06 latency
// fix (#43) moved to the aggregator worker. If this regresses (someone
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

// TestListAssetsBaseSelectSQL_pushdownStillRenders guards the
// strings.Replace anchor: both the unfiltered and issuer-pushdown
// renderings must still produce a query that reads the rollup, and the
// pushdown path must still prepend the chosen_assets CTE (used by the
// price CTEs).
func TestListAssetsBaseSelectSQL_pushdownStillRenders(t *testing.T) {
	plain := listAssetsBaseSelectSQL("")
	if !strings.Contains(plain, "FROM asset_volume_24h") {
		t.Errorf("unfiltered render lost the rollup read")
	}
	if strings.Contains(plain, "/*PUSHDOWN_") {
		t.Errorf("unfiltered render should have stripped PUSHDOWN markers")
	}

	pushed := listAssetsBaseSelectSQL("issuer_g_strkey = $1")
	if !strings.Contains(pushed, "WITH chosen_assets AS") {
		t.Errorf("pushdown render must prepend chosen_assets CTE")
	}
	if !strings.Contains(pushed, "FROM asset_volume_24h") {
		t.Errorf("pushdown render lost the rollup read")
	}
	// The price CTEs still pushdown against chosen_assets.
	if !strings.Contains(pushed, "AND base_asset IN (SELECT asset_id FROM chosen_assets)") {
		t.Errorf("pushdown render must filter the price CTEs against chosen_assets")
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

// TestRefreshAssetVolumeUpsert_sargable asserts the window predicate is
// a bare `bucket >= … AND bucket < now()` comparison — no function
// wrapped around the indexed `bucket` column (the class of bug the
// price-latency-sargable incident fixed). A function on bucket would
// defeat chunk pruning and re-introduce the full-history scan.
func TestRefreshAssetVolumeUpsert_sargable(t *testing.T) {
	if !strings.Contains(refreshAssetVolumeUpsert, "bucket >= now() - INTERVAL '24 hours'") {
		t.Errorf("upsert must use a bare `bucket >= now() - INTERVAL` floor:\n%s", refreshAssetVolumeUpsert)
	}
	if !strings.Contains(refreshAssetVolumeUpsert, "bucket  <  now()") {
		t.Errorf("upsert must use a bare `bucket < now()` ceiling:\n%s", refreshAssetVolumeUpsert)
	}
	for _, banned := range []string{"date_trunc(", "time_bucket(", "bucket + INTERVAL", "bucket - INTERVAL"} {
		if strings.Contains(refreshAssetVolumeUpsert, banned) {
			t.Errorf("upsert WHERE must not wrap/offset the indexed bucket column (%q):\n%s", banned, refreshAssetVolumeUpsert)
		}
	}
}

// TestRefreshAssetVolumePrune_sargable asserts the prune deletes on a
// bare computed_at comparison (index-friendly, no function on column).
func TestRefreshAssetVolumePrune_sargable(t *testing.T) {
	if !strings.Contains(refreshAssetVolumePrune, "computed_at < now()") {
		t.Errorf("prune must compare computed_at < now() directly, got: %q", refreshAssetVolumePrune)
	}
}
