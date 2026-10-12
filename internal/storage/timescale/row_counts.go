package timescale

import (
	"context"
	"database/sql"
	"fmt"
)

// opsVerifyStatementTimeoutMS bounds statement_timeout for trusted ops counts (CountRowsByLedger, MinLedger) that scan full-history
// hypertables for tens of minutes; a short cap silently fails projection verify. Not for the gap detector (15 min context; a 2h PG
// timeout orphans backends, see gapDetectorStatementTimeoutMS).
const opsVerifyStatementTimeoutMS = 7200000 // 2 hours, in ms

// MinLedger returns the smallest ledger in a per-source table over [from, to], i.e. where that target's served data begins, the lower
// bound of the projection reconcile; a hardcoded floor would certify only a recent slice.
func (s *Store) MinLedger(ctx context.Context, table, ledgerColumn, whereFilter string, from, to uint32) (uint32, bool, error) {
	filter := ""
	if whereFilter != "" {
		filter = " AND (" + whereFilter + ")"
	}
	//nolint:gosec // G201: identifiers are compile-time-trusted per ADR-0030, never user input.
	query := fmt.Sprintf(`SELECT MIN(%[1]s) FROM %[2]s WHERE %[1]s BETWEEN $1 AND $2%[3]s`,
		ledgerColumn, table, filter)

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, false, fmt.Errorf("timescale: MinLedger begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, fmt.Sprintf("SET LOCAL statement_timeout = '%d'", opsVerifyStatementTimeoutMS)); err != nil {
		return 0, false, fmt.Errorf("timescale: MinLedger SET: %w", err)
	}
	var minL sql.NullInt64
	if err := tx.QueryRowContext(ctx, query, int64(from), int64(to)).Scan(&minL); err != nil {
		return 0, false, fmt.Errorf("timescale: MinLedger %s [%d,%d]: %w", table, from, to, err)
	}
	if !minL.Valid {
		return 0, false, nil
	}
	return uint32(minL.Int64), true, nil
}

// CountRowsByLedger returns rows-per-ledger for a per-source protocol table over [from, to], the actual side of projection
// reconciliation; table, ledgerColumn and whereFilter are interpolated, so callers must pass trusted constants.
func (s *Store) CountRowsByLedger(ctx context.Context, table, ledgerColumn, whereFilter string, from, to uint32) (map[uint32]int, error) {
	if to < from {
		return nil, fmt.Errorf("timescale: CountRowsByLedger: to (%d) < from (%d)", to, from)
	}
	filter := ""
	if whereFilter != "" {
		filter = " AND (" + whereFilter + ")"
	}
	//nolint:gosec // G201: identifiers are compile-time-trusted per ADR-0030, never user input.
	query := fmt.Sprintf(
		`SELECT %[1]s, COUNT(*) FROM %[2]s WHERE %[1]s BETWEEN $1 AND $2%[3]s GROUP BY %[1]s`,
		ledgerColumn, table, filter,
	)

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("timescale: CountRowsByLedger begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, fmt.Sprintf("SET LOCAL statement_timeout = '%d'", opsVerifyStatementTimeoutMS)); err != nil {
		return nil, fmt.Errorf("timescale: CountRowsByLedger SET: %w", err)
	}

	rows, err := tx.QueryContext(ctx, query, int64(from), int64(to))
	if err != nil {
		return nil, fmt.Errorf("timescale: CountRowsByLedger %s [%d,%d]: %w", table, from, to, err)
	}
	defer func() { _ = rows.Close() }()

	out := make(map[uint32]int)
	for rows.Next() {
		var ledger int64
		var n int64
		if err := rows.Scan(&ledger, &n); err != nil {
			return nil, fmt.Errorf("timescale: CountRowsByLedger scan: %w", err)
		}
		out[uint32(ledger)] = int(n)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("timescale: CountRowsByLedger rows: %w", err)
	}
	return out, nil
}
