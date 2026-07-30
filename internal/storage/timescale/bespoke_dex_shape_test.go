// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package timescale

import (
	"strings"
	"testing"
)

// assertDEXNumericSafe guards the ADR-0003 rule for the DEX suite: USD
// figures round via exact NUMERIC round(x, 2), never a float literal, and
// USD values source ONLY from usd_volume / its CAGG sums.
func assertDEXNumericSafe(t *testing.T, name, q string) {
	t.Helper()
	for _, bad := range []string{"1e6", "1e+06", "1e7", "::float", "::double"} {
		if strings.Contains(q, bad) {
			t.Errorf("%s must never use float arithmetic (ADR-0003); found %q", name, bad)
		}
	}
}

// assertWindowBounded guards against unbounded trades/CAGG walks: every
// windowed DEX query binds the $2::interval window on its time column.
func assertWindowBounded(t *testing.T, name, q string) {
	t.Helper()
	if !strings.Contains(q, "> now() - $2::interval") {
		t.Errorf("%s must be window-bounded (never an unbounded scan)", name)
	}
}

// TestDexRawWindowOK guards the SDEX raw-scan gate: raw-trades-derived
// surfaces (traders, avg size, largest trades) are served for SDEX only at
// the 24h/7d windows (5M rows/week measured 1.4s; ~15–20s extrapolated at
// 90d), and always for the AMM sources (≤1M rows at 90d).
func TestDexRawWindowOK(t *testing.T) {
	for _, c := range []struct {
		source string
		days   int
		want   bool
	}{
		{"sdex", 1, true},
		{"sdex", 7, true},
		{"sdex", 30, false},
		{"sdex", 90, false},
		{"soroswap", 90, true},
		{"aquarius", 90, true},
		{"phoenix", 90, true},
		{"comet", 90, true},
	} {
		if got := dexRawWindowOK(c.source, c.days); got != c.want {
			t.Errorf("dexRawWindowOK(%s, %d) = %v, want %v", c.source, c.days, got, c.want)
		}
	}
}

// TestDexActivitySeriesQueryShape — the 24h window MUST come from the
// hourly source_volume_1h rollup with hour-carrying timestamps (a daily
// bucket collapses 24h into one partial point); longer windows come from
// the daily pair CAGG.
func TestDexActivitySeriesQueryShape(t *testing.T) {
	hourly := dexActivitySeriesQuery(1)
	if !strings.Contains(hourly, "source_volume_1h") {
		t.Error("24h activity series must read the hourly source_volume_1h CAGG")
	}
	if !strings.Contains(hourly, `HH24:00`) {
		t.Error("24h activity series must carry the hour in point timestamps")
	}
	if !strings.Contains(hourly, "sum_usd_priced") {
		t.Error("24h activity series USD volume must sum the CAGG's sum_usd_priced (= SUM(usd_volume))")
	}

	daily := dexActivitySeriesQuery(90)
	if !strings.Contains(daily, "dex_volume_by_pair_1d") {
		t.Error("90d activity series must read the daily pair CAGG, not raw trades")
	}
	if strings.Contains(daily, "HH24") {
		t.Error("90d activity series must not carry an hour component")
	}

	for _, q := range []string{hourly, daily} {
		assertWindowBounded(t, "activity series", q)
		assertDEXNumericSafe(t, "activity series", q)
	}
}

// TestDexWindowKPIQueryShape — the >1d KPI counts pairs via a hash-agg
// GROUP BY subquery, NOT count(DISTINCT (base,quote)): the row-comparison
// sort of the latter measured 6.0s on sdex 90d vs 0.67s for the GROUP BY
// shape (r1, 2026-07-30).
func TestDexWindowKPIQueryShape(t *testing.T) {
	q := dexWindowKPIQuery(90)
	if strings.Contains(q, "count(DISTINCT (base_asset, quote_asset))") {
		t.Error("window KPI must not count pairs via count(DISTINCT row) — measured 6.0s on sdex 90d; use the GROUP BY subquery")
	}
	if !strings.Contains(q, "GROUP BY base_asset, quote_asset") {
		t.Error("window KPI must count pairs via the hash-agg GROUP BY subquery")
	}
	if !strings.Contains(q, "round(COALESCE(sum(vol),0),2)") {
		t.Error("window KPI USD volume must be the CAGG vol sum rounded via exact NUMERIC round")
	}
	assertWindowBounded(t, "window KPI", q)

	h := dexWindowKPIQuery(1)
	if !strings.Contains(h, "source_volume_1h") {
		t.Error("24h KPI must read the hourly rollup")
	}
	assertWindowBounded(t, "24h KPI", h)
}

// TestDexRawKPIQueryShape — avg trade size is exact NUMERIC division of
// the usd_volume sum by the PRICED trade count: NULL-usd trades are
// excluded from both sides (count(usd_volume) skips NULLs; sum ignores
// them), and NULLIF guards the zero-priced window.
func TestDexRawKPIQueryShape(t *testing.T) {
	q := dexRawKPIQuery()
	if !strings.Contains(q, "sum(usd_volume) / NULLIF(count(usd_volume),0)") {
		t.Error("avg trade size must divide the usd_volume sum by the priced-trade count with a NULLIF guard")
	}
	if !strings.Contains(q, "count(DISTINCT taker)") {
		t.Error("raw KPI must count distinct takers")
	}
	assertWindowBounded(t, "raw KPI", q)
	assertDEXNumericSafe(t, "raw KPI", q)
}

// TestDexTradersSeriesQueryShape — hourly buckets at 24h, daily otherwise,
// and NULL takers excluded (they are absence of data, not a trader).
func TestDexTradersSeriesQueryShape(t *testing.T) {
	hourly := dexTradersSeriesQuery(1)
	if !strings.Contains(hourly, "date_trunc('hour', ts)") || !strings.Contains(hourly, "HH24:00") {
		t.Error("24h traders series must bucket by hour with the hour in the timestamp format")
	}
	daily := dexTradersSeriesQuery(90)
	if !strings.Contains(daily, "date_trunc('day', ts)") || strings.Contains(daily, "HH24") {
		t.Error("90d traders series must bucket by day with a date-only format")
	}
	for _, q := range []string{hourly, daily} {
		if !strings.Contains(q, "taker IS NOT NULL") {
			t.Error("traders series must exclude NULL takers")
		}
		if !strings.Contains(q, "count(DISTINCT taker)") {
			t.Error("traders series must count distinct takers per bucket")
		}
		assertWindowBounded(t, "traders series", q)
	}
}

// TestDexPairBreakdownQueryShape — top 8 pairs + a SQL-side "Others" fold
// (rank in SQL so sdex's ~99k window pairs never cross the wire), CAGG for
// >1d, raw trades only at the 24h grain.
func TestDexPairBreakdownQueryShape(t *testing.T) {
	daily := dexPairBreakdownQuery(90)
	if !strings.Contains(daily, "dex_volume_by_pair_1d") || strings.Contains(daily, "FROM trades") {
		t.Error("90d breakdown must read the daily pair CAGG, not raw trades")
	}
	hourly := dexPairBreakdownQuery(1)
	if !strings.Contains(hourly, "FROM trades") {
		t.Error("24h breakdown must read raw trades (the daily CAGG cannot bucket 24h)")
	}
	for _, q := range []string{daily, hourly} {
		if !strings.Contains(q, "rn <= 8") {
			t.Error("breakdown must keep the top 8 pairs")
		}
		if !strings.Contains(q, "row_number() OVER (ORDER BY vol DESC") {
			t.Error("breakdown must rank pairs by USD volume in SQL")
		}
		if !strings.Contains(q, "ORDER BY min(rn) ASC") {
			t.Error("breakdown must order top pairs by rank with the Others fold last")
		}
		assertWindowBounded(t, "pair breakdown", q)
		assertDEXNumericSafe(t, "pair breakdown", q)
	}
}

// TestDexTopPairsSeriesQueryShape — top-5 cap; the >1d form joins the
// CAGG directly against the top-5 CTE (re-aggregating the CAGG through a
// grouped CTE measured 19.8s on sdex 90d vs 0.87s direct; CAGG rows are
// already unique per (source, pair, bucket) — materialized_only=true).
func TestDexTopPairsSeriesQueryShape(t *testing.T) {
	daily := dexTopPairsSeriesQuery(90)
	if !strings.Contains(daily, "LIMIT 5") {
		t.Error("top-pairs series must cap at 5 pairs")
	}
	if !strings.Contains(daily, "FROM dex_volume_by_pair_1d t JOIN top") {
		t.Error("90d top-pairs series must join the CAGG directly (the grouped-CTE form measured 19.8s on sdex 90d)")
	}
	hourly := dexTopPairsSeriesQuery(1)
	if !strings.Contains(hourly, "date_trunc('hour', ts)") || !strings.Contains(hourly, "HH24:00") {
		t.Error("24h top-pairs series must bucket raw trades hourly")
	}
	if !strings.Contains(hourly, "LIMIT 5") {
		t.Error("24h top-pairs series must cap at 5 pairs")
	}
	for _, q := range []string{daily, hourly} {
		assertWindowBounded(t, "top-pairs series", q)
		assertDEXNumericSafe(t, "top-pairs series", q)
	}
}

// TestDexLargestTradesQueryShape — top-10 by usd_volume, priced rows only,
// window-bounded, per-trade NUMERIC amounts as text.
func TestDexLargestTradesQueryShape(t *testing.T) {
	q := dexLargestTradesQuery()
	if !strings.Contains(q, "ORDER BY usd_volume DESC LIMIT 10") {
		t.Error("largest trades must be the top 10 by usd_volume")
	}
	if !strings.Contains(q, "usd_volume IS NOT NULL") {
		t.Error("largest trades must exclude unpriced rows (they cannot rank by USD)")
	}
	if !strings.Contains(q, "base_amount::text") || !strings.Contains(q, "quote_amount::text") {
		t.Error("largest trades must serve amounts as NUMERIC text (ADR-0003)")
	}
	assertWindowBounded(t, "largest trades", q)
	assertDEXNumericSafe(t, "largest trades", q)
}

// TestDexSinceTotalsQueryShape — deliberately unwindowed over the
// materialized-only CAGG, and it must surface the floor date so the KPI
// label can be "since <date>", never "all-time" (the rollup floor is
// 2026-03-18 on r1 while raw sdex history reaches 2018).
func TestDexSinceTotalsQueryShape(t *testing.T) {
	q := dexSinceTotalsQuery()
	if strings.Contains(q, "$2") || strings.Contains(q, "interval") {
		t.Error("since-totals is the lifetime-of-the-rollup query and must not be window-bounded")
	}
	if !strings.Contains(q, "min(bucket)::date") {
		t.Error("since-totals must return the rollup floor date for the honest 'since' label")
	}
	if !strings.Contains(q, "dex_volume_by_pair_1d") {
		t.Error("since-totals must read the CAGG, never raw trades (300M+ rows unbounded)")
	}
	assertDEXNumericSafe(t, "since totals", q)
}

// TestDexAssetLabels — honest labelling: native + verified SACs resolve to
// tickers (XLM SAC + USDC SAC verified against their live r1 pair usage),
// unverified contracts truncate rather than guess, classic ids show their
// code.
func TestDexAssetLabels(t *testing.T) {
	for asset, want := range map[string]string{
		"native": "XLM",
		// The native-XLM SAC (derived, and the dominant aquarius quote leg
		// on r1).
		"CAS3J7GYLGXMF6TDJBBYYSE3HQ6BBSMLNUQ34T6TZMYMW2EVH34XOWMA": "XLM",
		// Circle USDC's SAC (derived from the catalogue's USDC issuance).
		"CCW67TSZV3SSS2HXMBQ5JFGCKJNXKZM7UQUWUZPUTHXSTZLEO7SJMI75": "USDC",
		// Classic canonical id → code.
		"USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN": "USDC",
		// Unverified token contract → truncated id, never a guessed symbol.
		"CBZ4DCE7PYMUTOAKKUTRSUPT3FJFVOWCSKWUM5A72D6SAVMUJE5JN2PJ": "CBZ4…N2PJ",
	} {
		if got := dexAssetLabel(asset); got != want {
			t.Errorf("dexAssetLabel(%s) = %q, want %q", asset, got, want)
		}
	}
	if got := dexPairLabel("native", "CCW67TSZV3SSS2HXMBQ5JFGCKJNXKZM7UQUWUZPUTHXSTZLEO7SJMI75"); got != "XLM/USDC" {
		t.Errorf("dexPairLabel = %q, want XLM/USDC", got)
	}
}
