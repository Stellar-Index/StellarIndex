// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package timescale

import (
	"context"
	"database/sql/driver"
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"
)

// [Store.PairMarketSubstance] (aggregates.go:2071) is the SQL half of the
// serving-side thin-market gate: internal/pricingguard.SubstanceGate feeds
// its three numbers straight into SubstanceOK, and a false pass there
// publishes an aggregated price for a market that has no substance behind
// it. It had no test in this package — only the interface fake in
// internal/pricingguard/substance_test.go, which never sees the SQL.
//
// Two mutations were live and uncaught (audit-2026-09-02 #340 item 7):
//
//   - dropping `AND bucket <= now() - INTERVAL '1 minute'` lets the
//     IN-PROGRESS bucket into the measurement, so a market can be pushed
//     over the volume/bucket floor by trades that ADR-0015 says are not
//     servable yet — the gate opens on activity the price read cannot
//     even see.
//   - dropping one of the two direction arms measures half the market
//     (the recurring UNAUTH-DOS-9 / MNY-06 class), so a genuinely
//     two-sided pair is withheld, or a one-sided-the-wrong-way pair reads
//     as dead.
//
// The query is built inline with fmt.Sprintf inside the method body, so
// the package-level template guards in closed_vwap_at_test.go cannot see
// it. The scripted driver can: it records the SQL text the store actually
// issued along with the bound args.

// substanceQuery runs PairMarketSubstance against one canned row and
// returns the statement the store issued plus the decoded result.
func substanceQuery(t *testing.T, window time.Duration, row []driver.Value) (recordedStmt, MarketSubstance) {
	t.Helper()
	store, conn := newScriptedStore(t, scriptedResult{
		cols: []string{"volume_usd", "buckets", "span_seconds"},
		rows: [][]driver.Value{row},
	})
	sub, err := store.PairMarketSubstance(context.Background(), testXLMUSDCPair(t), window)
	if err != nil {
		t.Fatalf("PairMarketSubstance: %v", err)
	}
	return conn.only(t), sub
}

// TestPairMarketSubstance_ClosedBucketPredicate pins the ADR-0015
// closed-bucket guard in its sargable form. `bucket <= now() - INTERVAL
// '1 minute'` and nothing else: the `bucket + INTERVAL '1 minute' <=
// now()` spelling puts a function on the indexed column, which is the
// drift UNAUTH-DOS-3 found on the sibling reader.
func TestPairMarketSubstance_ClosedBucketPredicate(t *testing.T) {
	stmt, _ := substanceQuery(t, time.Hour, []driver.Value{"0", int64(0), int64(0)})

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

// TestPairMarketSubstance_ReadsBothStoredDirections pins both arms of the
// orientation filter. The CAGGs hold a market as both (A,B) and (B,A)
// rows (see TestCAGGPairReadsFoldBothDirections); substance is a COUNT
// and a SUM, so no Go-side fold is needed — but the OR is load-bearing,
// and a one-armed measurement understates every two-sided market.
func TestPairMarketSubstance_ReadsBothStoredDirections(t *testing.T) {
	stmt, _ := substanceQuery(t, time.Hour, []driver.Value{"0", int64(0), int64(0)})

	norm := regexp.MustCompile(`\s+`).ReplaceAllString(stmt.sql, " ")
	for _, arm := range []string{
		"(base_asset = $1 AND quote_asset = $2)",
		"(base_asset = $2 AND quote_asset = $1)",
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
	if !strings.Contains(norm, "$1 AND quote_asset = $2) OR (base_asset = $2") {
		t.Errorf("the two direction arms are not OR'd together:\n%s", indent(stmt.sql))
	}
}

// TestPairMarketSubstance_BindsPairAndLiteralLowerBound pins the split
// the #nosec note at aggregates.go:2075 depends on: the pair strings BIND
// as $1/$2, and the only interpolated value is the trailing-window lower
// bound, rendered as a literal timestamptz for plan-time chunk pruning.
func TestPairMarketSubstance_BindsPairAndLiteralLowerBound(t *testing.T) {
	pair := testXLMUSDCPair(t)
	before := time.Now().UTC()
	stmt, _ := substanceQuery(t, 6*time.Hour, []driver.Value{"0", int64(0), int64(0)})
	after := time.Now().UTC()

	if len(stmt.args) != 2 {
		t.Fatalf("bound %d args, want exactly 2 (base, quote): %v", len(stmt.args), stmt.args)
	}
	if got := stmt.arg(t, 1); got != pair.Base.String() {
		t.Errorf("$1 = %v, want the base asset %s", got, pair.Base)
	}
	if got := stmt.arg(t, 2); got != pair.Quote.String() {
		t.Errorf("$2 = %v, want the quote asset %s", got, pair.Quote)
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

// TestPairMarketSubstance_GroupsByBucket pins the DISTINCT-bucket
// semantics the doc comment promises: Buckets counts closed MINUTES with
// a trade, not CAGG rows. Counting rows would let a two-sided minute (one
// row per direction) report 2, so a single-minute burst on a two-sided
// dust market would clear a MinBuckets=2 floor on its own.
func TestPairMarketSubstance_GroupsByBucket(t *testing.T) {
	stmt, _ := substanceQuery(t, time.Hour, []driver.Value{"0", int64(0), int64(0)})

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

// TestPairMarketSubstance_VolumeIsAnExactDecimalString is the ADR-0003
// half: VolumeUSD crosses the seam as the NUMERIC's own text, and
// pricingguard compares it as a big.Rat. A float64 hop anywhere in the
// scan would round this fixture in the 17th significant digit — enough to
// move a pair across a floor set at the same magnitude.
func TestPairMarketSubstance_VolumeIsAnExactDecimalString(t *testing.T) {
	const exact = "123456789012345678.123456789"
	_, sub := substanceQuery(t, time.Hour, []driver.Value{exact, int64(7), int64(1860)})

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
}

// TestPairMarketSubstance_EmptyPairIsZeroNotError — absence of market is
// a measurement. The gate must be able to tell "no substance" from "the
// measurement failed"; collapsing them would make a DB blip look like a
// thin market (or, worse, the reverse if the caller defaulted open).
func TestPairMarketSubstance_EmptyPairIsZeroNotError(t *testing.T) {
	_, sub := substanceQuery(t, time.Hour, []driver.Value{"0", int64(0), int64(0)})

	if sub != (MarketSubstance{VolumeUSD: "0"}) {
		t.Errorf("empty pair = %+v, want {VolumeUSD:0 Buckets:0 SpanSeconds:0}", sub)
	}
}

// TestPairMarketSubstance_NonPositiveWindowErrorsBeforeQuerying — a zero
// window would render `bucket >= now()` and measure nothing, which reads
// as a thin market for every pair. It must be rejected, and rejected
// without issuing SQL.
func TestPairMarketSubstance_NonPositiveWindowErrorsBeforeQuerying(t *testing.T) {
	for _, w := range []time.Duration{0, -time.Minute} {
		store, conn := newScriptedStore(t)
		_, err := store.PairMarketSubstance(context.Background(), testXLMUSDCPair(t), w)
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

	sub, err := store.PairMarketSubstance(context.Background(), testXLMUSDCPair(t), time.Hour)
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
