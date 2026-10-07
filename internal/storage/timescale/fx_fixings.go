package timescale

import (
	"context"
	"database/sql"
	"fmt"
	"math/big"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// FXFixing is one fx_fixings row (migration 0193): a vendor-time FX bar,
// units of Ticker per 1 USD. RateUSD is exact NUMERIC text on write and read.
type FXFixing struct {
	Ticker     string
	Grain      string // FXGrainHour or FXGrainDay
	BarStart   time.Time
	BarEnd     time.Time
	RateUSD    string
	Source     string
	Generation int64
}

// fx_fixings grains.
const (
	FXGrainHour = "1h"
	FXGrainDay  = "1d"
)

// FXFixingLag separates a closed bucket end E from the newest bar it may
// bind: bar T is first seen closed at T+δ (δ ≤ 1h + settle), and one missed
// hourly tick still writes it well before T+3h, so every region holds the
// bar before any bucket that binds it is served.
const FXFixingLag = 3 * time.Hour

// fxFixingWriteTimeout caps one batch transaction. The API cache's delta load
// re-reads rows ingested up to this long before its previous load, so every
// writer must stay inside it.
const fxFixingWriteTimeout = 20 * time.Second

// FX resolutions on the wire.
const (
	FXResolutionHourly = "hourly"
	FXResolutionDaily  = "daily"
)

// FXFixingBinding is what one ticker binds to at a bucket end: an fx_fixings
// row, or on the daily arm (no fixings) an fx_quotes day as a daily bar.
type FXFixingBinding struct {
	FXFixing
	Resolution string // FXResolutionHourly or FXResolutionDaily
}

// InsertFXFixingBatch appends rows at the store's derive generation and
// reports how many were new. Rows already present at that generation are
// left untouched: the table is never updated in place, so re-running a
// generation-0 write cannot revert a higher-generation correction.
func (s *Store) InsertFXFixingBatch(ctx context.Context, rows []FXFixing) (int64, error) {
	if len(rows) == 0 {
		return 0, nil
	}
	n := len(rows)
	tickers, grains, rates, sources := make([]string, n), make([]string, n), make([]string, n), make([]string, n)
	starts, ends := make([]time.Time, n), make([]time.Time, n)
	for i, r := range rows {
		if r.Ticker == "" || r.RateUSD == "" {
			return 0, fmt.Errorf("timescale: InsertFXFixingBatch: row %d has no ticker or rate", i)
		}
		tickers[i], grains[i], rates[i], sources[i] = r.Ticker, r.Grain, r.RateUSD, r.Source
		starts[i], ends[i] = r.BarStart.UTC(), r.BarEnd.UTC()
	}
	const stmt = `
		INSERT INTO fx_fixings (ticker, grain, bar_start, bar_end, rate_usd, source, generation)
		SELECT u.ticker, u.grain, u.bar_start, u.bar_end, u.rate::numeric, u.source, $7::bigint
		  FROM unnest($1::text[], $2::text[], $3::timestamptz[], $4::timestamptz[], $5::text[], $6::text[])
		       AS u(ticker, grain, bar_start, bar_end, rate, source)
		ON CONFLICT (ticker, grain, bar_start, generation) DO NOTHING
	`
	ctx, cancel := context.WithTimeout(ctx, fxFixingWriteTimeout)
	defer cancel()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("timescale: InsertFXFixingBatch begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, fmt.Sprintf("SET LOCAL statement_timeout = '%d'", fxFixingWriteTimeout.Milliseconds())); err != nil {
		return 0, fmt.Errorf("timescale: InsertFXFixingBatch statement_timeout: %w", err)
	}
	res, err := tx.ExecContext(ctx, stmt, tickers, grains, starts, ends, rates, sources, s.deriveGeneration)
	if err != nil {
		return 0, fmt.Errorf("timescale: InsertFXFixingBatch: %w", err)
	}
	inserted, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("timescale: InsertFXFixingBatch rows affected: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("timescale: InsertFXFixingBatch commit: %w", err)
	}
	return inserted, nil
}

// fxFixingAtOrBeforeSelect is the binding rule: per ticker, the row with the
// greatest bar_end ≤ cutoff inside the lookback, hourly over daily on a tie,
// then the highest generation. Every parameter is cast: an untyped parameter
// beside an interval fails to resolve an operator. The bar_start bound is
// implied by the bar_end window (a bar is at most a day wide); it lets the
// planner exclude chunks, which are partitioned on bar_start.
const fxFixingAtOrBeforeSelect = `
	SELECT DISTINCT ON (ticker)
	       ticker, grain, bar_start, bar_end, rate_usd::text, source, generation
	  FROM fx_fixings
	 WHERE ticker = ANY($1::text[])
	   AND bar_end <= $2::timestamptz
	   AND bar_end >  $2::timestamptz - $3::interval
	   AND bar_start <  $2::timestamptz
	   AND bar_start >  $2::timestamptz - $3::interval - interval '1 day'
	 ORDER BY ticker, bar_end DESC, grain DESC, generation DESC
`

// FXFixingAtOrBefore binds each ticker's FX leg for a closed bucket ending at
// e: the fixing with the greatest bar_end ≤ e − [FXFixingLag] within maxAge.
// A ticker whose fixings start after e − lag (or that has none) binds its
// fx_quotes day instead, one day earlier still, as a daily bar. A ticker
// absent from the result is a miss: a gap inside the fixings era, or no
// daily row either. The answer is a function of e and the stored series only.
func (s *Store) FXFixingAtOrBefore(ctx context.Context, tickers []string, e time.Time, maxAge time.Duration) (map[string]FXFixingBinding, error) {
	out := make(map[string]FXFixingBinding, len(tickers))
	if len(tickers) == 0 {
		return out, nil
	}
	cutoff := e.UTC().Add(-FXFixingLag)
	fixings, err := s.fxFixingRowsAtOrBefore(ctx, tickers, cutoff, maxAge)
	if err != nil {
		return nil, err
	}
	for t, f := range fixings {
		out[t] = FXFixingBinding{FXFixing: f, Resolution: fxResolution(f.Grain)}
	}
	var missing []string
	for _, t := range tickers {
		if _, ok := out[t]; !ok {
			missing = append(missing, t)
		}
	}
	if len(missing) == 0 {
		return out, nil
	}
	return out, s.fxFixingDailyArm(ctx, missing, cutoff, out)
}

// fxFixingRowsAtOrBefore runs [fxFixingAtOrBeforeSelect] for tickers at
// cutoff; a ticker with no row in (cutoff − maxAge, cutoff] is absent.
func (s *Store) fxFixingRowsAtOrBefore(ctx context.Context, tickers []string, cutoff time.Time, maxAge time.Duration) (map[string]FXFixing, error) {
	rows, err := s.db.QueryContext(ctx, fxFixingAtOrBeforeSelect, tickers, cutoff.UTC(), pgInterval(maxAge))
	if err != nil {
		return nil, fmt.Errorf("timescale: fx_fixings at or before: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := make(map[string]FXFixing, len(tickers))
	for rows.Next() {
		var f FXFixing
		if err := rows.Scan(&f.Ticker, &f.Grain, &f.BarStart, &f.BarEnd, &f.RateUSD, &f.Source, &f.Generation); err != nil {
			return nil, fmt.Errorf("timescale: fx_fixings at or before scan: %w", err)
		}
		f.BarStart, f.BarEnd = f.BarStart.UTC(), f.BarEnd.UTC()
		out[f.Ticker] = f
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("timescale: fx_fixings at or before rows: %w", err)
	}
	return out, nil
}

// fxFixingStoreMaxAge bounds the fixings arm of [Store.FXQuoteAtOrBefore];
// it matches the API's default cross max age so both paths accept the
// same weekend carry.
const fxFixingStoreMaxAge = 76 * time.Hour

// fxFixingSnapAtOrBefore is the fixings arm of [Store.FXQuoteAtOrBefore]:
// the pair priced from each leg's fixing at or before cutoff − lag. It
// returns [ErrNoFXQuote] unless every leg has a fixing, so the caller keeps
// its fx_quotes path for the era before fixings.
func (s *Store) fxFixingSnapAtOrBefore(ctx context.Context, pair canonical.Pair, cutoff time.Time) (*big.Rat, time.Time, string, error) {
	tickers := fxSnapTickers(pair)
	if len(tickers) == 0 {
		return nil, time.Time{}, "", ErrNoFXQuote
	}
	fixings, err := s.fxFixingRowsAtOrBefore(ctx, tickers, cutoff.Add(-FXFixingLag), fxFixingStoreMaxAge)
	if err != nil {
		return nil, time.Time{}, "", err
	}
	rows := make(map[string]fxSnapRow, len(fixings))
	for t, f := range fixings {
		rows[t] = fxSnapRow{Bucket: f.BarEnd, RateUSD: f.RateUSD, Source: f.Source}
	}
	return fxSnapFromRows(pair, rows)
}

// fxFixingDailyArm binds the era before fixings. A ticker takes it iff cutoff
// is before its first fixing's bar_end or it has no fixings; a miss after
// the first fixing is a gap inside the era and stays a miss.
func (s *Store) fxFixingDailyArm(ctx context.Context, tickers []string, cutoff time.Time, out map[string]FXFixingBinding) error {
	firsts, err := s.fxFixingFirstBarEnds(ctx, tickers)
	if err != nil {
		return err
	}
	var daily []string
	for _, t := range tickers {
		if first, ok := firsts[t]; !ok || cutoff.Before(first) {
			daily = append(daily, t)
		}
	}
	if len(daily) == 0 {
		return nil
	}
	// A grouped-daily row closes at bucket + 24h, so the day that closed by
	// cutoff is the one bucketed at cutoff − 24h or earlier.
	quotes, err := s.fxQuotesRowsAtOrBefore(ctx, daily, cutoff.Add(-24*time.Hour))
	if err != nil {
		return err
	}
	for t, q := range quotes {
		src := q.Source
		if src == "" {
			src = fxQuotesSourceLabel
		}
		out[t] = FXFixingBinding{
			FXFixing: FXFixing{
				Ticker: t, Grain: FXGrainDay, BarStart: q.Bucket, BarEnd: q.Bucket.Add(24 * time.Hour),
				RateUSD: q.RateUSD, Source: src,
			},
			Resolution: FXResolutionDaily,
		}
	}
	return nil
}

// fxFixingFirstBarEnds returns, per ticker that has any fixing, the bar_end
// of its earliest bar. Ordering on the partitioning column lets the
// chunk-ordered scan stop at the first chunk holding the ticker.
func (s *Store) fxFixingFirstBarEnds(ctx context.Context, tickers []string) (map[string]time.Time, error) {
	const q = `
		SELECT t.ticker, f.bar_end
		  FROM unnest($1::text[]) AS t(ticker)
		  CROSS JOIN LATERAL (
		        SELECT bar_end FROM fx_fixings
		         WHERE ticker = t.ticker
		         ORDER BY bar_start ASC, bar_end ASC
		         LIMIT 1) f
	`
	rows, err := s.db.QueryContext(ctx, q, tickers)
	if err != nil {
		return nil, fmt.Errorf("timescale: fx_fixings first bar_end: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := make(map[string]time.Time, len(tickers))
	for rows.Next() {
		var t string
		var first time.Time
		if err := rows.Scan(&t, &first); err != nil {
			return nil, fmt.Errorf("timescale: fx_fixings first bar_end scan: %w", err)
		}
		out[t] = first
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("timescale: fx_fixings first bar_end rows: %w", err)
	}
	return out, nil
}

// LoadFXFixingWindow reads, in one snapshot, every row whose bar_end is at or
// after L − horizon and, when ingestedAfter is non-zero, that was ingested
// after it. L is the snapshot's statement time and is returned with the rows:
// a cache holding them answers exactly what [FXFixingAtOrBefore] answered at L.
func (s *Store) LoadFXFixingWindow(ctx context.Context, horizon time.Duration, ingestedAfter time.Time) ([]FXFixing, time.Time, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true, Isolation: sql.LevelRepeatableRead})
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("timescale: LoadFXFixingWindow begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var loadedAt time.Time
	if err := tx.QueryRowContext(ctx, `SELECT statement_timestamp()`).Scan(&loadedAt); err != nil {
		return nil, time.Time{}, fmt.Errorf("timescale: LoadFXFixingWindow clock: %w", err)
	}
	floor := loadedAt.UTC().Add(-horizon)
	// bar_start bounds let the hypertable prune chunks; a bar is at most a
	// day wide.
	const q = `
		SELECT ticker, grain, bar_start, bar_end, rate_usd::text, source, generation
		  FROM fx_fixings
		 WHERE bar_start >= $1::timestamptz - INTERVAL '1 day'
		   AND bar_end >= $1::timestamptz
		   AND ($2::timestamptz IS NULL OR ingested_at > $2::timestamptz)
	`
	var after any
	if !ingestedAfter.IsZero() {
		after = ingestedAfter.UTC()
	}
	rows, err := tx.QueryContext(ctx, q, floor, after)
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("timescale: LoadFXFixingWindow: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []FXFixing
	for rows.Next() {
		var f FXFixing
		if err := rows.Scan(&f.Ticker, &f.Grain, &f.BarStart, &f.BarEnd, &f.RateUSD, &f.Source, &f.Generation); err != nil {
			return nil, time.Time{}, fmt.Errorf("timescale: LoadFXFixingWindow scan: %w", err)
		}
		out = append(out, f)
	}
	if err := rows.Err(); err != nil {
		return nil, time.Time{}, fmt.Errorf("timescale: LoadFXFixingWindow rows: %w", err)
	}
	return out, loadedAt.UTC(), nil
}

// FXFixingIngestSlack is how far before a previous load L a delta load must
// re-read ingested_at: a row committed after L was stamped at most one write
// transaction earlier.
const FXFixingIngestSlack = fxFixingWriteTimeout

func fxResolution(grain string) string {
	if grain == FXGrainDay {
		return FXResolutionDaily
	}
	return FXResolutionHourly
}

// pgInterval renders d as a Postgres interval literal in whole microseconds.
func pgInterval(d time.Duration) string {
	return fmt.Sprintf("%d microseconds", d.Microseconds())
}
