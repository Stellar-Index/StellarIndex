package timescale

import (
	"context"
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

// mustBuildAssetsQuery composes a listing query for a filter set the
// spine can express. Every case below passes such a set, so an error is
// the test's own bug and not a result worth asserting on — the
// rejection path has its own test.
func mustBuildAssetsQuery(
	t *testing.T, limit int, issuer, code, cursor, q, typ string, order AssetsOrder,
) (string, []any) {
	t.Helper()
	sql, args, err := buildAssetsQuery(limit, issuer, code, cursor, q, typ, order)
	if err != nil {
		t.Fatalf("buildAssetsQuery(typ=%q): %v", typ, err)
	}
	return sql, args
}

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
	sql, args := mustBuildAssetsQuery(t, 100, "", "", "", "", "", AssetsOrderObservationCountDesc)
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
	sql, args := mustBuildAssetsQuery(t, 100, issuer, "", "", "", "", AssetsOrderObservationCountDesc)

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
	sql, args := mustBuildAssetsQuery(t, 50, "", "", "", "USDC", "", AssetsOrderObservationCountDesc)
	if got := strings.Count(sql, "LOWER($1)"); got != 3 {
		t.Errorf("q filter must compare code, slug and issuer against $1; got %d references", got)
	}
	if len(args) != 2 || args[0] != "%USDC%" || args[1] != 50 {
		t.Errorf("expected args=[%%USDC%%, 50]; got %v", args)
	}
}

// TestBuildAssetsQuery_QFilterEscapesLikeMetacharacters pins F175: `q`
// is a caller-supplied literal, not a pattern, so a client sending its
// own `%` or `_` must match those characters literally rather than
// have them act as SQL LIKE wildcards once wrapped in `%...%`. Before
// the fix, `q="A_B"` (typed to search for the three literal characters
// "A_B") matched any row with "A", any single character, then "B" —
// e.g. "AxB" — which is not what the caller searched for.
func TestBuildAssetsQuery_QFilterEscapesLikeMetacharacters(t *testing.T) {
	t.Parallel()
	sql, args := mustBuildAssetsQuery(t, 50, "", "", "", "A_B%C", "", AssetsOrderObservationCountDesc)
	if len(args) != 2 {
		t.Fatalf("expected 2 args (pattern, limit); got %v", args)
	}
	want := `%A\_B\%C%`
	if args[0] != want {
		t.Errorf("q=%q must bind the LIKE-escaped pattern %q so its own `_`/`%%` match literally; got %q", "A_B%C", want, args[0])
	}
	if !strings.Contains(sql, "ESCAPE '\\'") {
		t.Errorf("q predicate must declare an ESCAPE clause so the bound backslash-escapes take effect; got:\n%s", sql)
	}
}

// TestBuildAssetsQuery_QFilterMatchesSorobanContractID pins RLT-023: a
// Soroban-native row has NULL code/slug/issuer (see the discovered-
// contract arm of listAssetsBaseSelect), so the q predicate's slug leg
// must fall back to ca.asset_id — exactly as the base SELECT's own
// "slug" column already does (COALESCE(ca.slug, ca.code, ca.asset_id))
// — or type=soroban&q=<contract id> can never match a row.
func TestBuildAssetsQuery_QFilterMatchesSorobanContractID(t *testing.T) {
	t.Parallel()
	sql, _ := mustBuildAssetsQuery(t, 50, "", "", "", "CAUP7", "soroban", AssetsOrderObservationCountDesc)
	// The trailing "LIKE LOWER(" pins this to the q PREDICATE specifically
	// — listAssetsBaseSelect's SELECT list also has a
	// "COALESCE(ca.slug, ca.code, ca.asset_id)" (its "slug" output
	// column), so matching that substring alone would pass whether or
	// not the WHERE clause was ever fixed.
	if !strings.Contains(sql, "LOWER(COALESCE(ca.slug, ca.code, ca.asset_id)) LIKE LOWER(") {
		t.Errorf("q predicate must fall back to ca.asset_id (the only identifying column a Soroban-native row has); got:\n%s", sql)
	}
}

// TestBuildAssetsQuery_IssuerAndQ pins the placeholder ORDER when both
// are set: issuer takes $1, the LIKE pattern $2.
func TestBuildAssetsQuery_IssuerAndQ(t *testing.T) {
	t.Parallel()
	issuer := "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
	sql, args := mustBuildAssetsQuery(t, 100, issuer, "", "", "USD", "", AssetsOrderObservationCountDesc)
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
	sql, args := mustBuildAssetsQuery(t, 100, "", "USDC", "", "", "", AssetsOrderObservationCountDesc)

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
	sql, args := mustBuildAssetsQuery(t, 100, issuer, "USDC", "", "", "", AssetsOrderObservationCountDesc)

	if !strings.Contains(sql, "ca.issuer_g_strkey = $1") || !strings.Contains(sql, "ca.code = $2") {
		t.Error("issuer+code query must keep both outer-WHERE predicates")
	}
	if len(args) != 3 || args[0] != issuer || args[1] != "USDC" || args[2] != 100 {
		t.Errorf("expected args=[issuer, code, limit]; got %v", args)
	}
}

// TestBuildAssetsQuery_TypeFilter pins the structural-class predicate.
// The spine is classic_assets UNION the traded Soroban-native
// contracts; only the issuer tells the two arms apart (classic_assets
// requires a G-issuer, the contract arm selects NULL for it). The
// filter carries no placeholder — the value is a closed enum validated
// at the edge, never interpolated — so the arg list must be untouched.
func TestBuildAssetsQuery_TypeFilter(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		typ  string
		want string
	}{
		{typ: "classic", want: "ca.issuer_g_strkey IS NOT NULL"},
		{typ: "soroban", want: "ca.issuer_g_strkey IS NULL"},
	} {
		t.Run(tc.typ, func(t *testing.T) {
			t.Parallel()
			sql, args := mustBuildAssetsQuery(t, 100, "", "", "", "", tc.typ, AssetsOrderObservationCountDesc)
			if !strings.Contains(sql, tc.want) {
				t.Errorf("type=%s query must carry %q", tc.typ, tc.want)
			}
			if len(args) != 1 || args[0] != 100 {
				t.Errorf("type filter must add no placeholder; got args=%v", args)
			}
		})
	}
}

// TestBuildAssetsQuery_TypeCombinesWithCode — the type predicate is
// composed with the others, and adding it must not shift the numbered
// placeholders the value-bearing filters bind to.
func TestBuildAssetsQuery_TypeCombinesWithCode(t *testing.T) {
	t.Parallel()
	sql, args := mustBuildAssetsQuery(t, 100, "", "USDC", "", "", "classic", AssetsOrderObservationCountDesc)
	if !strings.Contains(sql, "ca.code = $1") {
		t.Error("code must still bind to $1 alongside a type filter")
	}
	if !strings.Contains(sql, "ca.issuer_g_strkey IS NOT NULL") {
		t.Error("type predicate dropped when combined with code")
	}
	if len(args) != 2 || args[0] != "USDC" || args[1] != 100 {
		t.Errorf("expected args=[code, limit]; got %v", args)
	}
}

// TestBuildAssetsQuery_UnknownTypeIsRefused — an unrecognised non-empty
// `type` must NOT compose away into the unfiltered page. The listing has
// ~199k rows and answers every request with a plausible 200, so a
// silently-dropped filter is served as data the caller believes is
// narrowed. Refusing is the only outcome the caller can tell apart.
func TestBuildAssetsQuery_UnknownTypeIsRefused(t *testing.T) {
	t.Parallel()
	for _, typ := range []string{"bogus", "native", "fiat", "CLASSIC", " classic"} {
		t.Run(typ, func(t *testing.T) {
			t.Parallel()
			sql, args, err := buildAssetsQuery(100, "", "", "", "", typ, AssetsOrderObservationCountDesc)
			if err == nil {
				// Query text elided — it is the whole 12KB spine, and the
				// point is that it was composed at all.
				t.Fatalf("type=%q was accepted (composed %d bytes of SQL, %d args) — an "+
					"unknown filter must not fall through to the unfiltered listing",
					typ, len(sql), len(args))
			}
			if sql != "" || args != nil {
				t.Errorf("a refused filter must yield no query; got %d bytes of SQL, args=%v",
					len(sql), args)
			}
		})
	}
}

// TestListAssetsExt_UnknownTypeSurfacesTheError — the refusal must reach
// the caller rather than being swallowed between the builder and the
// query. The Store carries a nil *sql.DB on purpose: that is what proves
// the guard runs BEFORE QueryContext, since reaching the round-trip at
// all panics on it.
func TestListAssetsExt_UnknownTypeSurfacesTheError(t *testing.T) {
	t.Parallel()
	s := &Store{}
	defer func() {
		if p := recover(); p != nil {
			t.Errorf("ListAssetsExt composed a query for type=bogus and dialled it (%v) — "+
				"an unrecognised filter must be refused before the round-trip", p)
		}
	}()
	rows, err := s.ListAssetsExt(context.Background(), ListAssetsOptions{Limit: 10, Type: "bogus"})
	if err == nil {
		t.Fatalf("ListAssetsExt accepted type=bogus and returned %d rows", len(rows))
	}
	if !strings.Contains(err.Error(), "unsupported type filter") {
		t.Errorf("error = %v, want it to name the unsupported filter", err)
	}
}
