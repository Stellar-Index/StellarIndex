// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package timescale

import (
	"context"
	"database/sql/driver"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"
)

// [Store.PairMarketSubstanceAt] is the SQL half of the POINT-IN-TIME
// thin-market verdict (finding T038): the substance of the market that
// existed in the window ending at a requested instant. Its sibling
// [Store.PairMarketSubstance] always ends its window at now, which is
// the defect — so what these tests pin is WHERE the window sits.
//
// The query is assembled inline, so the scripted driver records the text
// the store actually issued. The executing proof against real Postgres
// is test/integration/pair_market_substance_at_test.go.

func substanceAtQuery(t *testing.T, asOf time.Time, window time.Duration, g HistoryGranularity) recordedStmt {
	t.Helper()
	store, conn := newScriptedStore(t, scriptedResult{
		cols: []string{"volume_usd", "buckets", "span_seconds"},
		rows: [][]driver.Value{{"0", int64(0), int64(0)}},
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

// Same shape guarantees as the live reader: both stored directions,
// distinct buckets, the now() closed-bucket guard at the grain's own
// width, sargable bounds, and the spelling sets bound rather than
// interpolated.
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
