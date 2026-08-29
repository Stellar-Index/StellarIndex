package timescale

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/obs"
)

// GapDetectorInterval is the cadence at which [RunGapDetector]
// re-scans every registered per-source hypertable for contiguous
// data-coverage gaps.
//
// Why 30 minutes:
//   - The expensive part is the LAG()-over-DISTINCT scan. Live r1
//     measurement (2026-05-28) clocked 4m51s against ~50M distinct
//     ledgers in soroban_events alone; the per-source tables add
//     a smaller per-target cost (<30s each on r1) but the sum
//     across 13 targets is ~7-10 min worst-case.
//   - The metric feeds a paging alert on a >threshold gap held
//     for 15 min; 30 min cadence keeps the alert latency in the
//     ~45-60 min envelope, which is appropriate for an "ingest
//     halt" page (not a sub-minute fast-failure signal).
//   - Future optimisation may incrementally refresh a
//     soroban_event_ledgers materialised view to bring the
//     dominant scan cost back under a second.
const GapDetectorInterval = 30 * time.Minute

// GapDetectorMinGapSize is the threshold below which a contiguous
// gap is treated as expected no-activity noise rather than an
// ingest gap. Matches `stellarindex-ops find-data-gaps`'s default
// of 1000 ledgers (~1.5 h of network time) — see the godoc on
// that subcommand for the rationale.
const GapDetectorMinGapSize = int64(1000)

// gapDetectorPerTargetTimeout caps one per-target scan. Sized for
// soroban_events's 5-min measurement with 3x headroom; the per-
// source tables complete in <30s typically so this is the upper
// bound, not the median. Per-target timeout means one slow table
// doesn't poison the rest of the cycle — each target runs in
// isolation.
const gapDetectorPerTargetTimeout = 15 * time.Minute

// gapDetectorStatementTimeoutMS is the PG-side `SET LOCAL
// statement_timeout` applied to BOTH per-target scan queries — the
// LAG-over-DISTINCT gap scan in [Store.FindPerSourceLedgerGaps] and the
// density count in [Store.CountDistinctLedgers]. It is the backstop
// for the case where the Go-side cancel does not reach PG (the
// database/sql cancel is best-effort; r1 2026-05-29 accumulated three
// concurrent SDEX scans that way).
//
// INVARIANT: MUST be <= gapDetectorPerTargetTimeout, and the two
// queries share ONE constant. Until 2026-08-28 CountDistinctLedgers
// reused opsVerifyStatementTimeoutMS (2h) while the Go context was
// 15 min: every soroban_events count that overran 15 min was
// abandoned by Go but kept running in PG for up to 2h, and the next
// 6h cycle (or every restart) stacked another — pg_stat_statements
// showed 121 calls, mean 556 s, 18.7 h total, load 19.6 and 503s from
// serving-path statement timeouts (r1 incident 2026-08-28 18:23Z).
// 13 min leaves 2 min of the 15-min Go budget for the SET + connect.
// Pinned by TestGapDetectorStatementTimeoutWithinGoBudget.
const gapDetectorStatementTimeoutMS = 780_000 // 13 min, in ms

// GapDetectorSafetyLookback bounds how far below tip a single steady-
// state detector cycle re-scans. In steady state the scanned window is
// just [previous-scan high-water, tip] — a few hundred ledgers per
// 30-min cycle; this constant only binds after the detector has been
// down for a target long enough that its high-water fell more than
// SafetyLookback below tip, at which point it caps the catch-up so one
// cycle can never walk back further than ~200k ledgers (~11.5 days of
// network time).
//
// DIVISION OF LABOR (ADR-0033): the gap detector's snapshots are a
// SUPPORTING signal for the RECENT ingest frontier. The authoritative
// deep-history verdict is completeness_snapshots (substrate continuity
// + hash chain + projection reconcile, from genesis, no sparsity
// threshold), which now covers all 17 sources. Re-walking [genesis,
// tip] every 30 min bought nothing and — once sep41_transfers reached
// ~13M distinct ledgers (~700M rows) — cost two ~13-min LAG-over-
// DISTINCT scans per cycle, near-continuous IO saturation that blew
// p95/p99 (live incident 2026-07-06). Trailing-window scanning keeps
// the detector cheap; deep history is the verdict system's job.
const GapDetectorSafetyLookback = int64(200_000)

// GapDetectorFirstScanCap bounds the FIRST-ever scan of a target (no
// persisted scan high-water yet). Generous — 2M ledgers (~115 days) so
// the initial coverage snapshot has a meaningful recent-history window
// — but finite, so a brand-new target (or a fresh deploy that lost its
// high-water cursor) never triggers the full-history walk this change
// exists to eliminate. Deep history below the cap is the ADR-0033
// completeness verdict's domain, not the detector's.
const GapDetectorFirstScanCap = int64(2_000_000)

// gapDetectorHighWaterSource is the ingestion_cursors `source` under
// which the detector persists each target's per-cycle scan high-water
// (the tip it last scanned to; Sub is the target's key). Reading it
// lets the next cycle scan only [high-water, tip] instead of re-walking
// [genesis, tip]. Deliberately kept out of the "backfill" /
// "ledgerstream" cursor namespaces so the diagnostics cursor
// aggregation (aggregateBackfill / ledgerStreamLagSeconds) ignores it.
const gapDetectorHighWaterSource = "gap-detector-scan"

// computeGapScanWindow returns the lower bound `from` of the trailing
// window a detector cycle scans for a target, given the live `tip`,
// the source `genesis`, the previously persisted scan `prevHighWater`
// (0 when unknown), and whether this is the target's `firstRun`:
//
//   - firstRun            → from = max(genesis, tip - FirstScanCap)
//   - steady / post-error → from = max(genesis, prevHighWater, tip - SafetyLookback)
//
// The result is clamped to be non-negative; callers pass it straight
// into FindPerSourceLedgerGaps, which still rejects from > tip (e.g. a
// source whose genesis sits above the current tip) exactly as before.
// Both the gap scan AND the distinct/expected coverage math use this
// same `from`, so density (= distinct / (tip - from + 1)) stays
// coherent — numerator and denominator both scoped to the window.
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

// gapScanHighWater reads the per-target scan high-water persisted by
// the previous cycle. Returns (highWater, firstRun): firstRun is true
// ONLY when no high-water has ever been written for this target (the
// generous FirstScanCap applies). A transient read error is treated as
// "not first run, no usable high-water" (highWater 0) so the window
// falls back to the bounded SafetyLookback trailing window rather than
// the wider FirstScanCap.
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

// RunGapDetector blocks until ctx is cancelled, periodically
// scanning every target in [DefaultGapDetectorTargets] for
// contiguous ledger-coverage gaps and emitting per-(source, table)
// gauges + meta-metrics.
//
// Data-derived complement to the cursor-derived density projection
// in /v1/diagnostics/ingestion. Cursor coverage measures process
// state ("did we walk this ledger") and can read 100% while data
// is missing — the F-0020 audit found exactly that, with the
// soroban_events writer halted across a 92,737-ledger contiguous
// window while the cursor inventory + density projection said
// fine. This worker scans every per-source data table directly
// and surfaces the honest signal as Prometheus gauges that
// operators (and an alert rule) can act on.
//
// Failure semantics: a transient Postgres error on one target's
// scan does NOT clear its gauges and does NOT halt the remaining
// targets in the cycle — the last-known value stays put and the
// loop continues. Operators rely on the paired
// `stellarindex_ingest_gap_detector_runs_total{outcome=error}`
// counter to detect a sustained per-target detector outage.
//
// The first cycle runs immediately on goroutine start so the gauges
// are populated before the first interval tick — a process that's
// just come up has a non-empty signal within seconds rather than
// ~37 min (= interval + first scan duration). Targets whose cadence
// has NOT elapsed since their last persisted scan are skipped by that
// first cycle (see [seedGapDetectorState]); their gauges are re-emitted
// from persisted state instead.
func RunGapDetector(ctx context.Context, store *Store, logger *slog.Logger) error {
	if store == nil {
		return nil
	}
	if logger == nil {
		logger = slog.Default()
	}

	// Per-target last-scan timestamps drive the per-target cadence
	// gate. Without per-target tracking, every target either scans
	// every cycle (pre-rc.100 behaviour — that's why SDEX +
	// soroban-events kept stacking concurrent queries on postgres)
	// or all targets stretch to the longest cadence. Per-target
	// tracking lets us run light targets every 30 min while
	// throttling huge-table targets (SDEX, soroban-events) to 6h.
	//
	// Seeded from the persisted scan high-water cursors so a RESTART
	// honours the cadence: until 2026-08-28 this map started empty,
	// so every deploy / crash-loop iteration re-ran the 6h-cadence
	// soroban_events + sdex scans immediately — each a >10-min IO
	// storm on r1 — regardless of when they last ran.
	snapshots, err := store.ListSourceCoverage(ctx)
	if err != nil {
		logger.Warn("gap-detector: read source_coverage_snapshots for boot seed failed; gauges stay empty until first scan", "err", err)
		snapshots = nil
	}
	lastScan := seedGapDetectorState(ctx, DefaultGapDetectorTargets, store.GetCursor, snapshots, logger, time.Now())

	runOneGapDetectorCycleScheduled(ctx, store, logger, DefaultGapDetectorTargets, lastScan)

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
			runOneGapDetectorCycleScheduled(ctx, store, logger, DefaultGapDetectorTargets, lastScan)
		}
	}
}

// targetKey is the dedupe identity for per-target last-scan
// tracking. (source, table) matches the metric labels so the
// bookkeeping aligns with the wire shape.
func targetKey(t GapDetectorTarget) string {
	return t.Source + "/" + t.Table
}

// runOneGapDetectorCycleScheduled wraps runOneGapDetectorCycle with
// per-target cadence enforcement — only scans targets whose
// EffectiveScanCadence has elapsed since the lastScan timestamp.
// Skipped targets retain their previous metric values
// (last-known-good); operators see the older signal until the next
// allowed cycle, but the postgres-load incident class doesn't
// recur.
func runOneGapDetectorCycleScheduled(ctx context.Context, store *Store, logger *slog.Logger, targets []GapDetectorTarget, lastScan map[string]time.Time) {
	due := dueGapDetectorTargets(targets, lastScan, time.Now())
	if len(due) == 0 {
		logger.Debug("gap-detector: no targets due this cycle")
		return
	}
	runOneGapDetectorCycle(ctx, store, logger, due)
}

// dueGapDetectorTargets returns the targets whose EffectiveScanCadence
// has elapsed since lastScan[key] (or that have no entry at all), and
// stamps each returned target's lastScan to `now`. The stamp is taken
// at dispatch, not on success, so a failing heavy scan is retried on
// its cadence rather than every 30-min tick.
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

// gapDetectorCursorReader is the one Store method the boot-time seed
// needs, as a func so [seedGapDetectorState] is unit-testable without
// a database. Satisfied by (*Store).GetCursor.
type gapDetectorCursorReader func(ctx context.Context, source, sub string) (Cursor, error)

// seedGapDetectorState builds the initial per-target lastScan map from
// persisted state so a process restart continues the schedule the
// previous process left instead of restarting it:
//
//   - The "gap-detector-scan" high-water cursor a target wrote after its
//     last SUCCESSFUL gap scan carries last_updated; that becomes the
//     target's lastScan, so the first cycle skips it while
//     now - last_updated < EffectiveScanCadence. (Clamped to `now` so a
//     future-dated row from clock skew cannot suppress a scan past one
//     cadence.)
//   - The same stamp re-emits stellarindex_ingest_gap_detector_last_
//     success_unix, so the `_silent` alert's staleness clock keeps
//     running across the restart. Without this a skipped target has NO
//     series until its next scan succeeds — a persistently failing scan
//     after a restart would never fire the alert.
//   - The target's source_coverage_snapshots row (written in the same
//     scan) re-emits the last-known gap max/count + distinct gauges, so
//     `stellarindex_ingest_gap_detected` is not blind for up to a full
//     cadence after every deploy. Same last-known-good contract as the
//     error path (gauges are never cleared on failure).
//
// A target with no cursor (never scanned, or ErrNotFound) gets no entry
// → first cycle scans it, as before. A cursor read error is logged and
// treated the same way (scan now), never as "skip".
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

// runOneGapDetectorCycle is one full pass over every target.
// Separated from RunGapDetector so the cycle is unit-testable
// (the integration test wires a real Store via testcontainers +
// asserts gauges directly).
//
// Each target runs in its own bounded sub-context so one slow
// scan can't starve the rest of the cycle.
func runOneGapDetectorCycle(ctx context.Context, store *Store, logger *slog.Logger, targets []GapDetectorTarget) {
	tip, err := resolveGapDetectorTip(ctx, store)
	if err != nil {
		// Tip resolution failure is global — every target is blocked
		// because they all need the tip as the upper scan bound.
		// Record one error per target so the per-target outcome
		// counter stays coherent.
		for _, target := range targets {
			obs.IngestGapDetectorRunsTotal.WithLabelValues(target.Source, target.Table, "error").Inc()
		}
		logger.Warn("gap-detector: tip resolve failed; skipping cycle", "err", err)
		return
	}

	// ADR-0031: emit tip as a gauge so the consumer (diagnostics
	// handler) can compute density denominator without a DB hit.
	// Set once per cycle BEFORE the per-target scans so the
	// consumer always reads a tip consistent with the
	// distinct-ledger gauges that follow.
	obs.IngestGapDetectorTip.Set(float64(tip))

	for _, target := range targets {
		scanOneGapDetectorTarget(ctx, store, logger, target, tip)
	}
}

// scanOneGapDetectorTarget runs one target's scan + metric
// emission under its own timeout. Separated from
// runOneGapDetectorCycle so the cycle loop reads as "for each
// target, scan it" and the failure-mode boilerplate (timeout,
// error counter, gauge non-clear) lives in one place.
//
// Gauges are NOT cleared on error — last-known value persists so
// an alert that was firing stays firing through a transient blip.
//
//nolint:gocognit // linear pipeline; the metric fan-out reads cleanly inline.
func scanOneGapDetectorTarget(ctx context.Context, store *Store, logger *slog.Logger, target GapDetectorTarget, tip int64) {
	start := time.Now()
	scanCtx, cancel := context.WithTimeout(ctx, gapDetectorPerTargetTimeout)
	defer cancel()

	// INCREMENTAL TRAILING WINDOW (2026-07-06 IO-saturation incident):
	// scan only [from, tip], not [genesis, tip]. Re-walking the whole
	// [genesis, tip] LAG-over-DISTINCT every 30 min cost two ~13-min
	// scans per cycle once sep41_transfers reached ~13M distinct
	// ledgers (~700M rows) — near-continuous IO saturation that blew
	// p95/p99. Deep history is the ADR-0033 completeness verdict's
	// domain (all 17 sources); the detector only needs the frontier.
	//
	// `from` is still floored at target.Genesis: below it live
	// pre-genesis "gaps" (ranges where the protocol didn't exist) that
	// used to deflate gap_free_pct — aquarius 2026-06-01 dropped to
	// 94.5% from a 551,779-ledger pre-genesis gap. The SAME `from`
	// feeds the distinct/expected coverage math below so density stays
	// coherent (numerator + denominator both scoped to the window).
	prevHighWater, firstRun := gapScanHighWater(scanCtx, store, logger, target)
	from := computeGapScanWindow(target.Genesis, tip, prevHighWater, firstRun)
	if firstRun {
		logger.Info("gap-detector: first scan for target — bounded to the trailing FirstScanCap window; deep history is the ADR-0033 completeness verdict's domain",
			"source", target.Source, "table", target.Table, "from", from, "tip", tip)
	}

	gaps, err := store.FindPerSourceLedgerGaps(scanCtx, target, from, tip, target.EffectiveMinGapSize())
	if err != nil {
		// A non-ok outcome MUST be loud: this is the only signal that a
		// heavy scan is timing out (Go ctx deadline / SQL statement_timeout)
		// rather than succeeding, and it is what advances the silent-detector
		// alert toward firing (the last-success gauge stops updating). The
		// 2026-07-06 incident showed a healthy detector reading as "silent";
		// the inverse — a genuinely failing scan — must never be quiet.
		// `elapsed_s` makes a timeout obvious: a deadline hit reads ~780s
		// (statement_timeout) / ~900s (Go timeout), a fast error reads <1s.
		elapsed := time.Since(start).Seconds()
		obs.IngestGapDetectorRunsTotal.WithLabelValues(target.Source, target.Table, "error").Inc()
		obs.IngestGapDetectorDurationSeconds.WithLabelValues(target.Source, target.Table, "error").
			Observe(elapsed)
		logger.Warn("gap-detector: scan failed (non-ok outcome — last-success gauge NOT advanced)",
			"source", target.Source, "table", target.Table, "outcome", "error",
			"err", err, "tip", tip, "from", from, "elapsed_s", elapsed)
		return
	}

	// Window scanned cleanly — advance the per-target scan high-water so
	// the next cycle starts here instead of re-walking from genesis.
	// UpsertCursor's monotonic-forward guard means the high-water only
	// ever grows. Non-fatal on failure: the next cycle re-reads the
	// stale (or absent) high-water and scans a slightly wider, still
	// SafetyLookback-bounded window.
	if tip > 0 {
		if err := store.UpsertCursor(scanCtx, gapDetectorHighWaterSource, targetKey(target), uint32(tip)); err != nil { //nolint:gosec // tip is the ledgerstream cursor's uint32 last_ledger widened to int64 upstream; always in range
			logger.Warn("gap-detector: persist scan high-water failed",
				"source", target.Source, "table", target.Table, "err", err)
		}
	}

	// ADR-0031: alongside the gap scan, count distinct ledgers over the
	// SAME trailing [from, tip] window so the data-derived density
	// signal has its numerator + denominator both aligned to what was
	// scanned. One extra SELECT per target per cycle — cheap relative
	// to the LAG scan. If this query fails we don't poison the gap
	// signal: emit the gap gauges anyway and skip the distinct/expected
	// emission so the data-derived projection just reads as "stale"
	// until the next cycle.
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
		// ADR-0031 Phase 1: also persist the projection to
		// source_coverage_snapshots so the API binary (separate
		// process) can read fresh density numbers without re-running
		// the heavy LAG-over-DISTINCT query at HTTP request time.
		// One UPSERT per target per cycle.
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

	if totalMissing > 0 {
		logger.Warn("gap-detector: data-coverage gaps detected",
			"source", target.Source,
			"table", target.Table,
			"tip", tip,
			"total_missing_ledgers", totalMissing,
			"gap_count", len(gaps),
			"max_gap_size", largest,
		)
	} else {
		logger.Debug("gap-detector: clean coverage",
			"source", target.Source, "table", target.Table, "tip", tip)
	}
}

// markGapDetectorScanSuccess records a clean per-target scan: the ok
// counter, the duration histogram, AND — critically — the wall-clock
// last-success gauge the `stellarindex_ingest_gap_detector_silent`
// alert keys off.
//
// `now` is threaded in (rather than read inside) so tests can assert
// the exact stamp. The gauge is the reset-proof liveness primitive: a
// rarely-incrementing counter reset to the same value (1) across a
// restart is invisible to rate(), which is why the silent alert
// false-fired for >7h on the 6h-cadence heavy targets (sdex/trades,
// soroban-events) on 2026-07-06. A wall-clock stamp re-set on every
// successful scan is not.
func markGapDetectorScanSuccess(target GapDetectorTarget, start, now time.Time) {
	obs.IngestGapDetectorRunsTotal.WithLabelValues(target.Source, target.Table, "ok").Inc()
	obs.IngestGapDetectorDurationSeconds.WithLabelValues(target.Source, target.Table, "ok").
		Observe(now.Sub(start).Seconds())
	obs.IngestGapDetectorLastSuccessUnix.WithLabelValues(target.Source, target.Table).
		Set(float64(now.Unix()))
}

// resolveGapDetectorTip reads the live ledgerstream cursor's
// last_ledger as the scan's upper bound. Used in lieu of
// "scan to MAX(ledger) in each table" because that would silently
// scan ABOVE tip if any table has stale rows from a previous test
// fixture; using the cursor is the authoritative "what's the live
// tip right now" answer.
//
// Returns 0 if no live cursor row exists (test fixture / region
// without live ingest); the callers' [FindPerSourceLedgerGaps] is
// safe at to=0 (returns nil with no error). The detector still
// emits per-target runs_total increments via the cycle loop so
// operators can tell the worker is alive and just has nothing to
// scan.
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
