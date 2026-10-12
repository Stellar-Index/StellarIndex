package timescale

import (
	"database/sql"
	"fmt"
	"strings"
	"testing"
)

// Regression: GET /v1/assets with a large limit returned 500 (NULL scanned into the non-nullable string
// slug). Soroban-native rows have slug and code both NULL, and small pages never reach one; asset_id is the
// right third arm because it is the asset's working URL segment.
func TestCatalogueSlugProjection_FallsBackToAssetID(t *testing.T) {
	t.Parallel()

	// Both orderings share the defect and the fix; assert on each
	// rendering the store uses. (There is no issuer-pushdown rendering:
	// the per-request price CTEs it narrowed are gone.)
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

// nullSorobanScanner mimics database/sql for a Soroban-native catalogue row, where slug/code/issuer_g_strkey
// are NULL. Scanning NULL into a *string is a hard error, which would fail the entire /v1/assets request.
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

// REGRESSION: fixing `slug` alone with a COALESCE just moves the failure to the next column, as the Soroban
// arm supplies slug, code and issuer_g_strkey as NULL. This pins the class: scanAssetRow must succeed with
// every NULL-by-nature column NULL.
func TestScanAssetRow_SorobanNullColumns(t *testing.T) {
	t.Parallel()

	// Indices 0,2,3 = slug, code, issuer_g_strkey (see the Scan call). slug is COALESCEd in SQL so it never
	// arrives NULL, but a future query edit that drops the COALESCE must fail here rather than in production.
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
