---
adr: 0032
title: Per-source tables are projections of soroban_events
status: Accepted
date: 2026-05-29
supersedes: []
superseded_by: null
---

# ADR-0032: Per-source tables are projections of soroban_events

## Context

Soroban events reached per-source tables by two independent paths: live dispatch and per-source backfill subcommands.
When one path failed the tables drifted, and recovery needed incident-grade orchestration instead of a replay.

## Decision

Per-source tables for Soroban-derived data are projections of the raw event record, written by one component, `internal/projector`.

- The raw record is the ClickHouse lake (`contract_events`) by default (`clickhouse_projector_source`, ADR-0034). Postgres `soroban_events` is the legacy fallback source, decommission pending (#803).
- The projector replays raw events through the source's own decoder and its normal `Insert*` writes. It keeps a cursor per source in `ingestion_cursors` (`source='projector'`, sub-source = source name). A projected table is a cache: wipe it and rewind the cursor to rebuild.
- Rollout is staged. With `persist_per_source = true` (the default, and what r1 runs) the dispatcher still writes the not-yet-promoted sources alongside the projector. Only the `sep41` domain is projector-only today (`IsSoleWriterProjected`). Setting `persist_per_source = false` makes the projector sole writer for the rest.
- The per-source `*-backfill` subcommands and `drain-cascade-window` are deleted (Phase 5). Phases: 3 projector alongside the dispatcher, 4 `persist_per_source = false` (projector sole writer). Catch-up is `stellarindex-ops projector-replay -config PATH -source NAME -from N [-write]`, or `projected-rebuild` for bulk (about 1M ledgers and up).
- `-sep41` is a projected domain: replay it with `projector-replay`, not `ch-rebuild -sep41`.
- A source the projector does not write is non-projected and catches up with `ch-rebuild` (`-sdex`, `-contract-calls`). `IsProjectedEvent`'s default branch is that list: sdex, the external CEX/FX connectors, `band`, `soroswap_router` (log-only) and the supply observers.
- The decision table for which command replays what is in `docs/architecture/ingest-pipeline.md#the-replay-decision-rule`.

## Invariant

- A projected source has a case in `internal/projector/registry.go::buildSource` and an arm in `internal/pipeline/sink.go::IsProjectedEvent`. `internal/pipeline/lockstep_ast_test.go` fails CI on a missed edit.
- The sole-writer set is a subset of the projected set, and the `sep41` domain is projector-only whatever `persist_per_source` says. `TestSoleWriter_SubsetOfProjected` and `TestSinkModeForProjector_Sep41SoleWriterInvariant` enforce it.
- `persist_per_source = false` is refused at indexer start unless every continuous aggregate's refresh offset covers the projector's stall bound. `pipeline.VerifySoleWriterCAGGCoverage` enforces it.
- No bespoke `<source>-backfill` command is added. `scripts/ci/lint-replay-plan.sh` requires a `Replay-Plan:` trailer on a decoder change that names the replay.
- `ch-rebuild -write` refuses a range a live projector is still inside unless `-allow-live-overlap` is passed, so a rebuild is never a second writer on a projected domain.
- Projector lag is observable as `stellarindex_projector_lag_ledgers`, with alerts in `deploy/monitoring/rules/projector.yml`.

## Consequences

- Recovery is routine: fix the decoder, then replay from the failure window. Raw data is never lost to a decoder error.
- Projected tables lag the raw record by the projector's cycle time. Price endpoints do not read them on a sub-second path.
- While `persist_per_source = true`, a promoted-later source is double-written. Idempotent inserts absorb the overlap.
- The projector is an always-on component whose wedge shows up as lag, not as an error.

## Evidence

`internal/projector/` (registry, projector loop and tests), `internal/pipeline/lockstep_ast_test.go`, `internal/pipeline/sole_writer_test.go`, `internal/pipeline/sole_writer_cagg.go`, `configs/ansible/roles/archival-node/templates/stellarindex.toml.j2`.
