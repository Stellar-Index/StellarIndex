package timescale

import (
	"context"
	"database/sql"
	"fmt"
)

// opsVerifyStatementTimeoutMS bounds the per-query statement_timeout for the
// full-history ops/verify counts (CountRowsByLedger, MinLedger). These are
// TRUSTED ops jobs — never the request path,
// whose DoS backstop is OpenServing's separate SESSION statement_timeout — and
// they scan full-history hypertables that legitimately take tens of minutes as
// the chain grows (e.g. sep41_supply_events: ~9.3M rows / 130 chunks over the
// Soroban range ≈ 40 min, index-only scan). The prior 5-minute cap SILENTLY
// FAILED the completeness projection verify on large sources ("canceling
// statement due to statement timeout"). 2h is generous headroom yet still
// catches a genuinely stuck query; the caller's context is the real outer
// bound. (per_source_gaps and sep41_supply_events already carry their own
// longer / caller-supplied timeouts. The aggregator's gap detector — gap
// scan AND source-coverage count — MUST NOT use this constant: its Go
// context is 15 min, and a 2h PG timeout there orphans backends; see
// gapDetectorStatementTimeoutMS.)
const opsVerifyStatementTimeoutMS = 7200000 // 2 hours, in ms

// MinLedger returns the smallest ledger present in a per-source table over
// [from, to] — where that target's served data ACTUALLY begins. It is the
// lower bound of the ADR-0033 Claim 2b projection reconcile (chops.targetScope),
// replacing the old hardcoded `tip - 1_500_000` floor which certified only the
// last ~100 days while the served tier holds full history (DAT-09/N-F2).
//
// No reconcile target has a retention policy (verified against the migrations
// and r1, 2026-07-25 — 0031 removed it from trades / prices_1m / prices_15m and
// 0040 from oracle_updates), so a RISING value here is unambiguously served-tier
// LOSS, not a drop_chunks artifact. chops.detectFloorLoss makes exactly that
// comparison against the durable floor in completeness_target_floors
// (migration 0116), which is why this value must stay a raw observation and not
// be clamped or defaulted. Returns ok=false if no rows.
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

// CountRowsByLedger returns rows-per-ledger for a per-source protocol
// table over [from, to] — the "actual" side of the ADR-0033 Claim 2b
// projection reconciliation. The expected side is the decoder
// re-derive over soroban_events (internal/completeness.ReDeriveOutputCounts).
//
// table / ledgerColumn / whereFilter are interpolated into the SQL, so
// callers MUST pass compile-time-trusted identifiers (the same
// discipline as GapDetectorTarget per ADR-0030) — never user input.
//
// A generous ops statement_timeout (opsVerifyStatementTimeoutMS)
// backstops the GROUP BY on large full-history hypertables; the caller's
// context is the real bound. Raised from the old 5-min cap that failed the
// completeness verify on grown sources (sep41_supply_events).
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
