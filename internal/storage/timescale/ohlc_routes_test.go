// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package timescale

import (
	"context"
	"database/sql/driver"
	"strconv"
	"strings"
	"testing"
	"time"
)

// ─── /v1/ohlc routes are ONE table, and the store serves all of it ───
//
// 2h, 12h, 3d and 2w were added to the serving reader's switch in #213
// while OHLCSeriesReBucketed kept its own hand-written allow-list of
// fold literals, which never learnt them. Every request at those four
// widths failed with "outInterval not in allow-list" — a 500 on the
// public API — from the day they shipped (launch plan W8-17). The
// allow-list is now the folded rows of [OHLCRoutes], so the check and
// the routing cannot diverge; these tests pin that the table is
// well-formed and that the store issues a query for every fold it
// declares.

// TestOHLCSeriesReBucketedServesEveryDeclaredFold drives each folded
// route through the real OHLCSeriesReBucketed against the scripted
// driver and requires a statement to reach the database that folds
// the declared source view by the declared literal. A route the
// allow-list refuses fails before any SQL is issued — which is
// exactly the 500 this guards against.
func TestOHLCSeriesReBucketedServesEveryDeclaredFold(t *testing.T) {
	bucket := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	row := []driver.Value{
		bucket, "0.179", "0.1805", "0.1785", "0.1801",
		"181018642392870", "32465341612945", int64(2100),
		"{sdex}",
	}
	folded := 0
	for _, route := range OHLCRoutes {
		if !route.Folded() {
			continue
		}
		folded++
		t.Run(route.Interval, func(t *testing.T) {
			store, conn := newScriptedStore(t, scriptedResult{
				cols: ohlcSourcesCols,
				rows: [][]driver.Value{row},
			})
			bars, err := store.OHLCSeriesReBucketed(context.Background(), ohlcSourcesPair(t),
				route.Source, route.Fold, bucket, bucket.Add(28*24*time.Hour), 10)
			if err != nil {
				t.Fatalf("%s: OHLCSeriesReBucketed(%s, %q) = %v — the API routes this "+
					"interval here, so this error is a 500 on /v1/ohlc?interval=%s",
					route.Interval, route.Source, route.Fold, err, route.Interval)
			}
			if len(bars) != 1 || bars[0].TradeCount != 2100 {
				t.Fatalf("%s: got %d bars %+v, want the one scripted row", route.Interval, len(bars), bars)
			}
			stmt := conn.only(t).sql
			for _, want := range []string{
				"INTERVAL '" + route.Fold + "'",
				"FROM prices_" + string(route.Source),
			} {
				if !strings.Contains(stmt, want) {
					t.Errorf("%s: issued SQL lacks %q:\n%s", route.Interval, want, stmt)
				}
			}
		})
	}
	// Self-accounting: a table with no folded rows would pass every
	// subtest by running none.
	if folded < 7 {
		t.Fatalf("only %d folded routes in OHLCRoutes, want at least the 7 the API serves "+
			"(5m 30m 2h 4h 12h 3d 2w)", folded)
	}
	t.Logf("executed %d of %d folded routes", folded, folded)
}

// TestOHLCSeriesReBucketedRefusesUndeclaredFold — the allow-list is the
// TABLE, not "any literal that appears in it": a source/fold pairing
// no route declares must be refused before SQL is composed, even when
// both halves are individually known.
func TestOHLCSeriesReBucketedRefusesUndeclaredFold(t *testing.T) {
	for _, tc := range []struct {
		source HistoryGranularity
		fold   string
	}{
		{Granularity1m, "4 hours"},   // fold declared, but for prices_1h
		{Granularity1h, "2 weeks"},   // fold declared, but for prices_1w
		{Granularity1h, "3 hours"},   // fold declared nowhere
		{Granularity1h, "1 hour"},    // a native width is not a fold
		{Granularity1mo, "2 months"}, // no route folds the month view
	} {
		store, conn := newScriptedStore(t)
		_, err := store.OHLCSeriesReBucketed(context.Background(), ohlcSourcesPair(t),
			tc.source, tc.fold, time.Unix(0, 0), time.Unix(3600, 0), 10)
		if err == nil {
			t.Errorf("(%s, %q): accepted an undeclared fold", tc.source, tc.fold)
		}
		if len(conn.stmts) != 0 {
			t.Errorf("(%s, %q): %d statements reached the driver, want 0 — the refusal "+
				"must precede SQL composition", tc.source, tc.fold, len(conn.stmts))
		}
	}
}

// TestOHLCRoutesAreWellFormed — every row is exactly one shape, names
// only granularities the store serves, and no interval appears twice
// (a duplicate would make OHLCRouteFor's answer depend on order).
func TestOHLCRoutesAreWellFormed(t *testing.T) {
	seen := map[string]bool{}
	for _, r := range OHLCRoutes {
		if r.Interval == "" {
			t.Fatalf("route with empty Interval: %+v", r)
		}
		if seen[r.Interval] {
			t.Errorf("%s: declared twice", r.Interval)
		}
		seen[r.Interval] = true
		switch {
		case r.Folded():
			if err := r.Source.Validate(); err != nil {
				t.Errorf("%s: folded route's Source: %v", r.Interval, err)
			}
			if r.Fold == "" {
				t.Errorf("%s: folded route has no Fold literal", r.Interval)
			}
		default:
			if err := r.Native.Validate(); err != nil {
				t.Errorf("%s: native route's Native: %v", r.Interval, err)
			}
			if r.Source != "" || r.Fold != "" {
				t.Errorf("%s: native route also carries Source/Fold %q/%q — pick one shape",
					r.Interval, r.Source, r.Fold)
			}
		}
		got, ok := OHLCRouteFor(r.Interval)
		if !ok || got != r {
			t.Errorf("OHLCRouteFor(%q) = %+v, %v; want the declared row", r.Interval, got, ok)
		}
	}
	if _, ok := OHLCRouteFor("7h"); ok {
		t.Error("OHLCRouteFor(\"7h\") resolved; nothing declares it")
	}
	list := OHLCIntervalList()
	if got := strings.Split(list, historyGranularitySep); len(got) != len(OHLCRoutes) {
		t.Errorf("OHLCIntervalList() = %q renders %d entries, want %d", list, len(got), len(OHLCRoutes))
	}
}

// TestOHLCRoutesFoldIsAMultipleOfItsSource pins the precondition
// OHLCSeriesReBucketed documents: time_bucket over source rows only
// folds cleanly when the output width is an integer multiple of the
// source bucket. A 3d fold of prices_1h would be fine (72×); a 2h fold
// of prices_4h would silently drop rows. Literal widths are parsed
// from the Postgres INTERVAL text, so this reads the table exactly as
// the query will.
func TestOHLCRoutesFoldIsAMultipleOfItsSource(t *testing.T) {
	for _, r := range OHLCRoutes {
		if !r.Folded() {
			continue
		}
		src := granularityWidth(t, r.Source)
		fold := intervalLiteralWidth(t, r.Fold)
		if fold <= src || fold%src != 0 {
			t.Errorf("%s: fold %q (%v) is not a whole multiple > 1 of prices_%s's bucket (%v)",
				r.Interval, r.Fold, fold, r.Source, src)
		}
	}
}

// granularityWidth is the bucket width of a source view. 1mo is
// calendar-sized and never a fold source, so it is not mapped.
func granularityWidth(t *testing.T, g HistoryGranularity) time.Duration {
	t.Helper()
	switch g {
	case Granularity1m:
		return time.Minute
	case Granularity15m:
		return 15 * time.Minute
	case Granularity1h:
		return time.Hour
	case Granularity4h:
		return 4 * time.Hour
	case Granularity1d:
		return 24 * time.Hour
	case Granularity1w:
		return 7 * 24 * time.Hour
	case Granularity1mo:
		t.Fatalf("%s is calendar-sized and cannot be a fold source", g)
	}
	t.Fatalf("no fixed width for source granularity %q", g)
	return 0
}

// intervalLiteralWidth parses the `N unit` Postgres INTERVAL text the
// routes carry. Units are the plural forms the table uses.
func intervalLiteralWidth(t *testing.T, lit string) time.Duration {
	t.Helper()
	fields := strings.Fields(lit)
	if len(fields) != 2 {
		t.Fatalf("interval literal %q is not `N unit`", lit)
	}
	n, err := strconv.Atoi(fields[0])
	if err != nil || n <= 0 {
		t.Fatalf("interval literal %q: bad count", lit)
	}
	var unit time.Duration
	switch fields[1] {
	case "minutes":
		unit = time.Minute
	case "hours":
		unit = time.Hour
	case "days":
		unit = 24 * time.Hour
	case "weeks":
		unit = 7 * 24 * time.Hour
	default:
		t.Fatalf("interval literal %q: unknown unit", lit)
	}
	return time.Duration(n) * unit
}
