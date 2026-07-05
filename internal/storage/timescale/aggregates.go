package timescale

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/StellarIndex/stellar-index/internal/aggregate/baseline"
	"github.com/StellarIndex/stellar-index/internal/canonical"
)

// HistoryPoint is a single bucket's worth of aggregated price +
// volume, returned by [Store.HistoryPoints]. Wire shape mirrors the
// API's `{t, p, v_usd}` triple — the API binary's adapter is a
// pure passthrough.
type HistoryPoint struct {
	Bucket    time.Time
	VWAP      string  // NUMERIC text from Postgres
	VolumeUSD *string // NULL when no usd_volume column entries — e.g. early classic-only ledgers
}

// HistoryGranularity is the CAGG selector for [Store.HistoryPoints].
// Stable string values matching the API's `granularity` query
// parameter and the migration's table names (prices_<granularity>).
type HistoryGranularity string

const (
	Granularity1m  HistoryGranularity = "1m"
	Granularity15m HistoryGranularity = "15m"
	Granularity1h  HistoryGranularity = "1h"
	Granularity4h  HistoryGranularity = "4h"
	Granularity1d  HistoryGranularity = "1d"
	Granularity1w  HistoryGranularity = "1w"
	Granularity1mo HistoryGranularity = "1mo"
)

// Validate reports whether g is one of the seven supported
// granularities. Caller surfaces unknown granularities as 400.
func (g HistoryGranularity) Validate() error {
	switch g {
	case Granularity1m, Granularity15m, Granularity1h, Granularity4h,
		Granularity1d, Granularity1w, Granularity1mo:
		return nil
	}
	return fmt.Errorf("unknown granularity %q (want one of: 1m, 15m, 1h, 4h, 1d, 1w, 1mo)", g)
}

// closedBucketInterval is the Postgres INTERVAL string that the
// closed-bucket guard subtracts from now() per ADR-0015. Equal to
// the granularity's bucket size.
func (g HistoryGranularity) closedBucketInterval() string {
	switch g {
	case Granularity1m:
		return "1 minute"
	case Granularity15m:
		return "15 minutes"
	case Granularity1h:
		return "1 hour"
	case Granularity4h:
		return "4 hours"
	case Granularity1d:
		return "1 day"
	case Granularity1w:
		return "1 week"
	case Granularity1mo:
		return "1 month"
	}
	return ""
}

// HistoryPoints returns every CLOSED bucket for the pair from the
// CAGG matching `granularity`, ordered chronologically (ASC). Used
// by /v1/history/since-inception to serve the full historical
// series at the requested granularity.
//
// Per ADR-0015 the in-progress bucket is excluded via a
// `bucket + <granularity> <= now()` guard.
//
// `limit` clamps the row count; passing 0 returns all rows. The
// API caller passes the spec-bounded value (default unbounded for
// since-inception; clients paginate via Pagination.next when we
// add that surface).
//
// Returns an empty slice + nil error when the pair has no closed
// buckets at this granularity yet.
func (s *Store) HistoryPoints(ctx context.Context, p canonical.Pair, granularity HistoryGranularity, limit int) ([]HistoryPoint, error) {
	if err := granularity.Validate(); err != nil {
		return nil, err
	}
	// SQL injection guard: granularity goes via Validate() against
	// a fixed enum, then composes into the table name. Body of the
	// query plus params binds via $1/$2. The fmt.Sprintf format is
	// safe — both `table` and `interval` come from a closed enum
	// after Validate(), not from user input.
	table := "prices_" + string(granularity)
	interval := granularity.closedBucketInterval()
	// #nosec G201 — table + interval are derived from the validated
	// enum, not user input. See HistoryGranularity.Validate above.
	q := fmt.Sprintf(`
		SELECT bucket, vwap::text, volume_usd::text
		  FROM %s
		 WHERE base_asset = $1
		   AND quote_asset = $2
		   AND bucket + INTERVAL '%s' <= now()
		 ORDER BY bucket ASC
	`, table, interval)
	args := []any{p.Base.String(), p.Quote.String()}
	if limit > 0 {
		q += " LIMIT $3"
		args = append(args, limit)
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("timescale: HistoryPoints[%s]: %w", granularity, err)
	}
	defer func() { _ = rows.Close() }()

	out := make([]HistoryPoint, 0, 1024)
	for rows.Next() {
		var pt HistoryPoint
		var vusd sql.NullString
		if err := rows.Scan(&pt.Bucket, &pt.VWAP, &vusd); err != nil {
			return nil, fmt.Errorf("timescale: HistoryPoints[%s] scan: %w", granularity, err)
		}
		if vusd.Valid {
			s := vusd.String
			pt.VolumeUSD = &s
		}
		out = append(out, pt)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("timescale: HistoryPoints[%s] rows: %w", granularity, err)
	}
	return out, nil
}

// HistoryPointsInRange is [HistoryPoints] with an explicit
// [from, to) time bound on the bucket column. Used by /v1/chart
// to serve a rolling-window view (timeframe → from = now-tf, to =
// now); the same closed-bucket guard from [HistoryPoints] applies.
//
// `from` zero disables the lower bound (equivalent to since-
// inception); `to` zero disables the upper bound. `limit` 0 returns
// all rows.
//
// Empty slice + nil error when the pair has no closed buckets in
// the requested window.
func (s *Store) HistoryPointsInRange(
	ctx context.Context,
	p canonical.Pair,
	granularity HistoryGranularity,
	from, to time.Time,
	limit int,
) ([]HistoryPoint, error) {
	if err := granularity.Validate(); err != nil {
		return nil, err
	}
	table := "prices_" + string(granularity)
	interval := granularity.closedBucketInterval()
	// Build args incrementally so the placeholder count matches the
	// optional from/to/limit clauses.
	args := []any{p.Base.String(), p.Quote.String()}
	clauses := "base_asset = $1\n   AND quote_asset = $2\n   AND bucket + INTERVAL '" + interval + "' <= now()"
	if !from.IsZero() {
		args = append(args, from.UTC())
		clauses += fmt.Sprintf("\n   AND bucket >= $%d", len(args))
	}
	if !to.IsZero() {
		args = append(args, to.UTC())
		clauses += fmt.Sprintf("\n   AND bucket < $%d", len(args))
	}
	// #nosec G201 — table + interval are derived from the validated
	// enum, not user input. See HistoryGranularity.Validate.
	q := fmt.Sprintf(`
		SELECT bucket, vwap::text, volume_usd::text
		  FROM %s
		 WHERE %s
		 ORDER BY bucket ASC
	`, table, clauses)
	if limit > 0 {
		args = append(args, limit)
		q += fmt.Sprintf(" LIMIT $%d", len(args))
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("timescale: HistoryPointsInRange[%s]: %w", granularity, err)
	}
	defer func() { _ = rows.Close() }()

	out := make([]HistoryPoint, 0, 1024)
	for rows.Next() {
		var pt HistoryPoint
		var vusd sql.NullString
		if err := rows.Scan(&pt.Bucket, &pt.VWAP, &vusd); err != nil {
			return nil, fmt.Errorf("timescale: HistoryPointsInRange[%s] scan: %w", granularity, err)
		}
		if vusd.Valid {
			s := vusd.String
			pt.VolumeUSD = &s
		}
		out = append(out, pt)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("timescale: HistoryPointsInRange[%s] rows: %w", granularity, err)
	}
	return out, nil
}

// Vwap1mRow is one row from the prices_1m continuous aggregate.
// The fields mirror migrations/0002_create_price_aggregates.up.sql
// — see that file for the SQL semantics. Bucket is the START of the
// 1-minute window; the window's END is `bucket + 1 minute`.
type Vwap1mRow struct {
	Bucket     time.Time
	BaseAsset  string
	QuoteAsset string
	// VWAP, FirstPrice, LastPrice, HighPrice, LowPrice are decimal
	// strings exactly as Postgres serialises NUMERIC. Storing them
	// as strings avoids a float round-trip (ADR-0003) — handlers
	// that need a numeric value parse with big.Rat.
	VWAP       string
	TradeCount int64
	Sources    []string
}

// RecentClosedVWAP1mForPair returns up to `limit` most-recent CLOSED
// 1-minute buckets from the prices_1m CAGG for the given pair,
// newest first. Same closed-bucket guard as
// [LatestClosedVWAP1mForPair] (ADR-0015).
//
// Returns an empty slice + nil error when the pair has no closed
// buckets in scope. The caller (typically the SEP-40 prices
// endpoint) distinguishes "no observations" from "asset unknown"
// by combining this with an asset-existence check.
//
// limit is the caller's responsibility to clamp; this method
// assumes a sane bound and doesn't second-guess.
func (s *Store) RecentClosedVWAP1mForPair(ctx context.Context, p canonical.Pair, limit int) ([]Vwap1mRow, error) {
	const q = `
        SELECT bucket, base_asset, quote_asset, vwap::text, trade_count, sources
          FROM prices_1m
         WHERE base_asset = $1
           AND quote_asset = $2
           AND bucket + INTERVAL '1 minute' <= now()
         ORDER BY bucket DESC
         LIMIT $3
    `
	rows, err := s.db.QueryContext(ctx, q,
		p.Base.String(), p.Quote.String(), limit,
	)
	if err != nil {
		return nil, fmt.Errorf("timescale: RecentClosedVWAP1mForPair: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := make([]Vwap1mRow, 0, limit)
	for rows.Next() {
		var row Vwap1mRow
		if err := rows.Scan(
			&row.Bucket, &row.BaseAsset, &row.QuoteAsset,
			&row.VWAP, &row.TradeCount,
			(*stringArray)(&row.Sources),
		); err != nil {
			return nil, fmt.Errorf("timescale: RecentClosedVWAP1mForPair scan: %w", err)
		}
		normalizeVwapSources(&row)
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("timescale: RecentClosedVWAP1mForPair rows: %w", err)
	}
	return out, nil
}

// ClosedVWAP1mAtOrBefore returns the most-recent CLOSED 1-minute
// bucket from the prices_1m CAGG for the given pair whose end
// timestamp (`bucket + 1 minute`) is at or before t. Used by
// /v1/assets/{id}'s change_24h_pct path to anchor the
// 24-hours-ago comparison price.
//
// Same closed-bucket guard as [LatestClosedVWAP1mForPair]
// (ADR-0015) — the open bucket is excluded. Returns
// [sql.ErrNoRows] when no closed bucket exists at-or-before t
// (e.g. the pair was first traded < 24h ago, or the prices_1m
// retention horizon (30 d) elided the row).
func (s *Store) ClosedVWAP1mAtOrBefore(ctx context.Context, p canonical.Pair, t time.Time) (Vwap1mRow, error) {
	const q = `
        SELECT bucket, base_asset, quote_asset, vwap::text, trade_count, sources
          FROM prices_1m
         WHERE base_asset = $1
           AND quote_asset = $2
           AND bucket + INTERVAL '1 minute' <= $3
         ORDER BY bucket DESC
         LIMIT 1
    `
	var row Vwap1mRow
	err := s.db.QueryRowContext(ctx, q,
		p.Base.String(), p.Quote.String(), t,
	).Scan(
		&row.Bucket,
		&row.BaseAsset,
		&row.QuoteAsset,
		&row.VWAP,
		&row.TradeCount,
		(*stringArray)(&row.Sources),
	)
	if errors.Is(err, sql.ErrNoRows) {
		return Vwap1mRow{}, sql.ErrNoRows
	}
	if err != nil {
		return Vwap1mRow{}, fmt.Errorf("timescale: ClosedVWAP1mAtOrBefore: %w", err)
	}
	normalizeVwapSources(&row)
	return row, nil
}

// LatestClosedVWAP1mForPair returns the most-recent CLOSED 1-minute
// bucket from the prices_1m CAGG for the given pair. Per ADR-0015
// the API serves only closed buckets — this method explicitly
// excludes the in-progress bucket via a `bucket + 1 minute <= now()`
// guard, even though the CAGG's refresh policy already drops the
// open bucket from materialised rows.
//
// Returns [sql.ErrNoRows] when the pair has no closed bucket yet —
// callers translate that to the API's price-not-found problem or
// fall back to the latest-trade path.
func (s *Store) LatestClosedVWAP1mForPair(ctx context.Context, p canonical.Pair) (Vwap1mRow, error) {
	// Combine BOTH stored directions of the market into the requested
	// orientation. The SDEX decoder records the same market both ways
	// (XLM/USDC and USDC/XLM), so reading only (base=$1, quote=$2) used
	// half the liquidity — and returned ErrNoRows if the latest minute
	// happened to trade only the flipped way. We read both, and for the
	// flipped rows invert the vwap (1/vwap) so every row expresses the
	// price of $1 in $2, then trade-count-weight them within the latest
	// closed bucket. Closed-bucket-only (ADR-0015) is preserved, and the
	// combine is deterministic across regions.
	// Find the latest closed bucket via the (base,quote,bucket DESC)
	// index — one fast max() per direction, UNIONed — then point-read +
	// combine just that bucket's 1-2 rows. The earlier form scanned the
	// pair's ENTIRE prices_1m history (back to 2015) before LIMIT 1,
	// which measured ~1s warm and ballooned to ~9s under load (it drove
	// a latency-burn incident on 2026-06-19).
	//
	// PERF (two layers, both required — 2026-06-20 latency-burn incident):
	//
	//  1. The "closed bucket" predicate MUST be `bucket <= now() - 1min`, NOT
	//     `bucket + 1min <= now()`. The latter is a function on the indexed
	//     `bucket` column → non-sargable → max() runs a per-chunk partial
	//     aggregate over the WHOLE history. The sargable form lets max() read
	//     the newest chunk via the index (446ms → 26ms execution).
	//  2. That still left ~280ms of PLANNING time: prices_1m has ~374 chunks,
	//     and `now()` is only known at RUN time, so TimescaleDB does runtime
	//     (startup) chunk exclusion — the PLANNER still enumerates all 374
	//     chunks. We add a LITERAL recent lower bound (`bucket >= <cutoff>`,
	//     cutoff computed in Go) so the planner excludes old chunks at PLAN
	//     time, collapsing planning to ~2ms. The literal is our own UTC
	//     timestamp — no injection surface.
	//
	// A single bounded query — NO unbounded fallback. An earlier two-tier
	// (bounded → unbounded) made the no-data case slow again: the handler
	// reads native/fiat:USD as an alias on every XLM query, that synthetic
	// pair has zero rows, so the bounded miss fell through to the slow
	// all-chunk scan finding nothing. A pair with no closed bucket in the
	// (generous, ~400-day) window returns ErrNoRows, which the price handler
	// already resolves via its Redis-triangulation / last-trade fallback
	// chain — the right path for a synthetic pair, and the honest answer for
	// a genuinely-dead asset (a >400-day-old "latest" is not a current price).
	// The window is wide enough to cover every actively- or recently-priced
	// pair (the staleness long tail is ~250k delisted assets) yet still lets
	// the planner exclude the bulk of prices_1m's ~374 chunks at PLAN time.
	cutoff := time.Now().UTC().Add(-latestVWAPWindow)
	return s.latestClosedVWAP1m(ctx, p, cutoff)
}

// latestVWAPWindow bounds the lookback for [LatestClosedVWAP1mForPair]. A
// LITERAL cutoff this many days back is interpolated into the query so the
// planner prunes old chunks at PLAN time (planning ~6ms vs ~280ms unbounded).
// 400 days is generous — it covers every recently-traded pair while still
// excluding most of prices_1m's decade of chunks.
const latestVWAPWindow = 400 * 24 * time.Hour

// latestClosedVWAP1m runs the combined-direction latest-closed-bucket query
// with a LITERAL `bucket >= since` lower bound on every prices_1m scan, so the
// planner prunes old chunks at PLAN time (see [LatestClosedVWAP1mForPair]).
// Returns [sql.ErrNoRows] when no closed bucket exists for the pair within the
// window.
//
// latestClosedVWAP1mTemplate is the query with a single `%[1]s` slot for the
// literal lower-bound clause, repeated on all three prices_1m scans.
const latestClosedVWAP1mTemplate = `
        WITH latest AS (
            SELECT max(b) AS b FROM (
                SELECT max(bucket) AS b FROM prices_1m
                 WHERE base_asset = $1 AND quote_asset = $2
                   AND bucket <= now() - INTERVAL '1 minute'
                   %[1]s
                UNION ALL
                SELECT max(bucket) AS b FROM prices_1m
                 WHERE base_asset = $2 AND quote_asset = $1
                   AND bucket <= now() - INTERVAL '1 minute'
                   %[1]s
            ) u
        ),
        r AS (
            SELECT base_asset, vwap, COALESCE(trade_count, 0) AS tc, sources
              FROM prices_1m
             WHERE bucket = (SELECT b FROM latest)
               %[1]s
               AND ((base_asset = $1 AND quote_asset = $2)
                 OR (base_asset = $2 AND quote_asset = $1))
        )
        SELECT (SELECT b FROM latest), $1::text, $2::text,
               (SUM((CASE WHEN base_asset = $1 THEN vwap
                          ELSE 1.0 / NULLIF(vwap, 0) END) * tc)
                  / NULLIF(SUM(tc), 0))::text AS vwap,
               SUM(tc)::bigint AS trade_count,
               (SELECT array_agg(DISTINCT sc) FROM r r2, unnest(r2.sources) sc) AS sources
          FROM r
         HAVING count(*) > 0
    `

func (s *Store) latestClosedVWAP1m(ctx context.Context, p canonical.Pair, since time.Time) (Vwap1mRow, error) {
	// Literal lower bound, interpolated (our own timestamp, not user input).
	// Applied to both max() arms AND the point-read so all three prices_1m
	// scans get plan-time chunk exclusion.
	lower := fmt.Sprintf("AND bucket >= TIMESTAMPTZ '%s'\n", since.Format("2006-01-02 15:04:05-07"))
	// G201 is suppressed below: the ONLY interpolated value is `lower`, built
	// from our own time.Time formatted to a fixed layout — never user input, no
	// injection surface. The pair strings stay bound parameters ($1/$2). The
	// literal (vs a $N bind parameter) is REQUIRED: TimescaleDB only does
	// plan-time chunk exclusion for a constant, not a bind parameter.
	q := fmt.Sprintf(latestClosedVWAP1mTemplate, lower) //nolint:gosec // G201: see note above
	var row Vwap1mRow
	err := s.db.QueryRowContext(ctx, q,
		p.Base.String(), p.Quote.String(),
	).Scan(
		&row.Bucket,
		&row.BaseAsset,
		&row.QuoteAsset,
		&row.VWAP,
		&row.TradeCount,
		(*stringArray)(&row.Sources),
	)
	if errors.Is(err, sql.ErrNoRows) {
		return Vwap1mRow{}, sql.ErrNoRows
	}
	if err != nil {
		return Vwap1mRow{}, fmt.Errorf("timescale: LatestClosedVWAP1mForPair: %w", err)
	}
	normalizeVwapSources(&row)
	return row, nil
}

// TimedVWAPsForPair1m returns chronologically-ordered (oldest-first)
// (vwap, bucket_end) pairs from prices_1m for the half-open window
// [from, to). Used by the multi-window baseline refresher (which
// needs the timestamp to apply [baseline.SplitByLookback] for the
// 1d / 7d / 30d sub-windows).
//
// The bucket-end timestamp is the bucket's start + 1 minute (CAGG
// stores the start; the API surface uses the end). Empty slice +
// nil error when the pair has no closed buckets in the window.
func (s *Store) TimedVWAPsForPair1m(ctx context.Context, p canonical.Pair, from, to time.Time) ([]baseline.TimedVWAP, error) {
	if !to.After(from) {
		return nil, fmt.Errorf("timescale: TimedVWAPsForPair1m: to %v <= from %v", to, from)
	}
	// Combine BOTH stored directions of the market into the requested
	// ($1, $2) orientation — same fix as LatestClosedVWAP1mForPair. The
	// SDEX decoder records the same market both ways (XLM/USDC and
	// USDC/XLM); reading only (base=$1, quote=$2) fed the anomaly
	// baseline half the liquidity (and no rows at all for minutes that
	// traded only the flipped way). Per bucket we invert the flipped
	// rows' vwap (1/vwap) and trade-count-weight, so every point
	// expresses the price of $1 in $2. See canonical.Orient.
	const q = `
        SELECT (SUM((CASE WHEN base_asset = $1 THEN vwap
                          ELSE 1.0 / NULLIF(vwap, 0) END) * COALESCE(trade_count, 0))
                  / NULLIF(SUM(COALESCE(trade_count, 0)), 0))::float8 AS vwap,
               bucket + INTERVAL '1 minute'
          FROM prices_1m
         WHERE ((base_asset = $1 AND quote_asset = $2)
             OR (base_asset = $2 AND quote_asset = $1))
           AND bucket >= $3
           AND bucket <  $4
         GROUP BY bucket
         ORDER BY bucket ASC
    `
	rows, err := s.db.QueryContext(ctx, q,
		p.Base.String(), p.Quote.String(),
		from.UTC(), to.UTC(),
	)
	if err != nil {
		return nil, fmt.Errorf("timescale: TimedVWAPsForPair1m: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := make([]baseline.TimedVWAP, 0, 256)
	for rows.Next() {
		var t baseline.TimedVWAP
		if err := rows.Scan(&t.VWAP, &t.BucketEnd); err != nil {
			return nil, fmt.Errorf("timescale: TimedVWAPsForPair1m scan: %w", err)
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("timescale: TimedVWAPsForPair1m rows: %w", err)
	}
	return out, nil
}

// VWAPsForPair1m returns chronologically-ordered (oldest-first) VWAP
// values from prices_1m where bucket falls in [from, to). Used by
// the baseline refresher to pull the 30-day training window for a
// pair. Returns the bare float series (not the full Vwap1mRow) —
// the caller's downstream consumer (`baseline.ReturnsFromVWAPs`)
// only needs the prices, not the metadata.
//
// Empty slice + nil error when the pair has no closed buckets in
// the window.
//
// VWAP is parsed from the NUMERIC column via the Postgres double-
// precision cast — the baseline math runs in float64 anyway and
// the small precision loss on a per-bucket VWAP doesn't matter for
// statistical aggregates over hundreds of buckets.
func (s *Store) VWAPsForPair1m(ctx context.Context, p canonical.Pair, from, to time.Time) ([]float64, error) {
	if !to.After(from) {
		return nil, fmt.Errorf("timescale: VWAPsForPair1m: to %v <= from %v", to, from)
	}
	// Combine BOTH stored directions into the requested ($1, $2)
	// orientation (invert + trade-count-weight the flipped rows), so the
	// 30-day baseline training window reflects the full market and not
	// just the direction the CAGG happened to store. See
	// TimedVWAPsForPair1m / canonical.Orient.
	const q = `
        SELECT (SUM((CASE WHEN base_asset = $1 THEN vwap
                          ELSE 1.0 / NULLIF(vwap, 0) END) * COALESCE(trade_count, 0))
                  / NULLIF(SUM(COALESCE(trade_count, 0)), 0))::float8 AS vwap
          FROM prices_1m
         WHERE ((base_asset = $1 AND quote_asset = $2)
             OR (base_asset = $2 AND quote_asset = $1))
           AND bucket >= $3
           AND bucket <  $4
         GROUP BY bucket
         ORDER BY bucket ASC
    `
	rows, err := s.db.QueryContext(ctx, q,
		p.Base.String(), p.Quote.String(),
		from.UTC(), to.UTC(),
	)
	if err != nil {
		return nil, fmt.Errorf("timescale: VWAPsForPair1m: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := make([]float64, 0, 256)
	for rows.Next() {
		var v float64
		if err := rows.Scan(&v); err != nil {
			return nil, fmt.Errorf("timescale: VWAPsForPair1m scan: %w", err)
		}
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("timescale: VWAPsForPair1m rows: %w", err)
	}
	return out, nil
}

// stringArray is a [sql.Scanner] for Postgres TEXT[] / VARCHAR[]
// columns scanning into a Go []string. Used by the `sources` column
// in prices_1m.
//
// Implements minimal parsing of the Postgres array text format:
// `{a,b,c}`. Quoted entries (with embedded commas) aren't supported
// — fine here because source names are identifier-shaped.
type stringArray []string

// normalizeVwapSources enforces a stable lexical ordering for the
// materialized-view `sources` array before it crosses the storage
// boundary. The current CAGG SQL uses `array_agg(DISTINCT source)`
// without an ORDER BY, so sorting here restores the public contract's
// deterministic contributor ordering for both existing and future rows
// without requiring an immediate CAGG rebuild.
func normalizeVwapSources(row *Vwap1mRow) {
	if len(row.Sources) < 2 {
		return
	}
	sort.Strings(row.Sources)
}

// Scan implements [sql.Scanner].
func (a *stringArray) Scan(src any) error {
	if src == nil {
		*a = nil
		return nil
	}
	var s string
	switch v := src.(type) {
	case []byte:
		s = string(v)
	case string:
		s = v
	default:
		return fmt.Errorf("stringArray: unsupported scan type %T", src)
	}
	if len(s) < 2 || s[0] != '{' || s[len(s)-1] != '}' {
		return fmt.Errorf("stringArray: malformed Postgres array literal %q", s)
	}
	inner := s[1 : len(s)-1]
	if inner == "" {
		*a = []string{}
		return nil
	}
	out := []string{}
	start := 0
	for i := 0; i <= len(inner); i++ {
		if i == len(inner) || inner[i] == ',' {
			elt := inner[start:i]
			// NULL elements come through as the literal "NULL"
			// (case-sensitive); array_agg(DISTINCT source) over a
			// non-null source column never produces these, but
			// guard anyway.
			if elt != "NULL" {
				out = append(out, elt)
			}
			start = i + 1
		}
	}
	*a = out
	return nil
}

// Volume24hUSDForAsset returns the trailing 24h USD-denominated
// trade volume for an asset, summing across every pair the asset
// participates in (as base OR quote). Reads from the prices_1m
// CAGG (1440 rolled buckets per 24h) — far cheaper than scanning
// the trades hypertable directly.
//
// Returns "0" when the asset has zero trades in the window;
// callers presenting the field should treat zero distinctly from
// null (zero = "asset existed and had no trades", null = "asset
// not tracked"). The string return matches the rest of the
// aggregate API — Postgres NUMERIC sums don't fit cleanly into
// any Go fixed-width type.
//
// assetKey is the canonical asset string (e.g.
// "native", "USDC:GA5...", "soroban:CC..."); matches what the
// trades hypertable stores in base_asset / quote_asset.
//
// # Scope caveat (launch-readiness L2.2)
//
// The CAGG sums `coalesce(usd_volume, 0)` per row. Per-trade
// `usd_volume` is populated at insert time (see `tradeUSDVolume`
// in trades.go) for:
//   - off-chain CEX/FX sources with `fiat:USD` or
//     USD-pegged-stablecoin quotes (uniform 10^8 external scale),
//   - on-chain DEX sources whose quote asset is in the operator's
//     `[trades].usd_pegged_classic_assets` allow-list or its SAC
//     wrapper, transitive via `[supply.sac_wrappers]` (L2.2 phase 1).
//
// Deployments that haven't configured the trades allow-list see
// on-chain trades contribute 0 to this sum (the pre-Phase-1
// default). On-chain trades quoted in non-USD assets (XLM/AQUA,
// XLM/BTC) still contribute 0; FX-anchor multiplication for
// non-USD on-chain quotes is L2.2 phase 2 (post-launch). The
// OpenAPI surface (`volume_24h_usd`) carries the same caveat.
func (s *Store) Volume24hUSDForAsset(ctx context.Context, assetKey string) (string, error) {
	const q = `
        SELECT COALESCE(sum(volume_usd), 0)::text
          FROM prices_1m
         WHERE (base_asset = $1 OR quote_asset = $1)
           AND bucket >= now() - INTERVAL '24 hours'
           AND bucket  < now()
    `
	var out string
	if err := s.db.QueryRowContext(ctx, q, assetKey).Scan(&out); err != nil {
		return "", fmt.Errorf("timescale: Volume24hUSDForAsset(%s): %w", assetKey, err)
	}
	return out, nil
}

// OHLCBar is one bucket of OHLC + volume + trade-count returned by
// [Store.OHLCSeries] / [Store.OHLCSeriesReBucketed]. Bucket is the
// START of the window; window end = bucket + interval.
//
// All price fields are decimal strings from the Postgres NUMERIC
// column (no float round-trip). Volume fields are integer-string
// sums (base/quote stroops). TradeCount is the row count.
type OHLCBar struct {
	Bucket      time.Time
	Open        string
	High        string
	Low         string
	Close       string
	BaseVolume  string
	QuoteVolume string
	TradeCount  int64
}

// OHLCSeries returns chronologically-ordered (oldest-first) OHLC
// bars from the CAGG matching `granularity` for the half-open
// window [from, to). Used by /v1/ohlc's multi-bar mode (F-0071).
//
// Bucket rule: the CAGG's native bucket size IS the interval, so
// rows map 1:1 to bars — no SQL-side re-bucketing. Callers that
// need a non-CAGG-native interval (5m, 30m, 4h) route through
// [Store.OHLCSeriesReBucketed].
//
// Per ADR-0015 the in-progress bucket is excluded via a
// `bucket + <interval> <= now()` guard. `limit` clamps row count
// (0 = unbounded). Returns empty slice + nil error when no
// closed buckets exist in window.
//
// `quote_amount` is derived as `vwap * volume` at SELECT time:
// VWAP is defined as Σ(price·base) / Σ(base), so vwap·Σ(base) =
// Σ(price·base) = Σ(quote). This is exact in NUMERIC arithmetic
// — no precision loss vs storing volume_quote in the CAGG.
func (s *Store) OHLCSeries(
	ctx context.Context,
	p canonical.Pair,
	granularity HistoryGranularity,
	from, to time.Time,
	limit int,
) ([]OHLCBar, error) {
	if err := granularity.Validate(); err != nil {
		return nil, err
	}
	if !to.After(from) {
		return nil, fmt.Errorf("timescale: OHLCSeries: to %v <= from %v", to, from)
	}
	table := "prices_" + string(granularity)
	interval := granularity.closedBucketInterval()
	// Combine BOTH stored directions of the market into the requested
	// ($1, $2) orientation (the SDEX decoder records XLM/USDC and
	// USDC/XLM as separate rows). The `norm` CTE re-expresses each row in
	// the requested orientation: flipped rows invert every price
	// (1/price) — which SWAPS high↔low — and swap base↔quote volume. Then
	// per bucket: high = max, low = min (order-independent extrema);
	// open/close prefer the requested-direction row and fall back to the
	// inverted flipped row (their intra-bucket ordering across directions
	// is unknowable from the CAGG); base/quote volume + trade_count sum.
	// See canonical.Orient / canonOrientSQL.
	// #nosec G201 — table + interval are derived from the validated
	// HistoryGranularity enum, not user input. See Validate.
	q := fmt.Sprintf(`
		WITH norm AS (
		    SELECT
		        bucket,
		        (base_asset = $1) AS req,
		        CASE WHEN base_asset = $1 THEN first_price ELSE 1.0 / NULLIF(first_price, 0) END AS o,
		        CASE WHEN base_asset = $1 THEN last_price  ELSE 1.0 / NULLIF(last_price, 0)  END AS c,
		        CASE WHEN base_asset = $1 THEN high_price  ELSE 1.0 / NULLIF(low_price, 0)   END AS hi,
		        CASE WHEN base_asset = $1 THEN low_price   ELSE 1.0 / NULLIF(high_price, 0)  END AS lo,
		        CASE WHEN base_asset = $1 THEN volume        ELSE vwap * volume END AS base_vol,
		        CASE WHEN base_asset = $1 THEN vwap * volume ELSE volume        END AS quote_vol,
		        trade_count AS tc
		      FROM %s
		     WHERE ((base_asset = $1 AND quote_asset = $2)
		         OR (base_asset = $2 AND quote_asset = $1))
		       AND bucket >= $3
		       AND bucket <  $4
		       AND bucket + INTERVAL '%s' <= now()
		)
		SELECT
		    bucket,
		    COALESCE((array_agg(o) FILTER (WHERE req))[1], (array_agg(o))[1])::text AS open,
		    max(hi)::text                                                           AS high,
		    min(lo)::text                                                           AS low,
		    COALESCE((array_agg(c) FILTER (WHERE req))[1], (array_agg(c))[1])::text AS close,
		    sum(base_vol)::text                                                     AS base_volume,
		    sum(quote_vol)::text                                                    AS quote_volume,
		    sum(tc)::bigint                                                         AS trade_count
		  FROM norm
		 GROUP BY bucket
		 ORDER BY bucket ASC
	`, table, interval)
	args := []any{p.Base.String(), p.Quote.String(), from.UTC(), to.UTC()}
	if limit > 0 {
		q += fmt.Sprintf(" LIMIT $%d", len(args)+1)
		args = append(args, limit)
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("timescale: OHLCSeries[%s]: %w", granularity, err)
	}
	defer func() { _ = rows.Close() }()

	out := make([]OHLCBar, 0, 128)
	for rows.Next() {
		var bar OHLCBar
		if err := rows.Scan(
			&bar.Bucket,
			&bar.Open, &bar.High, &bar.Low, &bar.Close,
			&bar.BaseVolume, &bar.QuoteVolume, &bar.TradeCount,
		); err != nil {
			return nil, fmt.Errorf("timescale: OHLCSeries[%s] scan: %w", granularity, err)
		}
		out = append(out, bar)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("timescale: OHLCSeries[%s] rows: %w", granularity, err)
	}
	return out, nil
}

// OHLCSeriesReBucketed is [Store.OHLCSeries] but re-buckets the
// source CAGG's rows into a coarser `outInterval` via Postgres
// `time_bucket`. Supports the requested intervals that don't have
// a native CAGG (5m, 30m, 4h) while still reading from a CAGG
// rather than the trades hypertable. Folds N source buckets into
// one output bucket per the standard OHLC roll-up:
//
//   - open  = first_price ORDERED BY bucket ASC  (first input bar's open)
//   - close = last_price  ORDERED BY bucket ASC  (last input bar's close)
//   - high  = max(high_price)
//   - low   = min(low_price)
//   - base_volume  = Σ volume
//   - quote_volume = Σ (vwap * volume)  (Σ quote per the VWAP identity)
//   - trade_count  = Σ trade_count
//
// `outInterval` MUST be an integer multiple of the source CAGG's
// native bucket size — caller responsibility (e.g. granularity=1m
// + outInterval='5 minutes'). Postgres time_bucket snaps to its
// configured origin, which for our CAGGs is the Unix epoch (UTC).
// 5m buckets land at 12:00/12:05/12:10..., 4h at 00:00/04:00/...
//
// `outInterval` composes directly into the SQL after a literal
// allow-list check — never user-passed verbatim. Same ADR-0015
// closed-bucket guard as [Store.OHLCSeries].
func (s *Store) OHLCSeriesReBucketed(
	ctx context.Context,
	p canonical.Pair,
	sourceGranularity HistoryGranularity,
	outInterval string,
	from, to time.Time,
	limit int,
) ([]OHLCBar, error) {
	if err := sourceGranularity.Validate(); err != nil {
		return nil, err
	}
	if !to.After(from) {
		return nil, fmt.Errorf("timescale: OHLCSeriesReBucketed: to %v <= from %v", to, from)
	}
	// Allow-list — outInterval composes directly into the SQL, so
	// it MUST NOT come from untrusted input. The handler maps
	// fixed-enum interval strings to these Postgres literals.
	switch outInterval {
	case "5 minutes", "15 minutes", "30 minutes", "1 hour", "4 hours",
		"1 day", "1 week":
	default:
		return nil, fmt.Errorf("timescale: OHLCSeriesReBucketed: outInterval %q not in allow-list", outInterval)
	}
	table := "prices_" + string(sourceGranularity)
	// Combine BOTH stored directions of the market before re-bucketing:
	// the `norm` CTE collapses each SOURCE bucket's two directions into
	// one bar expressed in the requested ($1, $2) orientation (flipped
	// rows invert prices — swapping high↔low — and swap base↔quote
	// volume; open/close prefer the requested direction), and the outer
	// query folds those normalized source bars into the coarser
	// out_bucket. See OHLCSeries / canonical.Orient.
	// #nosec G201 — table comes from the validated enum;
	// outInterval comes from the allow-list above. No user input
	// reaches the SQL string.
	q := fmt.Sprintf(`
		WITH norm AS (
		    SELECT bucket,
		           COALESCE((array_agg(o) FILTER (WHERE req))[1], (array_agg(o))[1]) AS open,
		           max(hi) AS high,
		           min(lo) AS low,
		           COALESCE((array_agg(c) FILTER (WHERE req))[1], (array_agg(c))[1]) AS close,
		           sum(base_vol)  AS base_vol,
		           sum(quote_vol) AS quote_vol,
		           sum(tc)        AS tc
		      FROM (
		        SELECT bucket, (base_asset = $1) AS req,
		               CASE WHEN base_asset = $1 THEN first_price ELSE 1.0 / NULLIF(first_price, 0) END AS o,
		               CASE WHEN base_asset = $1 THEN last_price  ELSE 1.0 / NULLIF(last_price, 0)  END AS c,
		               CASE WHEN base_asset = $1 THEN high_price  ELSE 1.0 / NULLIF(low_price, 0)   END AS hi,
		               CASE WHEN base_asset = $1 THEN low_price   ELSE 1.0 / NULLIF(high_price, 0)  END AS lo,
		               CASE WHEN base_asset = $1 THEN volume        ELSE vwap * volume END AS base_vol,
		               CASE WHEN base_asset = $1 THEN vwap * volume ELSE volume        END AS quote_vol,
		               trade_count AS tc
		          FROM %[1]s
		         WHERE ((base_asset = $1 AND quote_asset = $2)
		             OR (base_asset = $2 AND quote_asset = $1))
		           AND bucket >= $3
		           AND bucket <  $4
		      ) raw
		     GROUP BY bucket
		)
		SELECT
		    time_bucket(INTERVAL '%[2]s', bucket)                       AS out_bucket,
		    (array_agg(open  ORDER BY bucket ASC))[1]::text             AS open,
		    max(high)::text                                             AS high,
		    min(low)::text                                              AS low,
		    (array_agg(close ORDER BY bucket DESC))[1]::text            AS close,
		    sum(base_vol)::text                                         AS base_volume,
		    sum(quote_vol)::text                                        AS quote_volume,
		    sum(tc)::bigint                                             AS trade_count
		  FROM norm
		 GROUP BY out_bucket
		 HAVING time_bucket(INTERVAL '%[2]s', bucket) + INTERVAL '%[2]s' <= now()
		 ORDER BY out_bucket ASC
	`, table, outInterval)
	args := []any{p.Base.String(), p.Quote.String(), from.UTC(), to.UTC()}
	if limit > 0 {
		q += fmt.Sprintf(" LIMIT $%d", len(args)+1)
		args = append(args, limit)
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("timescale: OHLCSeriesReBucketed[%s→%s]: %w", sourceGranularity, outInterval, err)
	}
	defer func() { _ = rows.Close() }()

	out := make([]OHLCBar, 0, 128)
	for rows.Next() {
		var bar OHLCBar
		if err := rows.Scan(
			&bar.Bucket,
			&bar.Open, &bar.High, &bar.Low, &bar.Close,
			&bar.BaseVolume, &bar.QuoteVolume, &bar.TradeCount,
		); err != nil {
			return nil, fmt.Errorf("timescale: OHLCSeriesReBucketed[%s→%s] scan: %w", sourceGranularity, outInterval, err)
		}
		out = append(out, bar)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("timescale: OHLCSeriesReBucketed[%s→%s] rows: %w", sourceGranularity, outInterval, err)
	}
	return out, nil
}
