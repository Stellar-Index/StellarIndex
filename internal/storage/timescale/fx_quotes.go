package timescale

import (
	"context"
	"fmt"
	"math/big"
	"sort"
	"strings"
	"time"

	"github.com/lib/pq"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// FXQuote is one (date, ticker) snapshot from the forex pipeline.
// Rates are NUMERIC in the DB; we round-trip them through string
// in the wire shape but use float64 here for the in-process
// comparison + chart math (precision loss above 2^53 isn't a
// concern for fx rates which are O(1)–O(10000)).
type FXQuote struct {
	Bucket     time.Time
	Ticker     string
	RateUSD    float64
	InverseUSD float64
	Source     string
}

// InsertFXQuoteBatch upserts a slice of fx quotes. Idempotent on
// the (ticker, bucket) primary key — re-running with the same
// (ticker, date) updates `rate_usd` + `inverse_usd` + `source`,
// preserving the original `observed_at` only by virtue of the
// DEFAULT not firing on UPDATE. (We deliberately don't refresh
// observed_at because it's diagnostic: the row's first observation
// date is more useful than its most-recent.)
//
// Empty slice is a no-op.
func (s *Store) InsertFXQuoteBatch(ctx context.Context, quotes []FXQuote) error {
	if len(quotes) == 0 {
		return nil
	}
	const stmt = `
		INSERT INTO fx_quotes (bucket, ticker, rate_usd, inverse_usd, source)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (ticker, bucket) DO UPDATE
		   SET rate_usd    = EXCLUDED.rate_usd,
		       inverse_usd = EXCLUDED.inverse_usd,
		       source      = EXCLUDED.source
	`
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("timescale: InsertFXQuoteBatch begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	for _, q := range quotes {
		if q.Ticker == "" || q.RateUSD <= 0 || q.InverseUSD <= 0 {
			continue
		}
		if _, err := tx.ExecContext(ctx, stmt,
			q.Bucket, q.Ticker, q.RateUSD, q.InverseUSD, q.Source,
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
		SELECT bucket, ticker, rate_usd, inverse_usd, COALESCE(source, '')
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
		if err := rows.Scan(&q.Bucket, &q.Ticker, &q.RateUSD, &q.InverseUSD, &q.Source); err != nil {
			return nil, fmt.Errorf("timescale: ListFXHistory scan: %w", err)
		}
		out = append(out, q)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("timescale: ListFXHistory rows: %w", err)
	}
	return out, nil
}

// ─── X2.5 forex-snap read path (fx_quotes-first, BACKLOG #42) ────────
//
// The triangulation forex-snap ([Store.FXQuoteAtOrBefore]) historically
// read the `trades` hypertable filtered by external.FXSources() — the
// connector-path FX sources (polygon-forex / exchangeratesapi / ecb)
// which are DISABLED in production. The ACTIVE FX feed (`massive`, the
// internal/sources/external/forex worker) writes the `fx_quotes` hypertable
// instead, so the snap always soft-fell-back to cached VWAP while fresh
// quotes sat one table over. The helpers below are the fx_quotes-first
// leg of the unified read path; the trades read survives only as the
// compatibility fallback for re-enabled connector-path sources.

// fxQuotesSnapLookback bounds how far back the fx_quotes snap read
// accepts a row. fx_quotes buckets are daily and the feed skips
// weekends/holidays for some tickers, so 7 days tolerates the longest
// routine gap while still refusing to price a chained-fiat leg off a
// quote stale enough to be wrong. Misses inside the window fall back to
// the legacy trades path; a total miss surfaces [ErrNoFXQuote] and the
// caller's cached-VWAP fallback. The floor also lets TimescaleDB prune
// to the window's chunks instead of walking the hypertable to genesis
// on a miss (same rationale as the G11-06 fix in usd_fx_resolver.go).
const fxQuotesSnapLookback = 7 * 24 * time.Hour

// fxQuotesSourceLabel is the provenance label for fx_quotes rows. It is
// both the source tag the forex worker stamps on every row it writes
// AND the label substituted for legacy backfill rows whose source
// column is NULL (migration 0028 allows NULL only for pre-attribution
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
// USD side contributing an exact 1. (M3: the earlier code divided the
// other way, rate_usd(B)/rate_usd(Q), which inverted every served
// fiat-quoted pair.) The cached float-derived `inverse_usd` column is
// deliberately NOT used — inversion happens in Rat space.
//
// observedAt is the OLDEST bucket among the rows used (the staler
// input governs the quote's freshness). The source label is the
// sorted "+"-join of the distinct row sources (a cross like EUR/GBP
// can mix providers); NULL-source legacy rows read as
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
	// is ticker-per-USD (M3). USD legs carry an exact 1.
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
		pq.Array(tickers), cutoff.UTC(), cutoff.UTC().Add(-fxQuotesSnapLookback),
	)
	if err != nil {
		return nil, time.Time{}, "", fmt.Errorf("timescale: fxQuotesSnapAtOrBefore: %w", err)
	}
	defer func() { _ = rows.Close() }()

	got := make(map[string]fxSnapRow, len(tickers))
	for rows.Next() {
		var (
			ticker string
			row    fxSnapRow
		)
		if err := rows.Scan(&ticker, &row.Bucket, &row.RateUSD, &row.Source); err != nil {
			return nil, time.Time{}, "", fmt.Errorf("timescale: fxQuotesSnapAtOrBefore scan: %w", err)
		}
		got[ticker] = row
	}
	if err := rows.Err(); err != nil {
		return nil, time.Time{}, "", fmt.Errorf("timescale: fxQuotesSnapAtOrBefore rows: %w", err)
	}
	return fxSnapFromRows(pair, got)
}

// LatestFXBucketPerTicker returns the most-recent (ticker, bucket)
// the table holds. Used by the forex worker's gap-detector to
// resume backfill from the newest persisted date instead of
// re-inserting everything.
//
// Empty map when the table is empty.
func (s *Store) LatestFXBucketPerTicker(ctx context.Context) (map[string]time.Time, error) {
	const stmt = `
		SELECT ticker, MAX(bucket)
		  FROM fx_quotes
		 GROUP BY ticker
	`
	rows, err := s.db.QueryContext(ctx, stmt)
	if err != nil {
		return nil, fmt.Errorf("timescale: LatestFXBucketPerTicker: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := map[string]time.Time{}
	for rows.Next() {
		var ticker string
		var bucket time.Time
		if err := rows.Scan(&ticker, &bucket); err != nil {
			return nil, fmt.Errorf("timescale: LatestFXBucketPerTicker scan: %w", err)
		}
		out[ticker] = bucket
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("timescale: LatestFXBucketPerTicker rows: %w", err)
	}
	return out, nil
}
