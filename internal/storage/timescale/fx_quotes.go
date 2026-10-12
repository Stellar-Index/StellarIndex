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

// FXQuote is one (date, ticker) forex snapshot; RateUSD is the write input and reads return exact NUMERIC text so no served price passes through a float.
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

// InsertFXQuoteBatch upserts fx quotes idempotently on (ticker, bucket), keeping the first observed_at; the upsert is generation-guarded because rate_usd is the denominator of every fiat-quoted usd_volume.
func (s *Store) InsertFXQuoteBatch(ctx context.Context, quotes []FXQuote) error {
	if len(quotes) == 0 {
		return nil
	}
	// inverse_usd is derived from rate_usd INSIDE the statement, in NUMERIC. A float64 reciprocal
	// carries a second rounding that would disagree with the exact 1/rate_usd in the last digits,
	// and served fiat legs read this column (ADR-0003).
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
		// IsNaN is not redundant: every comparison with NaN is false in Go, so `<= 0` lets it
		// through, and Postgres orders NaN ABOVE every numeric so the column's CHECK
		// (rate_usd > 0) accepts it. NaN is absorbing under sum(), so one would poison every
		// usd_volume derived from that ticker and every prices_* bucket containing one.
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

// ListFXHistory returns daily snapshots for `ticker` in [from, to], ascending (empty when nothing matches), for the `history_*` fields of /v1/currencies/{ticker}.
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

// LatestFXQuotes returns the newest fx_quotes row per ticker whose bucket is at or after since, rates as exact NUMERIC text.
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
// The active FX feed (`massive`) writes `fx_quotes`, while the connector-path sources that feed
// `trades` are disabled in production, so a trades-only forex-snap ([Store.FXQuoteAtOrBefore])
// would always fall back to cached VWAP. The helpers below are the fx_quotes-first leg; the
// trades read is only a fallback for re-enabled connectors.

// fxQuotesSnapLookback bounds how far back the snap read accepts a row. Buckets are daily and the
// feed skips weekends/holidays for some tickers, so 7 days tolerates the longest routine gap while
// refusing a stale chained-fiat leg. A total miss surfaces [ErrNoFXQuote]. The floor also lets
// TimescaleDB prune to the window's chunks instead of scanning to genesis on a miss.
const fxQuotesSnapLookback = 7 * 24 * time.Hour

// fxQuotesSourceLabel is the provenance label for fx_quotes rows, also substituted for rows whose
// source column is NULL (pre-attribution recovery rows from the same pipeline).
const fxQuotesSourceLabel = "massive"

// usdFiatCode is the anchor currency fx_quotes rates are expressed against. `rate_usd` is
// UNITS-OF-TICKER PER 1 USD (rate_usd(EUR) ~ 0.92 means 1 USD buys 0.92 EUR).
const usdFiatCode = "USD"

// fxSnapRow is one latest-per-ticker fx_quotes observation feeding [fxSnapFromRows]. RateUSD
// carries the NUMERIC column's text form, parsed to *big.Rat, never through a float (ADR-0003).
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

// fxSnapFromRows computes the pair price (quote units per base unit, the trades path's
// QuoteAmount/BaseAmount orientation) from latest-per-ticker fx_quotes rows keyed by ticker.
//
// Math is exact *big.Rat: rate_usd(T) is "T per 1 USD", so price(B/Q) = rate_usd(Q) / rate_usd(B),
// with a USD side contributing exactly 1; the other direction would invert every fiat-quoted
// pair. The cached `inverse_usd` column is NOT used; inversion happens in Rat space.
//
// observedAt is the OLDEST bucket used (the staler input governs freshness). The source label is
// the sorted "+"-join of distinct row sources (a cross can mix providers); NULL-source rows read
// as [fxQuotesSourceLabel].
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

// fxQuotesSnapAtOrBefore is the fx_quotes leg of [Store.FXQuoteAtOrBefore]: the newest observation
// per needed ticker with `bucket <= cutoff` within [fxQuotesSnapLookback]. DISTINCT ON over the
// (ticker, bucket DESC) index makes each ticker a bounded descending scan.
//
// Returns [ErrNoFXQuote] when any needed ticker has no row in the window (caller falls back to
// trades). Other DB errors propagate: a broken read leaves no trustworthy chained-fiat output.
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

// fxQuoteBucketAtOrBeforeSelect finds the newest bucket for one ticker at or before a cutoff,
// within a lookback floor. The floor lets TimescaleDB prune chunks on a miss (as for
// [fxQuotesSnapLookback]) and is also the caller's as-of tolerance, hence a parameter.
const fxQuoteBucketAtOrBeforeSelect = `
	SELECT bucket
	  FROM fx_quotes
	 WHERE ticker = $1
	   AND bucket <= $2
	   AND bucket >= $3
	 ORDER BY bucket DESC
	 LIMIT 1
`

// FXQuoteBucketAtOrBefore returns the newest `fx_quotes` bucket for ticker at or before `at` within `lookback`; ok=false means the caller must REFUSE to price, never reach forward or extrapolate.
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
