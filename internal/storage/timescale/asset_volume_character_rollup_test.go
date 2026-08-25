package timescale

import (
	"strings"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// TestAssetVolumeCharacterRollupSQL_Shape pins the all-asset roll to the
// SAME signal definitions the per-asset assetVolumeCharacterSQL uses, so a
// future edit can't silently let the rollup drift from the value the detail
// used to compute live (the oracle guards it end-to-end; this guards the
// SQL text cheaply).
func TestAssetVolumeCharacterRollupSQL_Shape(t *testing.T) {
	q := assetVolumeCharacterRollupSQLTemplate
	musts := []string{
		// Priced volume only, on BOTH sides (base + quote projections).
		"t.usd_volume IS NOT NULL",
		"LEFT JOIN alias_map bm ON bm.form = t.base_asset",
		"LEFT JOIN alias_map qm ON qm.form = t.quote_asset",
		"UNION ALL",
		// Trailing window bound + interval param.
		"t.ts >= now() - $1::interval",
		// UNORDERED account pair so a round-trip folds to one pair.
		"GROUP BY asset_id, LEAST(maker, taker), GREATEST(maker, taker)",
		// Self-cross share.
		"a.maker IS NOT NULL AND a.maker = a.taker",
		// Issuer-side predicate, derived per canonical asset, no-op when ''.
		"a.issuer <> '' AND (a.maker = a.issuer OR a.taker = a.issuer)",
		// Issuer derivation: the G-strkey suffix of a classic id.
		"split_part(asset_id, '-', 2)",
		// Market-surface (real price surface) detection on the RAW counterpart.
		"a.counterpart = 'native'",
		"a.counterpart LIKE 'fiat:%'",
		"a.counterpart LIKE 'USDC-%'",
		// Exact NUMERIC volume for the stored column (ADR-0003).
		"SUM(a.v_num)::text",
		// Grouped per canonical asset.
		"GROUP BY a.asset_id",
	}
	for _, m := range musts {
		if !strings.Contains(q, m) {
			t.Errorf("assetVolumeCharacterRollupSQLTemplate missing %q", m)
		}
	}
	// The %-carrying LIKE patterns must survive templating untouched — the
	// sentinel is replaced by strings.Replace, never fmt.Sprintf.
	if strings.Contains(q, "%s") {
		t.Errorf("template still uses a %%s verb — Sprintf would mangle the LIKE %% patterns")
	}
}

// TestBuildAliasMapValues_XLMBaseline proves the alias-fold VALUES bind the
// XLM baseline (crypto:XLM + the SAC → native) as ::text params starting at
// the given index, in deterministic order.
func TestBuildAliasMapValues_XLMBaseline(t *testing.T) {
	canonical.InstallAliasRegistry(nil) // XLM-only baseline

	valuesSQL, args := buildAliasMapValues(2)
	if !strings.Contains(valuesSQL, "($2::text, $3::text)") ||
		!strings.Contains(valuesSQL, "($4::text, $5::text)") {
		t.Errorf("buildAliasMapValues(2) placeholders = %q, want $2..$5 ::text pairs", valuesSQL)
	}
	if len(args) != 4 {
		t.Fatalf("buildAliasMapValues args = %d, want 4 (two form→canon pairs)", len(args))
	}
	// Every canon target on the baseline is 'native'.
	for i := 1; i < len(args); i += 2 {
		if args[i] != "native" {
			t.Errorf("alias arg[%d] canon = %v, want native", i, args[i])
		}
	}
}

// TestAdjustedVolumeExpr_Wired proves the §4-B concentration-adjusted sort
// key is the SAME expression in all three places that must agree: the
// listing SELECT (as sort_vol_usd), assetsOrderBy, and
// assetsCursorPredicate — a drift would break keyset pagination.
func TestAdjustedVolumeExpr_Wired(t *testing.T) {
	if !strings.Contains(listAssetsBaseSelect, adjustedVolume24hExpr) {
		t.Error("listAssetsBaseSelect does not embed adjustedVolume24hExpr as sort_vol_usd")
	}
	if !strings.Contains(listAssetsBaseSelect, "AS sort_vol_usd") {
		t.Error("listAssetsBaseSelect missing the sort_vol_usd column")
	}
	if !strings.Contains(listAssetsBaseSelect, "avc.character") ||
		!strings.Contains(listAssetsBaseSelect, "AS volume_character") {
		t.Error("listAssetsBaseSelect missing the volume_character column")
	}
	if !strings.Contains(listAssetsBaseSelect, "LEFT JOIN asset_volume_character avc") {
		t.Error("listAssetsBaseSelect missing the asset_volume_character LEFT JOIN")
	}
	if ob := assetsOrderBy(AssetsOrderVolume24hUSDDesc); !strings.Contains(ob, adjustedVolume24hExpr) {
		t.Errorf("assetsOrderBy(volume) = %q, does not rank on the adjusted expr", ob)
	}
	if cp := assetsCursorPredicate(AssetsOrderVolume24hUSDDesc, 5); !strings.Contains(cp, adjustedVolume24hExpr) {
		t.Errorf("assetsCursorPredicate(volume) = %q, does not compare the adjusted expr", cp)
	}
	// The demotion only touches concentrated/operational; market/unrated
	// keep raw volume (ELSE 1).
	if !strings.Contains(adjustedVolume24hExpr, "avc.character IN ('concentrated', 'operational')") {
		t.Error("adjustedVolume24hExpr must demote only concentrated/operational")
	}
	if !strings.Contains(adjustedVolume24hExpr, "1::numeric - avc.top_account_pair_vol_share::numeric") {
		t.Error("adjustedVolume24hExpr must scale raw volume by (1 - top_account_pair_vol_share)")
	}
}
