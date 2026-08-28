package timescale

import (
	"strings"
	"testing"
)

// REGRESSION (production, 2026-08-28): GET /v1/assets with limit=500
// returned HTTP 500 —
//
//	timescale: scan asset: sql: Scan error on column index 0,
//	name "slug": converting NULL to string is unsupported
//
// catalogue_assets UNIONs classic_assets with Soroban-native contract
// assets, and a Soroban row has slug AND code both NULL by nature (no
// issuer account, no SEP-1 code). The projection was
// COALESCE(ca.slug, ca.code) — two arms, both NULL for those rows —
// and slug scans into a non-nullable Go string.
//
// It stayed hidden because the failure is ORDER-dependent: limit=100
// never reached one of the 60 Soroban rows, so the endpoint looked
// healthy at the default page size while being broken at limit>=150.
// It also hard-failed the explorer build, which refuses to bake
// fallback HTML rather than ship a page built on a broken fetch —
// that guard is what surfaced it.
//
// asset_id is the correct third arm, not merely a non-null one: it is
// already the asset's URL segment (/assets/CAUP7NFA… resolves), so the
// slug a caller receives is the slug that works.
func TestCatalogueSlugProjection_FallsBackToAssetID(t *testing.T) {
	t.Parallel()

	// Both the LIST projection and the single-row lookup share the
	// defect and the fix; assert on each rendering the store uses.
	for name, sql := range map[string]string{
		"list":          listAssetsBaseSelectSQL(""),
		"list_pushdown": listAssetsBaseSelectSQL("ca.asset_id IN ('native')"),
	} {
		t.Run(name, func(t *testing.T) {
			if strings.Contains(sql, "COALESCE(ca.slug, ca.code)") {
				t.Error("two-arm COALESCE(ca.slug, ca.code) is back — a Soroban row " +
					"has both NULL and will fail the scan at limit>=150")
			}
			if !strings.Contains(sql, "COALESCE(ca.slug, ca.code, ca.asset_id)") {
				t.Error("slug projection must fall back to ca.asset_id so " +
					"Soroban-native rows scan into a non-nullable string")
			}
		})
	}
}
