---
adr: 0030
title: Per-source coverage invariant
status: Accepted
date: 2026-05-28
supersedes: []
superseded_by: null
---

# ADR-0030: Per-source coverage invariant

## Context

A 14-hour writer halt (103,396 ledgers) went unseen because every coverage signal measured process state, not data state. A gap detector fixes that only while every per-source table is registered with it, and a new table can silently skip registration.

## Decision

Every per-source hypertable is registered as a `GapDetectorTarget` in `internal/storage/timescale/per_source_gaps.go` in the same PR that creates it. A table may be exempt only through `excludedFromGapDetector`, with a prose reason; "leftover from refactor" is not one. Coverage metrics are labelled `{source, table}` and alerts aggregate with `max by (source)`. Identifiers fed to `FindPerSourceLedgerGaps` (`Table`, `LedgerColumn`, `WhereFilter`) come only from `DefaultGapDetectorTargets`, never from user input, because Postgres cannot bind identifiers.

## Invariant

`TestGapDetectorTargetsCoverAllPerSourceHypertables` fails CI when a migration creates a table whose name matches the per-source pattern and the table is neither a target nor excluded. Headline density is data-derived, not cursor-derived (see ADR-0031).

## Consequences

- A cascade in any per-source table pages through the same alert; nothing new to add per source.
- Adding a per-source table means touching `per_source_gaps.go`. That friction is intended.
- Scan time grows linearly with targets on a 30-minute cycle (`GapDetectorInterval`).
- `WhereFilter` is an injection surface if it is ever fed user data; no linter enforces that, so keep its use to the existing constant filters.

## Evidence

- `internal/storage/timescale/per_source_gaps.go`, `gap_detector.go`, `gap_targets_test.go`.
- Alert `stellarindex_ingest_gap_detected`: `deploy/monitoring/rules/ingestion.yml`.
- Runbooks: `docs/operations/runbooks/ingest-gap.md#stellarindex_ingest_gap_detected`, `ingest-gap.md`, `projector.md#stellarindex_projector_replay_stalled`.
