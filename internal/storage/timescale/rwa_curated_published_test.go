// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package timescale

import (
	"context"
	"strings"
	"testing"
	"time"
)

func publishedRowsN(n int) []CuratedRWAPublishedRow {
	out := make([]CuratedRWAPublishedRow, 0, n)
	for i := range n {
		out = append(out, CuratedRWAPublishedRow{
			Series:      CuratedRWASeriesMonthlyTotal,
			MonthEnd:    time.Date(2024, time.Month(1+i%12), 28, 0, 0, 0, 0, time.UTC),
			ValueUSD:    "436023166.55",
			SourceQuery: 6961845,
			ExecutedAt:  time.Date(2026, 9, 17, 5, 0, 0, 0, time.UTC),
		})
	}
	return out
}

func TestBuildCuratedRWAPublishedInsert_PlaceholderLayout(t *testing.T) {
	t.Parallel()

	q, args := buildCuratedRWAPublishedInsert("dune:stellar", publishedRowsN(3))
	if got, want := len(args), 3*7; got != want {
		t.Fatalf("len(args) = %d, want %d (7 per row)", got, want)
	}
	if strings.Contains(q, "$22") {
		t.Error("statement references $22 — placeholder math drifted past the arg list")
	}
	// observed_at is transaction-stable now(): the reader's recognition
	// bound depends on every row of one run sharing a clock.
	if strings.Contains(q, "clock_timestamp()") {
		t.Error("observed_at must be transaction-stable now(), never clock_timestamp()")
	}
	// Explicit casts on every typed bind — an untyped parameter beside
	// a date or numeric column is the 42883-class trap.
	for _, want := range []string{"$3::date", "$5::numeric", "$6::bigint", "$7::timestamptz"} {
		if !strings.Contains(q, want) {
			t.Errorf("statement must bind %s explicitly; got:\n%s", want, q)
		}
	}
	// The month travels as a date literal, never as a timestamp the
	// server would shift into its own zone.
	if args[2] != "2024-01-28" {
		t.Errorf("month_end bind = %v, want the YYYY-MM-DD literal", args[2])
	}
}

func TestDedupCuratedRWAPublishedRows_KeepsTheLastPerBucket(t *testing.T) {
	t.Parallel()

	m := time.Date(2025, 8, 31, 0, 0, 0, 0, time.UTC)
	rows := []CuratedRWAPublishedRow{
		{Series: CuratedRWASeriesMonthlyTotal, MonthEnd: m, ValueUSD: "1"},
		{Series: CuratedRWASeriesMonthlyBySubclass, MonthEnd: m, Subclass: "US Treasuries", ValueUSD: "2"},
		{Series: CuratedRWASeriesMonthlyTotal, MonthEnd: m, ValueUSD: "3"},
		{Series: CuratedRWASeriesMonthlyBySubclass, MonthEnd: m, Subclass: "Private Credit", ValueUSD: "4"},
	}
	got := dedupCuratedRWAPublishedRows(rows)
	if len(got) != 3 {
		t.Fatalf("len = %d, want 3 (one duplicate total bucket collapsed)", len(got))
	}
	if got[0].ValueUSD != "3" {
		t.Errorf("duplicate bucket kept %q, want the LAST value 3", got[0].ValueUSD)
	}
	if got[1].Subclass != "US Treasuries" || got[2].Subclass != "Private Credit" {
		t.Errorf("distinct subclasses must both survive: %+v", got)
	}
}

func TestReplaceCuratedRWAPublished_Refusals(t *testing.T) {
	t.Parallel()

	s := &Store{}
	if _, err := s.ReplaceCuratedRWAPublished(context.Background(), "", publishedRowsN(1)); err == nil {
		t.Error("empty curator accepted")
	}
	if _, err := s.ReplaceCuratedRWAPublished(context.Background(), "dune:stellar", nil); err == nil {
		t.Error("empty row set accepted — it would empty the curator into silence")
	}
	if _, err := s.ReplaceCuratedRWAPublished(context.Background(), "dune:stellar",
		[]CuratedRWAPublishedRow{{Series: CuratedRWASeriesMonthlyTotal, MonthEnd: time.Now(), ValueUSD: "1"}}); err == nil {
		t.Error("a row with no execution time accepted")
	}
	if _, err := s.LatestCuratedPublished(context.Background(), ""); err == nil {
		t.Error("empty curator accepted on read")
	}
}

// The recognition bound is in the statements, not in a caller's hands.
func TestCuratedRWAPublished_BoundIsEnforcedInSQL(t *testing.T) {
	t.Parallel()

	for name, q := range map[string]string{"total": curatedRWAPublishedTotalSQL, "split": curatedRWAPublishedSplitSQL} {
		if !strings.Contains(q, "observed_at > now() - INTERVAL '"+curatedRWARecognitionMaxAge+"'") {
			t.Errorf("%s statement does not carry the recognition bound:\n%s", name, q)
		}
	}
	if !strings.Contains(curatedRWAPublishedTotalSQL, "ORDER BY month_end ASC") {
		t.Error("the total series must come back oldest first — the reader takes the LAST row as the latest month")
	}
	if !strings.Contains(curatedRWAPublishedSplitSQL, "month_end = $2::date") {
		t.Error("the split must be bound to one month with an explicit date cast")
	}
	// The select list casts value_usd to text under the SAME output name,
	// and Postgres resolves an unqualified ORDER BY against output names
	// first: an unqualified `ORDER BY value_usd DESC` sorts the split as
	// text ("904795860.00" above "3100000000.00"). The integration test
	// caught it; this pins the qualified form.
	if !strings.Contains(curatedRWAPublishedSplitSQL, "ORDER BY curated_rwa_published_series.value_usd DESC") {
		t.Error("the split's ORDER BY must name the TABLE column, or it sorts the text-cast output column")
	}
}
