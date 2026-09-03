package timescale

import (
	"database/sql"
	"fmt"
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

	// Both orderings share the defect and the fix; assert on each
	// rendering the store uses. (There used to be a third rendering here,
	// the issuer-pushdown one; #331 F1 removed the pushdown along with
	// the per-request price CTEs it narrowed.)
	for name, sql := range map[string]string{
		"list_volume": listAssetsBaseSelectSQL(AssetsOrderVolume24hUSDDesc),
		"list_obs":    listAssetsBaseSelectSQL(AssetsOrderObservationCountDesc),
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

// nullSorobanScanner mimics database/sql's behaviour for the row a
// Soroban-native catalogue entry produces: slug/code/issuer_g_strkey
// arrive as NULL. Assigning NULL to a *string is a hard error there
// ("converting NULL to string is unsupported"); assigning it to a
// *sql.NullString just marks it invalid. Reproducing that rule is the
// whole point — it is what turns "one row has no code" into "the
// entire /v1/assets request fails".
type nullSorobanScanner struct {
	// nullAt are the dest indices the driver reports as NULL.
	nullAt map[int]bool
}

func (s nullSorobanScanner) Scan(dest ...any) error {
	for i, d := range dest {
		if !s.nullAt[i] {
			continue
		}
		switch v := d.(type) {
		case *sql.NullString:
			*v = sql.NullString{Valid: false}
		case *sql.NullInt64:
			*v = sql.NullInt64{Valid: false}
		case *string:
			return fmt.Errorf(
				"sql: Scan error on column index %d: converting NULL to string is unsupported", i)
		case *int64:
			return fmt.Errorf(
				"sql: Scan error on column index %d: converting NULL to int64 is unsupported", i)
		}
	}
	return nil
}

// REGRESSION, second attempt (production 2026-08-28).
//
// v0.47.1 fixed `slug` with a SQL COALESCE and shipped — and production
// immediately moved on to "column index 2, code". The defect was never
// about one column: catalogue_assets' Soroban arm supplies slug, code
// AND issuer_g_strkey as NULL, and all three scanned into non-nullable
// Go strings. Fixing them one deploy at a time is the instance, not the
// class.
//
// This pins the class: hand scanAssetRow a row where every
// NULL-by-nature column is NULL, and require it to succeed.
func TestScanAssetRow_SorobanNullColumns(t *testing.T) {
	t.Parallel()

	// Indices 0,2,3 = slug, code, issuer_g_strkey (see the Scan call).
	// slug is COALESCEd in SQL so it never actually arrives NULL, but
	// include it: a future query edit that drops the COALESCE must fail
	// here rather than in production.
	r, err := scanAssetRow(nullSorobanScanner{nullAt: map[int]bool{0: true, 2: true, 3: true}})
	if err != nil {
		t.Fatalf("Soroban-native row must scan, got: %v", err)
	}
	if r.Code != "" {
		t.Errorf("Code = %q, want empty — a contract asset has no SEP-1 code", r.Code)
	}
	if r.IssuerGStrkey != "" {
		t.Errorf("IssuerGStrkey = %q, want empty — a contract asset has no issuer account", r.IssuerGStrkey)
	}
}
