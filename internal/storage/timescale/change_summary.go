package timescale

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// ChangeSummaryRow mirrors the change_summary_5m table.
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

	// Normalized marks a AssetChangeFromSnapshot row: CurrentValue is already decimals-normalised USD, so readers must not rescale it.
	Normalized bool
}

// AssetChangeFromSnapshot reads an asset's headline USD price and 1h/24h/7d change from asset_price_snapshot, for assets the change-summary
// worker has no row for (it covers configured pairs only). No 30d, ATH/ATL or streak exists there. sql.ErrNoRows when the asset is
// unpriced or its row is past the listing's age ceiling.
func (s *Store) AssetChangeFromSnapshot(ctx context.Context, assetID string) (ChangeSummaryRow, error) {
	const q = `SELECT asset_id, computed_at, price_usd::text, change_1h_pct::float8, change_24h_pct::float8, change_7d_pct::float8
		  FROM asset_price_snapshot
		 WHERE asset_id = $1 AND computed_at >= now() - INTERVAL '` + assetPriceSnapshotMaxAge + `'`
	row := ChangeSummaryRow{EntityType: "coin", Normalized: true}
	var h1, h24, d7 sql.NullFloat64
	if err := s.db.QueryRowContext(ctx, q, assetID).Scan(&row.EntityID, &row.RefreshedAt, &row.CurrentValue, &h1, &h24, &d7); err != nil {
		return ChangeSummaryRow{}, err
	}
	row.H1DeltaPct = nullFloat(h1)
	row.H24DeltaPct = nullFloat(h24)
	row.D7DeltaPct = nullFloat(d7)
	return row, nil
}

// UpsertChangeSummary inserts or replaces the row keyed on (entity_type, entity_id).
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

// GetChangeSummary returns the current row for (entity_type, entity_id), or sql.ErrNoRows before the worker has computed it.
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

// DeleteChangeSummary removes the row for (entity_type, entity_id) and reports whether one existed; the only way to clear an ath/atl
// pinned by the upsert ratchet.
func (s *Store) DeleteChangeSummary(ctx context.Context, entityType, entityID string) (bool, error) {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM change_summary_5m WHERE entity_type = $1 AND entity_id = $2`,
		entityType, entityID)
	if err != nil {
		return false, fmt.Errorf("timescale: DeleteChangeSummary %s/%s: %w", entityType, entityID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("timescale: DeleteChangeSummary %s/%s: %w", entityType, entityID, err)
	}
	return n > 0, nil
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

// timedVWAPs1mForChangeSummaryQuery reads both stored orientations over closed buckets in [from, to), oldest-first (ADR-0015).
// A bucket is admitted only once ended by both `to` and the DB clock, so the live minute never ratchets ath/atl; both directions are read
// ([dirVWAP], TestSourceShapeRules/cagg_pair_reads_fold_both_directions) since the decoder does not normalise orientation. ASC suits [scanCombinedVwap1mRows].
const timedVWAPs1mForChangeSummaryQuery = `
		SELECT bucket, base_asset, vwap::text, COALESCE(volume_priced, 0)::text,
		       COALESCE(trade_count, 0), sources
		  FROM (
		    SELECT bucket, base_asset, vwap, volume_priced, trade_count, sources
		      FROM prices_1m
		     WHERE base_asset = $1 AND quote_asset = $2
		       AND bucket >= $3
		       AND bucket <= LEAST($4::timestamptz, now()) - INTERVAL '1 minute'
		    UNION ALL
		    SELECT bucket, base_asset, vwap, volume_priced, trade_count, sources
		      FROM prices_1m
		     WHERE base_asset = $2 AND quote_asset = $1
		       AND bucket >= $3
		       AND bucket <= LEAST($4::timestamptz, now()) - INTERVAL '1 minute'
		  ) u
		 ORDER BY bucket ASC, base_asset ASC
	`

// TimedVWAPs1mForChangeSummary adapts TimedVWAPsForPair1m to [changesummary.PriceSource]; points are oldest-first, combined across both
// directions as the price of Base in Quote, with `At` the bucket end.
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

// ChangeSummaryPoint is the read-projection used by the changesummary worker.
type ChangeSummaryPoint struct {
	At    time.Time
	Value string
}
