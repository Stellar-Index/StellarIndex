package timescale

import (
	"context"
	"database/sql"
	"fmt"
)

// MinLedger returns the smallest ledger present in a per-source table over
// [from, to] — the ACTUAL retained boundary for drop_chunks-managed tables
// (trades retains ~90d). Used to floor the projection reconcile at where served
// data really begins: retentionStart (tip-1.5M ≈ 100d) can fall BELOW the oldest
// retained chunk, and counting census>0 vs served=0 for those dropped ledgers is
// a retention artifact, not a coverage gap. Returns ok=false if no rows.
//
// Identifiers are interpolated — callers MUST pass compile-time-trusted values
// (same discipline as CountRowsByLedger / ADR-0030).
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
	if _, err := tx.ExecContext(ctx, "SET LOCAL statement_timeout = '300000'"); err != nil {
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

// CountRowsByLedger returns rows-per-ledger for a per-source protocol
// table over [from, to] — the "actual" side of the ADR-0033 Claim 2b
// projection reconciliation. The expected side is the decoder
// re-derive over soroban_events (internal/completeness.ReDeriveOutputCounts).
//
// table / ledgerColumn / whereFilter are interpolated into the SQL, so
// callers MUST pass compile-time-trusted identifiers (the same
// discipline as GapDetectorTarget per ADR-0030) — never user input.
//
// A 5-minute SQL statement_timeout backstops the GROUP BY on large
// tables (trades) the same way CountDistinctLedgers does.
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
	if _, err := tx.ExecContext(ctx, "SET LOCAL statement_timeout = '300000'"); err != nil {
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
