package timescale

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"math/big"
	"sort"
	"strings"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// FXQuote is one (date, ticker) snapshot from the forex pipeline.
// Rates are NUMERIC in the DB. RateUSD is the write input; the read
// path returns both columns as exact NUMERIC text so no served price
// passes through a float (ADR-0003).
type FXQuote struct {
	Bucket time.Time
	Ticker string
	//floatmoney:ok known debt — write-input boundary from the forex ingest pipeline (worker.go/cache.go RateUSD), which is float end to end today; the read side already returns NUMERIC text (RateUSDText) per the doc comment above
	RateUSD float64
	// InverseUSD is IGNORED on write: [Store.InsertFXQuoteBatch] derives
	// inverse_usd from rate_usd in NUMERIC so the column never carries a
	// float64 quotient. Unset on read; use InverseUSDText.
	InverseUSD float64
	// RateUSDText and InverseUSDText are rate_usd and inverse_usd's exact
	// NUMERIC text on read (unset on write).
	RateUSDText    string
	InverseUSDText string
	Source         string
}

// InsertFXQuoteBatch upserts a slice of fx quotes. Idempotent on the
// (ticker, bucket) primary key — re-running with the same (ticker, date)
// updates `rate_usd` + `inverse_usd` + `source`, keeping the original
// `observed_at` (the DEFAULT does not fire on UPDATE; the first observation
// date is the more useful diagnostic).
//
// Generation-guarded corrective upsert (migration 0141): rate_usd is the
// denominator of every fiat-quoted usd_volume, so the DO UPDATE is guarded by
// `derive_generation <= EXCLUDED.derive_generation`. The live forex worker
// writes at generation 0; the operator fx-history-backfill tool stamps a
// POSITIVE generation ([SetDeriveGeneration]) so its corrected rate wins the
// conflict AND survives a later live gen-0 refresh. A gen-0-over-gen-0 write
// re-writes the same row.
//
// The per-source `entries` tally (source_entry_counts, migration 0035) is
// bumped INLINE, as the trades / oracle_updates inserts do: `xmax = 0` marks
// a genuinely new row, and the HAVING clause makes the counter upsert produce
// nothing on a duplicate, an update or a generation-guard skip, so a re-run
// never inflates the tally and [Store.SeedSourceEntryCounts]'s fx_quotes fold
// reconciles to the same number.
//
// Empty slice is a no-op.
func (s *Store) InsertFXQuoteBatch(ctx context.Context, quotes []FXQuote) error {
	if len(quotes) == 0 {
		return nil
	}
	// inverse_usd is derived from rate_usd INSIDE the statement, in
	// NUMERIC. The worker's float64 reciprocal (1.0 / rate) carries a
	// second rounding the stored rate does not, so the cached column and
	// the exact 1/rate_usd the money path computes disagreed in the last
	// digits; the served /v1/chart and asset-listing fiat legs read this
	// column. One rate, one reciprocal, both NUMERIC (ADR-0003).
	const stmt = `
		WITH ins AS (
			INSERT INTO fx_quotes (bucket, ticker, rate_usd, inverse_usd, source, derive_generation)
			VALUES ($1, $2, $3, 1::numeric / $3::numeric, $4, $5)
			ON CONFLICT (ticker, bucket) DO UPDATE
			   SET rate_usd          = EXCLUDED.rate_usd,
			       inverse_usd       = EXCLUDED.inverse_usd,
			       source            = EXCLUDED.source,
			       derive_generation = EXCLUDED.derive_generation
			 WHERE fx_quotes.derive_generation <= EXCLUDED.derive_generation
			RETURNING (xmax = 0) AS inserted
		)
		INSERT INTO source_entry_counts AS sec (source, entry_count, updated_at)
		SELECT $4, count(*) FILTER (WHERE inserted), now() FROM ins
		HAVING count(*) FILTER (WHERE inserted) > 0
		ON CONFLICT (source) DO UPDATE
		  SET entry_count = sec.entry_count + EXCLUDED.entry_count,
		      updated_at  = EXCLUDED.updated_at
	`
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("timescale: InsertFXQuoteBatch begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	for _, q := range quotes {
		// IsNaN is not redundant. Every comparison with NaN is false in
		// Go, so `<= 0` lets a NaN rate through — and Postgres does not
		// catch it either: it orders NaN ABOVE every numeric, so the
		// column's own `CHECK (rate_usd > 0)` accepts it, and the driver
		// passes a float64 NaN through as a `NaN` the numeric type takes.
		// A NaN rate would carry a NaN inverse with it, and
		// NaN is absorbing under sum(), so every usd_volume derived from
		// that ticker — and every prices_* CAGG bucket containing one —
		// becomes NaN. Latent today only because the one live producer
		// pre-filters non-finite values; this is an exported method with
		// a second caller, and both the guard and the CHECK read as
		// "positive rates only".
		if q.Ticker == "" || math.IsNaN(q.RateUSD) || math.IsInf(q.RateUSD, 0) || q.RateUSD <= 0 {
			continue
		}
		if _, err := tx.ExecContext(ctx, stmt,
			q.Bucket, q.Ticker, q.RateUSD, q.Source, s.deriveGeneration,
		); err != nil {
			return fmt.Errorf("timescale: InsertFXQuoteBatch ticker=%q: %w", q.Ticker, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("timescale: InsertFXQuoteBatch commit: %w", err)
	}
	return nil
}

// ListFXHistory returns daily snapshots for `ticker` in
// [from, to], ascending. Empty slice when nothing matches.
//
// Used by /v1/currencies/{ticker} to populate `history_1y`,
// `history_all`, etc. on the response.
func (s *Store) ListFXHistory(ctx context.Context, ticker string, from, to time.Time) ([]FXQuote, error) {
	const stmt = `
		SELECT bucket, ticker, rate_usd::text, inverse_usd::text, COALESCE(source, '')
		  FROM fx_quotes
		 WHERE ticker = $1
		   AND bucket BETWEEN $2 AND $3
		 ORDER BY bucket ASC
	`
	rows, err := s.db.QueryContext(ctx, stmt, ticker, from, to)
	if err != nil {
		return nil, fmt.Errorf("timescale: ListFXHistory: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []FXQuote
	for rows.Next() {
		var q FXQuote
		if err := rows.Scan(&q.Bucket, &q.Ticker, &q.RateUSDText, &q.InverseUSDText, &q.Source); err != nil {
			return nil, fmt.Errorf("timescale: ListFXHistory scan: %w", err)
		}
		out = append(out, q)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("timescale: ListFXHistory rows: %w", err)
	}
	return out, nil
}

// LatestFXQuotes returns the newest fx_quotes row per ticker whose bucket
// is at or after since, rates as exact NUMERIC text. The forex worker
// seeds its held rates from it on cold start.
func (s *Store) LatestFXQuotes(ctx context.Context, since time.Time) ([]FXQuote, error) {
	const stmt = `
		SELECT DISTINCT ON (ticker)
		       bucket, ticker, rate_usd::text, inverse_usd::text, COALESCE(source, '')
		  FROM fx_quotes
		 WHERE bucket >= $1::timestamptz
		 ORDER BY ticker, bucket DESC
	`
	rows, err := s.db.QueryContext(ctx, stmt, since.UTC())
	if err != nil {
		return nil, fmt.Errorf("timescale: LatestFXQuotes: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []FXQuote
	for rows.Next() {
		var q FXQuote
		if err := rows.Scan(&q.Bucket, &q.Ticker, &q.RateUSDText, &q.InverseUSDText, &q.Source); err != nil {
			return nil, fmt.Errorf("timescale: LatestFXQuotes scan: %w", err)
		}
		if q.Source == "" {
			q.Source = fxQuotesSourceLabel
		}
		out = append(out, q)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("timescale: LatestFXQuotes rows: %w", err)
	}
	return out, nil
}

// ─── forex-snap read path (fx_quotes-first) ─────────────────────────
//
// The connector-path FX sources (exchangeratesapi / ecb) that
// external.FXSources() selects from the `trades` hypertable are DISABLED
// in production. The ACTIVE FX feed (`massive`, the
// internal/sources/external/forex worker) writes the `fx_quotes`
// hypertable instead, so a trades-only triangulation forex-snap
// ([Store.FXQuoteAtOrBefore]) would always soft-fall-back to cached VWAP
// while fresh quotes sat one table over. The helpers below are the
// fx_quotes-first leg of the unified read path; the trades read
// survives only as the compatibility fallback for re-enabled
// connector-path sources.

// fxQuotesSnapLookback bounds how far back the fx_quotes snap read
// accepts a row. fx_quotes buckets are daily and the feed skips
// weekends/holidays for some tickers, so 7 days tolerates the longest
// routine gap while still refusing to price a chained-fiat leg off a
// quote stale enough to be wrong. Misses inside the window fall back to
// the trades path; a total miss surfaces [ErrNoFXQuote] and the
// caller's cached-VWAP fallback. The floor also lets TimescaleDB prune
// to the window's chunks instead of walking the hypertable to genesis
// on a miss (same rationale as USDPriceAt's lower bucket bound in
// usd_fx_resolver.go).
const fxQuotesSnapLookback = 7 * 24 * time.Hour

// fxQuotesSourceLabel is the provenance label for fx_quotes rows. It is
// both the source tag the forex worker stamps on every row it writes
// AND the label substituted for backfill rows whose source column
// is NULL (migration 0028 allows NULL only for pre-attribution
// recovery rows — same pipeline, provenance merely unrecorded).
const fxQuotesSourceLabel = "massive"

// usdFiatCode is the anchor currency fx_quotes rates are expressed
// against. `rate_usd` is the price of 1 USD denominated in the ticker —
// i.e. UNITS-OF-TICKER PER 1 USD (e.g. rate_usd(EUR) ≈ 0.92 means 1 USD
// buys 0.92 EUR). See internal/sources/external/forex/client.go
// (LatestUSDRates) + cache.go (Currency.RateUSD) for the source-of-truth
// orientation the `massive` feed writes.
const usdFiatCode = "USD"

// fxSnapRow is one latest-per-ticker fx_quotes observation feeding
// [fxSnapFromRows]. RateUSD carries the NUMERIC column's text form —
// parsed to *big.Rat, never through a float (ADR-0003).
type fxSnapRow struct {
	Bucket  time.Time
	RateUSD string
	Source  string
}

// fxSnapTickers returns the fx_quotes tickers needed to price `pair`,
// or nil when the pair cannot be priced from fx_quotes at all (either
// side non-fiat, or the degenerate USD/USD). USD needs no row — it is
// the rate_usd anchor, exactly 1 by definition.
func fxSnapTickers(pair canonical.Pair) []string {
	if pair.Base.Type != canonical.AssetFiat || pair.Quote.Type != canonical.AssetFiat {
		return nil
	}
	out := make([]string, 0, 2)
	if pair.Base.Code != usdFiatCode {
		out = append(out, pair.Base.Code)
	}
	if pair.Quote.Code != usdFiatCode {
		out = append(out, pair.Quote.Code)
	}
	return out
}

// fxSnapFromRows computes the pair price (quote units per base unit —
// the same QuoteAmount/BaseAmount orientation the trades path returns)
// from latest-per-ticker fx_quotes rows, keyed by ticker.
//
// Math is exact *big.Rat throughout: rate_usd(T) is "T per 1 USD", so
// price(B/Q) = quote-per-base = rate_usd(Q) / rate_usd(B), with either
// USD side contributing an exact 1. (Dividing the other way,
// rate_usd(B)/rate_usd(Q), would invert every served fiat-quoted
// pair.) The cached float-derived `inverse_usd` column is
// deliberately NOT used — inversion happens in Rat space.
//
// observedAt is the OLDEST bucket among the rows used (the staler
// input governs the quote's freshness). The source label is the
// sorted "+"-join of the distinct row sources (a cross like EUR/GBP
// can mix providers); NULL-source rows read as
// [fxQuotesSourceLabel].
//
// Returns [ErrNoFXQuote] when a needed ticker has no row.
func fxSnapFromRows(pair canonical.Pair, rows map[string]fxSnapRow) (*big.Rat, time.Time, string, error) {
	tickers := fxSnapTickers(pair)
	if len(tickers) == 0 {
		return nil, time.Time{}, "", ErrNoFXQuote
	}

	var observedAt time.Time
	sourceSet := map[string]struct{}{}
	resolve := func(ticker string) (*big.Rat, error) {
		row, ok := rows[ticker]
		if !ok {
			return nil, ErrNoFXQuote
		}
		r, ok := new(big.Rat).SetString(row.RateUSD)
		if !ok || r.Sign() <= 0 {
			return nil, fmt.Errorf("timescale: fx_quotes snap: invalid rate_usd %q for ticker %s", row.RateUSD, ticker)
		}
		if observedAt.IsZero() || row.Bucket.Before(observedAt) {
			observedAt = row.Bucket
		}
		src := row.Source
		if src == "" {
			src = fxQuotesSourceLabel
		}
		sourceSet[src] = struct{}{}
		return r, nil
	}

	baseRate := big.NewRat(1, 1)
	quoteRate := big.NewRat(1, 1)
	var err error
	if pair.Base.Code != usdFiatCode {
		if baseRate, err = resolve(pair.Base.Code); err != nil {
			return nil, time.Time{}, "", err
		}
	}
	if pair.Quote.Code != usdFiatCode {
		if quoteRate, err = resolve(pair.Quote.Code); err != nil {
			return nil, time.Time{}, "", err
		}
	}

	sources := make([]string, 0, len(sourceSet))
	for s := range sourceSet {
		sources = append(sources, s)
	}
	sort.Strings(sources)

	// price = quote-per-base = rate_usd(Q)/rate_usd(B) because rate_usd
	// is ticker-per-USD. USD legs carry an exact 1.
	return new(big.Rat).Quo(quoteRate, baseRate), observedAt, strings.Join(sources, "+"), nil
}

// fxQuotesSnapAtOrBefore is the fx_quotes leg of [Store.FXQuoteAtOrBefore]:
// the most recent fx_quotes observation per needed ticker whose
// `bucket <= cutoff`, within [fxQuotesSnapLookback]. One round-trip via
// DISTINCT ON; the (ticker, bucket DESC) index makes each ticker a
// bounded descending scan.
//
// Returns [ErrNoFXQuote] when any needed ticker has no row in the
// window (caller falls back to the trades path). Other DB errors
// propagate — a broken fx_quotes read means no chained-fiat output can
// be trusted this tick.
func (s *Store) fxQuotesSnapAtOrBefore(
	ctx context.Context,
	pair canonical.Pair,
	cutoff time.Time,
) (*big.Rat, time.Time, string, error) {
	tickers := fxSnapTickers(pair)
	if len(tickers) == 0 {
		return nil, time.Time{}, "", ErrNoFXQuote
	}

	got, err := s.fxQuotesRowsAtOrBefore(ctx, tickers, cutoff)
	if err != nil {
		return nil, time.Time{}, "", err
	}
	return fxSnapFromRows(pair, got)
}

// fxQuotesRowsAtOrBefore returns each ticker's newest fx_quotes row with
// bucket ≤ cutoff within [fxQuotesSnapLookback]; a ticker with none is absent.
func (s *Store) fxQuotesRowsAtOrBefore(ctx context.Context, tickers []string, cutoff time.Time) (map[string]fxSnapRow, error) {
	const q = `
        SELECT DISTINCT ON (ticker)
               ticker, bucket, rate_usd::text, COALESCE(source, '')
          FROM fx_quotes
         WHERE ticker = ANY($1)
           AND bucket <= $2
           AND bucket >= $3
         ORDER BY ticker, bucket DESC
    `
	rows, err := s.db.QueryContext(ctx, q,
		tickers, cutoff.UTC(), cutoff.UTC().Add(-fxQuotesSnapLookback),
	)
	if err != nil {
		return nil, fmt.Errorf("timescale: fxQuotesSnapAtOrBefore: %w", err)
	}
	defer func() { _ = rows.Close() }()

	got := make(map[string]fxSnapRow, len(tickers))
	for rows.Next() {
		var (
			ticker string
			row    fxSnapRow
		)
		if err := rows.Scan(&ticker, &row.Bucket, &row.RateUSD, &row.Source); err != nil {
			return nil, fmt.Errorf("timescale: fxQuotesSnapAtOrBefore scan: %w", err)
		}
		got[ticker] = row
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("timescale: fxQuotesSnapAtOrBefore rows: %w", err)
	}
	return got, nil
}

// fxQuoteBucketAtOrBeforeSelect finds the newest bucket for one ticker
// at or before a cutoff, within a lookback floor. The floor is what lets
// TimescaleDB prune to the window's chunks instead of walking the
// hypertable to genesis on a miss (the same rationale as
// [fxQuotesSnapLookback]); it also IS the as-of tolerance for the caller
// below, which is why it is a parameter rather than the constant.
const fxQuoteBucketAtOrBeforeSelect = `
	SELECT bucket
	  FROM fx_quotes
	 WHERE ticker = $1
	   AND bucket <= $2
	   AND bucket >= $3
	 ORDER BY bucket DESC
	 LIMIT 1
`

// FXQuoteBucketAtOrBefore returns the newest `fx_quotes` bucket for
// ticker at or before `at`, looking back no further than `lookback`.
// ok=false means the table holds no quote for that ticker in
// [at-lookback, at] — the caller must then REFUSE to price rather than
// reach forward to a later bucket or extrapolate from an older one.
//
// AT OR BEFORE is day-bucket granularity, not publication time: the
// bucket's date is <= at, but its rate is overwritten by every later
// refresh that day and by the trailing-7d history bars, so it can carry
// a rate published up to a day after `at`. Never a later bucket, though.
func (s *Store) FXQuoteBucketAtOrBefore(ctx context.Context, ticker string, at time.Time, lookback time.Duration) (time.Time, bool, error) {
	var bucket time.Time
	err := s.db.QueryRowContext(ctx, fxQuoteBucketAtOrBeforeSelect,
		ticker, at.UTC(), at.UTC().Add(-lookback)).Scan(&bucket)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return time.Time{}, false, nil
	case err != nil:
		return time.Time{}, false, fmt.Errorf("timescale: fx quote bucket for %s at or before %s: %w",
			ticker, at.Format(time.RFC3339), err)
	}
	return bucket.UTC(), true, nil
}
