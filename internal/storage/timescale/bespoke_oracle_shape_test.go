// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package timescale

import (
	"strings"
	"testing"
)

// assertOracleWindowBounded guards the bounded-scan rule: every windowed
// oracle query is chunk-excluded via the ts index, never an unbounded walk.
func assertOracleWindowBounded(t *testing.T, name, q string) {
	t.Helper()
	if !strings.Contains(q, "ts > now() - $2::interval") {
		t.Errorf("%s must be window-bounded on ts", name)
	}
	if !strings.Contains(q, "source = $1") {
		t.Errorf("%s must scope to the page's source", name)
	}
}

// assertOracleCountsAndTimestampsOnly guards the page's honesty contract:
// the new oracle analytics are update COUNTS + TIMESTAMPS only — no price
// column touched, no averaging (aggregated pricing is the aggregator's
// domain and oracle observations never feed VWAP).
func assertOracleCountsAndTimestampsOnly(t *testing.T, name, q string) {
	t.Helper()
	if strings.Contains(q, "price") {
		t.Errorf("%s must not touch the price column (counts + timestamps only)", name)
	}
	for _, agg := range []string{"avg(", "sum("} {
		if strings.Contains(q, agg) {
			t.Errorf("%s must not aggregate values (%s) — counts + timestamps only", name, agg)
		}
	}
}

// TestOracleUpdatesSeriesQueryShape guards the total update series: hourly
// buckets at the 24h window (a daily bucket collapses it to one point),
// daily otherwise, window-bounded, counts only.
func TestOracleUpdatesSeriesQueryShape(t *testing.T) {
	hourly := oracleUpdatesSeriesQuery(1)
	if !strings.Contains(hourly, "date_trunc('hour', ts)") || !strings.Contains(hourly, "HH24:00") {
		t.Error("24h query must bucket by hour with the hour in the timestamp format")
	}
	daily := oracleUpdatesSeriesQuery(30)
	if !strings.Contains(daily, "date_trunc('day', ts)") || strings.Contains(daily, "HH24:00") {
		t.Error("30d query must bucket by day with a date-only timestamp format")
	}
	for _, q := range []string{hourly, daily} {
		assertOracleWindowBounded(t, "updates series", q)
		assertOracleCountsAndTimestampsOnly(t, "updates series", q)
		if !strings.Contains(q, "count(*)") {
			t.Error("updates series must count rows")
		}
	}
}

// TestOraclePerFeedSeriesQueryShape guards the per-feed lines: capped at
// the top 5 feeds by window count, joined on the full (asset, quote) pair,
// grain-aware, window-bounded, counts only.
func TestOraclePerFeedSeriesQueryShape(t *testing.T) {
	hourly := oraclePerFeedSeriesQuery(1)
	if !strings.Contains(hourly, "date_trunc('hour', u.ts)") {
		t.Error("24h per-feed query must bucket by hour")
	}
	daily := oraclePerFeedSeriesQuery(90)
	if !strings.Contains(daily, "date_trunc('day', u.ts)") {
		t.Error("90d per-feed query must bucket by day")
	}
	for _, q := range []string{hourly, daily} {
		assertOracleWindowBounded(t, "per-feed series", q)
		assertOracleCountsAndTimestampsOnly(t, "per-feed series", q)
		if !strings.Contains(q, "LIMIT 5") {
			t.Error("per-feed series must cap at the top 5 feeds by window count")
		}
		if !strings.Contains(q, "t.asset = u.asset AND t.quote = u.quote") {
			t.Error("per-feed series must join on the full (asset, quote) pair — an asset-only join merges distinct quotes")
		}
	}
}

// TestOracleMedianIntervalQueryShape guards the batch-honesty rule: the
// median publish interval is measured between DISTINCT publication
// timestamps (row-to-row gaps within one batch push are zero and would
// report a dishonest 0s cadence), NULL-safe, and window-bounded.
func TestOracleMedianIntervalQueryShape(t *testing.T) {
	q := oracleMedianIntervalQuery()
	if !strings.Contains(q, "SELECT DISTINCT ts") {
		t.Error("median interval must be computed over DISTINCT publication timestamps (batch pushes land many rows at one ts)")
	}
	if !strings.Contains(q, "percentile_cont(0.5)") {
		t.Error("median interval must take the 0.5 percentile of the gaps")
	}
	if !strings.Contains(q, "COALESCE(") {
		t.Error("median interval must render an em dash, not NULL, when fewer than two publications exist")
	}
	assertOracleWindowBounded(t, "median interval", q)
	assertOracleCountsAndTimestampsOnly(t, "median interval", q)
}

// TestOracleKPIQueriesShape guards the KPI splits: the window query is
// bounded and carries the freshest-update age (now minus max ts, truncated
// to seconds — rendered honestly, never a fabricated freshness); the
// all-time query is deliberately unbounded (retained history) and touches
// counts + first-seen date only.
func TestOracleKPIQueriesShape(t *testing.T) {
	w := oracleWindowKPIQuery()
	if !strings.Contains(w, "ts > now() - $3::interval") || !strings.Contains(w, "source = $1") {
		t.Error("window KPI query must be source-scoped and window-bounded")
	}
	if !strings.Contains(w, "now() - max(ts)") {
		t.Error("window KPI query must compute the freshest-update age from max(ts)")
	}
	if !strings.Contains(w, "date_trunc('second'") {
		t.Error("freshest-update age must be truncated to seconds for honest rendering")
	}
	if !strings.Contains(w, "count(DISTINCT (asset, quote)) FILTER (WHERE asset LIKE 'raw:%')") {
		t.Error("window KPI query must count the unmapped (raw:) feeds as their own KPI — totality shows them, it does not hide them")
	}
	assertOracleCountsAndTimestampsOnly(t, "window KPIs", w)

	a := oracleAllTimeKPIQuery()
	if strings.Contains(a, "interval") || strings.Contains(a, "$2") {
		t.Error("all-time KPI query must not be window-bounded (it is the retained-history total)")
	}
	if !strings.Contains(a, "count(*)") || !strings.Contains(a, "min(ts)") {
		t.Error("all-time KPI query must return the retained count and the first-seen date")
	}
	assertOracleCountsAndTimestampsOnly(t, "all-time KPIs", a)
}

// TestOracleFeedBreakdownAndTableShape guards the composition + table SQL:
// count-descending per-pair grouping, the top-15 cap with per-feed last
// update on the Feeds table, counts + timestamps only.
func TestOracleFeedBreakdownAndTableShape(t *testing.T) {
	bd := oracleFeedBreakdownQuery()
	assertOracleWindowBounded(t, "feed breakdown", bd)
	assertOracleCountsAndTimestampsOnly(t, "feed breakdown", bd)
	if !strings.Contains(bd, "GROUP BY asset, quote") || !strings.Contains(bd, "ORDER BY count(*) DESC") {
		t.Error("feed breakdown must group per (asset, quote) pair, count-descending")
	}

	ft := oracleFeedsTableQuery()
	assertOracleWindowBounded(t, "feeds table", ft)
	assertOracleCountsAndTimestampsOnly(t, "feeds table", ft)
	if !strings.Contains(ft, "LIMIT 15") {
		t.Error("feeds table must cap at the top 15 feeds")
	}
	if !strings.Contains(ft, "max(ts)") {
		t.Error("feeds table must carry each feed's last update time")
	}
}

// TestOracleLatestPricesQueryShape guards the one price-bearing surface:
// the latest raw observation per feed, shown verbatim (DISTINCT ON latest
// pick — never averaged, summed, or rescaled).
func TestOracleLatestPricesQueryShape(t *testing.T) {
	q := oracleLatestPricesQuery()
	assertOracleWindowBounded(t, "latest prices", q)
	if !strings.Contains(q, "DISTINCT ON (asset, quote)") {
		t.Error("latest prices must pick one latest row per feed")
	}
	for _, agg := range []string{"avg(", "sum(price", "/ 1"} {
		if strings.Contains(q, agg) {
			t.Errorf("latest prices must show the raw observation verbatim — found %q (no averaging or rescaling; ADR-0003 / aggregator's domain)", agg)
		}
	}
}

// TestFoldBreakdownRows guards the top-N + Others fold used by the count-
// weighted donuts: short lists pass through, the tail folds into one
// "Others" row whose Value/Count are the tail's summed COUNTS (the helper
// is count-only by contract — token amounts across feeds/vaults are
// different units and must never be summed).
func TestFoldBreakdownRows(t *testing.T) {
	short := []BespokeBreakdownRow{{Label: "a", Value: "3", Count: 3}, {Label: "b", Value: "1", Count: 1}}
	if got := foldBreakdownRows(short, 10); len(got) != 2 {
		t.Errorf("short list fold = %d rows, want 2 (unchanged)", len(got))
	}

	long := []BespokeBreakdownRow{
		{Label: "a", Value: "5", Count: 5},
		{Label: "b", Value: "4", Count: 4},
		{Label: "c", Value: "3", Count: 3},
		{Label: "d", Value: "2", Count: 2},
		{Label: "e", Value: "1", Count: 1},
	}
	got := foldBreakdownRows(long, 3)
	if len(got) != 4 {
		t.Fatalf("fold(5 rows, top 3) = %d rows, want 4 (top 3 + Others)", len(got))
	}
	if got[0].Label != "a" || got[2].Label != "c" {
		t.Errorf("fold must preserve the query's count-descending head order, got %v", got)
	}
	others := got[3]
	if others.Label != "Others" || others.Count != 3 || others.Value != "3" {
		t.Errorf("Others = %+v, want {Others 3 3} (tail counts summed)", others)
	}
	// The input must not be mutated beyond the shared head slice.
	if long[3].Label != "d" || long[4].Label != "e" {
		t.Error("fold must not clobber the input tail rows")
	}
}
