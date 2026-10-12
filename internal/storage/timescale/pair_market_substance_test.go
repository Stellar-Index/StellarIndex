// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package timescale

import (
	"context"
	"database/sql/driver"
	"errors"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// [Store.PairMarketSubstance] is the SQL half of the thin-market gate. These tests catch two mutations:
// dropping the closed-bucket bound (an in-progress bucket opens the gate) and dropping a direction arm (half
// the market). The query is built inline, so the scripted driver records the SQL and args.

// testXLMUSDCLegs is the spelling sets the substance gate asks about for
// XLM/USDC: every alias of each leg.
func testXLMUSDCLegs(t *testing.T) (bases, quotes []canonical.Asset) {
	t.Helper()
	pair := testXLMUSDCPair(t)
	return canonical.AssetAliases(pair.Base), canonical.AssetAliases(pair.Quote)
}

func assetKeys(set []canonical.Asset) []string {
	out := make([]string, len(set))
	for i, a := range set {
		out[i] = a.String()
	}
	return out
}

// substanceQuery runs PairMarketSubstance against one canned row and
// returns the statement the store issued plus the decoded result.
func substanceQuery(t *testing.T, window time.Duration, row []driver.Value) (recordedStmt, MarketSubstance) {
	t.Helper()
	store, conn := newScriptedStore(t, scriptedResult{
		cols: []string{"volume_usd", "buckets", "span_seconds", "valued_buckets"},
		rows: [][]driver.Value{row},
	})
	bases, quotes := testXLMUSDCLegs(t)
	sub, err := store.PairMarketSubstance(context.Background(), bases, quotes, window)
	if err != nil {
		t.Fatalf("PairMarketSubstance: %v", err)
	}
	return conn.only(t), sub
}

// TestPairMarketSubstance_ClosedBucketPredicate pins the ADR-0015 closed-bucket guard in its
// sargable form: `bucket <= now() - INTERVAL '1 minute'` and nothing else. The `bucket +
// INTERVAL '1 minute' <= now()` spelling puts a function on the indexed column.
func TestPairMarketSubstance_ClosedBucketPredicate(t *testing.T) {
	stmt, _ := substanceQuery(t, time.Hour, []driver.Value{"0", int64(0), int64(0), int64(0)})

	if !strings.Contains(stmt.sql, "bucket <= now() - INTERVAL '1 minute'") {
		t.Errorf(`substance query is missing the closed-bucket predicate
`+"`AND bucket <= now() - INTERVAL '1 minute'`"+`.

Without it the in-progress bucket counts toward the thin-market gate, so
a pair clears the volume/bucket floor on trades ADR-0015 forbids serving
— the gate opens on activity no price read may return.

SQL:
%s`, indent(stmt.sql))
	}
	if strings.Contains(stmt.sql, "bucket + INTERVAL") {
		t.Error("substance query uses the non-sargable `bucket + INTERVAL` form " +
			"(function on the indexed column): the planner can neither use the " +
			"bucket index nor prune chunks at plan time")
	}
}

// TestPairMarketSubstance_ReadsBothStoredDirections pins both arms of the orientation filter: the CAGGs hold
// a market as (A,B) and (B,A), and substance is a COUNT and SUM, so a one-armed read understates two-sided
// markets.
func TestPairMarketSubstance_ReadsBothStoredDirections(t *testing.T) {
	stmt, _ := substanceQuery(t, time.Hour, []driver.Value{"0", int64(0), int64(0), int64(0)})

	norm := regexp.MustCompile(`\s+`).ReplaceAllString(stmt.sql, " ")
	for _, arm := range []string{
		"base_asset = ANY($1) AND quote_asset = ANY($2)",
		"base_asset = ANY($2) AND quote_asset = ANY($1)",
	} {
		if !strings.Contains(norm, arm) {
			t.Errorf(`substance query does not read the %s direction.

The CAGG stores this market in BOTH orientations, so measuring one arm
measures half the market: a two-sided pair is withheld as thin, and a
pair that traded only the other way reads as no market at all.

SQL:
%s`, arm, indent(stmt.sql))
		}
	}
	if !strings.Contains(norm, "quote_asset = ANY($2) AND bucket <= now() - INTERVAL '1 minute'") ||
		!strings.Contains(norm, "quote_asset = ANY($1) AND bucket <= now() - INTERVAL '1 minute'") {
		t.Errorf("each direction arm must carry its own closed-bucket bound:\n%s", indent(stmt.sql))
	}
	if !strings.Contains(norm, "UNION ALL") || orientationDisjunction.MatchString(stmt.sql) {
		t.Errorf("the two direction arms must be UNION ALL'd, not OR'd:\n%s", indent(stmt.sql))
	}
	if !strings.Contains(norm, "AND NOT (base_asset = ANY($1) AND quote_asset = ANY($2))") {
		t.Errorf("the reverse arm must exclude rows the forward arm already selected, "+
			"or a spelling shared by both sets counts its minutes twice:\n%s", indent(stmt.sql))
	}
}

// TestPairMarketSubstance_BindsPairAndLiteralLowerBound pins the split the method's #nosec note depends on:
// the pair binds as $1/$2 and the only interpolated value is the window lower bound, a literal for chunk
// pruning.
func TestPairMarketSubstance_BindsPairAndLiteralLowerBound(t *testing.T) {
	pair := testXLMUSDCPair(t)
	bases, quotes := testXLMUSDCLegs(t)
	before := time.Now().UTC()
	stmt, _ := substanceQuery(t, 6*time.Hour, []driver.Value{"0", int64(0), int64(0), int64(0)})
	after := time.Now().UTC()

	if len(stmt.args) != 2 {
		t.Fatalf("bound %d args, want exactly 2 (bases, quotes): %v", len(stmt.args), stmt.args)
	}
	if got := stmt.arg(t, 1); !reflect.DeepEqual(got, assetKeys(bases)) {
		t.Errorf("$1 = %v, want every base spelling %v", got, assetKeys(bases))
	}
	if got := stmt.arg(t, 2); !reflect.DeepEqual(got, assetKeys(quotes)) {
		t.Errorf("$2 = %v, want every quote spelling %v", got, assetKeys(quotes))
	}
	// The asset ids must never reach the SQL text; that is the whole
	// reason the interpolation is confined to the timestamp.
	if strings.Contains(stmt.sql, pair.Quote.String()) {
		t.Error("the quote asset id was interpolated into the SQL text instead of bound")
	}

	m := regexp.MustCompile(`bucket >= TIMESTAMPTZ '([^']+)'`).FindStringSubmatch(stmt.sql)
	if m == nil {
		t.Fatalf(`substance query has no literal `+"`bucket >= TIMESTAMPTZ '…'`"+` lower
bound, so the read is unbounded below and cannot be chunk-pruned at plan
time — a full trailing scan of prices_1m per gated pair.

SQL:
%s`, indent(stmt.sql))
	}
	lower, err := time.Parse("2006-01-02 15:04:05-07", m[1])
	if err != nil {
		t.Fatalf("lower bound %q does not parse as the package's timestamptz layout: %v", m[1], err)
	}
	// window=6h: the bound must sit 6h back, not 0 and not some other
	// unit. One second of slack for the clock read inside the method.
	wantMin, wantMax := before.Add(-6*time.Hour-time.Second), after.Add(-6*time.Hour+time.Second)
	if lower.Before(wantMin) || lower.After(wantMax) {
		t.Errorf("lower bound = %s, want now-6h (in [%s, %s]) — the window "+
			"argument is not being applied as a duration", lower, wantMin, wantMax)
	}
}

// TestPairMarketSubstance_GroupsByBucket pins DISTINCT-bucket semantics: Buckets counts closed MINUTES
// with a trade, not CAGG rows, else a two-sided minute reports 2 and a one-minute dust burst alone clears
// a MinBuckets=2 floor.
func TestPairMarketSubstance_GroupsByBucket(t *testing.T) {
	stmt, _ := substanceQuery(t, time.Hour, []driver.Value{"0", int64(0), int64(0), int64(0)})

	norm := regexp.MustCompile(`\s+`).ReplaceAllString(stmt.sql, " ")
	if !strings.Contains(norm, "GROUP BY bucket") {
		t.Errorf("substance query does not GROUP BY bucket, so count(*) counts "+
			"per-direction ROWS not distinct closed minutes:\n%s", indent(stmt.sql))
	}
	if !strings.Contains(norm, "sum(volume_usd) AS bucket_usd") {
		t.Errorf("substance query does not sum volume_usd per bucket:\n%s", indent(stmt.sql))
	}
	if !strings.Contains(norm, "EXTRACT(EPOCH FROM (max(bucket) - min(bucket)))") {
		t.Errorf("substance query does not derive the span from max(bucket)-min(bucket):\n%s",
			indent(stmt.sql))
	}
}

// TestPairMarketSubstance_VolumeIsAnExactDecimalString is the ADR-0003 half: VolumeUSD crosses
// the seam as the NUMERIC's own text and pricingguard compares it as a big.Rat. A float64 hop
// would round in the 17th significant digit, enough to move a pair across a floor.
func TestPairMarketSubstance_VolumeIsAnExactDecimalString(t *testing.T) {
	const exact = "123456789012345678.123456789"
	_, sub := substanceQuery(t, time.Hour, []driver.Value{exact, int64(7), int64(1860), int64(5)})

	if sub.VolumeUSD != exact {
		t.Errorf("VolumeUSD = %s, want the NUMERIC verbatim %s (ADR-0003: no float64 hop)",
			sub.VolumeUSD, exact)
	}
	if sub.Buckets != 7 {
		t.Errorf("Buckets = %d, want 7", sub.Buckets)
	}
	if sub.SpanSeconds != 1860 {
		t.Errorf("SpanSeconds = %d, want 1860", sub.SpanSeconds)
	}
	if sub.ValuedBuckets != 5 {
		t.Errorf("ValuedBuckets = %d, want 5", sub.ValuedBuckets)
	}
}

// TestPairMarketSubstance_CountsUSDValuedBuckets pins the fourth column, the per-bucket valued count the gate
// uses to tell an unvaluable market from a thin one; it must be computed over the bucket-grouped USD sum, not
// raw rows.
func TestPairMarketSubstance_CountsUSDValuedBuckets(t *testing.T) {
	stmt, _ := substanceQuery(t, time.Hour, []driver.Value{"0", int64(0), int64(0), int64(0)})
	if !strings.Contains(stmt.sql, "count(*) FILTER (WHERE bucket_usd > 0)") {
		t.Errorf("PairMarketSubstance does not count USD-valued buckets:\n%s", stmt.sql)
	}
}

// TestPairMarketSubstance_EmptyPairIsZeroNotError pins that an empty pair yields zero, not an error, so the
// gate can tell "no substance" from "measurement failed" and a DB blip does not look like a thin market.
func TestPairMarketSubstance_EmptyPairIsZeroNotError(t *testing.T) {
	_, sub := substanceQuery(t, time.Hour, []driver.Value{"0", int64(0), int64(0), int64(0)})

	if sub != (MarketSubstance{VolumeUSD: "0"}) {
		t.Errorf("empty pair = %+v, want {VolumeUSD:0 Buckets:0 SpanSeconds:0}", sub)
	}
}

// TestPairMarketSubstance_NonPositiveWindowErrorsBeforeQuerying: a zero window would render `bucket >= now()`
// and read as a thin market for every pair, so it must be rejected without issuing SQL.
func TestPairMarketSubstance_NonPositiveWindowErrorsBeforeQuerying(t *testing.T) {
	for _, w := range []time.Duration{0, -time.Minute} {
		store, conn := newScriptedStore(t)
		bases, quotes := testXLMUSDCLegs(t)
		_, err := store.PairMarketSubstance(context.Background(), bases, quotes, w)
		if err == nil {
			t.Fatalf("window=%v returned no error; a non-positive window measures "+
				"an empty interval and would withhold every price", w)
		}
		if !strings.Contains(err.Error(), "non-positive window") {
			t.Errorf("window=%v error = %v, want it to name the non-positive window", w, err)
		}
		if len(conn.stmts) != 0 {
			t.Errorf("window=%v issued %d statements, want 0", w, len(conn.stmts))
		}
	}
}

// TestPairMarketSubstance_QueryErrorIsWrapped — the gate logs and
// fails-closed on error, so the error has to arrive as an error (and name
// the method) rather than as a zero MarketSubstance.
func TestPairMarketSubstance_QueryErrorIsWrapped(t *testing.T) {
	boom := errors.New("connection reset")
	store, _ := newScriptedStore(t, scriptedResult{err: boom})

	bases, quotes := testXLMUSDCLegs(t)
	sub, err := store.PairMarketSubstance(context.Background(), bases, quotes, time.Hour)
	if err == nil {
		t.Fatal("a failed substance query returned no error; the gate cannot " +
			"distinguish a broken measurement from a thin market")
	}
	if !errors.Is(err, boom) {
		t.Errorf("error = %v, does not wrap the driver error", err)
	}
	if !strings.Contains(err.Error(), "PairMarketSubstance") {
		t.Errorf("error = %v, want it to name PairMarketSubstance", err)
	}
	if sub != (MarketSubstance{}) {
		t.Errorf("error path returned %+v, want the zero MarketSubstance", sub)
	}
}

// TestPairMarketSubstance_EmptySpellingSetErrorsBeforeQuerying — an empty
// set selects no rows and would read as "no market", withholding the pair
// on a caller's mistake. It is rejected up front instead.
func TestPairMarketSubstance_EmptySpellingSetErrorsBeforeQuerying(t *testing.T) {
	bases, quotes := testXLMUSDCLegs(t)
	for _, tc := range []struct{ bases, quotes []canonical.Asset }{{nil, quotes}, {bases, nil}} {
		store, conn := newScriptedStore(t)
		if _, err := store.PairMarketSubstance(context.Background(), tc.bases, tc.quotes, time.Hour); err == nil {
			t.Errorf("bases=%d quotes=%d: want an error, got nil", len(tc.bases), len(tc.quotes))
		}
		if _, err := store.PairMarketSubstanceAt(context.Background(), tc.bases, tc.quotes,
			time.Now(), time.Hour, Granularity1h); err == nil {
			t.Errorf("At: bases=%d quotes=%d: want an error, got nil", len(tc.bases), len(tc.quotes))
		}
		if len(conn.stmts) != 0 {
			t.Errorf("issued %d statements for an empty spelling set, want 0", len(conn.stmts))
		}
	}
}

// [Store.PairMarketSubstanceAt] is the SQL half of the point-in-time thin-market verdict, the substance in
// the window ending at a requested instant (its sibling always ends at now), so these tests pin WHERE the
// window sits. Executing proof against real Postgres: test/integration/pg_pricing_markets_test.go.

func substanceAtQuery(t *testing.T, asOf time.Time, window time.Duration, g HistoryGranularity) recordedStmt {
	t.Helper()
	store, conn := newScriptedStore(t, scriptedResult{
		cols: []string{"volume_usd", "buckets", "span_seconds", "valued_buckets"},
		rows: [][]driver.Value{{"0", int64(0), int64(0), int64(0)}},
	})
	bases, quotes := testXLMUSDCLegs(t)
	if _, err := store.PairMarketSubstanceAt(context.Background(), bases, quotes, asOf, window, g); err != nil {
		t.Fatalf("PairMarketSubstanceAt: %v", err)
	}
	return conn.only(t)
}

func literalBound(t *testing.T, sql, op string) time.Time {
	t.Helper()
	m := regexp.MustCompile(`bucket ` + regexp.QuoteMeta(op) + ` TIMESTAMPTZ '([^']+)'`).FindStringSubmatch(sql)
	if m == nil {
		t.Fatalf("no literal `bucket %s TIMESTAMPTZ '…'` bound in:\n%s", op, indent(sql))
	}
	ts, err := time.Parse("2006-01-02 15:04:05-07", m[1])
	if err != nil {
		t.Fatalf("bound %q does not parse: %v", m[1], err)
	}
	return ts
}

// The window must END at asOf (closed buckets only) and START one
// window before it — for an instant years in the past, not at now.
func TestPairMarketSubstanceAt_WindowEndsAtTheInstant(t *testing.T) {
	asOf := time.Date(2021, 3, 1, 9, 0, 0, 0, time.UTC)
	cases := []struct {
		grain     HistoryGranularity
		table     string
		wantUpper time.Time
	}{
		{Granularity1m, "FROM prices_1m", asOf.Add(-time.Minute)},
		{Granularity1h, "FROM prices_1h", asOf.Add(-time.Hour)},
	}
	for _, tc := range cases {
		t.Run(string(tc.grain), func(t *testing.T) {
			stmt := substanceAtQuery(t, asOf, 24*time.Hour, tc.grain)
			if !strings.Contains(stmt.sql, tc.table) {
				t.Errorf("grain %s did not read %q:\n%s", tc.grain, tc.table, indent(stmt.sql))
			}
			if got := literalBound(t, stmt.sql, "<="); !got.Equal(tc.wantUpper) {
				t.Errorf("upper bound = %s, want %s (asOf − one bucket: only buckets already "+
					"CLOSED at the instant may count, ADR-0015)", got, tc.wantUpper)
			}
			if got, want := literalBound(t, stmt.sql, ">="), asOf.Add(-24*time.Hour); !got.Equal(want) {
				t.Errorf("lower bound = %s, want %s (asOf − window)", got, want)
			}
		})
	}
}

// Same shape guarantees as the live reader: both stored directions, distinct buckets, the now() closed-
// bucket guard at the grain's own width, sargable bounds, and spelling sets bound rather than interpolated.
func TestPairMarketSubstanceAt_KeepsTheLiveReadersShape(t *testing.T) {
	pair := testXLMUSDCPair(t)
	bases, quotes := testXLMUSDCLegs(t)
	stmt := substanceAtQuery(t, time.Date(2024, 6, 1, 15, 0, 0, 0, time.UTC), 24*time.Hour, Granularity1h)
	norm := regexp.MustCompile(`\s+`).ReplaceAllString(stmt.sql, " ")

	for _, want := range []string{
		"base_asset = ANY($1) AND quote_asset = ANY($2) AND bucket <= now() - INTERVAL '1 hour'",
		"UNION ALL",
		"base_asset = ANY($2) AND quote_asset = ANY($1) AND bucket <= now() - INTERVAL '1 hour'",
		"AND NOT (base_asset = ANY($1) AND quote_asset = ANY($2))",
		"GROUP BY bucket",
	} {
		if !strings.Contains(norm, want) {
			t.Errorf("query is missing %q:\n%s", want, indent(stmt.sql))
		}
	}
	if orientationDisjunction.MatchString(stmt.sql) {
		t.Errorf("the two direction arms must be UNION ALL'd, not OR'd:\n%s", indent(stmt.sql))
	}
	if strings.Contains(stmt.sql, "bucket + INTERVAL") {
		t.Error("non-sargable `bucket + INTERVAL` form: function on the indexed column")
	}
	if len(stmt.args) != 2 || !reflect.DeepEqual(stmt.arg(t, 1), assetKeys(bases)) ||
		!reflect.DeepEqual(stmt.arg(t, 2), assetKeys(quotes)) {
		t.Errorf("args = %v, want exactly (bases, quotes) bound as $1/$2", stmt.args)
	}
	if strings.Contains(stmt.sql, pair.Quote.String()) {
		t.Error("the quote asset id was interpolated into the SQL text instead of bound")
	}

	minute := substanceAtQuery(t, time.Now().UTC(), 24*time.Hour, Granularity1m)
	if !strings.Contains(minute.sql, "bucket <= now() - INTERVAL '1 minute'") {
		t.Errorf("minute grain lost the 1-minute closed-bucket guard:\n%s", indent(minute.sql))
	}
}

// Only the two grains with a stated serve floor are accepted, and a bad
// argument must fail BEFORE a query is issued.
func TestPairMarketSubstanceAt_RejectsBadArgumentsBeforeQuerying(t *testing.T) {
	store, conn := newScriptedStore(t)
	bases, quotes := testXLMUSDCLegs(t)
	at := time.Date(2024, 6, 1, 15, 0, 0, 0, time.UTC)

	for _, g := range []HistoryGranularity{Granularity15m, Granularity4h, Granularity1d, HistoryGranularity("trades; --")} {
		if _, err := store.PairMarketSubstanceAt(context.Background(), bases, quotes, at, time.Hour, g); err == nil {
			t.Errorf("grain %q: want an error, got nil", g)
		}
	}
	for _, w := range []time.Duration{0, -time.Hour} {
		if _, err := store.PairMarketSubstanceAt(context.Background(), bases, quotes, at, w, Granularity1h); err == nil {
			t.Errorf("window %v: want an error, got nil", w)
		}
	}
	if n := len(conn.statements()); n != 0 {
		t.Errorf("issued %d statement(s) for arguments that must be rejected up front", n)
	}
}

// [pairMarketQuery] (/v1/pairs) cost is per request, so two shapes are pinned: both orientations as UNION ALL
// of single-direction branches (an OR defeats the pair index), and each aggregate bounded to its smallest
// window (MAX(ts) probe, 24h count), not one 14-day scan of `trades`.
func TestPairMarketQueryShape(t *testing.T) {
	q := pairMarketQuery

	// ── the disjunction is the defect; its absence is the guard ──
	if strings.Contains(q, "OR (base_asset") || strings.Contains(q, "OR (t.base_asset") {
		t.Errorf("pairMarketQuery folds both directions with an OR disjunction. "+
			"The planner cannot drive the (base_asset, quote_asset, ts|bucket DESC) "+
			"indexes from an OR of two (base,quote) pairs, so it degrades to a bare "+
			"time-index scan with the pair as a post-index filter. Use a UNION ALL of "+
			"two single-direction branches.\n\nquery:\n%s", q)
	}
	if n := strings.Count(q, "UNION ALL"); n < 3 {
		t.Errorf("pairMarketQuery has %d UNION ALL branches, want >= 3 — the "+
			"last_trade_at, vol_24h_usd and last_price reads each fold two stored "+
			"orientations and each must be its own indexable branch", n)
	}

	// Correctness half: both orientations must still be read. A UNION ALL that dropped a leg would be fast and
	// wrong: the SDEX decoder records XLM/USDC and USDC/XLM as separate rows, so a single-leg read halves
	// count_24h and vol_24h_usd.
	for _, leg := range []struct{ what, sql string }{
		{"forward, trades", "base_asset = $2 AND t.quote_asset = $3"},
		{"flipped, trades", "base_asset = $3 AND t.quote_asset = $2"},
		{"forward, prices_1m", "base_asset = $2 AND quote_asset = $3"},
		{"flipped, prices_1m", "base_asset = $3 AND quote_asset = $2"},
	} {
		if !strings.Contains(q, leg.sql) {
			t.Errorf("pairMarketQuery lost the %s direction (%q)", leg.what, leg.sql)
		}
	}

	// Window half: nothing may read MarketsRecencyWindow in bulk. $1 (the 14-day bound) may appear only on the
	// two LIMIT 1 last_trade_at index probes; any other use is a 14-day scan answering a 24-hour question.
	if n := strings.Count(q, "$1"); n != 2 {
		t.Errorf("pairMarketQuery references the MarketsRecencyWindow bound $1 %d "+
			"times, want exactly 2 (one per last_trade_at direction probe). Every "+
			"other aggregate needs at most 24 hours; widening one back to 14 days is "+
			"how this query came to materialise 17.2M rows for crypto:BTC/crypto:USDT."+
			"\n\nquery:\n%s", n, q)
	}
	// Each $1 branch must terminate at one row — an unbounded branch
	// re-introduces the full-window walk this guard prevents.
	probes := regexp.MustCompile(`(?s)t\.ts >= \$1\s+ORDER BY t\.ts DESC LIMIT 1`).FindAllString(q, -1)
	if len(probes) != 2 {
		t.Errorf("pairMarketQuery has %d bounded `ts >= $1 ORDER BY ts DESC LIMIT 1` "+
			"probes, want 2 — MAX(ts) over the recency window must be a single "+
			"backwards index probe per direction, not an aggregate over the window",
			len(probes))
	}
	// The old form computed count_24h as a FILTER over the 14-day scan.
	// The FILTER is cheap; the scan it rode on was not.
	if strings.Contains(q, "count(*) FILTER") {
		t.Error("pairMarketQuery computes count_24h as a FILTER over a wider scan. " +
			"Bind the 24-hour predicate into the scan itself so the planner reads " +
			"only the last day's chunks")
	}
	// Every non-$1 branch (count_24h x2, vol_24h_usd x2, last_price x2) must carry its own
	// 24-hour bound. The $1/$2/$3 checks above prove the $1 branches are the ONLY 14-day reads;
	// this proves none of the other six reads unbounded or rides the 14-day window back in.
	if n := strings.Count(q, "NOW() - INTERVAL '24 hours'"); n != 6 {
		t.Errorf("pairMarketQuery has %d \"NOW() - INTERVAL '24 hours'\" bounds, want "+
			"6 — one on each of count_24h's two directions, vol_24h_usd's two "+
			"directions and last_price's two directions. Fewer means a branch lost "+
			"its bound (an unbounded prices_1m/trades walk); more means a bound was "+
			"duplicated onto a branch that should stay a $1 recency-window probe."+
			"\n\nquery:\n%s", n, q)
	}

	// ── determinism: the last_price sort needs a total order ──
	// `bucket DESC` alone is not one once a bucket holds both orientations, so which leg won
	// would be planner-defined. Same tiebreaker, and same reasoning, as the prices_1m readers.
	if !strings.Contains(q, "ORDER BY bucket DESC, base_asset LIMIT 1") {
		t.Error("pairMarketQuery's last_price sort lacks the base_asset tiebreaker. " +
			"With both directions present in one bucket the winner would be " +
			"planner-defined, which ADR-0015 byte-identical cross-region serving " +
			"cannot rely on")
	}
}
