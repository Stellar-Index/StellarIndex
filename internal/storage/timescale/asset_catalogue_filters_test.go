package timescale

import (
	"context"
	"reflect"
	"strings"
	"testing"
)

// This file replaced asset_catalogue_pushdown_test.go, which guarded the
// `chosen_assets` pushdown: a CTE prepended ahead of the spine that
// narrowed the listing's eight per-asset price CTEs to one issuer's
// assets, so a FILTERED /v1/assets did not read 256k prices_1m rows for
// a one-row result.
//
// The pushdown was removed along with the thing it narrowed. The price CTEs moved to
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

// TestBuildAssetsQuery_FilterBinding pins, per filter set, which outer
// predicates survive and which positional placeholder each value takes.
// The type filter carries no placeholder (a closed enum, never
// interpolated), so it must not shift the numbered ones.
func TestBuildAssetsQuery_FilterBinding(t *testing.T) {
	t.Parallel()
	issuer := "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
	for _, tc := range []struct {
		name                 string
		limit                int
		issuer, code, q, typ string
		contains             []string
		notContains          []string
		counts               map[string]int
		wantArgs             []any
	}{
		{
			// `ca.` is the spine alias, so any composed outer predicate
			// mentions it; the CTE bodies have WHEREs of their own.
			name: "no filters", limit: 100,
			notContains: []string{" WHERE ca."},
			wantArgs:    []any{100},
		},
		{
			name: "issuer", limit: 100, issuer: issuer,
			contains: []string{"ca.issuer_g_strkey = $1"},
			wantArgs: []any{issuer, 100},
		},
		{
			name: "q binds once across code, slug and issuer", limit: 50, q: "USDC",
			counts:   map[string]int{"LOWER($1)": 3},
			wantArgs: []any{"%USDC%", 50},
		},
		{
			name: "issuer and q", limit: 100, issuer: issuer, q: "USD",
			contains: []string{"ca.issuer_g_strkey = $1", "LIKE LOWER($2)"},
			wantArgs: []any{issuer, "%USD%", 100},
		},
		{
			name: "code", limit: 100, code: "USDC",
			contains: []string{"ca.code = $1"},
			wantArgs: []any{"USDC", 100},
		},
		{
			name: "issuer and code", limit: 100, issuer: issuer, code: "USDC",
			contains: []string{"ca.issuer_g_strkey = $1", "ca.code = $2"},
			wantArgs: []any{issuer, "USDC", 100},
		},
		{
			name: "type classic", limit: 100, typ: "classic",
			contains: []string{"ca.issuer_g_strkey IS NOT NULL"},
			wantArgs: []any{100},
		},
		{
			name: "type soroban", limit: 100, typ: "soroban",
			contains: []string{"ca.issuer_g_strkey IS NULL"},
			wantArgs: []any{100},
		},
		{
			name: "type combines with code", limit: 100, code: "USDC", typ: "classic",
			contains: []string{"ca.code = $1", "ca.issuer_g_strkey IS NOT NULL"},
			wantArgs: []any{"USDC", 100},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			sql, args := mustBuildAssetsQuery(t, tc.limit, tc.issuer, tc.code, "", tc.q, tc.typ, AssetsOrderObservationCountDesc)
			for _, w := range tc.contains {
				if !strings.Contains(sql, w) {
					t.Errorf("query must carry %q", w)
				}
			}
			for _, w := range tc.notContains {
				if strings.Contains(sql, w) {
					t.Errorf("query must not carry %q", w)
				}
			}
			for w, n := range tc.counts {
				if got := strings.Count(sql, w); got != n {
					t.Errorf("%q count = %d, want %d", w, got, n)
				}
			}
			if !reflect.DeepEqual(args, tc.wantArgs) {
				t.Errorf("args = %v, want %v", args, tc.wantArgs)
			}
		})
	}
}

// TestBuildAssetsQuery_QFilterEscapesLikeMetacharacters pins that `q`
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

// TestBuildAssetsQuery_QFilterMatchesSorobanContractID pins that a
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
