// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package timescale

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"
)

// TestListAssetsBaseSelect_NoPerRequestPriceScan: the /v1/assets listing must read NO time-series relation.
// Prices come from the PK-keyed rollup, since inlining prices_1m reads is hidden by the cache until a miss.
func TestListAssetsBaseSelect_NoPerRequestPriceScan(t *testing.T) {
	t.Parallel()

	// Assert on the EXECUTABLE SQL: both this query and the derivation
	// carry prose about prices_1m in their comments, and a test that
	// cannot tell a comment from a scan is a test of the prose.
	listing := sqlWithoutComments(listAssetsBaseSelect)
	for _, banned := range []string{
		"prices_1m", "prices_15m", "prices_1h", "prices_1d",
		"DISTINCT ON",
	} {
		if strings.Contains(listing, banned) {
			t.Errorf("listing SELECT reads %q — the per-request price scan is back; "+
				"a new column belongs on a rollup, not in this query", banned)
		}
	}
	// Word-bounded so a rollup column such as asset_volume_24h.unpriced_trades
	// is not mistaken for a read of the trades hypertable.
	if regexp.MustCompile(`\btrades\b`).MatchString(listing) {
		t.Errorf("listing SELECT reads \"trades\" — the per-request price scan is back; " +
			"a new column belongs on a rollup, not in this query")
	}

	// Positive half: it must actually read the two rollups, or the
	// negative half above is satisfiable by simply serving nothing.
	for _, want := range []string{
		"FROM asset_volume_24h",
		"LEFT JOIN asset_price_snapshot",
	} {
		if !strings.Contains(listAssetsBaseSelect, want) {
			t.Errorf("listing SELECT must contain %q", want)
		}
	}

	// And the derivation must still EXIST somewhere — moved, not
	// dropped. Eight per-asset CTEs plus four XLM/USD scalars.
	for _, cte := range []string{
		"direct_usd AS (", "direct_usd_1h AS (", "direct_usd_24h AS (", "direct_usd_7d AS (",
		"asset_vs_xlm AS (", "asset_vs_xlm_1h AS (", "asset_vs_xlm_24h AS (", "asset_vs_xlm_7d AS (",
		"xlm_usd AS (", "xlm_usd_1h AS (", "xlm_usd_24h AS (", "xlm_usd_7d AS (",
	} {
		if !strings.Contains(assetPriceCTEs, cte) {
			t.Errorf("price derivation lost its %q CTE — the listing would go priceless, "+
				"not faster", cte)
		}
	}
	if got := strings.Count(sqlWithoutComments(assetPriceCTEs), "WHERE bucket = newest"); got != 8 {
		t.Errorf("price derivation has %d newest-bucket arms, want 8 (4 direct_usd* + 4 asset_vs_xlm*)", got)
	}
}

// TestPriceArms_UnionBothDirectionsAndPickByRecency: each per-asset arm reads both stored directions of its
// markets and is chosen by observation recency, not a fixed direct-then-XLM order, in the rollup and
// detail queries. The executing proof is test/integration/pg_pricing_test.go.
func TestPriceArms_UnionBothDirectionsAndPickByRecency(t *testing.T) {
	t.Parallel()
	usdList := strings.Join(strings.Fields(usdProxyQuotes), " ")
	fold := func(q string) string { return strings.Join(strings.Fields(sqlWithoutComments(q)), " ") }
	for name, c := range map[string]struct{ q, arms, joins string }{
		"rollup": {refreshAssetPriceSnapshotUpsert, assetPriceArmCTEs("", xlmQuotesBound1), priceArmJoins(xlmUSDGridJoin)},
		"detail": {
			getAssetBySlugSQL, assetPriceArmCTEs("(SELECT asset_id FROM chosen)", xlmQuotesBound(2)),
			priceArmJoins(xlmUSDLateralJoin("$2::text")),
		},
	} {
		sql, arms := fold(c.q), fold(c.arms)
		if !strings.Contains(sql, arms) {
			t.Fatalf("%s: does not splice its eight price arms", name)
		}
		for _, want := range []string{
			"quote_asset IN (" + usdList + ")",
			"base_asset IN (" + usdList + ")",
		} {
			if got := strings.Count(arms, want); got != 4 {
				t.Errorf("%s: %d direct_usd* arms read %q, want 4 (now, 1h, 24h, 7d)", name, got, want)
			}
		}
		if strings.Contains(sql, "inverted") {
			t.Errorf("%s: an arm still ranks one stored direction over the other", name)
		}
		if strings.Contains(sql, "COALESCE( CASE WHEN ca.asset_id = 'native'") {
			t.Errorf("%s: the price is still a fixed-order COALESCE across arms", name)
		}
		for _, want := range []string{
			fold(c.joins),
			"AND (ax.vwap IS NULL OR direct.bucket >= vs_xlm.bucket)",
		} {
			if !strings.Contains(sql, want) {
				t.Errorf("%s: missing the shared arm pick %.80q", name, want)
			}
		}
	}
}

// TestAssetPriceSnapshot_StalenessBoundIsEnforcedInSQL pins fail-closed staleness: the join floors
// computed_at, so a wedged aggregator makes the row render with no price rather than serve stale prices as
// current. It asserts the rendered predicate, since a constant nothing splices in bounds nothing.
func TestAssetPriceSnapshot_StalenessBoundIsEnforcedInSQL(t *testing.T) {
	t.Parallel()

	want := "aps.computed_at > now() - INTERVAL '" + assetPriceSnapshotMaxAge + "'"
	for _, order := range []AssetsOrder{AssetsOrderVolume24hUSDDesc, AssetsOrderObservationCountDesc} {
		sql := listAssetsBaseSelectSQL(order)
		if !strings.Contains(sql, want) {
			t.Errorf("order %v: listing join must bound snapshot age with %q — "+
				"without it a wedged aggregator serves indefinitely-old prices as current", order, want)
		}
		// The bound belongs to the JOIN, not the WHERE: in the WHERE it
		// would DROP every unpriced row from the listing instead of
		// rendering it unpriced.
		joinIdx := strings.Index(sql, "LEFT JOIN asset_price_snapshot")
		boundIdx := strings.Index(sql, want)
		nextJoinIdx := strings.Index(sql[joinIdx+1:], "LEFT JOIN ")
		if joinIdx < 0 || boundIdx < joinIdx || (nextJoinIdx >= 0 && boundIdx > joinIdx+1+nextJoinIdx) {
			t.Errorf("order %v: the computed_at floor must sit in the asset_price_snapshot "+
				"JOIN condition, not elsewhere", order)
		}
	}

	// The bound must be a parseable Postgres interval and a multiple of the refresh cadence: wide enough that a
	// restart or slow pass never blanks the flagship page, narrow enough that a price cannot drift materially.
	m := regexp.MustCompile(`^(\d+) (minutes|hours)$`).FindStringSubmatch(assetPriceSnapshotMaxAge)
	if m == nil {
		t.Fatalf("assetPriceSnapshotMaxAge = %q, want `<n> minutes` or `<n> hours`", assetPriceSnapshotMaxAge)
	}
	d, err := time.ParseDuration(strings.ReplaceAll(strings.ReplaceAll(
		assetPriceSnapshotMaxAge, " minutes", "m"), " hours", "h"))
	if err != nil {
		t.Fatalf("assetPriceSnapshotMaxAge %q does not parse: %v", assetPriceSnapshotMaxAge, err)
	}
	// The refresh cadence is assetvolrollup.DefaultInterval (2m); this
	// package must not import internal/aggregate, so the cadence is
	// restated as the bound's own floor.
	const refreshCadence = 2 * time.Minute
	if d < 3*refreshCadence {
		t.Errorf("staleness bound %s is under 3 refresh cadences (%s) — one slow pass blanks the listing",
			d, 3*refreshCadence)
	}
	if d > time.Hour {
		t.Errorf("staleness bound %s exceeds an hour — that is no longer a bound on a money column", d)
	}
}

// TestRefreshAssetPriceSnapshotUpsert_shape asserts the writer derives
// every column the listing stopped deriving, keyed one row per PRICED
// asset, upserted idempotently and stamped with the pass timestamp.
func TestRefreshAssetPriceSnapshotUpsert_shape(t *testing.T) {
	t.Parallel()
	for _, want := range []string{
		"INSERT INTO asset_price_snapshot",
		"priced_assets AS (", // spine = the assets that CAN price
		"SELECT asset_id FROM direct_usd",
		"SELECT asset_id FROM asset_vs_xlm",
		"SELECT 'native'::text",       // XLM has no (native, native) row
		"WHERE price_usd IS NOT NULL", // a row means "priced"
		"ON CONFLICT (asset_id) DO UPDATE",
		"computed_at    = EXCLUDED.computed_at",
	} {
		if !strings.Contains(refreshAssetPriceSnapshotUpsert, want) {
			t.Errorf("price upsert missing %q", want)
		}
	}
	for _, col := range []string{"change_1h_pct", "change_24h_pct", "change_7d_pct", "source_count"} {
		if !strings.Contains(refreshAssetPriceSnapshotUpsert, col) {
			t.Errorf("price upsert does not derive %q — the listing no longer can", col)
		}
	}
}

// TestAssetPriceSnapshot_MoneyStaysNumeric guards ADR-0003: the refresh must store the price and
// change ratios UNROUNDED and UNFORMATTED, and the listing must apply the same wire rendering as
// before, or the rollup changes the served decimal string.
func TestAssetPriceSnapshot_MoneyStaysNumeric(t *testing.T) {
	t.Parallel()

	// Writer side: no rounding, no formatting, no float.
	assertNoFloatSQL(t, "price upsert", refreshAssetPriceSnapshotUpsert)
	for _, banned := range []string{"ROUND(", "to_char(", "::bigint", "::int8"} {
		if strings.Contains(refreshAssetPriceSnapshotUpsert, banned) {
			t.Errorf("price upsert must store money as unrounded NUMERIC; found %q", banned)
		}
	}

	// Reader side: the same rendering the inline derivation used.
	if !strings.Contains(listAssetsBaseSelect, "ROUND("+listingPriceUSDExpr+", 10)::text  AS price_usd") {
		t.Error("listing must still render price_usd as ROUND(<price>, 10)::text — the wire " +
			"string has to be byte-identical to the pre-rollup form")
	}
	for _, col := range []string{"change_1h_pct", "change_24h_pct", "change_7d_pct"} {
		want := "to_char(aps." + col
		if !strings.Contains(listAssetsBaseSelect, want) {
			t.Errorf("listing must render %s via to_char on the stored NUMERIC ratio", col)
		}
	}
	if got := strings.Count(listAssetsBaseSelect, "'FM999999990.00'"); got != 3 {
		t.Errorf("listing uses the change format string %d times, want 3 (1h/24h/7d) — "+
			"a changed format changes the wire value", got)
	}

	// And the one derivation the tier, the ORDER BY and the keyset WHERE
	// all read is the rollup column, not a second inline copy.
	if listingPriceUSDExpr != "aps.price_usd" {
		t.Errorf("listingPriceUSDExpr = %q, want the rollup column `aps.price_usd`", listingPriceUSDExpr)
	}
}

// TestRollupPrunes_sargable: each rollup prune deletes on a bare
// computed_at comparison (index-friendly, no function on the column), so
// the sweep stays cheap and a lapsed row cannot linger as if current.
func TestRollupPrunes_sargable(t *testing.T) {
	t.Parallel()
	for name, q := range map[string]string{
		"refreshAssetVolumePrune":        refreshAssetVolumePrune,
		"refreshAssetPriceSnapshotPrune": refreshAssetPriceSnapshotPrune,
		"refreshProtocolEventsPrune":     refreshProtocolEventsPrune,
	} {
		if !strings.Contains(q, "computed_at < now()") {
			t.Errorf("%s must compare computed_at < now() directly, got: %q", name, q)
		}
	}
}

// TestRefreshAssetListingRollups_oneTransaction pins the refresh path: both rollups, four statements, one
// transaction, ordered so no window has this pass's volume with last pass's price, driven through the
// existing RefreshAssetVolume24h seam.
func TestRefreshAssetListingRollups_oneTransaction(t *testing.T) {
	t.Parallel()

	store, script := newScriptedStore(t,
		scriptedResult{}, // SET LOCAL work_mem
		scriptedResult{}, // SET LOCAL statement_timeout
		scriptedResult{rowsAffected: 10},
		scriptedResult{rowsAffected: 1},
		scriptedResult{rowsAffected: 10},
		scriptedResult{rowsAffected: 1},
	)

	if err := store.RefreshAssetVolume24h(context.Background()); err != nil {
		t.Fatalf("RefreshAssetVolume24h: %v", err)
	}

	got := script.statements()
	want := []string{
		// Footprint bounds first, inside the transaction, so they revert
		// on COMMIT and cannot leak onto the pooled connection.
		"SET LOCAL work_mem",
		"SET LOCAL statement_timeout",
		"INSERT INTO asset_volume_24h",
		"DELETE FROM asset_volume_24h",
		"INSERT INTO asset_price_snapshot",
		"DELETE FROM asset_price_snapshot",
	}
	if len(got) != len(want) {
		t.Fatalf("expected %d statements in the refresh, got %d: %v", len(want), len(got), got)
	}
	for i, w := range want {
		if !strings.Contains(got[i], w) {
			t.Errorf("statement %d = %.60q…, want it to contain %q", i, got[i], w)
		}
	}
	if !script.committed() {
		t.Error("refresh must COMMIT — an uncommitted pass leaves both rollups at the previous values")
	}
}

// TestRefreshAssetListingRollups_priceFailureRollsBackVolume: the two
// rollups are joined onto the same listing row, so a half-applied pass
// (fresh volume, last pass's price) must not commit.
func TestRefreshAssetListingRollups_priceFailureRollsBackVolume(t *testing.T) {
	t.Parallel()

	boom := errors.New("prices_1m read failed")
	store, script := newScriptedStore(t,
		scriptedResult{},                 // SET LOCAL work_mem
		scriptedResult{},                 // SET LOCAL statement_timeout
		scriptedResult{rowsAffected: 10}, // volume upsert
		scriptedResult{rowsAffected: 1},  // volume prune
		scriptedResult{err: boom},        // price upsert fails
	)

	err := store.RefreshAssetVolume24h(context.Background())
	if err == nil {
		t.Fatal("a failing price upsert must fail the whole refresh")
	}
	if !errors.Is(err, boom) {
		t.Errorf("error must wrap the driver error; got %v", err)
	}
	if script.committed() {
		t.Error("a failed pass must not COMMIT the volume half")
	}
}

// TestRefreshAssetListingRollups_zeroRowPassKeepsLastGood: an upsert that wrote nothing (prices_1m
// empty or stalled) must not run the pass prune, which on a now()-stamped transaction would delete
// every row. It runs the expiry prune instead; each half decides on its own upsert's count.
func TestRefreshAssetListingRollups_zeroRowPassKeepsLastGood(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name               string
		volRows, priceRows int64
		wantVolPrune       string
		wantPricePrune     string
	}{
		{"both empty", 0, 0, refreshAssetVolumePruneExpired, refreshAssetPriceSnapshotPruneExpired},
		{"price empty", 10, 0, refreshAssetVolumePrune, refreshAssetPriceSnapshotPruneExpired},
		{"volume empty", 0, 10, refreshAssetVolumePruneExpired, refreshAssetPriceSnapshotPrune},
		{"both populated", 10, 10, refreshAssetVolumePrune, refreshAssetPriceSnapshotPrune},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			store, script := newScriptedStore(t,
				scriptedResult{}, // SET LOCAL work_mem
				scriptedResult{}, // SET LOCAL statement_timeout
				scriptedResult{rowsAffected: tc.volRows},
				scriptedResult{},
				scriptedResult{rowsAffected: tc.priceRows},
				scriptedResult{},
			)
			if err := store.RefreshAssetListingRollups(context.Background()); err != nil {
				t.Fatalf("RefreshAssetListingRollups: %v", err)
			}
			got := script.statements()
			if len(got) != 6 {
				t.Fatalf("expected 6 statements, got %d: %v", len(got), got)
			}
			if got[3] != tc.wantVolPrune {
				t.Errorf("volume prune = %q, want %q", got[3], tc.wantVolPrune)
			}
			if got[5] != tc.wantPricePrune {
				t.Errorf("price prune = %q, want %q", got[5], tc.wantPricePrune)
			}
			if !script.committed() {
				t.Error("a zero-row pass still commits the other half")
			}
		})
	}
}

// TestRollupExpiryPrunes_boundedByValidityWindow: the expiry prune keeps
// a row no longer than the window the rollup describes, so a lasting
// upstream outage ends in an empty rollup, never an indefinitely stale one.
func TestRollupExpiryPrunes_boundedByValidityWindow(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct{ prune, window string }{
		{refreshAssetVolumePruneExpired, "24 hours"},
		{refreshAssetPriceSnapshotPruneExpired, assetPriceSnapshotMaxAge},
		{refreshAssetVolumeCharacterPruneExpired, fmt.Sprintf("%d days", int(volumeCharacterWindow.Hours())/24)},
	} {
		want := "computed_at < now() - INTERVAL '" + tc.window + "'"
		if !strings.HasSuffix(tc.prune, want) {
			t.Errorf("expiry prune %q must end with %q", tc.prune, want)
		}
	}
	if !strings.Contains(refreshAssetVolumeUpsert, "bucket >= now() - INTERVAL '24 hours'") {
		t.Error("asset_volume_24h window moved; move refreshAssetVolumePruneExpired with it")
	}
}

// sqlWithoutComments strips `-- ...` line comments so an assertion about what a query reads cannot be
// satisfied or defeated by what its prose says; both consts here comment on the very table names under test.
func sqlWithoutComments(sql string) string {
	var b strings.Builder
	for _, line := range strings.Split(sql, "\n") {
		if i := strings.Index(line, "--"); i >= 0 {
			line = line[:i]
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return b.String()
}

// TestAssetPriceSnapshot_DecimalsNormalisedOnceAtTheWriter pins that the nonstandard-decimals correction
// lives only in the rollup writer; a reader scaling aps.price_usd again would be off by the factor squared.
// The executing proof is test/integration/pg_pricing_test.go.
func TestAssetPriceSnapshot_DecimalsNormalisedOnceAtTheWriter(t *testing.T) {
	t.Parallel()

	upsert := sqlWithoutComments(refreshAssetPriceSnapshotUpsert)
	for _, want := range []string{
		"LEFT JOIN nonstandard_decimals_assets nda ON nda.asset = ca.asset_id",
		"power(10::numeric, (nda.decimals - 7)::numeric)",
		// An asset with no confirmed row takes the untouched arm, so its
		// stored NUMERIC is the one it always was.
		"CASE WHEN nda.decimals IS NULL",
	} {
		if !strings.Contains(upsert, want) {
			t.Errorf("price upsert missing %q", want)
		}
	}
	if got := strings.Count(upsert, "power(10::numeric"); got != 1 {
		t.Errorf("price upsert applies the decimals factor %d times, want exactly 1 "+
			"(the change columns are same-scale ratios and take none)", got)
	}

	// The listing spine reads the corrected column and must not correct
	// it again.
	if reader := sqlWithoutComments(listAssetsBaseSelect); strings.Contains(reader, "nonstandard_decimals_assets") {
		t.Error("the listing SELECT joins nonstandard_decimals_assets — asset_price_snapshot " +
			"is already normalised by its writer, so this would apply the factor twice")
	}
}

// TestCatalogueReads_RoundAfterTheDecimalsCorrection: the per-asset row and price-history reads stay RAW
// but must not round a confirmed token's ratio on a flat 10-place scale (14 USD became exactly 10).
func TestCatalogueReads_RoundAfterTheDecimalsCorrection(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct{ sql, places string }{
		"GetAssetPriceHistory24h":       {getAssetPriceHistory24hSQL, catalogueRoundPlacesForAliases},
		"GetAssetPriceHistory7d":        {getAssetPriceHistory7dSQL, catalogueRoundPlacesForAliases},
		"GetAssetBySlug":                {getAssetBySlugSQL, catalogueRoundPlacesForRow},
		"GetAssetsPriceHistory24hBatch": {getAssetsPriceHistory24hBatchSQL, catalogueRoundPlacesForWanted},
		"GetAssetsPriceHistory7dBatch":  {getAssetsPriceHistory7dBatchSQL, catalogueRoundPlacesForWanted},
	} {
		if got := strings.Count(tc.sql, "), "+tc.places+")::text"); got != 1 {
			t.Errorf("%s rounds its price on the decimals-aware scale %d times, want exactly 1", name, got)
		}
		if strings.Contains(sqlWithoutComments(tc.sql), "), 10)::text") {
			t.Errorf("%s still rounds a price to a flat 10 places before the correction can run", name)
		}
		// Rounding scale only: a multiply here would stack on the API's.
		if strings.Contains(sqlWithoutComments(tc.sql), "power(") {
			t.Errorf("%s scales the price in SQL — the API already does, this would apply the factor twice", name)
		}
	}

	places := sqlWithoutComments(catalogueRoundPlacesForRow)
	for _, want := range []string{
		"10 + COALESCE(",                // no confirmed row: exactly ROUND(…, 10)
		"GREATEST(nda.decimals - 7, 0)", // scaling DOWN never narrows the rounding
		"nonstandard_decimals_assets",
	} {
		if !strings.Contains(places, want) {
			t.Errorf("rounding-scale expression missing %q: %s", want, places)
		}
	}
}
