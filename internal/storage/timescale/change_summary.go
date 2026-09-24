package timescale

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// ChangeSummaryRow mirrors the change_summary_5m table. Pointer
// fields are nullable — a young entity with <1h of history has
// every window pointer left nil; the SQL writes those as NULL.
type ChangeSummaryRow struct {
	EntityType   string
	EntityID     string
	RefreshedAt  time.Time
	CurrentValue string

	H1Value     *string
	H1DeltaPct  *float64
	H24Value    *string
	H24DeltaPct *float64
	D7Value     *string
	D7DeltaPct  *float64
	D30Value    *string
	D30DeltaPct *float64

	ATHValue *string
	ATHAt    *time.Time
	ATLValue *string
	ATLAt    *time.Time

	StreakDirection string
	StreakDays      *int
	Acceleration    string
}

// UpsertChangeSummary inserts or replaces the row keyed on
// (entity_type, entity_id). Refreshed every 5 min by the worker —
// stale rows (>10 min) are surfaced via the diagnostics page so an
// operator can spot a stalled worker.
func (s *Store) UpsertChangeSummary(ctx context.Context, row ChangeSummaryRow) error {
	if row.EntityType == "" {
		return errors.New("timescale: UpsertChangeSummary: empty entity_type")
	}
	if row.EntityID == "" {
		return errors.New("timescale: UpsertChangeSummary: empty entity_id")
	}
	if row.CurrentValue == "" {
		return errors.New("timescale: UpsertChangeSummary: empty current_value")
	}
	const q = `
		INSERT INTO change_summary_5m (
		    entity_type, entity_id, refreshed_at, current_value,
		    h1_value, h1_delta_pct,
		    h24_value, h24_delta_pct,
		    d7_value, d7_delta_pct,
		    d30_value, d30_delta_pct,
		    ath_value, ath_at,
		    atl_value, atl_at,
		    streak_direction, streak_days,
		    acceleration
		) VALUES (
		    $1, $2, $3, $4,
		    $5, $6, $7, $8, $9, $10, $11, $12,
		    $13, $14, $15, $16,
		    $17, $18, $19
		)
		ON CONFLICT (entity_type, entity_id) DO UPDATE SET
		    refreshed_at      = EXCLUDED.refreshed_at,
		    current_value     = EXCLUDED.current_value,
		    h1_value          = EXCLUDED.h1_value,
		    h1_delta_pct      = EXCLUDED.h1_delta_pct,
		    h24_value         = EXCLUDED.h24_value,
		    h24_delta_pct     = EXCLUDED.h24_delta_pct,
		    d7_value          = EXCLUDED.d7_value,
		    d7_delta_pct      = EXCLUDED.d7_delta_pct,
		    d30_value         = EXCLUDED.d30_value,
		    d30_delta_pct     = EXCLUDED.d30_delta_pct,
		    ath_value         = GREATEST(change_summary_5m.ath_value, EXCLUDED.ath_value),
		    ath_at            = CASE WHEN EXCLUDED.ath_value > change_summary_5m.ath_value
		                             THEN EXCLUDED.ath_at
		                             ELSE change_summary_5m.ath_at END,
		    atl_value         = LEAST(change_summary_5m.atl_value, EXCLUDED.atl_value),
		    atl_at            = CASE WHEN EXCLUDED.atl_value < change_summary_5m.atl_value
		                             THEN EXCLUDED.atl_at
		                             ELSE change_summary_5m.atl_at END,
		    streak_direction  = EXCLUDED.streak_direction,
		    streak_days       = EXCLUDED.streak_days,
		    acceleration      = EXCLUDED.acceleration
	`
	_, err := s.db.ExecContext(ctx, q,
		row.EntityType, row.EntityID, row.RefreshedAt.UTC(), row.CurrentValue,
		strPtrOrNil(row.H1Value), floatOrNil(row.H1DeltaPct),
		strPtrOrNil(row.H24Value), floatOrNil(row.H24DeltaPct),
		strPtrOrNil(row.D7Value), floatOrNil(row.D7DeltaPct),
		strPtrOrNil(row.D30Value), floatOrNil(row.D30DeltaPct),
		strPtrOrNil(row.ATHValue), timeOrNil(row.ATHAt),
		strPtrOrNil(row.ATLValue), timeOrNil(row.ATLAt),
		strOrNil(row.StreakDirection), intOrNil(row.StreakDays),
		strOrNil(row.Acceleration),
	)
	if err != nil {
		return fmt.Errorf("timescale: UpsertChangeSummary %s/%s: %w",
			row.EntityType, row.EntityID, err)
	}
	return nil
}

// GetChangeSummary returns the current row for (entity_type,
// entity_id), or sql.ErrNoRows when the worker hasn't computed it
// yet. API handlers translate that into the price-not-found path.
func (s *Store) GetChangeSummary(ctx context.Context, entityType, entityID string) (ChangeSummaryRow, error) {
	const q = `
		SELECT entity_type, entity_id, refreshed_at, current_value::text,
		       h1_value::text, h1_delta_pct, h24_value::text, h24_delta_pct,
		       d7_value::text, d7_delta_pct, d30_value::text, d30_delta_pct,
		       ath_value::text, ath_at, atl_value::text, atl_at,
		       streak_direction, streak_days, acceleration
		  FROM change_summary_5m
		 WHERE entity_type = $1 AND entity_id = $2
	`
	var row ChangeSummaryRow
	var (
		h1V, h24V, d7V, d30V sql.NullString
		h1D, h24D, d7D, d30D sql.NullFloat64
		athV, atlV           sql.NullString
		athAt, atlAt         sql.NullTime
		streakDir, accel     sql.NullString
		streakDays           sql.NullInt64
	)
	err := s.db.QueryRowContext(ctx, q, entityType, entityID).Scan(
		&row.EntityType, &row.EntityID, &row.RefreshedAt, &row.CurrentValue,
		&h1V, &h1D, &h24V, &h24D, &d7V, &d7D, &d30V, &d30D,
		&athV, &athAt, &atlV, &atlAt,
		&streakDir, &streakDays, &accel,
	)
	if err != nil {
		return ChangeSummaryRow{}, err
	}
	row.H1Value = nullStr(h1V)
	row.H1DeltaPct = nullFloat(h1D)
	row.H24Value = nullStr(h24V)
	row.H24DeltaPct = nullFloat(h24D)
	row.D7Value = nullStr(d7V)
	row.D7DeltaPct = nullFloat(d7D)
	row.D30Value = nullStr(d30V)
	row.D30DeltaPct = nullFloat(d30D)
	row.ATHValue = nullStr(athV)
	row.ATLValue = nullStr(atlV)
	if athAt.Valid {
		t := athAt.Time
		row.ATHAt = &t
	}
	if atlAt.Valid {
		t := atlAt.Time
		row.ATLAt = &t
	}
	if streakDir.Valid {
		row.StreakDirection = streakDir.String
	}
	if streakDays.Valid {
		v := int(streakDays.Int64)
		row.StreakDays = &v
	}
	if accel.Valid {
		row.Acceleration = accel.String
	}
	return row, nil
}

func floatOrNil(f *float64) any {
	if f == nil {
		return nil
	}
	return *f
}

func timeOrNil(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.UTC()
}

func intOrNil(i *int) any {
	if i == nil {
		return nil
	}
	return *i
}

func strOrNil(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nullFloat(n sql.NullFloat64) *float64 {
	if !n.Valid {
		return nil
	}
	return &n.Float64
}

func nullStr(n sql.NullString) *string {
	if !n.Valid {
		return nil
	}
	return &n.String
}

func strPtrOrNil(s *string) any {
	if s == nil {
		return nil
	}
	return *s
}

// timedVWAPs1mForChangeSummaryQuery reads both stored orientations of
// the pair over the CLOSED buckets in [from, to), oldest-first.
//
// Closed per ADR-0015: a bucket is admitted only once it has ended by
// both the caller's `to` and the database clock, so the in-progress
// minute never becomes current_value or a ratcheted ath/atl (the upsert
// keeps GREATEST/LEAST for good). Sargable form, as in aggregates.go.
//
// Both directions for the same reason every other pair-bound CAGG read
// does it (see [dirVWAP] and TestCAGGPairReadsFoldBothDirections): the
// decoder does not normalise orientation, so the market lands in
// prices_1m as both (A,B) and (B,A) rows. This query filtered one
// orientation until 2026-08-31 — the third instance of that class, and
// the one neither wave-D UNAUTH-DOS-9 nor its skeptic found; the class
// guard did.
//
// Ordering is ASC here rather than DESC, which
// [scanCombinedVwap1mRows] handles unchanged — it only requires that
// rows of the same bucket be adjacent, which any bucket ordering
// gives.
const timedVWAPs1mForChangeSummaryQuery = `
		SELECT bucket, base_asset, vwap::text, COALESCE(volume, 0)::text,
		       COALESCE(trade_count, 0), sources
		  FROM (
		    SELECT bucket, base_asset, vwap, volume, trade_count, sources
		      FROM prices_1m
		     WHERE base_asset = $1 AND quote_asset = $2
		       AND bucket >= $3
		       AND bucket <= LEAST($4::timestamptz, now()) - INTERVAL '1 minute'
		    UNION ALL
		    SELECT bucket, base_asset, vwap, volume, trade_count, sources
		      FROM prices_1m
		     WHERE base_asset = $2 AND quote_asset = $1
		       AND bucket >= $3
		       AND bucket <= LEAST($4::timestamptz, now()) - INTERVAL '1 minute'
		  ) u
		 ORDER BY bucket ASC, base_asset ASC
	`

// TimedVWAPs1m is a thin adapter so [changesummary.PriceSource] is
// satisfied by the existing TimedVWAPsForPair1m without dragging the
// baseline package's TimedVWAP type into the changesummary public
// surface. Returns oldest-first, each point COMBINED across both
// stored market directions so the value is the price of Base in Quote.
//
// `At` is the bucket END (`bucket + 1 minute`), which is what the
// change-summary worker timestamps a closed bucket by; the addition is
// done in Go rather than SQL so the query keeps the raw per-direction
// shape the combine needs.
func (s *Store) TimedVWAPs1mForChangeSummary(ctx context.Context, pair canonical.Pair, from, to time.Time) ([]ChangeSummaryPoint, error) {
	rows, err := s.db.QueryContext(ctx, timedVWAPs1mForChangeSummaryQuery,
		pair.Base.String(), pair.Quote.String(), from.UTC(), to.UTC())
	if err != nil {
		return nil, fmt.Errorf("timescale: TimedVWAPs1mForChangeSummary: %w", err)
	}
	defer func() { _ = rows.Close() }()

	folded, err := scanCombinedVwap1mRows(rows, pair, 0, "TimedVWAPs1mForChangeSummary")
	if err != nil {
		return nil, err
	}
	out := make([]ChangeSummaryPoint, 0, len(folded))
	for _, r := range folded {
		out = append(out, ChangeSummaryPoint{
			At:    r.Bucket.Add(time.Minute),
			Value: r.VWAP,
		})
	}
	return out, nil
}

// ChangeSummaryPoint is the read-projection used by the changesummary
// worker. Decoupled from baseline.TimedVWAP so the worker package
// doesn't need to import baseline.
type ChangeSummaryPoint struct {
	At    time.Time
	Value string
}
