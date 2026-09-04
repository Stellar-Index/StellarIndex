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

// earliestBucketSQL finds the START of the oldest materialised bucket a
// prices_<granularity> CAGG holds for a pair, across every canonical
// identity form of both legs AND both stored market directions, inside
// the half-open window [$3, $4).
//
// Shape, and why it is this shape rather than the obvious one: the
// obvious `WHERE base_asset = ANY($1) AND quote_asset = ANY($2)` does
// NOT reach prices_<g>_pair_bucket_idx — a ScalarArrayOpExpr on the
// leading index columns is planned as a bucket-ordered scan with the
// pair as a post-filter, so proving a sparse or absent direction empty
// walks every chunk. Measured on r1 against prices_1d (2026-09-03,
// EXPLAIN ANALYZE): the array form ran 5 666 ms and touched ~4.2M rows
// for XLM/USD; this form — one correlated `min(bucket)` per (form,
// form, direction) combination, each an equality lookup the index
// satisfies as an Index Only Scan — ran 6.9 ms with 20 ms planning and
// zero heap fetches. Same answer, same single round trip.
//
// The alias forms arrive as bound arrays and are cross-joined here, so
// the SQL text is STATIC: no per-request arm generation, no injection
// surface, one prepared plan for every pair.
//
// The window is required and both bounds are Go-side literals rather
// than now(): TimescaleDB does run-time chunk exclusion for now(),
// which leaves the PLANNER enumerating every chunk (the same trap
// [Store.LatestClosedVWAP1mForPair] documents), so a caller-supplied
// upper bound is what keeps planning flat. The ADR-0015 closed-bucket
// guard rides on that same bound as `bucket <= $4 - INTERVAL` — the
// sargable spelling, never `bucket + INTERVAL <= $4`, which is a
// function on the indexed column and gives the predicate back to the
// filter stage.
//
// Both bounds are bound with an explicit `::timestamptz`, and the cast
// on $4 is load-bearing. Its FIRST use in the statement is the operand
// of `- INTERVAL`, and PostgreSQL resolves a binary operator with one
// untyped operand by assuming it has the other operand's type — so an
// uncast $4 there is parsed as `interval - interval`, and the whole
// statement fails with "operator does not exist: timestamp with time
// zone <= interval" (42883) before a single row is read. A probe error
// is silent by design (no signal, one warning), so this is exactly the
// failure the integration test in test/integration exists to execute —
// and the one it caught on its first run against the migrated schema.
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

// earliestBucketStoredSQL is [earliestBucketSQL] without the flipped
// arm: the oldest bucket the CAGG holds for the pair in the orientation
// it was ASKED for, alias-complete on both legs. It backs the floor of
// a surface whose serving read spans one stored orientation — see
// [Store.EarliestBucketAsStored].
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

// EarliestBucket returns the START of the oldest CLOSED bucket the
// prices_<granularity> CAGG holds for the pair inside [from, to), and
// whether one exists at all. It is the coverage-FLOOR primitive behind
// the API's outside-coverage signal: an empty series is only worth
// annotating if the server can say when its own history for that pair
// begins, and that answer is one bounded read rather than a property
// any of the serving reads happen to return.
//
// Alias-complete on BOTH legs and BOTH stored directions. The serving
// reads this floor explains ([Store.OHLCSeries], [Store.HistoryPoints],
// [Store.HistoryPointsInRange]) each take ONE literal spelling per leg;
// it is the API layer that walks canonical.AssetAliases across both
// legs before calling them and serves whatever the first populated
// spelling holds. XLM's native / crypto:XLM / SAC forms are disjoint
// venue populations, and the SDEX decoder records a market in whichever
// orientation the venue used, so the floor of what the API serves is
// the floor across that whole walk — one read that spans it, rather
// than one per spelling. A floor read against one spelling of one
// direction would report a floor years later than the one the serving
// walk actually honours — and a floor that is too LATE is exactly the
// input that would make a caller's window look like it predates the
// held history when it does not.
//
// The direction fold matches the CAGG-backed series reads
// ([Store.OHLCSeries], [Store.HistoryPointsInRange]), which combine
// both stored orientations into the requested one. A surface whose
// serving read spans ONE stored orientation must not use this fold —
// see [Store.EarliestBucketAsStored].
//
// The alias fold on the QUOTE leg is the same conditional claim: it
// belongs to a surface whose read walks the quote's spellings, which
// /v1/chart, /v1/price/at and the non-fiat /v1/ohlc series all do. The
// fiat-quoted /v1/ohlc series does not — it reads each USD-pegged
// constituent in one named quote spelling — so it takes
// [Store.EarliestBucketLiteralQuote] instead.
//
// [from, to) is mandatory and half-open; `to` must be strictly after
// `from` (the guard [Store.OHLCSeries] applies for the same reason —
// a degenerate window is a caller bug, not an empty answer). Callers
// pass the network's first possible bucket as `from` and a Go-side
// `now` as `to`, which keeps the read bounded at both ends and the
// plan flat.
//
// Returns (zero, false, nil) when the pair has no bucket in the window.
func (s *Store) EarliestBucket(
	ctx context.Context,
	p canonical.Pair,
	granularity HistoryGranularity,
	from, to time.Time,
) (time.Time, bool, error) {
	return s.earliestBucket(ctx, "EarliestBucket", earliestBucketSQL, p, granularity, from, to, true)
}

// EarliestBucketAsStored is [Store.EarliestBucket] over the requested
// orientation ONLY: the oldest closed bucket stored with base_asset in
// the base leg's alias family and quote_asset in the quote leg's, and
// never the flipped rows.
//
// It exists for /v1/history. That page is served by
// [Store.TradesInRangeAfter], which reads one stored orientation and is
// never flipped by its caller, so a market the decoder recorded only as
// USDC/AQUA answers `base=AQUA&quote=USDC` with an empty page every
// time. A floor folded across both orientations would find the
// USDC/AQUA bucket and describe that page as a quiet market with a
// floor years back — a coverage claim about rows the page can never
// return. The floor of a surface is a property of the read that serves
// it, so this variant spans exactly what that read spans.
//
// Same window contract, same alias fold on each leg, same closed-bucket
// guard as [Store.EarliestBucket].
func (s *Store) EarliestBucketAsStored(
	ctx context.Context,
	p canonical.Pair,
	granularity HistoryGranularity,
	from, to time.Time,
) (time.Time, bool, error) {
	return s.earliestBucket(ctx, "EarliestBucketAsStored", earliestBucketStoredSQL, p, granularity, from, to, true)
}

// EarliestBucketLiteralQuote is [Store.EarliestBucket] with the alias
// fold on the QUOTE leg removed: the oldest closed bucket the CAGG
// holds with quote_asset equal to the spelling that was asked for (or,
// through the flipped arm, base_asset equal to it), the base leg still
// alias-complete and both stored directions still folded.
//
// It exists for the fiat-quoted /v1/ohlc series. That series is served
// by combining the USD-pegged constituents
// ([v1.Server.usdPeggedConstituents]), and each constituent is read
// under the ONE quote spelling the peg expansion named it in — a
// declared peg in its classic form, an abstract backer, or the fiat
// itself. [Store.OHLCSeries] takes that spelling literally; it folds
// the two stored directions but never a second spelling of a leg. A
// declared peg's SAC wrapper is therefore unreachable from that
// surface, and Soroban AMMs quote in exactly that wrapper — so a floor
// folded across the quote family would find a pool's bucket and
// describe the surface as quiet since it, a coverage claim about bars
// the series can never return. The base leg keeps its fold because the
// combine itself enumerates every base spelling.
//
// Same window contract, same closed-bucket guard, and the same
// statement text as [Store.EarliestBucket] — the narrowing is in the
// bound array, so both reads share one prepared plan.
func (s *Store) EarliestBucketLiteralQuote(
	ctx context.Context,
	p canonical.Pair,
	granularity HistoryGranularity,
	from, to time.Time,
) (time.Time, bool, error) {
	return s.earliestBucket(ctx, "EarliestBucketLiteralQuote", earliestBucketSQL, p, granularity, from, to, false)
}

// earliestBucketLegs builds the two bound leg arrays a floor statement
// cross-joins. `quoteAliases` is the whole difference between
// [Store.EarliestBucket] and [Store.EarliestBucketLiteralQuote], which
// share one statement text: with it the quote leg spans its alias
// family, without it exactly the spelling that was asked for. The base
// leg always spans its family — every caller's serving read enumerates
// the base spellings itself.
//
// Split out from the query so the narrowing is provable without a
// database: the SQL cannot show which of the two reads issued it.
func earliestBucketLegs(p canonical.Pair, quoteAliases bool) (bases, quotes []string) {
	bases = canonical.AssetAliasStrings(p.Base)
	if quoteAliases {
		return bases, canonical.AssetAliasStrings(p.Quote)
	}
	return bases, []string{p.Quote.String()}
}

// earliestBucket runs one of the three floor reads. `op` names the
// caller in errors; `tmpl` binds the table as %[1]s and the
// closed-bucket interval as %[2]s, once per arm, so the two-arm and
// one-arm statements take the same two operands; `quoteAliases` selects
// the leg arrays ([earliestBucketLegs]). The arrays are BOUND, never
// interpolated: the statement text stays static whichever spellings a
// caller spans.
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
	// #nosec G201 — table + interval are derived from the validated
	// HistoryGranularity enum, not user input. See Validate.
	q := fmt.Sprintf(tmpl, table, interval)
	bases, quotes := earliestBucketLegs(p, quoteAliases)

	var bucket sql.NullTime
	err := s.db.QueryRowContext(ctx, q, bases, quotes, from.UTC(), to.UTC()).Scan(&bucket)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		// min() over an empty set still yields one NULL row, so this is
		// unreachable today; treated as "no floor" rather than an error
		// so a future query rewrite can't turn a miss into a 500.
		return time.Time{}, false, nil
	case err != nil:
		return time.Time{}, false, fmt.Errorf("timescale: %s[%s]: %w", op, granularity, err)
	case !bucket.Valid:
		return time.Time{}, false, nil
	}
	return bucket.Time.UTC(), true, nil
}
