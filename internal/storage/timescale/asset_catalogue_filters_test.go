package timescale

import (
	"strings"
	"testing"
)

// This file used to be asset_catalogue_pushdown_test.go and guarded the
// #27 `chosen_assets` pushdown: a CTE prepended ahead of the spine that
// narrowed the listing's eight per-asset price CTEs to one issuer's
// assets, so a FILTERED /v1/assets did not read 256k prices_1m rows for
// a one-row result.
//
// #331 F1 deleted the thing it narrowed. The price CTEs moved to
// refreshAssetPriceSnapshotUpsert and the listing LEFT JOINs the rollup,
// so a filtered listing is narrowed by the outer WHERE on `ca` alone
// over a keyed-on-PK price lookup — the shape pushdown existed to
// approximate. What survives, and is still worth pinning, is the
// ARGUMENT-BINDING contract of buildAssetsQuery: which filter takes
// which positional placeholder, and that a filter never silently drops.
//
// TestListAssetsBaseSelect_NoPerRequestPriceScan (asset_price_snapshot_test.go)
// is the guard that the pushdown must never come back, because the CTEs
// it narrowed must never come back.

// TestListAssetsBaseSelectSQL_NoPushdownMachinery asserts the renderer
// emits no chosen_assets CTE and no marker comments for either order.
func TestListAssetsBaseSelectSQL_NoPushdownMachinery(t *testing.T) {
	t.Parallel()
	for _, order := range []AssetsOrder{AssetsOrderVolume24hUSDDesc, AssetsOrderObservationCountDesc} {
		sql := listAssetsBaseSelectSQL(order)
		for _, banned := range []string{"/*PUSHDOWN_BASE*/", "/*PUSHDOWN_QUOTE*/", "chosen_assets"} {
			if strings.Contains(sql, banned) {
				t.Errorf("order %v: rendered SQL still carries %q", order, banned)
			}
		}
	}
}

// TestBuildAssetsQuery_NoFilters is the baseline arg shape: LIMIT only.
func TestBuildAssetsQuery_NoFilters(t *testing.T) {
	t.Parallel()
	sql, args := buildAssetsQuery(100, "", "", "", "", AssetsOrderObservationCountDesc)
	// `ca.` is the spine alias, so any composed outer predicate mentions
	// it; the CTE bodies have WHEREs of their own that must not count.
	if strings.Contains(sql, " WHERE ca.") {
		t.Error("no-filter query must not emit an outer WHERE on the spine")
	}
	if len(args) != 1 || args[0] != 100 {
		t.Errorf("expected args=[100] (just LIMIT); got %v", args)
	}
}

// TestBuildAssetsQuery_IssuerFilter binds the issuer to $1 and keeps the
// outer WHERE on the spine alias.
func TestBuildAssetsQuery_IssuerFilter(t *testing.T) {
	t.Parallel()
	issuer := "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
	sql, args := buildAssetsQuery(100, issuer, "", "", "", AssetsOrderObservationCountDesc)

	if !strings.Contains(sql, "ca.issuer_g_strkey = $1") {
		t.Error("issuer-filter query must keep the outer WHERE on ca.issuer_g_strkey")
	}
	if len(args) != 2 || args[0] != issuer || args[1] != 100 {
		t.Errorf("expected args=[issuer, limit]; got %v", args)
	}
}

// TestBuildAssetsQuery_QFilter binds the LIKE pattern once and applies
// it to all three searchable columns.
func TestBuildAssetsQuery_QFilter(t *testing.T) {
	t.Parallel()
	sql, args := buildAssetsQuery(50, "", "", "", "USDC", AssetsOrderObservationCountDesc)
	if got := strings.Count(sql, "LOWER($1)"); got != 3 {
		t.Errorf("q filter must compare code, slug and issuer against $1; got %d references", got)
	}
	if len(args) != 2 || args[0] != "%USDC%" || args[1] != 50 {
		t.Errorf("expected args=[%%USDC%%, 50]; got %v", args)
	}
}

// TestBuildAssetsQuery_IssuerAndQ pins the placeholder ORDER when both
// are set: issuer takes $1, the LIKE pattern $2.
func TestBuildAssetsQuery_IssuerAndQ(t *testing.T) {
	t.Parallel()
	issuer := "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
	sql, args := buildAssetsQuery(100, issuer, "", "", "USD", AssetsOrderObservationCountDesc)
	if !strings.Contains(sql, "ca.issuer_g_strkey = $1") {
		t.Error("issuer must stay bound to $1 when q is also set")
	}
	if !strings.Contains(sql, "LIKE LOWER($2)") {
		t.Error("q-LIKE pattern must use $2 when issuer is $1")
	}
	if len(args) != 3 || args[0] != issuer || args[1] != "%USD%" || args[2] != 100 {
		t.Errorf("expected args=[issuer, %%USD%%, 100]; got %v", args)
	}
}

// TestBuildAssetsQuery_CodeFilter binds an exact, case-sensitive code
// equality on the indexed classic_assets.code column (BACKLOG #54).
func TestBuildAssetsQuery_CodeFilter(t *testing.T) {
	t.Parallel()
	sql, args := buildAssetsQuery(100, "", "USDC", "", "", AssetsOrderObservationCountDesc)

	if !strings.Contains(sql, "ca.code = $1") {
		t.Error("code-filter query must keep the outer WHERE on ca.code")
	}
	if len(args) != 2 || args[0] != "USDC" || args[1] != 100 {
		t.Errorf("expected args=[code, limit]; got %v", args)
	}
}

// TestBuildAssetsQuery_IssuerAndCode pins the two-filter placeholder
// order — the "pin exactly one classic asset" case (BACKLOG #54).
func TestBuildAssetsQuery_IssuerAndCode(t *testing.T) {
	t.Parallel()
	issuer := "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
	sql, args := buildAssetsQuery(100, issuer, "USDC", "", "", AssetsOrderObservationCountDesc)

	if !strings.Contains(sql, "ca.issuer_g_strkey = $1") || !strings.Contains(sql, "ca.code = $2") {
		t.Error("issuer+code query must keep both outer-WHERE predicates")
	}
	if len(args) != 3 || args[0] != issuer || args[1] != "USDC" || args[2] != 100 {
		t.Errorf("expected args=[issuer, code, limit]; got %v", args)
	}
}
