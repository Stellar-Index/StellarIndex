package timescale

import (
	"context"
	"fmt"
	"time"
)

// SourceCoverage is the ADR-0031 unified read shape for "how
// covered is source S right now." Derived purely from data
// (count-distinct vs gap-detector gauges); no cursor inputs.
//
// Two complementary numbers per source:
//
//   - **DensityPct** = DistinctLedgers / ExpectedLedgers (capped
//     [0, 1]). Raw coverage. Dense sources (SDEX, Soroswap)
//     approach 1.0; sparse sources (Blend auctions, CCTP) are
//     naturally low because the contract doesn't emit per ledger.
//   - **GapFreePct** = 1 - MaxGapLedgers / ExpectedLedgers. Goes
//     to 1.0 when no contiguous gap larger than the per-target
//     [GapDetectorTarget.EffectiveMinGapSize] threshold exists.
//     Sparse sources running cleanly hit 1.0 even though their
//     DensityPct is low — they're emitting on cadence, just not
//     every ledger.
//
// Both honest. Together they say "we have X% of emitted events
// AND no unexplained breaks." A cascade incident drops both;
// natural sparsity drops only DensityPct.
//
// LastUpdated is the gap detector's last successful cycle for
// this source — operators read this to confirm the signal isn't
// stale during an incident.
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

// CountDistinctLedgers returns COUNT(DISTINCT <ledger column>)
// for the given target's hypertable, optionally filtered by
// the target's WhereFilter, restricted to [from, to].
//
// SAFETY: target.Table, target.LedgerColumn, target.WhereFilter
// are interpolated directly into the SQL — same identifier
// injection concern + same construction discipline as
// [FindPerSourceLedgerGaps] (ADR-0030: caller MUST pass a
// compile-time const from [DefaultGapDetectorTargets]).
//
// Returns 0 (no error) when to == 0 — the caller hasn't resolved
// tip yet, so there's nothing meaningful to scan. Same shape as
// [FindPerSourceLedgerGaps] so callers can use both in one
// branch.
func (s *Store) CountDistinctLedgers(ctx context.Context, target GapDetectorTarget, from, to int64) (int64, error) {
	if from < 0 || to < from {
		return 0, fmt.Errorf("timescale: CountDistinctLedgers invalid range [%d,%d]", from, to)
	}
	if to == 0 {
		return 0, nil
	}

	filter := ""
	if target.WhereFilter != "" {
		filter = " AND (" + target.WhereFilter + ")"
	}
	//nolint:gosec // G201: identifiers from compile-time const list per ADR-0030
	query := fmt.Sprintf(
		`SELECT COUNT(DISTINCT %[1]s) FROM %[2]s WHERE %[1]s BETWEEN $1 AND $2%[3]s`,
		target.LedgerColumn, target.Table, filter,
	)

	var n int64
	if err := s.db.QueryRowContext(ctx, query, from, to).Scan(&n); err != nil {
		return 0, fmt.Errorf("timescale: CountDistinctLedgers %s [%d,%d]: %w",
			target.Table, from, to, err)
	}
	return n, nil
}

// ExpectedLedgersFor returns the size of the [genesis, tip]
// window for a source. For sources without a known genesis
// (off-chain CEX/FX, external pollers) returns 0 + nil — the
// caller surfaces a freshness signal instead of a density.
//
// Encoded here rather than in obs/handler so the projection's
// numerator and denominator come from one helper, no risk of the
// two being computed against different windows. ADR-0031 §
// "Single SQL helper, one read path".
func ExpectedLedgersFor(genesis, tip int64) int64 {
	if genesis <= 0 || tip <= 0 || tip < genesis {
		return 0
	}
	return tip - genesis + 1
}

// SourceCoverageFromCounts assembles a [SourceCoverage] from the
// raw inputs the gap detector already computes per cycle:
//
//   - distinctLedgers: from CountDistinctLedgers
//   - expectedLedgers: from ExpectedLedgersFor(genesis, tip)
//   - maxGap, gapCount: from the existing FindPerSourceLedgerGaps
//     result reduction
//
// Centralised so the percentage math has one home — the
// /v1/diagnostics/ingestion handler reads the resulting struct,
// it doesn't recompute. Returned percentages are honest 0..1
// floats; the handler renders them as basis points or %.
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

// percentagesFromCounts factors the density + gap_free math into
// one place so the formulas can be reviewed in isolation. Both
// returns are capped [0, 1].
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
