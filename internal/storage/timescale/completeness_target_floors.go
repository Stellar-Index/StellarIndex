package timescale

import (
	"context"
	"fmt"
)

// CompletenessTargetFloor is one projection target's durable verification
// floor (migration 0116, ADR-0033 — the N-F2 residual).
//
// VerifiedFrom is the LOWEST ledger this target has ever been reconciled
// from. It exists because compute-completeness otherwise derives its floor
// from MIN(ledger) of the target itself, which makes the check blind to the
// one event it exists to catch: if the oldest served rows are deleted,
// MIN(ledger) rises with the loss and the reconcile range follows it up, so
// "rows were dropped" and "we never projected below there" are
// indistinguishable.
type CompletenessTargetFloor struct {
	Source string
	// Table and Filter together identify the target. Filter is
	// reconTarget.whereFilter ("" when the whole table belongs to the
	// source) and is part of the identity because one table — `trades` —
	// is split across sources, and those slices can honestly have
	// different true floors.
	Table        string
	Filter       string
	VerifiedFrom uint32
}

// UpsertCompletenessTargetFloor records that `floor.Table` has been
// reconciled from `floor.VerifiedFrom`, keeping the LOWEST value ever seen.
//
// The LEAST() is the whole point and is deliberately not a plain assignment.
// The floor must only ever fall:
//
//   - A run reconciling from a LOWER ledger than recorded is new ground and
//     legitimately lowers the floor.
//   - A run starting HIGHER is either real loss — which
//     [Store.CompletenessTargetFloors]'s caller must FAIL on, not record — or
//     a deliberately narrowed `-from` window, which is not evidence about
//     the target's true floor at all.
//
// Letting the column rise would allow a partial or narrowed run to quietly
// ratchet the floor up to the post-loss MIN and re-create the exact bug
// migration 0116 closes. Same regressive-run hazard CS-083 guards on
// completeness_snapshots, in the opposite direction.
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
// A target absent from the map has no recorded floor yet — that is the
// first-run case and must be treated as "nothing to compare against", never
// as a floor of zero. Reporting loss against a floor that was never
// established would fail every target on the first run after this migration.
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
// Exported so the caller keys its lookups identically rather than
// reimplementing the join and drifting.
//
// The separator is "\x00" rather than something readable: whereFilter is
// arbitrary SQL and can contain any punctuation, so a printable separator
// could be produced by the filter text itself and collide two distinct
// targets onto one key.
func TargetFloorKey(source, table, filter string) string {
	return source + "\x00" + table + "\x00" + filter
}
