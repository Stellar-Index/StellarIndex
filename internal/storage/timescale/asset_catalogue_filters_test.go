package timescale

import (
	"context"
	"database/sql"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

// buildAssetsQuery's argument-binding contract: each filter takes its own positional placeholder and none is
// silently dropped. Per-request price CTEs are guarded by TestListAssetsBaseSelect_NoPerRequestPriceScan.

// mustBuildAssetsQuery builds a listing query for a filter set the spine can express, so an error is the
// test's own bug; the rejection path has its own test.
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

// TestBuildAssetsQuery_FilterBinding pins, per filter set, which outer predicates survive and which
// placeholder each value takes. The type filter is a closed enum with no placeholder, so it must not shift
// the numbered ones.
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

// TestBuildAssetsQuery_QFilterEscapesLikeMetacharacters pins that `q` is a caller-supplied
// literal, not a pattern: a client's own `%` or `_` must match literally rather than act as SQL
// LIKE wildcards once wrapped in `%...%`, e.g. `q="A_B"` must not match "AxB".
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

// TestBuildAssetsQuery_QFilterMatchesSorobanContractID: a Soroban-native row has NULL code/slug/issuer, so
// the q predicate's slug leg must fall back to ca.asset_id like the base SELECT's slug COALESCE, or
// type=soroban&q=<contract id> never matches.
func TestBuildAssetsQuery_QFilterMatchesSorobanContractID(t *testing.T) {
	t.Parallel()
	sql, _ := mustBuildAssetsQuery(t, 50, "", "", "", "CAUP7", "soroban", AssetsOrderObservationCountDesc)
	// The trailing "LIKE LOWER(" pins this to the q PREDICATE: the SELECT list also has
	// "COALESCE(ca.slug, ca.code, ca.asset_id)" (its "slug" column), so that substring alone
	// would pass regardless of the WHERE clause.
	if !strings.Contains(sql, "LOWER(COALESCE(ca.slug, ca.code, ca.asset_id)) LIKE LOWER(") {
		t.Errorf("q predicate must fall back to ca.asset_id (the only identifying column a Soroban-native row has); got:\n%s", sql)
	}
}

// TestBuildAssetsQuery_UnknownTypeIsRefused: an unrecognised non-empty `type` must not compose away into the
// unfiltered page. The listing answers with a plausible 200, so a silently dropped filter reads as narrowed
// data; refusal is the only outcome the caller can tell apart.
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

// TestListAssetsExt_UnknownTypeSurfacesTheError: the refusal must reach the caller rather than be
// swallowed between the builder and the query. The Store carries a nil *sql.DB on purpose: it
// proves the guard runs BEFORE QueryContext, since reaching the round-trip panics.
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

// Rank-tier lockstep: listingRankTierExpr, the listing's LEADING ORDER BY key, must appear identically in the
// SELECT (as rank_tier, so the cursor encodes it), the ORDER BY and the keyset WHERE. Dropping it from one
// skips or repeats rows; from the ORDER BY it lets a flagged scam token outrank real assets.

func TestListingRankTierExpr_ThreeCallSitesStayInStep(t *testing.T) {
	t.Parallel()
	for _, order := range []AssetsOrder{AssetsOrderVolume24hUSDDesc, AssetsOrderObservationCountDesc} {
		tier := listingRankTierExpr(order)
		sel := listAssetsBaseSelectSQL(order)
		if strings.Contains(sel, rankTierMarker) {
			t.Errorf("order %v: rendered SELECT still holds the %s marker — that is a syntax error, not a comment", order, rankTierMarker)
		}
		if !strings.Contains(sel, tier+" AS rank_tier") {
			t.Errorf("order %v: SELECT does not emit listingRankTierExpr AS rank_tier", order)
		}
		if !strings.Contains(sel, "LEFT JOIN account_directory") {
			t.Errorf("order %v: SELECT is missing the account_directory join the tier reads", order)
		}
		// The tier's price arm reads asset_price_snapshot (listingPriceUSDExpr ==
		// aps.price_usd), so the tier depends on that join and on its staleness floor, which
		// puts "priced too long ago to serve" in the same tier as "never priced".
		if !strings.Contains(sel, "LEFT JOIN asset_price_snapshot") {
			t.Errorf("order %v: SELECT is missing the asset_price_snapshot join the tier reads", order)
		}
		orderBy := assetsOrderBy(order)
		if !strings.HasPrefix(orderBy, " ORDER BY "+tier+" ASC,") {
			t.Errorf("order %v: ORDER BY must LEAD with the rank tier ascending; got %q", order, orderBy)
		}
		pred := assetsCursorPredicate(order, 3)
		if !strings.HasPrefix(pred, "("+tier+" > $1::int OR ("+tier+" = $1::int AND ") {
			t.Errorf("order %v: keyset predicate must compare the rank tier FIRST; got %q", order, pred)
		}
	}
}

// The observation-count order is an activity ranking: it demotes flagged issuers whatever the sort key, but
// must not reorder on price, which would silently change the long-standing default /v1/assets contract.
func TestListingRankTierExpr_PriceTierIsVolumeOrderOnly(t *testing.T) {
	t.Parallel()
	vol := listingRankTierExpr(AssetsOrderVolume24hUSDDesc)
	obs := listingRankTierExpr(AssetsOrderObservationCountDesc)
	if !strings.Contains(vol, "IS NULL THEN 1") {
		t.Error("volume order must carry the unpriced tier (1)")
	}
	if strings.Contains(obs, "THEN 1") {
		t.Errorf("observation-count order must NOT reorder on price; got %q", obs)
	}
	for _, expr := range []string{vol, obs} {
		if !strings.Contains(expr, "THEN 2") || !strings.Contains(expr, "unnest(dir.tags)") {
			t.Errorf("every order must demote directory-flagged issuers to tier 2; got %q", expr)
		}
	}
}

// The SQL tag vocabulary is generated from the ONE Go list, so "shows the
// ⚠ Flagged pill", "has its price withheld" and "is demoted in the
// ranking" cannot disagree.
func TestScamFlagTagsSQLArray_GeneratedFromTheOneList(t *testing.T) {
	t.Parallel()
	want := "ARRAY['malicious', 'unsafe', 'fraud', 'scam', 'hack', 'phishing']::text[]"
	if scamFlagTagsSQLArray != want {
		t.Fatalf("scamFlagTagsSQLArray = %q, want %q", scamFlagTagsSQLArray, want)
	}
	for _, tag := range DirectoryScamFlagTags {
		if !strings.Contains(scamFlagTagsSQLArray, "'"+tag+"'") {
			t.Errorf("tag %q from DirectoryScamFlagTags is missing from the SQL literal", tag)
		}
	}
}

func TestMustSQLTextArrayLiteral_RejectsAnythingButLowercaseWords(t *testing.T) {
	t.Parallel()
	for _, bad := range [][]string{
		nil,
		{""},
		{"Unsafe"},
		{"un safe"},
		{"un'safe"},
		{"unsafe--"},
	} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("mustSQLTextArrayLiteral(%q) must panic", bad)
				}
			}()
			_ = mustSQLTextArrayLiteral(bad)
		}()
	}
}

// EncodeAssetsCursor is the ONLY encoder; it must emit every ORDER BY key
// (tier first) and round-trip through the validator and the parsers.
func TestEncodeAssetsCursor_RoundTrip(t *testing.T) {
	t.Parallel()
	tier := 2
	vol := "62341.98422258"
	row := AssetRow{
		AssetID:          "JFKBANK2-GB7KFNUR5IAIN5NTYM2BUWWUTM6QMUBXF7NHXXKAMRPFLFWR7KL5BANK",
		ObservationCount: 1779,
		SortVolume24hUSD: &vol,
		RankTier:         &tier,
	}

	gotVol := EncodeAssetsCursor(row, AssetsOrderVolume24hUSDDesc)
	if want := "2:62341.98422258:" + row.AssetID; gotVol != want {
		t.Fatalf("volume cursor = %q, want %q", gotVol, want)
	}
	if err := ValidateAssetsCursor(gotVol, AssetsOrderVolume24hUSDDesc); err != nil {
		t.Fatalf("emitted volume cursor rejected by its own validator: %v", err)
	}
	if tr, v, id := parseVolumeCursor(gotVol); tr != 2 || v != vol || id != row.AssetID {
		t.Fatalf("parseVolumeCursor(%q) = (%d, %q, %q), want (2, %q, %q)", gotVol, tr, v, id, vol, row.AssetID)
	}

	gotObs := EncodeAssetsCursor(row, AssetsOrderObservationCountDesc)
	if want := "2:1779:" + row.AssetID; gotObs != want {
		t.Fatalf("observation cursor = %q, want %q", gotObs, want)
	}
	if err := ValidateAssetsCursor(gotObs, AssetsOrderObservationCountDesc); err != nil {
		t.Fatalf("emitted observation cursor rejected by its own validator: %v", err)
	}
	if tr, n, id := parseAssetCursor(gotObs); tr != 2 || n != 1779 || id != row.AssetID {
		t.Fatalf("parseAssetCursor(%q) = (%d, %d, %q), want (2, 1779, %q)", gotObs, tr, n, id, row.AssetID)
	}

	// A row with no rank tier (not from the listing query) encodes as
	// tier 0 rather than dropping the field and shifting the shape.
	if got := EncodeAssetsCursor(AssetRow{AssetID: "native"}, AssetsOrderVolume24hUSDDesc); got != "0::native" {
		t.Fatalf("nil-tier / nil-volume cursor = %q, want %q", got, "0::native")
	}
}

// A cursor minted before the rank tier existed has two fields. It must resume in tier 0
// rather than 400 — an in-flight pager should not break on deploy.
func TestSplitAssetsCursor_LegacyTwoFieldCursorResumesInTierZero(t *testing.T) {
	t.Parallel()
	tier, sortKey, assetID, ok := splitAssetsCursor("41106.07558771:yXLM-GARDNV3Q7YGT4AKSDF25LT32YSCCW4EV22Y2TV3I2PU2MMXJTEDL5T55")
	if !ok {
		t.Fatal("legacy cursor must still split")
	}
	if tier != "0" || sortKey != "41106.07558771" || assetID != "yXLM-GARDNV3Q7YGT4AKSDF25LT32YSCCW4EV22Y2TV3I2PU2MMXJTEDL5T55" {
		t.Fatalf("legacy split = (%q, %q, %q), want (0, 41106.07558771, yXLM-…)", tier, sortKey, assetID)
	}
	if err := ValidateAssetsCursor("41106.07558771:yXLM-GARD", AssetsOrderVolume24hUSDDesc); err != nil {
		t.Errorf("legacy volume cursor must stay valid: %v", err)
	}
	if err := ValidateAssetsCursor("1779:JFKBANK2-GB7K", AssetsOrderObservationCountDesc); err != nil {
		t.Errorf("legacy observation cursor must stay valid: %v", err)
	}
	// ...but a malformed rank tier in the three-field shape is a client error.
	if err := ValidateAssetsCursor("x:100:native", AssetsOrderObservationCountDesc); err == nil {
		t.Error("non-numeric rank tier must be rejected")
	}
}

// TestAssetsCursorPredicate_MixedDirectionKeysetIsSpelledOut: /v1/assets orderings are mixed-direction, but
// a row-constructor `(a, b) < ($1, $2)` compares same-direction and corrupts the keyset on ties. Each
// ordering must use the expanded form; the test derives orderings from source so new ones are covered.
func TestAssetsCursorPredicate_MixedDirectionKeysetIsSpelledOut(t *testing.T) {
	t.Parallel()
	orders := allAssetsOrders()
	if len(orders) == 0 {
		t.Fatal("no AssetsOrder values found — the enumeration is broken, and a " +
			"guard with an empty subject set passes forever")
	}
	for _, order := range orders {
		orderBy := assetsOrderBy(order)
		if !strings.HasSuffix(orderBy, "ca.asset_id ASC") {
			// Not a mixed-direction tie-break — this invariant does not apply.
			continue
		}
		pred := assetsCursorPredicate(order, 3)

		// A row-constructor compare cannot express mixed directions.
		if strings.Contains(pred, ", ca.asset_id) <") || strings.Contains(pred, ", ca.asset_id) >") {
			t.Errorf("order %v: keyset predicate uses a row-constructor compare %q, but the "+
				"ORDER BY is mixed-direction (%q). A row constructor compares every element in "+
				"the SAME direction, so on a tie in the sort key this re-selects rows already "+
				"served and skips the rest — silent truncation, reported as has_more=false. "+
				"Spell the comparison out: (key < $n OR (key = $n AND ca.asset_id > $m)).",
				order, pred, orderBy)
		}

		// The tie-break half must walk asset_id FORWARD, matching ASC.
		if !strings.Contains(pred, "ca.asset_id > $") {
			t.Errorf("order %v: ORDER BY breaks ties on ca.asset_id ASC, so the keyset predicate "+
				"must resume with `ca.asset_id > $n`; got %q", order, pred)
		}
	}
}

// allAssetsOrders enumerates every AssetsOrder the package defines. It walks the int enum upward until
// assetsOrderBy stops producing a distinct ORDER BY, so a third ordering is covered the day it is written.
func allAssetsOrders() []AssetsOrder {
	var out []AssetsOrder
	seen := map[string]bool{}
	for i := 0; i < 64; i++ {
		o := AssetsOrder(i)
		ob := assetsOrderBy(o)
		if ob == "" || seen[ob] {
			continue
		}
		seen[ob] = true
		out = append(out, o)
	}
	return out
}

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

// TestListAssets_readsAssetVolumeRollup asserts the per_asset_24h_vol CTE reads the asset_volume_24h rollup
// rather than inlining the trailing-24h SUM(volume_usd), which would bring back the ~4.8s cold scan.
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

// TestListAssetsBaseSelectSQL_rendersForBothOrders guards the one render-time substitution: every ordering
// must read the volume rollup and leave no /*RANK_TIER*/ marker (listAssetsBaseSelectSQL panics if it is
// absent).
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

// TestAssetVolumeUnpricedTrades_shape pins the lower-bound plumbing: the refresh counts usd_volume IS NULL
// trades onto existing volume rows only (LEFT JOIN from vol, never admitting a contract to the Soroban
// spine) on a bare ts window, and the listing emits the flag from that count.
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

// TestRefreshAssetVolumeUpsert_sargable asserts the window predicate is a bare `bucket >= ... AND bucket <=
// now() - 1 minute` comparison; a function wrapped around the indexed bucket column would defeat chunk
// pruning.
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

// ValidateAssetsCursor pins per-shape rejection: garbage that a loose parser read as (0, "") would fall
// through silently into an empty page that looks like end-of-pagination (see parseAssetCursor).

func TestValidateAssetsCursor_emptyAlwaysOK(t *testing.T) {
	for _, order := range []AssetsOrder{
		AssetsOrderObservationCountDesc,
		AssetsOrderVolume24hUSDDesc,
	} {
		if err := ValidateAssetsCursor("", order); err != nil {
			t.Errorf("empty cursor must be valid for order %v, got %v", order, err)
		}
	}
}

func TestValidateAssetsCursor_obsCountOrder(t *testing.T) {
	cases := []struct {
		in        string
		wantErr   bool
		wantTagIn string // substring expected in error message
	}{
		{"100:USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN", false, ""},
		{"0:native", false, ""},
		{"99999999999999:XLM-GABC", false, ""},
		{"missing-colon", true, "separator"},
		{"100:", true, "asset_id"},
		{":native", true, "observation_count"},
		{"abc:native", true, "observation_count"},
		{"-5:native", true, "observation_count"}, // negative not produced; reject
		{"1.5:native", true, "observation_count"},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			err := ValidateAssetsCursor(tc.in, AssetsOrderObservationCountDesc)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil")
				}
				if !strings.Contains(err.Error(), tc.wantTagIn) {
					t.Errorf("expected error containing %q, got %v", tc.wantTagIn, err)
				}
				return
			}
			if err != nil {
				t.Errorf("unexpected error: %v", err)
			}
		})
	}
}

func TestValidateAssetsCursor_volumeOrder(t *testing.T) {
	cases := []struct {
		in      string
		wantErr bool
	}{
		// Empty volume prefix is what nextAssetCursor emits when the
		// last row had a null vol_usd — must round-trip cleanly.
		{":USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN", false},
		{"123.45:native", false},
		{"0:native", false},
		{"1234567890:native", false},
		{"missing-colon", true},
		{"123.45:", true},      // empty asset_id
		{"abc:native", true},   // non-numeric prefix
		{"1.2.3:native", true}, // two dots
		{"-5.0:native", true},  // leading minus
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			err := ValidateAssetsCursor(tc.in, AssetsOrderVolume24hUSDDesc)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Errorf("unexpected error: %v", err)
			}
		})
	}
}

// TestValidateAssetsCursor_rejectsShapesThatReachTheDatabase pins cursors that must be refused
// at the boundary as a 400. Each case below, if accepted, produces a database-level error or an
// empty 200 page indistinguishable from end-of-pagination.
func TestValidateAssetsCursor_rejectsShapesThatReachTheDatabase(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		cursor string
		order  AssetsOrder
		why    string
	}{
		{
			name:   "lone dot volume prefix",
			cursor: "0:.:USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN",
			order:  AssetsOrderVolume24hUSDDesc,
			why: "isNumericPrefix required no digit, so '.' validated and was bound " +
				"as $n::numeric, which Postgres rejects",
		},
		{
			name:   "rank tier past int32",
			cursor: "2147483648:100:USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN",
			order:  AssetsOrderObservationCountDesc,
			why:    "the tier is bound to a $n::int placeholder; a digit-string check alone lets it overflow",
		},
		{
			name:   "observation_count past int64",
			cursor: "0:99999999999999999999:USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN",
			order:  AssetsOrderObservationCountDesc,
			why: "parseAssetCursor's ParseInt fails and degenerates the cursor to (0,0,\"\"), " +
				"which matches no rows — a silent empty page that looks like the end of the walk",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if err := ValidateAssetsCursor(tc.cursor, tc.order); err == nil {
				t.Errorf("cursor %q accepted; it must be rejected at the boundary — %s",
					tc.cursor, tc.why)
			}
		})
	}
}

// The shapes the listing actually emits must keep validating — a
// tightened validator that rejects a server-emitted cursor would break
// pagination outright, which is worse than the bug it closes.
func TestValidateAssetsCursor_serverEmittedShapesStillValid(t *testing.T) {
	t.Parallel()
	id := "USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
	for _, tc := range []struct {
		name   string
		cursor string
		order  AssetsOrder
	}{
		{"obs count", "0:4200:" + id, AssetsOrderObservationCountDesc},
		{"flagged tier", "2:17:" + id, AssetsOrderObservationCountDesc},
		{"volume with decimal", "0:1234.56:" + id, AssetsOrderVolume24hUSDDesc},
		{"volume integral", "0:1234:" + id, AssetsOrderVolume24hUSDDesc},
		// A NULL vol_usd row emits an EMPTY volume prefix. This must stay
		// valid: it is a shape the server itself produces.
		{"volume empty (null vol_usd)", "0::" + id, AssetsOrderVolume24hUSDDesc},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if err := ValidateAssetsCursor(tc.cursor, tc.order); err != nil {
				t.Errorf("server-emitted cursor %q rejected: %v", tc.cursor, err)
			}
		})
	}
}
