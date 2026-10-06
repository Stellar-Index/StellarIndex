---
title: Storage tiers — ClickHouse raw lake, Postgres served tier, r1 capacity
last_verified: 2026-10-05
status: living doc
---

# Storage tiers

Where each kind of data lives, what "retention" means here, and the
capacity facts for r1. The decision of record is
[ADR-0034](../adr/0034-tiered-clickhouse-architecture.md); the ingest
and re-derive paths are in [ingest-pipeline.md](ingest-pipeline.md).
Read before recommending a trim, a retention change or a read-path move.

## The tiers

| Tier | Store | Holds | Notes |
|---|---|---|---|
| 0 archive | Galexie → MinIO (S3 API, never Galexie's filesystem backend: ADR-0002) | LCM/XDR, immutable | Source of truth. Below ledger 49,984,000 it is read from `aws-public-blockchain` (ADR-0043 §2, see Move D) |
| 1 raw lake | ClickHouse on r1 | Structural, decoder-independent decode of every ledger, tx, op, op result, contract event and ledger-entry change, all history | Certified genesis-to-tip; the re-derive source |
| 3 served tier | Postgres/TimescaleDB | Decoded protocol entities, trades, pricing CAGGs: the recent working set | Not the full archive. Some v1 surfaces read ClickHouse directly (ADR-0038, ADR-0048) |

Tier 2 (fuzzy search) and a Parquet cold tier were planned and not
built; exact-id search is served from ClickHouse.

**Why two stores.** The chain firehose (billions of append-only rows)
sat in a row store with unique-PK indexes larger than RAM, so every
insert was random IO and bulk reprocessing ran at ~0.24 ledgers/s
(months). Columnar append-only storage fixes the class: ClickHouse
loads and scans in bulk and dedups on merge instead of `ON CONFLICT`,
which also removes the `event_index` collision class of silent drop.
Pricing stays in Postgres: closed-bucket serving (ADR-0015), i128 as
`NUMERIC` (ADR-0003), the CAGG ladder and the Go aggregator already
work there and are small. Rejected: scaling Postgres (Citus, fewer
indexes), Parquet + DuckDB/Trino as the serving engine, a managed
warehouse (conflicts with self-hosting), decode-on-read from Galexie,
and pricing in ClickHouse.

**Dataflow.** One structural Galexie walk fills the lake; protocol
decoders read lake rows, and the served tier is re-derived from the
lake, never from a second Galexie walk. Live ingest dual-sinks (CH
structural + Postgres semantic) so pricing latency does not depend on
the lake. Details: [ingest-pipeline.md § The structural lake ingest](ingest-pipeline.md#the-structural-lake-ingest)
and [§ Re-deriving from the lake](ingest-pipeline.md#re-deriving-from-the-lake).

### Tier-1 lake schema

DDL: `deploy/clickhouse/tier1_schema.sql`. `MergeTree` family,
`PARTITION BY intDiv(ledger_seq, 1000000)`, `ORDER BY` the
query-natural key, dedup by `ReplacingMergeTree` or idempotent
partition-replace on backfill. Tables: `ledgers` (also the ADR-0033
substrate/census record), `transactions`, `operations` (`body_xdr`),
`operation_results` (`result_xdr`), `contract_events` (topics, data,
op args XDR), `ledger_entry_changes` and `ledger_entries_current` (which carry contract code/data). Keeping the
raw XDR is what lets every decoder class run from the lake.

`ReplacingMergeTree` dedups only on merge: an unmerged recent partition
can hold a row twice, so count with `FINAL` or `uniqExact` on the sort
key.

### Migration status

The ADR-0034 migration (lake stand-up, historic backfill, served-tier
rebuild from the lake, completeness on the new model, explorer and
per-protocol pages) has shipped. The projector reads the lake by
default (`clickhouse_projector_source` and `clickhouse_live_sink`, both
on). Not done: decommissioning the Postgres landing zone. The
`soroban_events` hypertable, `ledger_ingest_log`,
`internal/sources/sorobanevents`, `internal/storage/timescale/{soroban_events,topic_samples,ledger_ingest_log}.go`
and `backfill -source soroban-events` are still present. When that
work runs, it also purges the orphan `ingestion_cursors` rows of
deleted subsources and reverts trades-chunk tuning
(`max_locks_per_transaction`) that no longer applies.

## Retention: what is kept, and what "retention-scoped" means

- **Raw `trades` are kept forever. NEVER put a retention policy on
  `trades`** (AGENTS.md invariant 8). Migration 0031 removed the old
  90-day policy; its `.down.sql` names re-adding one as the "rogue
  retention on trades" data-loss drift. A `drop_after` on `trades` is
  drift: remove it.
- Arming one would also fire the completeness verifier: migration 0116
  and `compute_completeness.go`'s `detectFloorLoss` read a rising
  `MIN(ledger)` on a reconcile target as loss.
  `test/integration/migrations_test.go` pins the absence
  (`assertPolicyAbsent(… "trades", "policy_retention")`).
- "Retention-scoped" in a coverage verdict means scoped to what has
  been **projected** into the served tier, not a database drop policy.
  The lake holds the full archive; Postgres holds the working set
  (see [coverage-matrix.md § Completeness](coverage-matrix.md#completeness-what-v1coverage-publishes)).
- Price aggregates: every rung except `prices_1m` is kept forever (see
  Datasets). The nine `prices_*`/`twap_*` aggregates totalled 141 GB on 2026-09-08
  and reach the earliest trade of every source.

## Supply flows in the lake

Supply is a flow: `total = Σmint − Σburn − Σclawback`. Classic-asset
flows are in the lake for all history, not only from P23: the archive
was re-generated by a modern core that writes V4 meta for every ledger
and emits the CAP-67 `mint`/`burn`/`clawback`/`transfer` events for
classic movements back to genesis. Those rows are replay-derived and
core-version-dependent (see [supply-pipeline.md](supply-pipeline.md)
for the genesis baseline built on them).

- `stellar.supply_flows` holds one decoded row per flow event
  (`ReplacingMergeTree` by event identity), keyed by `contract_id` (SAC
  for classic, token contract for SEP-41). The indexer writes it live
  via its decode-at-ingest CH sink.
- The API's `SupplyReader.TokenSupply()` sums it live with `FINAL` on
  every request: no rollup, no refresh lag.
- `stellarindex-ops ch-supply -seed-flows` (`internal/ops/chops/ch_supply.go`)
  re-seeds `[last-seeded+1, tip]` from the lake. `run-ch-supply.sh` runs
  it daily under `ch-supply.timer` as a defensive gap-filler (normally a
  no-op). The `-write` flag and its `stellar.token_supply` rollup are
  retired; do not confuse the two flags.
- Amount decode type-tests a bare `i128` or the map variant's `amount`
  field; ~99.997% of flows decode (the rest are U32/Vec/Void bodies).
- Open caveat: the sample re-run partitions 25/45/62 are still
  duplicated in `contract_events`, so tokens active around ledgers
  25M, 45M and 62.7M read supply-inflated (≲0.14% for tokens active
  across history, more for tokens concentrated there).
- The 25/45/62 partitions are not deduped: a full `FINAL` over
  `contract_events` takes ~10 h and `uniqExact` dedup OOMs at ClickHouse's
  memory cap.
- Operator follow-up: drop the orphaned `stellar.token_supply` table on r1
  (its `computed_at` stopped advancing after 2026-06-08).
- XLM: total from `ledgers.total_coins`; it is not part of the
  mint/burn flows.

## Storage-package layering

`internal/storage` should not import compute or source packages. The
persisted shapes moved to `internal/domain`; six files still import
upward because they pull in real compute logic. They are grandfathered
by name in the shrink-only `scripts/ci/lint-imports.baseline`: a new
upward import fails CI. There is no stricter storage-purity rule.

## r1 capacity

### Pool topology

```
zpool: data
  topology:  raidz1, one 4-wide vdev across 4 × 7.68 TB Samsung MZQL27T6HBLA-00A07
  raw:       27.7 TB
  usable:    ~18.3 TB (~16.8 TiB), measured
  parity:    ONE drive. At DEGRADED there is ZERO remaining redundancy.
```

This fact has drifted before. It is raidz1, not raidz2: the measured
footprint (~16.8 T) cannot fit the ~13.85 TB that raidz2 leaves on these
drives. The authority is `zfs_data_pool_type` in
`configs/ansible/inventory/r1.example.yml`; `scripts/ci/lint-docs.sh`
§18 lints r1-scoped files against it. OpenZFS on r1 is a local 2.3.4
build (`apt-mark hold`). All four bays are in use.

### Datasets

| Dataset | Mount | Role | Last measured |
|---|---|---|---|
| `data/minio` | `/var/lib/minio` | MinIO: `galexie-archive` 2,336 GiB + `galexie-live` 433 GiB | 2.70 TiB (2026-09-30, compressratio 1.27x) |
| ClickHouse | | Tier-1 lake | 7.5 T (2026-07-17) |
| pgBackRest | | Postgres backups | 2.49 T (2026-07-17) |
| `data/postgres` | `/var/lib/postgresql` | TimescaleDB served tier | 1.21 T (2026-07-17). Grows indefinitely: raw trades are retained forever (migration 0031). One exception since migration 0156: `prices_1m` alone may carry a 90-day window, shipped disabled, and arming it releases 22 GB of that view's 69 GB on the first run. Every other price aggregate is still kept forever |
| `data/archive` | `/srv/history-archive` | SDF-format history archive, `history/` + `ledger/` only | 21 GB (after Move A) |
| `data/galexie` | `/var/lib/galexie` | Galexie captive-core working dir | 7.83 GB |

### `/srv/history-archive`

A local cache of the SDF archive, not the canonical source
(`history_archive_url` points at SDF). Only `history/` and `ledger/`
remain; ADR-0017 contracts 3+4 bind to them. `archive-completeness
fix` is the only writer (`ledger/`, plus `history/` manifests).
Readers: `archive-completeness verify` (nightly) and `verify-archive
-tier checkpoint` (Tier B, operator-run) read `ledger/` + `history/`;
`verify-archive -tier chain` (Tier A, nightly) reads only MinIO; the
`-archive-root` subcommands read `ledger/`. The indexer, aggregator
and API never read it. Tier E (`-tier archivist`) cannot pass locally
since Move A and runs only against a full archive via `-archivist-url`
([galexie-backfill.md](../operations/galexie-backfill.md) §Tier E).
R1 can offer R2/R3 Tier B only.

### Capacity levers

| Move | What | Status |
|---|---|---|
| A | Drop `/srv/history-archive` `bucket/` + `transactions/` + `results/` + `scp/` (~7.1 TB) | Done. Rebuild from SDF by `stellar-archivist mirror` takes 4-10 h if ever needed |
| B, C, E | Drop `/srv/history-archive` entirely; raidz2 to raidz1; retention on `trades` | Rejected or not a lever: B violates ADR-0017 contracts 3+4 and loses Tier B; the pool is already raidz1; E is forbidden (see Retention) |
| D | ADR-0027 cold tier + bulk trim of `galexie-archive` | Done: 780 partitions below ledger 49,984,000 deleted, 1.07 TB reclaimed. Early history is sparse; most bytes sit in the Soroban era. The trim deletes an object only after the matching AWS object HEADs OK; ADR-0043 §2 accepts the `aws-public-blockchain` dependency for `[64000, 49983999]` |
| F | Re-enable trades compression job 1000 + tighter compression | Available: ~50-150 GB, CPU cost only. Job 1000 was disabled to stop decompress-on-write storms during heavy backfills |
| G | Decode pre-Soroban classic issuance into our own observer tables | Mission work, not capacity: the only way to own that history, now read from `aws-public-blockchain` |

The Move D trim deletes only from `galexie-archive`
(`internal/ops/archive/trim_galexie_archive.go`, `S3BucketArchive`; its
MinIO identity is scoped to that bucket), never `galexie-live`.

Capacity levers left: Move F, ZSTD recompression, and a second server.
