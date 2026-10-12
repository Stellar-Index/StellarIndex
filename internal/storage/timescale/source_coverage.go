package timescale

import (
	"context"
	"fmt"
	"time"
)

// SourceCoverage is the data-derived read shape for how covered a source is (distinct-ledger density vs gap-detector gauges), with no cursor inputs; DensityPct is DistinctLedgers/ExpectedLedgers capped to [0, 1].
type SourceCoverage struct {
	Source          string
	Table           string
	DistinctLedgers int64
	ExpectedLedgers int64
	MaxGapLedgers   int64
	GapCount        int64
	DensityPct      float64
	GapFreePct      float64
	LastUpdated     time.Time
}

// countDistinctLedgersQuery is the statement [Store.CountDistinctLedgers] runs for a target: its DistinctLedgerCountSQL override, else the
// generic COUNT(DISTINCT <ledger column>); factored out for TestCountDistinctLedgersQueryRoutesOnlySorobanEventsToCensus.
func countDistinctLedgersQuery(target GapDetectorTarget) string {
	if target.DistinctLedgerCountSQL != "" {
		return target.DistinctLedgerCountSQL
	}
	filter := ""
	if target.WhereFilter != "" {
		filter = " AND (" + target.WhereFilter + ")"
	}
	//nolint:gosec // G201: identifiers from compile-time const list per ADR-0030
	return fmt.Sprintf(
		`SELECT COUNT(DISTINCT %[1]s) FROM %[2]s WHERE %[1]s BETWEEN $1 AND $2%[3]s`,
		target.LedgerColumn, target.Table, filter,
	)
}

// CountDistinctLedgers returns the number of ledgers in [from, to] the target has data for, via COUNT(DISTINCT <ledger column>) (optionally filtered) or the target's cheaper DistinctLedgerCountSQL when set.
func (s *Store) CountDistinctLedgers(ctx context.Context, target GapDetectorTarget, from, to int64) (int64, error) {
	if from < 0 || to < from {
		return 0, fmt.Errorf("timescale: CountDistinctLedgers invalid range [%d,%d]", from, to)
	}
	if to == 0 {
		return 0, nil
	}
	query := countDistinctLedgersQuery(target)

	// SQL-level statement_timeout, as in FindPerSourceLedgerGaps: Go-side cancellation does not
	// always propagate to PG, so the PG-side abort is what stops scans accumulating across
	// cycles. It MUST be the gap detector's own [gapDetectorStatementTimeoutMS] (13 min, under the
	// 15-min Go per-target budget), NOT the 2h ops constant, which would let an over-budget count
	// outlive its Go context as an orphan backend.
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("timescale: CountDistinctLedgers begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, fmt.Sprintf("SET LOCAL statement_timeout = '%d'", gapDetectorStatementTimeoutMS)); err != nil {
		return 0, fmt.Errorf("timescale: CountDistinctLedgers SET: %w", err)
	}
	var n int64
	if err := tx.QueryRowContext(ctx, query, from, to).Scan(&n); err != nil {
		return 0, fmt.Errorf("timescale: CountDistinctLedgers %s [%d,%d]: %w",
			target.Table, from, to, err)
	}
	return n, nil
}

// ExpectedLedgersFor returns the size of the [genesis, tip] window for a source.
func ExpectedLedgersFor(genesis, tip int64) int64 {
	if genesis <= 0 || tip <= 0 || tip < genesis {
		return 0
	}
	return tip - genesis + 1
}

// SourceCoverageFromCounts assembles a SourceCoverage from the gap detector's per-cycle inputs (distinct and expected ledgers, maxGap, gapCount) so the derivation lives in one place.
func SourceCoverageFromCounts(source, table string, distinctLedgers, expectedLedgers, maxGap, gapCount int64, lastUpdated time.Time) SourceCoverage {
	density, gapFree := percentagesFromCounts(distinctLedgers, expectedLedgers, maxGap)
	return SourceCoverage{
		Source:          source,
		Table:           table,
		DistinctLedgers: distinctLedgers,
		ExpectedLedgers: expectedLedgers,
		MaxGapLedgers:   maxGap,
		GapCount:        gapCount,
		DensityPct:      density,
		GapFreePct:      gapFree,
		LastUpdated:     lastUpdated,
	}
}

// percentagesFromCounts holds the density and gap_free math in one place; both returns are capped to [0, 1].
func percentagesFromCounts(distinct, expected, maxGap int64) (density, gapFree float64) {
	if expected <= 0 {
		return 0, 0
	}
	density = float64(distinct) / float64(expected)
	if density > 1 {
		density = 1
	} else if density < 0 {
		density = 0
	}
	gapFree = 1 - float64(maxGap)/float64(expected)
	if gapFree > 1 {
		gapFree = 1
	} else if gapFree < 0 {
		gapFree = 0
	}
	return density, gapFree
}

// UpsertSourceCoverage writes one source_coverage_snapshots row for (source, table), overwriting on PK conflict.
func (s *Store) UpsertSourceCoverage(ctx context.Context, cov SourceCoverage) error {
	const q = `
        INSERT INTO source_coverage_snapshots
            (source, "table", distinct_ledgers, expected_ledgers,
             max_gap_ledgers, gap_count, density_pct, gap_free_pct,
             last_updated)
        VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
        ON CONFLICT (source, "table") DO UPDATE
          SET distinct_ledgers = EXCLUDED.distinct_ledgers,
              expected_ledgers = EXCLUDED.expected_ledgers,
              max_gap_ledgers  = EXCLUDED.max_gap_ledgers,
              gap_count        = EXCLUDED.gap_count,
              density_pct      = EXCLUDED.density_pct,
              gap_free_pct     = EXCLUDED.gap_free_pct,
              last_updated     = EXCLUDED.last_updated
    `
	if _, err := s.db.ExecContext(ctx, q,
		cov.Source, cov.Table,
		cov.DistinctLedgers, cov.ExpectedLedgers,
		cov.MaxGapLedgers, cov.GapCount,
		cov.DensityPct, cov.GapFreePct,
		cov.LastUpdated,
	); err != nil {
		return fmt.Errorf("timescale: UpsertSourceCoverage: %w", err)
	}
	return nil
}

// ListSourceCoverage returns every source_coverage_snapshots row ordered by source, one cheap query for the /v1/diagnostics/ingestion handler.
func (s *Store) ListSourceCoverage(ctx context.Context) ([]SourceCoverage, error) {
	const q = `
        SELECT source, "table", distinct_ledgers, expected_ledgers,
               max_gap_ledgers, gap_count, density_pct, gap_free_pct,
               last_updated
          FROM source_coverage_snapshots
         ORDER BY source ASC
    `
	rows, err := s.db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("timescale: ListSourceCoverage: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []SourceCoverage
	for rows.Next() {
		var c SourceCoverage
		if err := rows.Scan(
			&c.Source, &c.Table,
			&c.DistinctLedgers, &c.ExpectedLedgers,
			&c.MaxGapLedgers, &c.GapCount,
			&c.DensityPct, &c.GapFreePct,
			&c.LastUpdated,
		); err != nil {
			return nil, fmt.Errorf("timescale: ListSourceCoverage scan: %w", err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("timescale: ListSourceCoverage rows: %w", err)
	}
	return out, nil
}
