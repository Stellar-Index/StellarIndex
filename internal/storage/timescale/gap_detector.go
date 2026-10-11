package timescale

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/obs"
)

// GapDetectorInterval is how often [RunGapDetector] re-scans every per-source hypertable for gaps.
// The LAG()-over-DISTINCT scan is expensive (soroban_events alone takes minutes), and the metric
// feeds a paging alert held for 15 min, so 30 min keeps alert latency in the 45-60 min envelope
// that suits an ingest-halt page.
const GapDetectorInterval = 30 * time.Minute

// GapDetectorMinGapSize is the threshold below which a gap is treated as no-activity noise rather
// than an ingest gap. It matches `stellarindex-ops find-data-gaps` (1000 ledgers, ~1.5 h).
const GapDetectorMinGapSize = int64(1000)

// gapDetectorPerTargetTimeout caps one per-target scan at 3x soroban_events's measured 5 min; the
// other tables finish far sooner. Per-target isolation keeps one slow table from poisoning the cycle.
const gapDetectorPerTargetTimeout = 15 * time.Minute

// gapDetectorStatementTimeoutMS is the PG-side `SET LOCAL statement_timeout` for BOTH per-target
// scans ([Store.FindPerSourceLedgerGaps] and [Store.CountDistinctLedgers]), the backstop for when
// the Go-side cancel does not reach PG.
//
// INVARIANT: MUST be <= gapDetectorPerTargetTimeout, one constant for both queries. A longer PG
// bound would let a query Go abandoned keep running for hours while each later cycle stacks
// another. Pinned by TestGapDetectorStatementTimeoutWithinGoBudget.
const gapDetectorStatementTimeoutMS = 780_000 // 13 min, in ms

// GapDetectorSafetyLookback bounds how far below tip one cycle re-scans (~200k ledgers, ~11.5
// days). Steady state scans only [previous high-water, tip]; this binds only after the detector
// was down long enough that the high-water fell further behind, capping the catch-up.
//
// The detector is a SUPPORTING signal for the recent ingest frontier (ADR-0033). The authoritative
// deep-history verdict is completeness_snapshots, from genesis. Re-walking [genesis, tip] every
// 30 min buys nothing and at sep41_transfers' size costs two ~13-min scans per cycle.
const GapDetectorSafetyLookback = int64(200_000)

// GapDetectorFirstScanCap bounds the FIRST-ever scan of a target (no high-water yet): 2M ledgers
// (~115 days) is a meaningful window yet finite, so a new target or a lost cursor never triggers
// the full-history walk. Deeper history is the ADR-0033 completeness verdict's domain.
const GapDetectorFirstScanCap = int64(2_000_000)

// gapDetectorHighWaterSource is the ingestion_cursors `source` holding each target's scan
// high-water (the tip last scanned; Sub is the target key), so the next cycle scans only
// [high-water, tip]. It is kept out of the "backfill" and "ledgerstream" namespaces so the
// diagnostics cursor aggregation (aggregateBackfill / ledgerStreamLagSeconds) ignores it.
const gapDetectorHighWaterSource = "gap-detector-scan"

// gapDetectorLastPresentSource is the ingestion_cursors `source` holding, per target, the highest
// ledger observed PRESENT in the last scanned window (unlike the high-water, which advances even
// when no row existed). It seeds [Store.FindPerSourceLedgerGaps] so a gap spanning a window
// boundary still pairs with the row after it once the writer resumes, instead of being dropped
// because the preceding row sat in an earlier window.
const gapDetectorLastPresentSource = "gap-detector-last-present"

// computeGapScanWindow returns the lower bound `from` of the trailing window a cycle scans:
//
//   - firstRun            -> from = max(genesis, tip - FirstScanCap)
//   - steady / post-error -> from = max(genesis, prevHighWater, tip - SafetyLookback)
//
// The result is clamped non-negative; FindPerSourceLedgerGaps still rejects from > tip. The gap
// scan and the coverage math share this `from`, so density (distinct / (tip - from + 1)) has
// numerator and denominator scoped to the same window.
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

// gapScanHighWater reads the high-water the previous cycle persisted. firstRun is true ONLY when
// none was ever written. A transient read error returns (0, false) so the window falls back to the
// bounded SafetyLookback rather than the wider FirstScanCap.
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

// gapScanSeed reads the last-present-ledger cursor (see [gapDetectorLastPresentSource]). It returns
// 0 ("no seed") when none exists or the read fails, so the scan falls back to interior-only gaps.
// DistinctLedgerCountSQL-override targets (soroban-events) never get a seed: the MAX(ledger) probe
// costs the same full scan the override exists to avoid (see [maxLedgerInWindowQuery]).
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

// persistGapScanSeed records this window's highest present ledger as the next cycle's seed (see
// [gapScanSeed]). UpsertCursor only moves forward, so a quiet window is a safe no-op. Failures are
// non-fatal: the next cycle falls back to an un-seeded scan.
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

// RunGapDetector blocks until ctx is cancelled, periodically scanning every applicable target in
// [DefaultGapDetectorTargets] (see [ApplicableGapDetectorTargets]) for ledger-coverage gaps and
// emitting per-(source, table) gauges.
//
// It complements the cursor-derived density in /v1/diagnostics/ingestion: cursor coverage measures
// process state and can read 100% while data is missing, so this scans the data tables directly.
//
// A transient Postgres error on one target neither clears its gauges nor halts the others; the
// paired `stellarindex_ingest_gap_detector_runs_total{outcome=error}` counter exposes a sustained
// outage. The first cycle runs immediately; targets whose cadence has not elapsed since their last
// persisted scan are skipped and re-emitted from persisted state (see [seedGapDetectorState]).
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

// preregisterGapDetectorSeries materialises runs_total at 0 for every (source, table, outcome) so
// a live process ALWAYS exposes the series (the label set is owned by DefaultGapDetectorTargets,
// not obs). A CounterVec creates a series on first Inc(), and since the schedule is seeded from
// persisted cursors (see [seedGapDetectorState]) a restart can run NO scan for hours. Without
// this, the `stellarindex_ingest_gap_detector_silent` alert's `absent_over_time(runs_total[15m])`
// clause would read "no scan due yet" as "detector dead"; the staleness clause on
// last_success_unix still covers a wedged target.
func preregisterGapDetectorSeries(targets []GapDetectorTarget) {
	for _, t := range targets {
		for _, outcome := range gapDetectorRunOutcomes {
			obs.IngestGapDetectorRunsTotal.WithLabelValues(t.Source, t.Table, outcome).Add(0)
		}
	}
}

// targetKey is the dedupe identity for per-target last-scan
// tracking. (source, table) matches the metric labels so the
// bookkeeping aligns with the wire shape.
func targetKey(t GapDetectorTarget) string {
	return t.Source + "/" + t.Table
}

// runOneGapDetectorCycleScheduled wraps runOneGapDetectorCycle with per-target
// cadence enforcement — only scans targets whose EffectiveScanCadence has
// elapsed since the lastScan timestamp.
func runOneGapDetectorCycleScheduled(ctx context.Context, store *Store, logger *slog.Logger, targets []GapDetectorTarget, lastScan map[string]time.Time) {
	due := dueGapDetectorTargets(targets, lastScan, time.Now())
	if len(due) == 0 {
		logger.Debug("gap-detector: no targets due this cycle")
		return
	}
	runOneGapDetectorCycle(ctx, store, logger, due)
}

// dueGapDetectorTargets returns targets whose EffectiveScanCadence has elapsed (or that have no
// entry) and stamps their lastScan to `now`. The stamp is taken at dispatch, not on success, so a
// failing heavy scan retries on its cadence rather than every 30-min tick.
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

// seedGapDetectorState builds the initial lastScan map from persisted state so a restart continues
// the previous schedule:
//
//   - The "gap-detector-scan" cursor's last_updated becomes the target's lastScan, so the first
//     cycle skips it until a cadence has elapsed. It is clamped to `now` so a future-dated row from
//     clock skew cannot suppress a scan past one cadence.
//   - The same stamp re-emits the last_success_unix gauge, so the `_silent` alert's staleness
//     clock survives the restart.
//   - The target's source_coverage_snapshots row re-emits the last-known gap and distinct gauges,
//     so `stellarindex_ingest_gap_detected` is not blind for a cadence after each deploy.
//
// A target with no cursor or a cursor read error (logged) gets no entry, so the first cycle scans it.
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

// gapScanFrom computes this cycle's trailing-window lower bound (see [computeGapScanWindow]) and
// logs a first-ever scan. It scans only [from, tip] because re-walking [genesis, tip] is two
// ~13-min scans per cycle at sep41_transfers' size; the detector only needs the frontier.
//
// `from` is floored at target.Genesis: below it lie pre-genesis "gaps" (the protocol did not
// exist) that would deflate gap_free_pct. The same `from` feeds the caller's distinct/expected
// math so density stays coherent.
func gapScanFrom(ctx context.Context, store *Store, logger *slog.Logger, target GapDetectorTarget, tip int64) int64 {
	prevHighWater, firstRun := gapScanHighWater(ctx, store, logger, target)
	from := computeGapScanWindow(target.Genesis, tip, prevHighWater, firstRun)
	if firstRun {
		logger.Info("gap-detector: first scan for target — bounded to the trailing FirstScanCap window; deep history is the ADR-0033 completeness verdict's domain",
			"source", target.Source, "table", target.Table, "from", from, "tip", tip)
	}
	return from
}

// scanOneGapDetectorTarget runs one target's scan and metric emission under its own timeout.
// Gauges are NOT cleared on error, so an alert that was firing stays firing through a transient blip.
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

// gapVerdictTrustworthy reports whether a zero-gap verdict is backed by positive evidence when
// density comes from a [GapDetectorTarget.DistinctLedgerCountSQL] override. Targets without one
// are always trustworthy: gaps and density come from the same table.
func gapVerdictTrustworthy(hasCensusOverride bool, distinctErr error, distinct int64) bool {
	if !hasCensusOverride {
		return true
	}
	return distinctErr == nil && distinct > 0
}

// markGapDetectorScanSuccess records a clean scan: the ok counter, the duration histogram, and the
// wall-clock last-success gauge the `stellarindex_ingest_gap_detector_silent` alert keys off.
// The gauge is the reset-proof liveness primitive: a rarely-incrementing counter that resets to the
// same value across a restart is invisible to rate(). `now` is passed in so tests can assert the stamp.
func markGapDetectorScanSuccess(target GapDetectorTarget, start, now time.Time) {
	obs.IngestGapDetectorRunsTotal.WithLabelValues(target.Source, target.Table, "ok").Inc()
	obs.IngestGapDetectorDurationSeconds.WithLabelValues(target.Source, target.Table, "ok").
		Observe(now.Sub(start).Seconds())
	obs.IngestGapDetectorLastSuccessUnix.WithLabelValues(target.Source, target.Table).
		Set(float64(now.Unix()))
}

// resolveGapDetectorTip reads the live ledgerstream cursor's last_ledger as the scan's upper bound.
// The cursor, not MAX(ledger) per table, is the authoritative tip: a table with stale rows would
// otherwise be scanned ABOVE tip. It returns 0 when no live cursor exists; [FindPerSourceLedgerGaps]
// is safe at to=0, and runs_total still increments so operators see the worker is alive.
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
