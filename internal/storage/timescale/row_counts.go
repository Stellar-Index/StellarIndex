package timescale

import (
	"context"
	"database/sql"
	"fmt"
)

// opsVerifyStatementTimeoutMS bounds statement_timeout for the full-history ops/verify counts
// (CountRowsByLedger, MinLedger). These are trusted ops jobs, never the request path (whose DoS
// backstop is OpenServing's session statement_timeout), and they scan full-history hypertables
// that legitimately take tens of minutes as the chain grows; a short cap silently fails the
// completeness projection verify on large sources. 2h leaves headroom yet still catches a stuck
// query; the caller's context is the real outer bound.
//
// The aggregator's gap detector must NOT use this constant: its Go context is 15 min, and a 2h PG
// timeout there orphans backends (see gapDetectorStatementTimeoutMS).
const opsVerifyStatementTimeoutMS = 7200000 // 2 hours, in ms

// MinLedger returns the smallest ledger present in a per-source table over
// [from, to], i.e. where that target's served data actually begins. It is the lower bound of the
// projection reconcile (chops.targetScope); a hardcoded floor such as `tip - 1_500_000` would
// certify only the last ~100 days while the served tier holds full history.
//
// No reconcile target has a retention policy, so a rising value here is unambiguously served-tier
// loss, not a drop_chunks artifact. The optional prices_1m retention does not weaken that: it is a
// continuous aggregate with no `ledger` column and not a reconcile target (raw event and trade
// tables only). chops.detectFloorLoss compares this value against the durable floor in
// completeness_target_floors, so it must stay a raw observation, never clamped or defaulted.
// Returns ok=false if no rows.
//
// Identifiers are interpolated; callers must pass compile-time-trusted values (as for
// CountRowsByLedger).
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

// CountRowsByLedger returns rows-per-ledger for a per-source protocol table over [from, to], the
// "actual" side of the projection reconciliation (the expected side is the decoder re-derive,
// internal/completeness.ReDeriveOutputCounts).
//
// table / ledgerColumn / whereFilter are interpolated into the SQL, so callers must pass
// compile-time-trusted identifiers (as for GapDetectorTarget), never user input.
//
// opsVerifyStatementTimeoutMS backstops the GROUP BY on large full-history hypertables; the
// caller's context is the real bound.
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
