---
adr: 0034
title: Tiered data architecture — ClickHouse raw lake, Postgres served tier
status: Accepted
date: 2026-06-05
supersedes: [0029]
superseded_by: null
---

# ADR-0034: Tiered data architecture — ClickHouse raw lake, Postgres served tier

## Context

An OLAP-scale append-only chain firehose sat in a row-oriented OLTP store: unique-PK indexes dwarfed RAM, every insert was random IO, and bulk reprocessing ran at about 0.24 ledgers/s, which is months.
This is a storage-class mismatch. The firehose belongs in columnar append-only storage, and the small served pricing and entity data belongs in Postgres.

## Decision

Route data by access pattern.

- **Tier 0, archive.** Galexie writes LCM/XDR to MinIO/S3, immutable. It is the source of truth.
- **Tier 1, raw lake.** ClickHouse holds a structural, decoder-independent decode of every ledger, transaction, operation, operation result, contract event and ledger-entry change, with all history. It runs on r1.
- **Tier 3, served tier.** Postgres/TimescaleDB holds the decoded protocol entities, pricing and continuous aggregates, the recent working set. It is not the full archive. Some v1 surfaces are served straight from ClickHouse (ADR-0038, ADR-0048).
- Protocol decoders read the lake, not galexie. The Postgres tier is re-derived from the lake and never from a second galexie walk, so re-deriving is a lake scan.
- The lake supersedes ADR-0029's Postgres `soroban_events` landing zone. The projector reads the lake by default (`clickhouse_projector_source`, with `clickhouse_live_sink`, both on by default, ADR-0041). `soroban_events`, `ledger_ingest_log` and `backfill -source soroban-events` are still present pending decommission (#803).
- Completeness is two-axis (ADR-0033). `lake_complete` is the archive's genesis-to-tip claim. `complete` is also gated by the projection window. "100% coverage" means the lake captured everything, and the served tier is faithful only within what it holds.
- `stellar.ledger_entry_changes` is populated and read by the explorer and the movement reconstruction (ADR-0038, ADR-0047).

## Invariant

- ClickHouse is the raw lake and Postgres the served tier. No retention policy (`drop_after`) is put on `trades`, and "retention-scoped" means scoped to what has been projected, not a drop policy.
- A re-derivation of the served tier reads the lake, never a Galexie walk. `stellarindex-ops backfill` is not a lake re-derive (`docs/architecture/ingest-pipeline.md#the-replay-decision-rule`).

## Consequences

- Bulk re-derivation is cheap and routine, and the lake's append-only model rules out the `event_index` collision class of silent drop.
- ClickHouse is a new stateful service to operate, and decoders need a lake input adapter.
- Tier 2 fuzzy search (OpenSearch or Meilisearch) and a Parquet cold tier were planned but not built. Exact-id search is served from ClickHouse.

## Evidence

`deploy/clickhouse/tier1_schema.sql`, `internal/storage/clickhouse/`, `internal/config/config.go` (`ClickHouseLiveSink`, `ClickHouseProjectorSource`), `docs/architecture/clickhouse-migration-plan.md`.
