package timescale

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/domain"
)

// LedgerProvider is defined by freeze_events.go (6047a9a33 landed first).
// Reusing it here keeps the seam consistent across sinks.

// DivergenceSink is the timescale-backed implementation of
// divergence.ObservationSink. Persists every per-reference
// comparison the worker computes to the `divergence_observations`
// hypertable.
//
// Today the worker writes the aggregate (median + boolean firing
// flag) to Redis with a TTL; the per-reference deltas are lost
// after the next tick. This sink keeps a queryable history so
// the explorer /divergences page can plot deltas over time and
// post-mortems can verify "Reflector drifted N% from us at
// ledger X" against ground truth.
type DivergenceSink struct {
	db        *sql.DB
	getLedger LedgerProvider
}

// NewDivergenceSink constructs the sink. Pass an optional ledger
// provider so observations carry observed_at_ledger; nil falls
// back to ledger 0 (acceptable for tests).
func NewDivergenceSink(s *Store, opts ...DivergenceSinkOption) *DivergenceSink {
	sink := &DivergenceSink{db: s.db}
	for _, opt := range opts {
		opt(sink)
	}
	return sink
}

// DivergenceSinkOption tunes a DivergenceSink at construction.
type DivergenceSinkOption func(*DivergenceSink)

// WithDivergenceLedgerProvider wires the ledger seam so inserts
// capture observed_at_ledger.
func WithDivergenceLedgerProvider(p LedgerProvider) DivergenceSinkOption {
	return func(s *DivergenceSink) {
		s.getLedger = p
	}
}

// RecordObservation implements divergence.ObservationSink.
//
// Inserts one row per call; the table's PK (asset_id, quote_id,
// reference, observed_at) makes concurrent inserts at the identical
// microsecond a no-op via ON CONFLICT — but since we control the
// observed_at upstream (the worker sets it) collisions are rare in
// practice.
//
// obs.OurPrice / RefPrice / DeltaPct are decimal strings (ADR-0003)
// bound directly into the our_price/ref_price/delta_pct NUMERIC
// columns — never a Go float64 — mirroring the ::text cast the read
// path ([Store.ListDivergenceLatest], [Store.ListDivergenceSeries])
// uses to hand the same columns back out as strings.
func (s *DivergenceSink) RecordObservation(ctx context.Context, obs domain.DivergenceObservationRecord) error {
	var ledger uint32
	if s.getLedger != nil {
		ledger = s.getLedger.LatestLedger()
	}

	status := "clear"
	if obs.Firing {
		status = "firing"
	}
	var refAt sql.NullTime
	if !obs.RefObservedAt.IsZero() {
		refAt = sql.NullTime{Time: obs.RefObservedAt.UTC(), Valid: true}
	}

	const q = `
		INSERT INTO divergence_observations (
		    asset_id, quote_id, reference,
		    observed_at, observed_at_ledger,
		    our_price, ref_price, delta_pct,
		    status, ref_observed_at
		)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		ON CONFLICT (asset_id, quote_id, reference, observed_at) DO NOTHING
	`
	if _, err := s.db.ExecContext(ctx, q,
		obs.Pair.Base.String(), obs.Pair.Quote.String(), obs.Reference,
		obs.ObservedAt.UTC(), int64(ledger),
		obs.OurPrice, obs.RefPrice, obs.DeltaPct,
		status, refAt,
	); err != nil {
		return fmt.Errorf("timescale: RecordObservation %s/%s/%s: %w",
			obs.Pair.Base.String(), obs.Pair.Quote.String(), obs.Reference, err)
	}
	return nil
}

// DivergenceRow is one divergence_observations row for the /v1/divergence
// read path. Prices + delta are decimal strings (ADR-0003).
type DivergenceRow struct {
	AssetID          string
	QuoteID          string
	Reference        string
	ObservedAt       time.Time
	ObservedAtLedger int64
	OurPrice         string
	RefPrice         string
	DeltaPct         string
	Status           string
	// RefObservedAt is when the reference observed RefPrice; nil on rows
	// written before migration 0186 recorded it.
	RefObservedAt *time.Time
}

// ListDivergenceLatest returns the LATEST observation per (asset,
// quote, reference) within the trailing `sinceDays` window, for at most
// `limit` (asset, quote) pairs — every reference of a returned pair is
// included, so a pair is never served with part of its comparison set.
// Pairs rank by their widest |delta_pct|; firingOnly keeps pairs with at
// least one reference firing at its latest observation. Rows are ordered
// pair by pair (widest first), then by |delta_pct| desc within a pair.
// limit ≤ 500.
//
// DISTINCT ON (asset, quote, reference) with the matching ORDER prefix
// uses the (asset, quote, reference, observed_at DESC) index to pick
// each triple's newest row without a separate sort.
func (s *Store) ListDivergenceLatest(ctx context.Context, sinceDays int, firingOnly bool, limit int) ([]DivergenceRow, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	if sinceDays <= 0 {
		sinceDays = 7
	}
	having := ""
	if firingOnly {
		having = ` HAVING bool_or(status = 'firing')`
	}
	q := `
		WITH latest AS (
			SELECT DISTINCT ON (asset_id, quote_id, reference)
			       asset_id, quote_id, reference, observed_at, observed_at_ledger,
			       our_price, ref_price, delta_pct, status, ref_observed_at
			  FROM divergence_observations
			 WHERE observed_at > now() - make_interval(days => $1)
			 ORDER BY asset_id, quote_id, reference, observed_at DESC
		), pairs AS (
			SELECT asset_id, quote_id, max(abs(delta_pct)) AS widest
			  FROM latest
			 GROUP BY asset_id, quote_id` + having + `
			 ORDER BY widest DESC, asset_id, quote_id
			 LIMIT $2
		)
		SELECT l.asset_id, l.quote_id, l.reference, l.observed_at, l.observed_at_ledger,
		       l.our_price::text, l.ref_price::text, l.delta_pct::text, l.status,
		       l.ref_observed_at
		  FROM latest l
		  JOIN pairs p USING (asset_id, quote_id)
		 ORDER BY p.widest DESC, l.asset_id, l.quote_id, abs(l.delta_pct) DESC, l.reference`
	rows, err := s.db.QueryContext(ctx, q, sinceDays, limit)
	if err != nil {
		return nil, fmt.Errorf("timescale: ListDivergenceLatest: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []DivergenceRow
	for rows.Next() {
		var r DivergenceRow
		var refAt sql.NullTime
		if err := rows.Scan(&r.AssetID, &r.QuoteID, &r.Reference, &r.ObservedAt,
			&r.ObservedAtLedger, &r.OurPrice, &r.RefPrice, &r.DeltaPct, &r.Status, &refAt); err != nil {
			return nil, fmt.Errorf("timescale: ListDivergenceLatest scan: %w", err)
		}
		if refAt.Valid {
			at := refAt.Time.UTC()
			r.RefObservedAt = &at
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("timescale: ListDivergenceLatest rows: %w", err)
	}
	return out, nil
}

// DivergenceSeriesPoint is one downsampled (bucket, reference) cell of
// a pair's divergence history. Values are the LAST observation of that
// reference inside the bucket (the board semantics, not an average —
// averaging would smooth exactly the spikes the chart exists to show);
// LastAt is that observation's time, so a caller can pick the bucket's
// newest our_price across references. Firing is true when ANY of the
// reference's observations in the bucket breached threshold, so a brief
// breach can't disappear into its bucket. Decimal strings (ADR-0003).
type DivergenceSeriesPoint struct {
	Bucket    time.Time
	Reference string
	LastAt    time.Time
	DeltaPct  string
	OurPrice  string
	RefPrice  string
	Firing    bool
}

// DivergenceSeriesBucket maps a whitelisted window to its bucket
// width. Widths are chosen so a series is always ≤ ~360 points
// regardless of the worker's tick cadence — the read budget is
// enforced here, not assumed from config. Exported so the API
// handler can surface the effective bucket width on the wire.
func DivergenceSeriesBucket(days int) time.Duration {
	switch {
	case days <= 1:
		return 5 * time.Minute // 288 buckets/day
	case days <= 7:
		return 30 * time.Minute // 336 buckets/7d
	default:
		return 2 * time.Hour // 360 buckets/30d
	}
}

// ListDivergenceSeries returns the bucketed Δ% history of every
// reference for ONE (asset, quote) pair over the trailing `sinceDays`
// window, ascending by bucket then reference. The sibling of
// ListDivergenceLatest: same access path minus the DISTINCT ON.
//
// Index reasoning: divergence_observations_pair_ref_idx
// (asset_id, quote_id, reference, observed_at DESC) covers the two
// equality predicates; each of the ≤ 8 references is a range scan over
// its window (hypertable chunk exclusion applies via the bare
// observed_at bound — the column is never wrapped in a function in
// WHERE; time_bucket appears only in SELECT/GROUP BY). The GROUP BY
// folds the window into ≤ ~360 buckets per reference.
func (s *Store) ListDivergenceSeries(ctx context.Context, assetID, quoteID string, sinceDays int) ([]DivergenceSeriesPoint, error) {
	if sinceDays <= 0 {
		sinceDays = 7
	}
	bucket := DivergenceSeriesBucket(sinceDays)
	const q = `
		SELECT time_bucket(make_interval(secs => $3), observed_at) AS bucket,
		       reference,
		       max(observed_at),
		       last(delta_pct, observed_at)::text,
		       last(our_price, observed_at)::text,
		       last(ref_price, observed_at)::text,
		       bool_or(status = 'firing')
		  FROM divergence_observations
		 WHERE asset_id = $1 AND quote_id = $2
		   AND observed_at > now() - make_interval(days => $4)
		 GROUP BY bucket, reference
		 ORDER BY bucket ASC, reference ASC`
	rows, err := s.db.QueryContext(ctx, q, assetID, quoteID, int64(bucket.Seconds()), sinceDays)
	if err != nil {
		return nil, fmt.Errorf("timescale: ListDivergenceSeries: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []DivergenceSeriesPoint
	for rows.Next() {
		var p DivergenceSeriesPoint
		if err := rows.Scan(&p.Bucket, &p.Reference, &p.LastAt, &p.DeltaPct, &p.OurPrice, &p.RefPrice, &p.Firing); err != nil {
			return nil, fmt.Errorf("timescale: ListDivergenceSeries scan: %w", err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("timescale: ListDivergenceSeries rows: %w", err)
	}
	return out, nil
}
