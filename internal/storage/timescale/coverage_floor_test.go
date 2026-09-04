// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package timescale

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// TestEarliestBucket_RejectsUnboundedAndDegenerateWindows pins the two
// guards that run BEFORE the query and therefore need no database: an
// unknown granularity, and a window that isn't strictly increasing.
//
// The window guard is the one that matters for the caller. A probe
// whose `to` collapsed onto (or behind) its `from` must not come back
// as "this pair has no coverage" — that answer would be handed
// straight to a client as `outside_coverage`, an assertion about the
// data, derived from a bug in the caller. Erroring is what turns it
// into no signal at all.
func TestEarliestBucket_RejectsUnboundedAndDegenerateWindows(t *testing.T) {
	s := &Store{} // no db — every case must return before the query
	pair, err := canonical.NewPair(canonical.NativeAsset(), canonical.Asset{Type: canonical.AssetFiat, Code: "USD"})
	if err != nil {
		t.Fatalf("NewPair: %v", err)
	}
	from := time.Date(2015, 9, 30, 0, 0, 0, 0, time.UTC)

	cases := []struct {
		name        string
		granularity HistoryGranularity
		from, to    time.Time
		wantErrPart string
	}{
		{"unknown granularity", "7h", from, from.Add(time.Hour), "unknown granularity"},
		{"to equals from", Granularity1d, from, from, "to"},
		{"to before from", Granularity1d, from, from.Add(-time.Hour), "to"},
		{"zero to", Granularity1d, from, time.Time{}, "to"},
	}
	reads := []struct {
		name string
		read func(context.Context, canonical.Pair, HistoryGranularity, time.Time, time.Time) (time.Time, bool, error)
	}{
		{"EarliestBucket", s.EarliestBucket},
		{"EarliestBucketAsStored", s.EarliestBucketAsStored},
		{"EarliestBucketLiteralQuote", s.EarliestBucketLiteralQuote},
	}
	for _, rd := range reads {
		for _, tc := range cases {
			t.Run(rd.name+"/"+tc.name, func(t *testing.T) {
				_, found, err := rd.read(context.Background(), pair, tc.granularity, tc.from, tc.to)
				if err == nil {
					t.Fatalf("%s(%v, %v) = nil error, want a guard error", rd.name, tc.from, tc.to)
				}
				if found {
					t.Errorf("found = true on an error return")
				}
				if !strings.Contains(err.Error(), tc.wantErrPart) {
					t.Errorf("err = %q, want it to mention %q", err, tc.wantErrPart)
				}
			})
		}
	}
}

// TestEarliestBucketLegs_NarrowsOnlyTheQuoteLeg pins the ONE difference
// between the two reads that share [earliestBucketSQL], which no
// assertion on the SQL text can see: the bound quote array.
//
// The fiat-quoted /v1/ohlc series reads each USD-pegged constituent
// under the one quote spelling the peg expansion named it in
// ([Store.OHLCSeries] takes the pair it is given), so a floor folded
// across that quote's alias family would measure a market the surface
// cannot serve — on r1 that is a Soroban pool quoted in a declared
// peg's SAC wrapper, whose first bucket would be published as the
// surface's floor and its silent window called quiet. The base leg
// keeps its family in both reads because every caller enumerates the
// base spellings itself.
func TestEarliestBucketLegs_NarrowsOnlyTheQuoteLeg(t *testing.T) {
	// XLM carries three canonical forms with no registry installed, so
	// this needs no fixture: as the QUOTE leg it is the fold under test.
	usdc, err := canonical.NewClassicAsset("USDC", "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN")
	if err != nil {
		t.Fatalf("NewClassicAsset: %v", err)
	}
	pair, err := canonical.NewPair(usdc, canonical.NativeAsset())
	if err != nil {
		t.Fatalf("NewPair: %v", err)
	}
	family := canonical.AssetAliasStrings(pair.Quote)
	if len(family) < 2 {
		t.Fatalf("quote leg has one spelling (%v); the fold under test is unobservable", family)
	}

	aliasBases, aliasQuotes := earliestBucketLegs(pair, true)
	literalBases, literalQuotes := earliestBucketLegs(pair, false)

	if len(aliasQuotes) != len(family) {
		t.Errorf("alias-folded quote array = %v, want the whole family %v", aliasQuotes, family)
	}
	if len(literalQuotes) != 1 || literalQuotes[0] != pair.Quote.String() {
		t.Errorf("literal quote array = %v, want exactly [%q]", literalQuotes, pair.Quote.String())
	}
	if strings.Join(aliasBases, ",") != strings.Join(literalBases, ",") {
		t.Errorf("base arrays differ: %v vs %v — only the quote leg narrows", aliasBases, literalBases)
	}
	if strings.Join(literalBases, ",") != strings.Join(canonical.AssetAliasStrings(pair.Base), ",") {
		t.Errorf("base array = %v, want the base's alias family %v", literalBases, canonical.AssetAliasStrings(pair.Base))
	}
}

// TestEarliestBucketSQL_IsPairIndexShaped guards the ONE property the
// probe's cost depends on and no unit test can measure: the pair
// predicate must be a plain equality against a bound alias form, never
// `= ANY(array)`.
//
// Measured on r1 (2026-09-03, EXPLAIN ANALYZE against prices_1d): the
// array form is planned as a bucket-ordered scan with the pair as a
// post-filter — 5 666 ms and ~4.2M rows discarded for XLM/USD — while
// the cross-joined equality form reaches prices_1d_pair_bucket_idx as
// an Index Only Scan and runs in 6.9 ms. Since the probe is reachable
// anonymously, a well-meaning "simplification" back to `= ANY` is a
// three-order-of-magnitude regression on a public path, so the shape is
// asserted rather than left to a comment.
func TestEarliestBucketSQL_IsPairIndexShaped(t *testing.T) {
	if strings.Contains(earliestBucketSQL, "ANY(") {
		t.Errorf("earliestBucketSQL uses `= ANY(...)` on the pair columns; that plan " +
			"cannot use prices_<g>_pair_bucket_idx — keep the unnest cross-join equality form")
	}
	for _, want := range []string{
		"p.base_asset  = b",  // requested direction
		"p.base_asset  = q",  // flipped direction
		"unnest($1::text[])", // alias family of the base leg
		"unnest($2::text[])", // alias family of the quote leg
		"p.bucket >= $3",     // bounded below
		// The upper bound's cast is not decoration: an untyped $4 whose
		// first use is beside `- INTERVAL` is resolved by PostgreSQL as
		// `interval - interval`, and the statement fails to parse
		// (42883) — which the probe would swallow as "no signal". The
		// integration test executes the statement; this pins the
		// spelling so the cast cannot be tidied away between runs.
		"$4::timestamptz - INTERVAL", // bounded above, typed at the bind
	} {
		if !strings.Contains(earliestBucketSQL, want) {
			t.Errorf("earliestBucketSQL missing %q", want)
		}
	}
	// The closed-bucket guard must stay sargable: a constant on the
	// right of the indexed column, never `bucket + INTERVAL <= …`,
	// which hands the predicate back to the filter stage.
	if strings.Contains(earliestBucketSQL, "p.bucket + INTERVAL") {
		t.Errorf("earliestBucketSQL puts a function on the indexed bucket column")
	}
}

// TestEarliestBucketStoredSQL_SpansOneOrientation pins the property
// that makes the stored-orientation variant honest for /v1/history: it
// carries the requested arm with the same index-shaped predicate and
// the same typed bounds, and it carries NO flipped arm — a flipped arm
// here would hand the raw-trade page a floor for rows its read never
// returns.
func TestEarliestBucketStoredSQL_SpansOneOrientation(t *testing.T) {
	for _, want := range []string{
		"p.base_asset  = b",
		"p.quote_asset = q",
		"unnest($1::text[])",
		"unnest($2::text[])",
		"p.bucket >= $3",
		"$4::timestamptz - INTERVAL",
	} {
		if !strings.Contains(earliestBucketStoredSQL, want) {
			t.Errorf("earliestBucketStoredSQL missing %q", want)
		}
	}
	for _, absent := range []string{"p.base_asset  = q", "p.quote_asset = b", "UNION", "ANY(", "p.bucket + INTERVAL"} {
		if strings.Contains(earliestBucketStoredSQL, absent) {
			t.Errorf("earliestBucketStoredSQL contains %q — the stored-orientation read must span one arm", absent)
		}
	}
}

// TestEarliestBucketSQL_RendersWithoutFormatResidue executes the bind
// the store performs — two operands into templates that consume them a
// different number of times — and checks the rendered statements carry
// no fmt residue. An `%!(EXTRA …)` or `%!s(MISSING)` in the text is a
// syntax error PostgreSQL reports at parse time, which the probe would
// swallow as "no signal" and every unit test above would miss.
func TestEarliestBucketSQL_RendersWithoutFormatResidue(t *testing.T) {
	for name, tmpl := range map[string]string{
		"either": earliestBucketSQL,
		"stored": earliestBucketStoredSQL,
	} {
		rendered := fmt.Sprintf(tmpl, "prices_1d", Granularity1d.closedBucketInterval())
		if strings.Contains(rendered, "%!") {
			t.Errorf("%s: rendered statement carries fmt residue:\n%s", name, rendered)
		}
		if strings.Contains(rendered, "%") {
			t.Errorf("%s: rendered statement still carries a verb:\n%s", name, rendered)
		}
		if !strings.Contains(rendered, "FROM prices_1d p") {
			t.Errorf("%s: table not bound:\n%s", name, rendered)
		}
	}
}
