package timescale

import (
	"context"
	"fmt"
)

// CompletenessTargetFloor is one projection target's durable verification floor; VerifiedFrom is the lowest ledger ever reconciled, kept apart from MIN(ledger) so deletions of the oldest rows cannot hide.
type CompletenessTargetFloor struct {
	Source string
	// Table and Filter together identify the target. Filter is reconTarget.whereFilter ("" when
	// the whole table belongs to the source); it is part of the identity because `trades` is split
	// across sources whose slices can honestly have different true floors.
	Table        string
	Filter       string
	VerifiedFrom uint32
}

// UpsertCompletenessTargetFloor records that `floor.Table` was reconciled from `floor.VerifiedFrom`, keeping the LOWEST value ever seen via LEAST(); the floor only falls, a higher start is loss or a narrower run.
func (s *Store) UpsertCompletenessTargetFloor(ctx context.Context, floor CompletenessTargetFloor) error {
	const q = `
        INSERT INTO completeness_target_floors (
            source, target_table, target_filter, projection_verified_from
        ) VALUES ($1,$2,$3,$4)
        ON CONFLICT (source, target_table, target_filter) DO UPDATE SET
            projection_verified_from = LEAST(
                completeness_target_floors.projection_verified_from,
                EXCLUDED.projection_verified_from),
            updated_at = now()`
	if _, err := s.db.ExecContext(ctx, q,
		floor.Source, floor.Table, floor.Filter, int64(floor.VerifiedFrom),
	); err != nil {
		return fmt.Errorf("timescale: UpsertCompletenessTargetFloor (%s/%s): %w",
			floor.Source, floor.Table, err)
	}
	return nil
}

// CompletenessTargetFloors returns every recorded floor keyed by (source, table, filter) via [TargetFloorKey]; an absent target has no floor yet (nothing to compare), never a floor of zero.
func (s *Store) CompletenessTargetFloors(ctx context.Context) (map[string]CompletenessTargetFloor, error) {
	const q = `
        SELECT source, target_table, target_filter, projection_verified_from
        FROM completeness_target_floors`
	rows, err := s.db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("timescale: CompletenessTargetFloors: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := make(map[string]CompletenessTargetFloor)
	for rows.Next() {
		var f CompletenessTargetFloor
		var from int64
		if err := rows.Scan(&f.Source, &f.Table, &f.Filter, &from); err != nil {
			return nil, fmt.Errorf("timescale: CompletenessTargetFloors scan: %w", err)
		}
		f.VerifiedFrom = uint32(from) //nolint:gosec // ledger seq, bounded by the network head
		out[TargetFloorKey(f.Source, f.Table, f.Filter)] = f
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("timescale: CompletenessTargetFloors rows: %w", err)
	}
	return out, nil
}

// TargetFloorKey builds the map key for a (source, table, filter) target.
func TargetFloorKey(source, table, filter string) string {
	return source + "\x00" + table + "\x00" + filter
}
