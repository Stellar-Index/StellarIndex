---
adr: 0031
title: Coverage signal is data-derived from authoritative stores
status: Accepted
date: 2026-05-29
supersedes: []
superseded_by: null
---

# ADR-0031: Coverage signal is data-derived from authoritative stores

## Context

Coverage was answered in four places (cursor inventory, gap detector, live cursor span, decoder counters) that disagreed under real faults: a writer halt left cursor-derived density at 100%, and a drain without a cursor row read 97-99% over whole data. Patching signals to agree with each other only tightened their coupling.

## Decision

There is one coverage signal per data domain, derived from the data itself, not from cursors. Per source, density counts distinct ledgers per gap-detector target (`Store.CountDistinctLedgers`, over `DefaultGapDetectorTargets`), computed in the gap-detector cycle and persisted to `source_coverage_snapshots`; `/v1/diagnostics/ingestion` reads that snapshot, never a live scan. Two numbers are published per source: `density_pct` (distinct over expected ledgers; low and correct for sparse sources) and `gap_free_pct` (`1 - max_gap / expected`). External CEX, FX and oracle pollers are measured by freshness (`last_event_ts`), not ledger density.

`ingestion_cursors` is an operational journal (resume point, backfill progress), never interpreted as coverage. The authoritative completeness verdict is ADR-0033's `completeness_snapshots`, computed against the ClickHouse lake (ADR-0034); this signal is the supporting, alerting one. Topic-level completeness is ADR-0033 claim 2a, answered by `Dispatcher.Recognize`.

## Invariant

No coverage figure served by `/v1/diagnostics/ingestion` is read from `ingestion_cursors`. One struct (`timescale.SourceCoverage`) carries density, gap-free share and gap counts per `{source, table}`, and alerts read the same gauges.

## Consequences

- A cascade cannot hide behind a healthy cursor; cursor-credit workarounds are unnecessary.
- Sparse-by-design sources (Blend auctions, CCTP) show low density with `gap_free_pct = 1.0`, which is the truth.
- Cost is one extra count per target per 30-minute cycle; no per-request scans.

## Evidence

- `internal/storage/timescale/source_coverage.go` (`SourceCoverage`, `CountDistinctLedgers`, `UpsertSourceCoverage`, `ListSourceCoverage`), migration `0048_source_coverage_snapshots`.
- `internal/api/v1/diagnostics_ingestion.go` (`density_pct`, `gap_free_pct`); metric `IngestSourceDistinctLedgers` in `internal/obs/metrics.go`.
