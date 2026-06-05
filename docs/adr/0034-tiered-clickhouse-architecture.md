---
adr: 0034
title: Tiered data architecture — ClickHouse raw lake, Postgres served tier
status: Accepted
date: 2026-06-05
supersedes: [0029]
superseded_by: null
---

# ADR-0034 — Tiered data architecture (galexie → ClickHouse → decoders → Postgres)

**Status:** Accepted (2026-06-05)
**Supersedes:** ADR-0029 (soroban_events raw landing zone in Postgres)
**Amends:** ADR-0030/0031 (coverage signal), ADR-0032 (projector source),
ADR-0033 Claim 3 (reconciliation oracle)
**Detailed plan + phases:** `docs/architecture/clickhouse-migration-plan.md`

## Context

We store an OLAP-scale append-only blockchain firehose in a row-oriented
OLTP store. On r1 (2026-06-05): `trades` is 324 GB / ~2.89 B rows;
`soroban_events` is 210 GB / ~3.25 B rows. With per-row unique PK indexes
+ `ON CONFLICT` dedup, the index dwarfs RAM and every insert is random IO,
so bulk reprocessing (recovering the `event_index`/`op_index` collision
loss, adding a decoder) runs at ~0.24 ledgers/s on an *idle* box — months,
i.e. infeasible. This is the wall that blocked the ADR-0033 recovery. It
is a storage-*class* mismatch, not a bug: the firehose belongs in
columnar, append-only storage; the small served pricing/entity data
belongs in Postgres, where it works well.

Separately, building toward a full searchable Stellar/Soroban explorer
(every tx/op/event/state, all history) is a big-data problem the current
Postgres approach cannot economically serve.

## Decision

Adopt a four-tier architecture, routing data by access pattern:

- **Tier 0 — Archive:** galexie → MinIO/S3 (LCM/XDR, immutable). Source of
  truth. Unchanged.
- **Tier 1 — Raw lake:** **ClickHouse** (columnar, append-only,
  merge-on-read dedup), holding a *structural, decoder-independent* decode
  of every ledger/tx/op/op-result/contract-event/ledger-entry-change +
  the retained topic/body/arg XDR blobs. All history. Cold/interchange =
  Parquet on the existing MinIO. **ClickHouse runs on r1** (decided
  2026-06-05): 20 cores, 188 GB RAM (cap CH memory ~32–48 GB as a
  resource-limited good neighbour to Postgres), CH data on the ZFS pool
  (18 TB free) + S3 disk for Parquet cold tier. Revisit a dedicated host
  only if the explorer outgrows co-tenancy.
- **Tier 2 — Search:** exact-id via ClickHouse; fuzzy/label via
  OpenSearch/Meilisearch over curated entities.
- **Tier 3 — Served semantic/pricing:** Postgres/TimescaleDB — the decoded
  protocol entities + pricing CAGGs (small, indexed, hot). The existing
  stack and the right tool for this tier.

**Dataflow:** one *structural* galexie walk populates ClickHouse (historic
backfill + live fan-out). **Protocol decoders read ClickHouse, not
galexie** (their logic is unchanged; only the input source moves). The
Postgres tier is **re-derived from ClickHouse**, never a second galexie
walk. Consequence: re-deriving Postgres (a decoder fix, a new protocol,
the collision recovery) becomes a ClickHouse scan (hours), not a MinIO
walk (months) — and ClickHouse's append-only model makes the silent-drop
class of bug (the `event_index` collision) impossible in the lake.

Alternatives (Citus/partition-harder, pure Parquet+DuckDB/Trino, managed
cloud warehouse, decode-on-read from galexie, all-in-ClickHouse) were
stress-tested and rejected — see the plan doc §2.

## Consequences

**Positive:** the scale wall is removed; bulk re-derivation is cheap and
routine; the full explorer is unlocked on a proven columnar engine; the
pricing proposal keeps its well-suited Postgres tier; provability
(ADR-0033 census) carries over and gets cheaper.

**Negative / cost:** a new stateful service to operate (ClickHouse;
mitigated by single-node-on-r1 to start, Parquet-on-MinIO durable copy,
galexie backstop); a decoder input-adapter to read CH; a multi-month,
phased build (additive — the proposal ships throughout).

**Decommissioning (no orphans):** the Postgres tier is *rebuilt* from
ClickHouse, not repaired — all current Postgres protocol/event data is
discarded and re-derived clean (eliminating the collision damage in one
move). `soroban_events` (Postgres), the oversized `trades`,
`ledger_ingest_log`, the `sorobanevents` package, the
`backfill -source soroban-events` path, the census/projector/gap-detector
soroban_events couplings, and the failed transient systemd units are all
removed in the same phase that replaces them. Full inventory in the plan
doc §10; clean-cutover guarantee (one authoritative path at all times) in
§10e.

This supersedes ADR-0029 (the Postgres landing zone) and amends the
coverage/projection ADRs to read from ClickHouse + reconcile against the
LCM census.
