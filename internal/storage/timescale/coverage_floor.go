// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package timescale

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// earliestBucketSQL finds the start of the oldest materialised prices_<g> bucket for a pair across all identity forms and both
// directions, in [$3, $4). A correlated min(bucket) per (form, form, direction) is an Index Only Scan; `= ANY` on both legs scans every
// chunk. Bounds are Go-side literals (run-time exclusion enumerates chunks); guard `bucket <= $4 - INTERVAL`, never `bucket + INTERVAL
// <= $4`. The ::timestamptz cast on $4 is load-bearing (42883) and probe errors are silent, so the integration test must execute this.
const earliestBucketSQL = `
	SELECT min(m) FROM (
	    SELECT (SELECT min(p.bucket)
	              FROM %[1]s p
	             WHERE p.base_asset  = b
	               AND p.quote_asset = q
	               AND p.bucket >= $3::timestamptz
	               AND p.bucket <= $4::timestamptz - INTERVAL '%[2]s') AS m
	      FROM unnest($1::text[]) AS b, unnest($2::text[]) AS q
	    UNION ALL
	    SELECT (SELECT min(p.bucket)
	              FROM %[1]s p
	             WHERE p.base_asset  = q
	               AND p.quote_asset = b
	               AND p.bucket >= $3::timestamptz
	               AND p.bucket <= $4::timestamptz - INTERVAL '%[2]s')
	      FROM unnest($1::text[]) AS b, unnest($2::text[]) AS q
	) t
`

// earliestBucketStoredSQL is [earliestBucketSQL] without the flipped arm: the oldest bucket in the
// orientation asked for, alias-complete on both legs. See [Store.EarliestBucketAsStored].
const earliestBucketStoredSQL = `
	SELECT min(m) FROM (
	    SELECT (SELECT min(p.bucket)
	              FROM %[1]s p
	             WHERE p.base_asset  = b
	               AND p.quote_asset = q
	               AND p.bucket >= $3::timestamptz
	               AND p.bucket <= $4::timestamptz - INTERVAL '%[2]s') AS m
	      FROM unnest($1::text[]) AS b, unnest($2::text[]) AS q
	) t
`

// EarliestBucket returns the START of the oldest CLOSED prices_<granularity> bucket for the pair in [from, to) and whether one exists,
// alias-complete on both legs and both stored directions; the coverage-floor primitive.
func (s *Store) EarliestBucket(
	ctx context.Context,
	p canonical.Pair,
	granularity HistoryGranularity,
	from, to time.Time,
) (time.Time, bool, error) {
	return s.earliestBucket(ctx, "EarliestBucket", earliestBucketSQL, p, granularity, from, to, true)
}

// EarliestBucketAsStored is [Store.EarliestBucket] over the requested orientation only, never flipped rows, matching /v1/history, which
// reads one stored orientation.
func (s *Store) EarliestBucketAsStored(
	ctx context.Context,
	p canonical.Pair,
	granularity HistoryGranularity,
	from, to time.Time,
) (time.Time, bool, error) {
	return s.earliestBucket(ctx, "EarliestBucketAsStored", earliestBucketStoredSQL, p, granularity, from, to, true)
}

// EarliestBucketLiteralQuote is [Store.EarliestBucket] with the alias fold removed from the QUOTE leg only, for the fiat-quoted
// /v1/ohlc series that reads each constituent under one quote spelling.
func (s *Store) EarliestBucketLiteralQuote(
	ctx context.Context,
	p canonical.Pair,
	granularity HistoryGranularity,
	from, to time.Time,
) (time.Time, bool, error) {
	return s.earliestBucket(ctx, "EarliestBucketLiteralQuote", earliestBucketSQL, p, granularity, from, to, false)
}

// earliestBucketLegs builds the two leg arrays a floor statement cross-joins; quoteAliases is the only difference between
// [Store.EarliestBucket] and [Store.EarliestBucketLiteralQuote]. Split out so the narrowing is provable without a database.
func earliestBucketLegs(p canonical.Pair, quoteAliases bool) (bases, quotes []string) {
	bases = canonical.AssetAliasStrings(p.Base)
	if quoteAliases {
		return bases, canonical.AssetAliasStrings(p.Quote)
	}
	return bases, []string{p.Quote.String()}
}

// earliestBucket runs one floor read; `tmpl` binds the table as %[1]s and the interval as %[2]s. Legs are bound, never interpolated.
func (s *Store) earliestBucket(
	ctx context.Context,
	op, tmpl string,
	p canonical.Pair,
	granularity HistoryGranularity,
	from, to time.Time,
	quoteAliases bool,
) (time.Time, bool, error) {
	if err := granularity.Validate(); err != nil {
		return time.Time{}, false, err
	}
	if !to.After(from) {
		return time.Time{}, false, fmt.Errorf("timescale: %s: to %v <= from %v", op, to, from)
	}
	table := "prices_" + string(granularity)
	interval := granularity.closedBucketInterval()
	// #nosec G201 — table + interval come from the validated HistoryGranularity enum, not user input.
	q := fmt.Sprintf(tmpl, table, interval)
	bases, quotes := earliestBucketLegs(p, quoteAliases)

	var bucket sql.NullTime
	err := s.db.QueryRowContext(ctx, q, bases, quotes, from.UTC(), to.UTC()).Scan(&bucket)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		// min() over an empty set yields one NULL row, so this is unreachable today; it maps
		// to "no floor" so a query rewrite cannot turn a miss into a 500.
		return time.Time{}, false, nil
	case err != nil:
		return time.Time{}, false, fmt.Errorf("timescale: %s[%s]: %w", op, granularity, err)
	case !bucket.Valid:
		return time.Time{}, false, nil
	}
	return bucket.Time.UTC(), true, nil
}
