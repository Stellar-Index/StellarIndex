---
adr: 0006
title: TimescaleDB for price time-series storage
status: Accepted
date: 2026-04-22
supersedes: []
superseded_by: null
---

# ADR-0006: TimescaleDB for price time-series storage

## Context

Raw trades, oracle feeds and aggregates at seven grains (1m to 1mo) need ordered high-throughput inserts,
window queries within 200 ms p95 on cached data, arbitrary-precision amounts (ADR-0003), declarative
materialisation, and mature HA, backup and PITR tooling.

## Decision

TimescaleDB on PostgreSQL 15 stores raw trades, oracle updates and derived price aggregates.

- `trades` is a hypertable partitioned on `ts` in 1-day chunks, columnar-compressed after 7 days, keyed on (`source`, `ledger`, `tx_hash`, `op_index`, `ts`) with a (`base_asset`, `quote_asset`, `ts`) pair index;
  `oracle_updates` has the same shape plus a `source` column.
- Seven continuous aggregates (`prices_1m`, `prices_15m`, `prices_1h`, `prices_4h`, `prices_1d`,
  `prices_1w`, `prices_1mo`) are kept fresh by `add_continuous_aggregate_policy`.
- Raw trades and price aggregates are retained indefinitely (migration 0031 removed the original
  90-day and 30-day policies). The one exception is `prices_1m`, which carries a 90-day policy shipped
  switched off (migrations 0156, 0166, 0187).
- Amounts are `NUMERIC`, never `bigint`, matching the `canonical.Amount` string wire form.
- HA is Patroni with one synchronous replica in the sibling region and one async in the distant
  region; backup is pgBackRest to MinIO with a 5-minute RPO.

Later ADR-0034 narrows Postgres to the served tier of the ClickHouse raw lake.

## Invariant

No retention policy (`drop_after`) exists on `trades`, and `prices_1m` is the only price aggregate
with one (AGENTS.md invariant 8; `internal/storage/timescale/retention_policy_test.go`). Retention
changes are declarative SQL in `migrations/`.

Amount columns are `NUMERIC` (ADR-0003; `scripts/ci/lint-migrations.sh`).

The aggregation layer reads the continuous aggregate matching the requested grain and falls back to
raw trades only inside the uncompressed-raw horizon.

## Consequences

Timescale supplies chunking, compression (about 10x on raw trades), continuous aggregates and the
whole Postgres toolchain. Costs: continuous aggregates, compression and tiering are under the TSL
licence (operators accept it for the binary; see `deploy/docker-compose/README.md`), a single-primary
write ceiling that would need app-layer sharding, refresh policies that can starve foreground queries,
and self-operated Postgres. Plain Postgres, ClickHouse (at the time), InfluxDB, Cassandra, Timestream
and Timescale Cloud were rejected; managed and AWS-only options break the self-host constraint.
Parquet-in-MinIO tiering stays a possible future option.

## Evidence

Migration 0031 (`migrations/0031_remove_trades_retention.up.sql`); HA design in
[docs/architecture/ha-plan.md](../architecture/ha-plan.md) and
[docs/architecture/infrastructure/multi-region-topology.md](../architecture/infrastructure/multi-region-topology.md).
