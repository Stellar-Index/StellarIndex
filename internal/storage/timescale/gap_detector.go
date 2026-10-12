package timescale

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/obs"
)

// GapDetectorInterval is how often [RunGapDetector] re-scans every per-source hypertable for gaps.
const GapDetectorInterval = 30 * time.Minute

// GapDetectorMinGapSize is the threshold below which a gap is treated as no-activity noise rather than an ingest gap.
const GapDetectorMinGapSize = int64(1000)

// gapDetectorPerTargetTimeout caps one per-target scan at 3x soroban_events's measured 5 min; the
// other tables finish far sooner. Per-target isolation keeps one slow table from poisoning the cycle.
const gapDetectorPerTargetTimeout = 15 * time.Minute

// gapDetectorStatementTimeoutMS is the PG-side statement_timeout for both per-target scans. INVARIANT: <= gapDetectorPerTargetTimeout,
// or abandoned queries stack across cycles (TestGapDetectorStatementTimeoutWithinGoBudget).
const gapDetectorStatementTimeoutMS = 780_000 // 13 min, in ms

// GapDetectorSafetyLookback bounds how far below tip one cycle re-scans (~200k ledgers, ~11.5 days), binding only after the detector was down long enough for the high-water to fall behind.
const GapDetectorSafetyLookback = int64(200_000)

// GapDetectorFirstScanCap bounds the FIRST-ever scan of a target to 2M ledgers (~115 days) so a new target or lost cursor never walks full history; deeper history is the completeness verdict's domain.
const GapDetectorFirstScanCap = int64(2_000_000)

// gapDetectorHighWaterSource is the ingestion_cursors source holding each target's scanned tip; kept out of "backfill"/"ledgerstream"
// namespaces.
const gapDetectorHighWaterSource = "gap-detector-scan"

// gapDetectorLastPresentSource holds, per target, the highest ledger PRESENT in the last window; it seeds
// [Store.FindPerSourceLedgerGaps] so a gap spanning a window boundary is not dropped.
const gapDetectorLastPresentSource = "gap-detector-last-present"

// computeGapScanWindow returns the window's lower bound: first run max(genesis, tip-FirstScanCap), else max(genesis, prevHighWater,
// tip-SafetyLookback). The gap scan and coverage math share it so density numerator and denominator match.
func computeGapScanWindow(genesis, tip, prevHighWater int64, firstRun bool) int64 {
	var from int64
	if firstRun {
		from = tip - GapDetectorFirstScanCap
	} else {
		from = tip - GapDetectorSafetyLookback
		if prevHighWater > from {
			from = prevHighWater
		}
	}
	if from < genesis {
		from = genesis
	}
	if from < 0 {
		from = 0
	}
	return from
}

// gapScanHighWater reads the persisted high-water; firstRun only if none was ever written. A read error returns (0, false) to use
// SafetyLookback.
func gapScanHighWater(ctx context.Context, store *Store, logger *slog.Logger, target GapDetectorTarget) (highWater int64, firstRun bool) {
	c, err := store.GetCursor(ctx, gapDetectorHighWaterSource, targetKey(target))
	switch {
	case err == nil:
		return int64(c.LastLedger), false
	case errors.Is(err, ErrNotFound):
		return 0, true
	default:
		logger.Warn("gap-detector: read scan high-water failed; using SafetyLookback trailing window",
			"source", target.Source, "table", target.Table, "err", err)
		return 0, false
	}
}

// gapScanSeed reads the last-present cursor, returning 0 (no seed) on none or error. DistinctLedgerCountSQL-override targets never get
// one: the MAX(ledger) probe costs the full scan the override avoids (see [maxLedgerInWindowQuery]).
func gapScanSeed(ctx context.Context, store *Store, target GapDetectorTarget) int64 {
	if target.DistinctLedgerCountSQL != "" {
		return 0
	}
	c, err := store.GetCursor(ctx, gapDetectorLastPresentSource, targetKey(target))
	if err != nil {
		return 0
	}
	return int64(c.LastLedger)
}

// persistGapScanSeed records the window's highest present ledger as the next seed (see [gapScanSeed]). UpsertCursor only moves forward;
// failures are non-fatal.
func persistGapScanSeed(ctx context.Context, store *Store, logger *slog.Logger, target GapDetectorTarget, from, tip int64) {
	if target.DistinctLedgerCountSQL != "" {
		return
	}
	maxPresent, ok, err := store.MaxLedgerInWindow(ctx, target, from, tip)
	if err != nil {
		logger.Warn("gap-detector: max-ledger-in-window failed (next-cycle seed unaffected)",
			"source", target.Source, "table", target.Table, "err", err)
		return
	}
	if !ok || maxPresent <= 0 {
		return
	}
	if err := store.UpsertCursor(ctx, gapDetectorLastPresentSource, targetKey(target), uint32(maxPresent)); err != nil { //nolint:gosec // maxPresent is bounded by tip, a uint32 ledger seq widened to int64
		logger.Warn("gap-detector: persist last-present seed failed",
			"source", target.Source, "table", target.Table, "err", err)
	}
}

// RunGapDetector blocks until ctx is cancelled, periodically scanning every applicable target in [DefaultGapDetectorTargets] for ledger-coverage gaps and emitting per-(source, table) gauges.
func RunGapDetector(ctx context.Context, store *Store, logger *slog.Logger, network string) error {
	if store == nil {
		return nil
	}
	if logger == nil {
		logger = slog.Default()
	}
	targets := ApplicableGapDetectorTargets(DefaultGapDetectorTargets, network, logger)

	// Per-target last-scan times gate each target's own cadence, so light targets run every 30 min
	// while huge tables (SDEX, soroban-events) run every 6h instead of stacking concurrent queries.
	// Seeded from the persisted high-water cursors so a RESTART honours the cadence; an empty map
	// would re-run the 6h scans on every deploy or crash-loop iteration.
	preregisterGapDetectorSeries(targets)
	snapshots, err := store.ListSourceCoverage(ctx)
	if err != nil {
		logger.Warn("gap-detector: read source_coverage_snapshots for boot seed failed; gauges stay empty until first scan", "err", err)
		snapshots = nil
	}
	lastScan := seedGapDetectorState(ctx, targets, store.GetCursor, snapshots, logger, time.Now())

	runOneGapDetectorCycleScheduled(ctx, store, logger, targets, lastScan)

	// Ticker fires at the LCD cadence (30 min). Each tick iterates
	// every target and only scans those whose individual cadence
	// has elapsed since the previous scheduled scan.
	ticker := time.NewTicker(GapDetectorInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			runOneGapDetectorCycleScheduled(ctx, store, logger, targets, lastScan)
		}
	}
}

// gapDetectorRunOutcomes is every value the gap detector emits on the
// `outcome` label of stellarindex_ingest_gap_detector_runs_total.
var gapDetectorRunOutcomes = []string{"ok", "error"}

// preregisterGapDetectorSeries sets runs_total to 0 per (source, table, outcome): a restart can run no scan for hours, and without the
// series the `stellarindex_ingest_gap_detector_silent` absent_over_time(runs_total[15m]) clause would read "not due" as "dead".
func preregisterGapDetectorSeries(targets []GapDetectorTarget) {
	for _, t := range targets {
		for _, outcome := range gapDetectorRunOutcomes {
			obs.IngestGapDetectorRunsTotal.WithLabelValues(t.Source, t.Table, outcome).Add(0)
		}
	}
}

// targetKey is the per-target last-scan dedupe identity; (source, table) matches the metric labels.
func targetKey(t GapDetectorTarget) string {
	return t.Source + "/" + t.Table
}

// runOneGapDetectorCycleScheduled wraps runOneGapDetectorCycle, scanning only targets whose EffectiveScanCadence has elapsed.
func runOneGapDetectorCycleScheduled(ctx context.Context, store *Store, logger *slog.Logger, targets []GapDetectorTarget, lastScan map[string]time.Time) {
	due := dueGapDetectorTargets(targets, lastScan, time.Now())
	if len(due) == 0 {
		logger.Debug("gap-detector: no targets due this cycle")
		return
	}
	runOneGapDetectorCycle(ctx, store, logger, due)
}

// dueGapDetectorTargets returns targets whose cadence elapsed (or never ran) and stamps lastScan at dispatch, not success, so a failing
// heavy scan retries on its cadence rather than every tick.
func dueGapDetectorTargets(targets []GapDetectorTarget, lastScan map[string]time.Time, now time.Time) []GapDetectorTarget {
	due := make([]GapDetectorTarget, 0, len(targets))
	for _, t := range targets {
		key := targetKey(t)
		if last, seen := lastScan[key]; seen && now.Sub(last) < t.EffectiveScanCadence() {
			continue
		}
		due = append(due, t)
		lastScan[key] = now
	}
	return due
}

// gapDetectorCursorReader is the one Store method the boot seed needs, as a func so
// [seedGapDetectorState] is testable without a database. (*Store).GetCursor satisfies it.
type gapDetectorCursorReader func(ctx context.Context, source, sub string) (Cursor, error)

// seedGapDetectorState rebuilds lastScan from persisted cursors so a restart continues the schedule. The cursor stamp is clamped to
// `now` (clock skew), re-emits the last_success gauge for the `_silent` alert, and the coverage snapshot re-emits the gap and distinct
// gauges. A target with no cursor, or a read error (logged), is scanned at once.
func seedGapDetectorState(ctx context.Context, targets []GapDetectorTarget, getCursor gapDetectorCursorReader, snapshots []SourceCoverage, logger *slog.Logger, now time.Time) map[string]time.Time {
	byKey := make(map[string]SourceCoverage, len(snapshots))
	for _, cov := range snapshots {
		byKey[cov.Source+"/"+cov.Table] = cov
	}
	lastScan := make(map[string]time.Time, len(targets))
	for _, t := range targets {
		key := targetKey(t)
		c, err := getCursor(ctx, gapDetectorHighWaterSource, key)
		switch {
		case errors.Is(err, ErrNotFound):
			continue
		case err != nil:
			logger.Warn("gap-detector: read scan cursor for boot seed failed; scanning target on first cycle",
				"source", t.Source, "table", t.Table, "err", err)
			continue
		}
		last := c.UpdatedAt
		if last.After(now) {
			last = now
		}
		lastScan[key] = last
		obs.IngestGapDetectorLastSuccessUnix.WithLabelValues(t.Source, t.Table).Set(float64(last.Unix()))
		if cov, ok := byKey[key]; ok {
			obs.IngestGapMaxSize.WithLabelValues(t.Source, t.Table).Set(float64(cov.MaxGapLedgers))
			obs.IngestGapCount.WithLabelValues(t.Source, t.Table).Set(float64(cov.GapCount))
			obs.IngestSourceDistinctLedgers.WithLabelValues(t.Source, t.Table).Set(float64(cov.DistinctLedgers))
		}
		logger.Info("gap-detector: seeded target schedule from persisted scan cursor",
			"source", t.Source, "table", t.Table, "last_scan", last.UTC().Format(time.RFC3339),
			"cadence", t.EffectiveScanCadence().String(), "next_due", last.Add(t.EffectiveScanCadence()).UTC().Format(time.RFC3339))
	}
	return lastScan
}

// runOneGapDetectorCycle is one full pass over every target, split out of RunGapDetector so it is
// unit-testable. Each target runs in its own bounded sub-context so a slow scan cannot starve the rest.
func runOneGapDetectorCycle(ctx context.Context, store *Store, logger *slog.Logger, targets []GapDetectorTarget) {
	tip, err := resolveGapDetectorTip(ctx, store)
	if err != nil {
		// A tip failure blocks every target (the tip is the upper scan bound), so record one error
		// per target to keep the per-target outcome counter coherent.
		for _, target := range targets {
			obs.IngestGapDetectorRunsTotal.WithLabelValues(target.Source, target.Table, "error").Inc()
		}
		logger.Warn("gap-detector: tip resolve failed; skipping cycle", "err", err)
		return
	}

	// Emit tip as a gauge so diagnostics can compute the density denominator without a DB hit. It is
	// set BEFORE the per-target scans so readers see a tip consistent with the distinct-ledger gauges.
	obs.IngestGapDetectorTip.Set(float64(tip))

	for _, target := range targets {
		scanOneGapDetectorTarget(ctx, store, logger, target, tip)
	}
}

// gapScanFrom computes the window lower bound (see [computeGapScanWindow]), floored at target.Genesis so pre-genesis "gaps" do not
// deflate gap_free_pct; the same `from` feeds the caller's density math.
func gapScanFrom(ctx context.Context, store *Store, logger *slog.Logger, target GapDetectorTarget, tip int64) int64 {
	prevHighWater, firstRun := gapScanHighWater(ctx, store, logger, target)
	from := computeGapScanWindow(target.Genesis, tip, prevHighWater, firstRun)
	if firstRun {
		logger.Info("gap-detector: first scan for target — bounded to the trailing FirstScanCap window; deep history is the ADR-0033 completeness verdict's domain",
			"source", target.Source, "table", target.Table, "from", from, "tip", tip)
	}
	return from
}

// scanOneGapDetectorTarget scans one target and emits metrics under its own timeout; gauges are NOT cleared on error so a firing alert
// persists.
//
//nolint:gocognit // linear pipeline; the metric fan-out reads cleanly inline.
func scanOneGapDetectorTarget(ctx context.Context, store *Store, logger *slog.Logger, target GapDetectorTarget, tip int64) {
	start := time.Now()
	scanCtx, cancel := context.WithTimeout(ctx, gapDetectorPerTargetTimeout)
	defer cancel()

	from := gapScanFrom(scanCtx, store, logger, target, tip)
	seed := gapScanSeed(scanCtx, store, target)
	gaps, err := store.FindPerSourceLedgerGaps(scanCtx, target, from, tip, target.EffectiveMinGapSize(), seed)
	if err != nil {
		// A failure MUST be loud: it is the only signal that a heavy scan is timing out, and the
		// stalled last-success gauge is what advances the silent-detector alert. `elapsed_s` tells
		// a deadline hit (~780s statement_timeout, ~900s Go timeout) from a fast error.
		elapsed := time.Since(start).Seconds()
		obs.IngestGapDetectorRunsTotal.WithLabelValues(target.Source, target.Table, "error").Inc()
		obs.IngestGapDetectorDurationSeconds.WithLabelValues(target.Source, target.Table, "error").
			Observe(elapsed)
		logger.Warn("gap-detector: scan failed (non-ok outcome — last-success gauge NOT advanced)",
			"source", target.Source, "table", target.Table, "outcome", "error",
			"err", err, "tip", tip, "from", from, "elapsed_s", elapsed)
		return
	}

	// The window scanned cleanly, so advance the high-water (UpsertCursor only moves forward).
	// Non-fatal on failure: the next cycle scans a slightly wider, still SafetyLookback-bounded window.
	if tip > 0 {
		if err := store.UpsertCursor(scanCtx, gapDetectorHighWaterSource, targetKey(target), uint32(tip)); err != nil { //nolint:gosec // tip is the ledgerstream cursor's uint32 last_ledger widened to int64 upstream; always in range
			logger.Warn("gap-detector: persist scan high-water failed",
				"source", target.Source, "table", target.Table, "err", err)
		}
	}

	persistGapScanSeed(scanCtx, store, logger, target, from, tip)

	// Count distinct ledgers over the SAME [from, tip] window so density's numerator and denominator
	// match what was scanned. If this fails, still emit the gap gauges and skip the distinct/expected
	// emission so the projection reads as stale rather than wrong.
	distinct, distinctErr := store.CountDistinctLedgers(scanCtx, target, from, tip)
	if distinctErr != nil {
		logger.Warn("gap-detector: count-distinct failed (gap signal unaffected)",
			"source", target.Source, "table", target.Table, "err", distinctErr, "tip", tip)
	}

	var totalMissing, largest int64
	for _, g := range gaps {
		totalMissing += g.Size
		if g.Size > largest {
			largest = g.Size
		}
	}

	obs.IngestGapLedgers.WithLabelValues(target.Source, target.Table).Set(float64(totalMissing))
	obs.IngestGapCount.WithLabelValues(target.Source, target.Table).Set(float64(len(gaps)))
	obs.IngestGapMaxSize.WithLabelValues(target.Source, target.Table).Set(float64(largest))
	if distinctErr == nil {
		obs.IngestSourceDistinctLedgers.WithLabelValues(target.Source, target.Table).Set(float64(distinct))
		// Persist the projection to source_coverage_snapshots so the API process reads density
		// without re-running the heavy LAG-over-DISTINCT query at request time.
		expected := ExpectedLedgersFor(from, tip)
		cov := SourceCoverageFromCounts(
			target.Source, target.Table,
			distinct, expected, largest, int64(len(gaps)),
			time.Now().UTC(),
		)
		if err := store.UpsertSourceCoverage(scanCtx, cov); err != nil {
			logger.Warn("gap-detector: persist source_coverage_snapshot failed",
				"source", target.Source, "table", target.Table, "err", err)
		}
	}
	markGapDetectorScanSuccess(target, start, time.Now())

	switch {
	case totalMissing > 0:
		logger.Warn("gap-detector: data-coverage gaps detected",
			"source", target.Source,
			"table", target.Table,
			"tip", tip,
			"total_missing_ledgers", totalMissing,
			"gap_count", len(gaps),
			"max_gap_size", largest,
		)
	case !gapVerdictTrustworthy(target.DistinctLedgerCountSQL != "", distinctErr, distinct):
		// Zero gaps from a table whose own census also saw nothing is NOT evidence of completeness:
		// a TRUNCATE leaves FindPerSourceLedgerGaps no present row to bracket a gap, for targets whose
		// density comes from a separate log (see sorobanEventsDistinctLedgerCountSQL). Loud, not Debug.
		logger.Warn("gap-detector: zero-gap verdict has no positive census corroboration — not trusted as clean",
			"source", target.Source, "table", target.Table, "tip", tip, "from", from, "distinct", distinct)
	default:
		logger.Debug("gap-detector: clean coverage",
			"source", target.Source, "table", target.Table, "tip", tip)
	}
}

// gapVerdictTrustworthy reports whether a zero-gap verdict has positive evidence when density comes from a
// [GapDetectorTarget.DistinctLedgerCountSQL] override; targets without one share a table for gaps and density.
func gapVerdictTrustworthy(hasCensusOverride bool, distinctErr error, distinct int64) bool {
	if !hasCensusOverride {
		return true
	}
	return distinctErr == nil && distinct > 0
}

// markGapDetectorScanSuccess records a clean scan: counter, histogram and the wall-clock last-success gauge the
// `stellarindex_ingest_gap_detector_silent` alert uses (a counter resetting across a restart is invisible to rate()). `now` is injected
// for tests.
func markGapDetectorScanSuccess(target GapDetectorTarget, start, now time.Time) {
	obs.IngestGapDetectorRunsTotal.WithLabelValues(target.Source, target.Table, "ok").Inc()
	obs.IngestGapDetectorDurationSeconds.WithLabelValues(target.Source, target.Table, "ok").
		Observe(now.Sub(start).Seconds())
	obs.IngestGapDetectorLastSuccessUnix.WithLabelValues(target.Source, target.Table).
		Set(float64(now.Unix()))
}

// resolveGapDetectorTip reads the live ledgerstream cursor's last_ledger as the scan's upper bound (not MAX(ledger), which stale rows
// would skew). Returns 0 with no live cursor; [FindPerSourceLedgerGaps] is safe at to=0 and runs_total still increments.
func resolveGapDetectorTip(ctx context.Context, store *Store) (int64, error) {
	c, err := store.GetCursor(ctx, "ledgerstream", "")
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return 0, nil
		}
		return 0, err
	}
	return int64(c.LastLedger), nil
}
