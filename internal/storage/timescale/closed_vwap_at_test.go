// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package timescale

import (
	"fmt"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"
)

// TestPriceAtResolutionLadder pins the CAGG ladder used by ClosedVWAPAtOrBefore: recent instants
// start at prices_1m, week-old at prices_1h, year-old at prices_1d. Each ladder is ordered
// finest to coarsest so the first in-window hit is the finest resolution holding the data.
func TestPriceAtResolutionLadder(t *testing.T) {
	cases := []struct {
		name string
		age  time.Duration
		want []HistoryGranularity
	}{
		{"now", 0, []HistoryGranularity{Granularity1m, Granularity15m, Granularity1h, Granularity4h, Granularity1d}},
		{"1h-ago", time.Hour, []HistoryGranularity{Granularity1m, Granularity15m, Granularity1h, Granularity4h, Granularity1d}},
		{"36h-ago", 36 * time.Hour, []HistoryGranularity{Granularity1m, Granularity15m, Granularity1h, Granularity4h, Granularity1d}},
		{"7d-ago", 7 * 24 * time.Hour, []HistoryGranularity{Granularity1h, Granularity4h, Granularity1d}},
		{"30d-ago", 30 * 24 * time.Hour, []HistoryGranularity{Granularity1h, Granularity4h, Granularity1d}},
		{"1y-ago", 365 * 24 * time.Hour, []HistoryGranularity{Granularity1d}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := priceAtResolutionLadder(tc.age)
			if fmt.Sprint(got) != fmt.Sprint(tc.want) {
				t.Fatalf("priceAtResolutionLadder(%v) = %v, want %v", tc.age, got, tc.want)
			}
			// Ladder MUST be strictly finest→coarsest (monotone
			// non-decreasing bucket width) so the first in-window hit
			// is the finest available resolution.
			for i := 1; i < len(got); i++ {
				if got[i].BucketDuration() <= got[i-1].BucketDuration() {
					t.Fatalf("ladder not finest-first: %v", got)
				}
			}
		})
	}
}

// TestHistoryGranularityBucketDuration pins the wall-clock width used
// for staleness math + window_seconds labeling.
func TestHistoryGranularityBucketDuration(t *testing.T) {
	cases := []struct {
		g        HistoryGranularity
		wantDur  time.Duration
		wantSecs int
	}{
		{Granularity1m, time.Minute, 60},
		{Granularity15m, 15 * time.Minute, 900},
		{Granularity1h, time.Hour, 3600},
		{Granularity4h, 4 * time.Hour, 14400},
		{Granularity1d, 24 * time.Hour, 86400},
		{Granularity1w, 7 * 24 * time.Hour, 604800},
		{Granularity1mo, 30 * 24 * time.Hour, 2592000},
	}
	for _, tc := range cases {
		if got := tc.g.BucketDuration(); got != tc.wantDur {
			t.Errorf("%s.BucketDuration() = %v, want %v", tc.g, got, tc.wantDur)
		}
		if got := tc.g.Seconds(); got != tc.wantSecs {
			t.Errorf("%s.Seconds() = %d, want %d", tc.g, got, tc.wantSecs)
		}
	}
}

// TestClosedVWAPAtOrBeforeQueryShape guards the sargability and both-directions invariants of the
// point-in-time query without a database: the non-sargable `bucket + INTERVAL <= …` form or a
// dropped stored direction would silently degrade the endpoint.
// nonSargableClosedGuard matches the `bucket + INTERVAL '…' <= now()` spelling of the
// closed-bucket guard. Arithmetic on the column defeats the (…, bucket DESC) index.
var nonSargableClosedGuard = regexp.MustCompile(`(?i)\+\s*INTERVAL\s*'[^']*'\s*<=?\s*now\(\)`)

// TestClosedBucketGuardSpelling scans every non-test source in the package,
// inline `const q` SQL included, so the guard has one spelling
// (`bucket <= now() - INTERVAL '…'`) everywhere, not only in the hoisted
// templates the shape tests below assert on.
func TestClosedBucketGuardSpelling(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	scanned := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		scanned++
		for i, line := range strings.Split(string(src), "\n") {
			if nonSargableClosedGuard.MatchString(line) {
				t.Errorf("%s:%d: non-sargable closed-bucket guard %q; spell it `bucket <= now() - INTERVAL '…'`",
					name, i+1, strings.TrimSpace(line))
			}
		}
	}
	if scanned == 0 {
		t.Fatal("scanned no package sources — the guard would be vacuous")
	}
}

// assertLiteralCount fails t unless literal appears in q exactly want times, catching a slot
// dropped from one UNION branch while the others still carry it (a plain strings.Contains
// stays green).
//
// want MUST be hardcoded, never derived from strings.Count(tmpl, placeholder) on the template
// being rendered: deleting a "%[1]s" shrinks both counts in lockstep, so they would always agree.
func assertLiteralCount(t *testing.T, q, literal string, want int) {
	t.Helper()
	got := strings.Count(q, literal)
	if got != want {
		t.Errorf("literal %q appears %d time(s) in the rendered query, want %d", literal, got, want)
	}
}

func TestClosedVWAPAtOrBeforeQueryShape(t *testing.T) {
	q := fmt.Sprintf(closedVWAPAtOrBeforeQueryTemplate,
		"prices_1m", "2024-01-01 00:00:00+00", "2023-12-01 00:00:00+00")

	// Sargable closed-bucket guard: the interval lives on the RHS as a
	// literal bound, NEVER as a function on the indexed bucket column.
	if !strings.Contains(q, "bucket <= TIMESTAMPTZ") {
		t.Error("query missing sargable `bucket <= TIMESTAMPTZ` upper bound")
	}
	if strings.Contains(q, "bucket + INTERVAL") {
		t.Error("query uses non-sargable `bucket + INTERVAL` form (function on indexed column)")
	}
	// The upper bound (%[2]s) sits in both `latest` max(bucket) branches (2 occurrences); the
	// lower bound (%[3]s) sits in those same two plus both `r` point-read branches (4), because
	// every branch prunes chunks on its own bound.
	assertLiteralCount(t, q, "2024-01-01 00:00:00+00", 2)
	assertLiteralCount(t, q, "2023-12-01 00:00:00+00", 4)
	// Both stored directions of the market are read.
	if !strings.Contains(q, "base_asset = $1 AND quote_asset = $2") ||
		!strings.Contains(q, "base_asset = $2 AND quote_asset = $1") {
		t.Error("query does not combine both stored directions of the pair")
	}
	// The query hands each direction's raw (base_asset, vwap,
	// volume_priced) to Go: the direction fold is [combineDirVWAP]'s exact
	// volume-weighted union, not SQL. volume_priced is vwap's own
	// denominator; `volume` also counts zero-leg trades.
	if !strings.Contains(q, "COALESCE(volume_priced, 0)::text AS volume_priced") {
		t.Error("query does not select each direction's volume (needed for the volume-weighted union)")
	}
	// The SQL must NOT weight or invert the directions itself: `1.0 / vwap` rounds the flipped
	// leg to the division's NUMERIC scale before weighting, and a trade-count-weighted mean of
	// {vwap, 1/vwap} is not the union VWAP.
	if strings.Contains(q, "1.0 / NULLIF(vwap, 0)") {
		t.Error("query still inverts the flipped leg in SQL; the exact combine belongs to combineDirVWAP")
	}
	if strings.Contains(q, "* tc") || strings.Contains(q, "SUM(tc)") {
		t.Error("query still weights the two directions by trade count (not a VWAP)")
	}
}

// TestRecentClosedVWAP1mExistsQueryShape guards the recent-existence gate that
// LatestClosedVWAP1mForPair runs before its value walk. The gate keeps an EMPTY pair
// (native/fiat:USD, read as an alias on every XLM query) off the full ~400-day walk, so it MUST
// stay sargable, literal-cutoff-bounded, both-directions and LIMIT 1, or the cold-start timeout
// returns.
func TestRecentClosedVWAP1mExistsQueryShape(t *testing.T) {
	lower := "AND bucket >= TIMESTAMPTZ '2026-06-01 00:00:00+00'\n"
	q := fmt.Sprintf(recentClosedVWAP1mExistsTemplate, lower)

	// Sargable closed-bucket guard: the interval is a constant on the RHS,
	// NEVER a function on the indexed bucket column (the
	// latency-burn shape).
	if !strings.Contains(q, "bucket <= now() - INTERVAL '1 minute'") {
		t.Error("gate missing sargable closed-bucket guard `bucket <= now() - INTERVAL '1 minute'`")
	}
	if strings.Contains(q, "bucket + INTERVAL") {
		t.Error("gate uses non-sargable `bucket + INTERVAL` form (function on indexed column)")
	}
	// Literal lower bound → PLAN-time chunk pruning: the whole point of the
	// gate is that a truly-empty pair proves emptiness over recent chunks
	// only, not the value walk's ~400-day span. Both UNION ALL branches
	// (one per stored direction) must carry it independently.
	assertLiteralCount(t, q, "bucket >= TIMESTAMPTZ '2026-06-01 00:00:00+00'", 2)
	// Both stored directions — else a flipped-only live pair (USDC/XLM with
	// no XLM/USDC rows) would be wrongly gated to ErrNoRows.
	if !strings.Contains(q, "base_asset = $1 AND quote_asset = $2") ||
		!strings.Contains(q, "base_asset = $2 AND quote_asset = $1") {
		t.Error("gate does not probe both stored directions of the pair")
	}
	// LIMIT 1 → a populated pair short-circuits at the first matching row.
	if !strings.Contains(q, "LIMIT 1") {
		t.Error("gate missing LIMIT 1 short-circuit")
	}
	// The gate is the freshness horizon and MUST be tighter than the value
	// walk's window, else it prunes nothing and the empty walk returns.
	if latestVWAPGateWindow <= 0 || latestVWAPGateWindow >= latestVWAPWindow {
		t.Errorf("latestVWAPGateWindow (%v) must be positive and < latestVWAPWindow (%v)",
			latestVWAPGateWindow, latestVWAPWindow)
	}
}

// TestRecentClosedVWAP1mCombinedQueryShape guards the trailing-baseline query behind the
// /v1/price serving-sanity guard. It MUST stay sargable (closed-bucket guard as a constant on the
// RHS), literal-cutoff-bounded (plan-time chunk pruning) and both-directions (a flipped-only pair
// still contributes), and it MUST hand raw per-direction rows to the same Go combine the served
// candidate uses, or the baseline drifts off the served value and false-positives.
func TestRecentClosedVWAP1mCombinedQueryShape(t *testing.T) {
	lower := "AND bucket >= TIMESTAMPTZ '2026-06-01 00:00:00+00'\n"
	q := fmt.Sprintf(recentClosedVWAP1mCombinedTemplate, lower)

	if !strings.Contains(q, "bucket <= now() - INTERVAL '1 minute'") {
		t.Error("combined query missing sargable closed-bucket guard")
	}
	if strings.Contains(q, "bucket + INTERVAL") {
		t.Error("combined query uses non-sargable `bucket + INTERVAL` form")
	}
	// %[1]s is injected into BOTH UNION ALL branches independently — a
	// slot dropped from one branch still renders valid SQL and a plain
	// strings.Contains here would stay green through that regression.
	assertLiteralCount(t, q, "bucket >= TIMESTAMPTZ '2026-06-01 00:00:00+00'", 2)
	if !strings.Contains(q, "base_asset = $1 AND quote_asset = $2") ||
		!strings.Contains(q, "base_asset = $2 AND quote_asset = $1") {
		t.Error("combined query does not read both stored directions")
	}
	// Raw per-direction rows: base_asset tells Go which rows are flipped
	// and volume_priced is the weight [combineDirVWAP] folds them with. The
	// baseline and the served candidate MUST run the identical combine.
	if !strings.Contains(q, "COALESCE(volume_priced, 0)::text") {
		t.Error("combined query does not select each direction's volume (needed for the volume-weighted union)")
	}
	if !strings.Contains(q, "SELECT bucket, base_asset, vwap::text") {
		t.Error("combined query does not return the raw per-direction rows the Go combine needs")
	}
	// The SQL must NOT fold the directions itself. Inverting the
	// flipped leg (`1.0 / vwap`, rounding it before it is weighted) and
	// weighting by trade COUNT is not a VWAP, and would diverge from the
	// served candidate on every two-sided market.
	if strings.Contains(q, "1.0 / NULLIF(vwap, 0)") {
		t.Error("combined query still inverts the flipped leg in SQL; the exact combine belongs to combineDirVWAP")
	}
	if strings.Contains(q, "* COALESCE(trade_count, 0))") {
		t.Error("combined query still weights the two directions by trade count (not a VWAP)")
	}
	// Newest-first so trailing[0] is the most recent closed bucket.
	if !strings.Contains(q, "ORDER BY bucket DESC") {
		t.Error("combined query must return newest-first")
	}
}

// TestLatestClosedVWAP1mQueryShape guards latestClosedVWAP1mTemplate, the value-walk query of
// LatestClosedVWAP1mForPair. Its four %[1]s slots (both UNION ALL branches of the `latest`
// max(bucket) CTE and of the `r` point-read CTE) each carry the literal lower bound
// independently; a slot dropped from any one still renders valid SQL.
func TestLatestClosedVWAP1mQueryShape(t *testing.T) {
	lower := "AND bucket >= TIMESTAMPTZ '2026-06-01 00:00:00+00'\n"
	q := fmt.Sprintf(latestClosedVWAP1mTemplate, lower)

	if !strings.Contains(q, "bucket <= now() - INTERVAL '1 minute'") {
		t.Error("query missing sargable closed-bucket guard `bucket <= now() - INTERVAL '1 minute'`")
	}
	if strings.Contains(q, "bucket + INTERVAL") {
		t.Error("query uses non-sargable `bucket + INTERVAL` form (function on indexed column)")
	}
	if !strings.Contains(q, "base_asset = $1 AND quote_asset = $2") ||
		!strings.Contains(q, "base_asset = $2 AND quote_asset = $1") {
		t.Error("query does not read both stored directions of the pair")
	}
	// Raw per-direction rows for Go's [combineDirVWAP] — same discipline
	// as recentClosedVWAP1mCombinedTemplate.
	if !strings.Contains(q, "COALESCE(volume_priced, 0)::text AS volume_priced") {
		t.Error("query does not select each direction's volume (needed for the volume-weighted union)")
	}
	if strings.Contains(q, "1.0 / NULLIF(vwap, 0)") {
		t.Error("query still inverts the flipped leg in SQL; the exact combine belongs to combineDirVWAP")
	}
	if strings.Contains(q, "* tc") || strings.Contains(q, "SUM(tc)") {
		t.Error("query still weights the two directions by trade count (not a VWAP)")
	}
	// The literal lower bound must prune chunks on EVERY %[1]s slot
	// independently: both branches of the `latest` max(bucket) UNION ALL
	// AND both branches of the `r` point-read UNION ALL (4 total).
	assertLiteralCount(t, q, "bucket >= TIMESTAMPTZ '2026-06-01 00:00:00+00'", 4)
}

// TestRecentClosedVWAP1mForPairQueryShape extends the sargability guard to the SEP-40 prices()
// passthrough read. A `bucket + INTERVAL '1 minute' <= now()` form is a function on the indexed
// column, so the planner can neither use the bucket index nor prune chunks at plan time.
// The query must stay a package-level template: the guards assert over those, and an inline
// `const q` is invisible to them.
func TestRecentClosedVWAP1mForPairQueryShape(t *testing.T) {
	q := recentClosedVWAP1mForPairQuery

	if !strings.Contains(q, "bucket <= now() - INTERVAL") {
		t.Error("query missing the sargable `bucket <= now() - INTERVAL` upper bound")
	}
	if strings.Contains(q, "bucket + INTERVAL") {
		t.Error("query uses the non-sargable `bucket + INTERVAL` form " +
			"(function on the indexed column) — the planner cannot use the " +
			"bucket index or prune chunks at plan time")
	}
	// Newest-first with a row cap: this is the closed-bucket SERIES read,
	// and reversing the order or dropping the LIMIT would walk the whole
	// hypertable for a pair.
	if !strings.Contains(q, "ORDER BY bucket DESC") {
		t.Error("query must return newest buckets first")
	}
	if !strings.Contains(q, "LIMIT $3") {
		t.Error("query must cap the row walk")
	}
}

// TestDailyMarketDaysQueryShape extends the sargability guard to DailyMarketDays' daily rollup
// read. A `bucket + INTERVAL '1 hour' <= now()` form is a function on the indexed column, so the
// planner can neither use the bucket index nor prune chunks ahead of the day/hour GROUP BY scan.
// The query must stay the package-level [dailyMarketDaysQuery]; an inline `const q` is invisible
// to these guards.
func TestDailyMarketDaysQueryShape(t *testing.T) {
	q := dailyMarketDaysQuery

	if !strings.Contains(q, "bucket <= now() - INTERVAL '1 hour'") {
		t.Error("query missing the sargable `bucket <= now() - INTERVAL '1 hour'` closed-bucket guard")
	}
	if strings.Contains(q, "bucket + INTERVAL") {
		t.Error("query uses the non-sargable `bucket + INTERVAL` form " +
			"(function on the indexed column) — the planner cannot use the " +
			"bucket index or prune chunks at plan time")
	}
}

// TestDailyMarketDaysQueryReadsWholeLastDay pins the upper bound to the END of the `to` day.
// prices_1h holds hour buckets, so `bucket <= $4` with `to` at a day start would read the last
// day from its 00:00 hour alone and never clear the two-hour floor. The bound stays on the
// parameter, so it stays sargable.
func TestDailyMarketDaysQueryReadsWholeLastDay(t *testing.T) {
	q := dailyMarketDaysQuery

	if strings.Contains(q, "bucket <= $4") {
		t.Error("query bounds hour buckets by `bucket <= $4`: a day-start `to` admits only that day's 00:00 hour")
	}
	if !strings.Contains(q, "bucket <  $4::timestamptz + INTERVAL '1 day'") {
		t.Error("query missing the exclusive end-of-day upper bound `bucket < $4::timestamptz + INTERVAL '1 day'`")
	}
}

var (
	closedVWAPUpperLiteral = regexp.MustCompile(`bucket <= TIMESTAMPTZ '([^']+)'`)
	closedVWAPLowerLiteral = regexp.MustCompile(`bucket >= TIMESTAMPTZ '([^']+)'`)
)

// closedVWAPAtOrBeforeBounds reads the upper and lower bucket literals
// out of a statement [Store.ClosedVWAPAtOrBefore] actually issued. Every
// arm repeats them, and every repetition must carry the same instant.
func closedVWAPAtOrBeforeBounds(t *testing.T, sql string) (upper, lower time.Time) {
	t.Helper()
	return singleTimestamptzLiteral(t, sql, closedVWAPUpperLiteral, "upper"),
		singleTimestamptzLiteral(t, sql, closedVWAPLowerLiteral, "lower")
}

func singleTimestamptzLiteral(t *testing.T, sql string, re *regexp.Regexp, name string) time.Time {
	t.Helper()
	ms := re.FindAllStringSubmatch(sql, -1)
	if len(ms) == 0 {
		t.Fatalf("statement has no literal %s bound (%s):\n%s", name, re, sql)
	}
	for _, m := range ms[1:] {
		if m[1] != ms[0][1] {
			t.Fatalf("%s bound differs between arms: %q vs %q:\n%s", name, ms[0][1], m[1], sql)
		}
	}
	got, err := time.Parse("2006-01-02 15:04:05-07", ms[0][1])
	if err != nil {
		t.Fatalf("%s bound %q does not parse as the package's timestamptz layout: %v", name, ms[0][1], err)
	}
	return got
}
