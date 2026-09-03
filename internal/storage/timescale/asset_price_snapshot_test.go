// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package timescale

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"
)

// TestListAssetsBaseSelect_NoPerRequestPriceScan is the regression guard
// for #331 F1, and the one that must never be relaxed.
//
// The /v1/assets listing used to DERIVE its price column per request:
// twelve `DISTINCT ON … FROM prices_1m` CTEs materialised for every
// asset in the catalogue, on every uncached variant, whatever page was
// asked for. On r1 (`pg_stat_statements`, 2026-07-06 → 2026-09-02) the
// unfiltered statement ran 8,019 times at mean 2,400 ms / max 10,295 ms
// with 380,324 shared-buffer hits per call, three sibling shapes added
// 10,483 more calls at 1.5-2.1 s, and `EXPLAIN (ANALYZE, BUFFERS)` on
// `?limit=50` measured 1,830 ms / 348,442 buffers / 51 MB of temp spill
// for fifty rows.
//
// The derivation now lives in refreshAssetPriceSnapshotUpsert and the
// listing reads a keyed-on-PK rollup. If a future change re-inlines a
// prices_1m read here — for one more column, for one more window, for
// "just the latest bucket" — the whole cost comes back, on the flagship
// endpoint, invisibly, because the SWR cache hides it until a variant
// misses. So: the listing SELECT reads NO time-series relation at all.
func TestListAssetsBaseSelect_NoPerRequestPriceScan(t *testing.T) {
	t.Parallel()

	// Assert on the EXECUTABLE SQL: both this query and the derivation
	// carry prose about prices_1m in their comments, and a test that
	// cannot tell a comment from a scan is a test of the prose.
	listing := sqlWithoutComments(listAssetsBaseSelect)
	for _, banned := range []string{
		"prices_1m", "prices_15m", "prices_1h", "prices_1d",
		"trades", "DISTINCT ON",
	} {
		if strings.Contains(listing, banned) {
			t.Errorf("listing SELECT reads %q — the per-request price scan is back; "+
				"a new column belongs on a rollup, not in this query", banned)
		}
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
	if got := strings.Count(sqlWithoutComments(assetPriceCTEs), "DISTINCT ON"); got != 8 {
		t.Errorf("price derivation has %d DISTINCT ON CTEs, want 8 (4 direct_usd* + 4 asset_vs_xlm*)", got)
	}
}

// TestAssetPriceSnapshot_StalenessBoundIsEnforcedInSQL pins the
// fail-closed half of the staleness contract.
//
// Moving the price onto a rollup makes the listing depend on the
// aggregator being alive. The failure mode that must NOT exist is the
// quiet one: an aggregator wedged for an hour while /v1/assets keeps
// serving hour-old prices as current. So the listing's join carries a
// floor on computed_at — past it the join MISSES, the row renders as an
// asset with no price (which the payload already models, and which is
// visible), and the aggregator's own heartbeat is what pages.
//
// Asserting the rendered predicate, not just the constant: a constant
// nothing splices in bounds nothing.
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

	// The bound has to be a real, parseable Postgres interval and has to
	// stay a multiple of the refresh cadence — wide enough that a
	// restart or a slow pass never blanks the flagship page, narrow
	// enough that a price cannot drift materially first.
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

// TestAssetPriceSnapshot_MoneyStaysNumeric guards ADR-0003 across the
// move. The refresh must store the price and the change ratios
// UNROUNDED and UNFORMATTED, and the listing must apply the identical
// wire rendering it always applied — otherwise the rollup changes the
// served decimal string, which is a money bug wearing a perf-fix badge.
func TestAssetPriceSnapshot_MoneyStaysNumeric(t *testing.T) {
	t.Parallel()

	// Writer side: no rounding, no formatting, no float.
	for _, banned := range []string{"ROUND(", "to_char(", "::float", "::double", "::real", "::bigint", "::int8"} {
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

// TestRefreshAssetPriceSnapshotPrune_sargable — the prune deletes on a
// bare computed_at comparison so a lapsed asset's last known price
// cannot linger as if it were current.
func TestRefreshAssetPriceSnapshotPrune_sargable(t *testing.T) {
	t.Parallel()
	if !strings.Contains(refreshAssetPriceSnapshotPrune, "computed_at < now()") {
		t.Errorf("prune must compare computed_at < now() directly, got: %q", refreshAssetPriceSnapshotPrune)
	}
}

// TestRefreshAssetListingRollups_oneTransaction pins the refresh path:
// both rollups, four statements, one transaction, in an order that
// leaves no window where an asset has this pass's volume and last
// pass's price.
//
// The scripted driver records every statement, so this also proves the
// aggregator's wired seam (RefreshAssetVolume24h) really does drive the
// price refresh — the shim is the only reason the fix is live without a
// change under internal/aggregate or cmd/.
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

// sqlWithoutComments strips `-- …` line comments so an assertion about
// what a query READS cannot be satisfied — or defeated — by what its
// prose SAYS. Both consts here are heavily commented, and both comment
// on the very table names under test.
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
