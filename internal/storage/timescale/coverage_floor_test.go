// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package timescale

import (
	"context"
	"database/sql/driver"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// TestEarliestBucket_RejectsUnboundedAndDegenerateWindows pins the two pre-query guards (unknown
// granularity, non-increasing window). A bad window must error, not answer "no coverage", which clients
// would read as `outside_coverage`.
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

// TestEarliestBucketLegs_NarrowsOnlyTheQuoteLeg pins the bound quote array that SQL-text assertions cannot
// see: the quote leg is narrowed to the peg spelling named, while the base leg keeps its alias family.
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

// TestEarliestBucketSQL_IsPairIndexShaped: the pair predicate must be a plain equality, never `= ANY(array)`,
// which plans as a bucket-ordered scan with a post-filter instead of an index-only scan.
// The probe is anonymously reachable, so the shape is asserted.
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
		// The cast is load-bearing: an untyped $4 beside `- INTERVAL` resolves as
		// `interval - interval` and fails with 42883, which the probe would swallow as "no
		// signal". The integration test executes the statement; this pins the spelling.
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

// TestEarliestBucketStoredSQL_SpansOneOrientation pins that the stored-orientation variant for /v1/history
// carries only the requested arm with the same index-shaped predicate and typed bounds; a flipped arm would
// give the raw-trade page a floor for rows its read never returns.
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

// TestEarliestBucketSQL_RendersWithoutFormatResidue executes the store's bind (two operands into templates
// that use them different numbers of times) and checks for fmt residue: `%!(EXTRA ...)` or `%!s(MISSING)`
// is a parse error the probe would swallow as "no signal".
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

var earliestBucketClosedGuard = regexp.MustCompile(`p\.bucket <= \$4::timestamptz - INTERVAL '(\d+) (\w+)'`)

// TestEarliestBucket_IssuedStatementCarriesTheClosedBucketGuard reads the ADR-0015 closed-bucket
// guard from the issued statement: `to` binds as $4 unshifted and every arm subtracts exactly one
// bucket of the requested granularity. A narrower guard admits the in-progress bucket as a floor.
func TestEarliestBucket_IssuedStatementCarriesTheClosedBucketGuard(t *testing.T) {
	pair := testXLMUSDCPair(t)
	to := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	from := to.Add(-90 * 24 * time.Hour)
	units := map[string]time.Duration{
		"minute": time.Minute, "minutes": time.Minute, "hour": time.Hour, "hours": time.Hour,
		"day": 24 * time.Hour, "week": 7 * 24 * time.Hour,
	}
	reads := []struct {
		name string
		arms int
		call func(*Store, HistoryGranularity) error
	}{
		{"EarliestBucket", 2, func(s *Store, g HistoryGranularity) error {
			_, _, err := s.EarliestBucket(context.Background(), pair, g, from, to)
			return err
		}},
		{"EarliestBucketAsStored", 1, func(s *Store, g HistoryGranularity) error {
			_, _, err := s.EarliestBucketAsStored(context.Background(), pair, g, from, to)
			return err
		}},
	}
	for _, rd := range reads {
		for _, g := range []HistoryGranularity{Granularity1m, Granularity15m, Granularity1h, Granularity4h, Granularity1d, Granularity1w} {
			store, conn := newScriptedStore(t, scriptedResult{cols: []string{"min"}, rows: [][]driver.Value{{nil}}})
			if err := rd.call(store, g); err != nil {
				t.Fatalf("%s[%s]: %v", rd.name, g, err)
			}
			stmt := conn.stmts[0]
			if got := stmt.arg(t, 3); got != any(from) {
				t.Errorf("%s[%s]: $3 = %v, want from %v", rd.name, g, got, from)
			}
			if got := stmt.arg(t, 4); got != any(to) {
				t.Errorf("%s[%s]: $4 = %v, want to %v unshifted", rd.name, g, got, to)
			}
			ms := earliestBucketClosedGuard.FindAllStringSubmatch(stmt.sql, -1)
			if len(ms) != rd.arms {
				t.Fatalf("%s[%s]: %d arms carry the closed-bucket guard, want %d:\n%s", rd.name, g, len(ms), rd.arms, stmt.sql)
			}
			for _, m := range ms {
				n, _ := strconv.Atoi(m[1])
				if got := time.Duration(n) * units[m[2]]; got != g.BucketDuration() {
					t.Errorf("%s[%s]: guard subtracts %s %s, want one %s bucket", rd.name, g, m[1], m[2], g.BucketDuration())
				}
			}
		}
	}
}
