package timescale

import (
	"context"
	"fmt"
)

// CompletenessTargetFloor is one projection target's durable verification floor (ADR-0033).
//
// VerifiedFrom is the lowest ledger the target has ever been reconciled from. Deriving the floor
// from MIN(ledger) of the target itself would blind the check to deletions: if the oldest rows
// are lost, MIN(ledger) rises with them, so "rows were dropped" and "never projected below
// there" become indistinguishable.
type CompletenessTargetFloor struct {
	Source string
	// Table and Filter together identify the target. Filter is reconTarget.whereFilter ("" when
	// the whole table belongs to the source); it is part of the identity because `trades` is split
	// across sources whose slices can honestly have different true floors.
	Table        string
	Filter       string
	VerifiedFrom uint32
}

// UpsertCompletenessTargetFloor records that `floor.Table` has been
// reconciled from `floor.VerifiedFrom`, keeping the LOWEST value ever seen.
//
// LEAST() is deliberate, not a plain assignment: the floor must only ever fall. A run from a lower
// ledger is new ground; a run starting higher is either real loss (the caller of
// [Store.CompletenessTargetFloors] must fail on it, not record it) or a narrowed `-from` window,
// which says nothing about the true floor. Letting the column rise would let such a run ratchet
// the floor up to the post-loss MIN and hide the very loss the durable floor exists to expose.
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

// CompletenessTargetFloors returns every recorded floor, keyed by
// (source, table, filter) via [TargetFloorKey].
//
// A target absent from the map has no recorded floor yet and means "nothing to compare against",
// never a floor of zero; reporting loss against an unestablished floor would fail every target on
// its first run.
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
// Exported so callers key lookups identically instead of reimplementing the join.
//
// The separator is "\x00" because whereFilter is arbitrary SQL: a printable separator could
// appear in the filter text itself and collide two distinct targets onto one key.
func TargetFloorKey(source, table, filter string) string {
	return source + "\x00" + table + "\x00" + filter
}
