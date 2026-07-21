---
title: R1 storage breakdown + cleanup opportunities
last_verified: 2026-07-20
status: active
---

# R1 storage breakdown (2026-07-20)

Full accounting of the ~20.5 TiB usable ZFS pool, with cleanup opportunities ranked by reclaim vs effort. Live-measured during the Phase-D backfill.

## 1. Physical footprint (ZFS datasets — the truth)
| Dataset | Physical used | ZFS ratio | What it is |
|---|---|---|---|
| **data/clickhouse** | **7.72 TiB** | 1.50× | The served lake (growing +~2 TiB from D1) |
| **data/minio** | **5.57 TiB** | 1.27× | galexie-archive raw LCM — the source of truth |
| **data/pgbackrest** | 888 GiB | 1.01× | pgBackRest backups (self-pruned to 1 full + diffs) |
| **data/postgres** | 688 GiB | 3.80× | Served tier (TimescaleDB hypertables) |
| galexie/archive/loki/prometheus | ~35 GiB | — | working sets, logs |
| **Total** | **~15.9 TiB / 20.5 (69%)** | | |

**Two datasets = 84% of usage: ClickHouse + the MinIO archive.**

## 2. ClickHouse internal (CH-reported sizes; ~1.6× physical after ZFS)
Top tables: `ledger_entry_changes` 4.37 T, `operations` 2.31 T, `transactions` 1.75 T, `operation_results` 1.60 T, **`tx_hash_index` 1.05 T**, `contract_events` 624 G, `account_movements` 451 G, `ledger_entries_current` 94 G, `operation_participants` 91 G, `contract_events_daily` 32 G, `supply_flows` 23 G, `ledgers` 19 G.

`ledger_entry_changes` (4.37 T) by column: `entry_xdr` 1.87 T, `key_xdr` 772 G, `account_id` 555 G, **`tx_hash` 441 G**, `asset` 307 G, `balance` 269 G.

### 🔑 The standout: `tx_hash` costs ~4.1 TiB across all tables
Stored as a **64-char hex `String`**: `transactions` 967 G + `tx_hash_index` 935 G + `operations` 723 G + `operation_results` 719 G + `ledger_entry_changes` 441 G + `account_movements` 139 G + `contract_events` 91 G + `operation_participants` 70 G. In tables where the hash is **unique per row** (`transactions`, `tx_hash_index`) it barely compresses (~58–64 B/row). A Stellar tx hash is 32 raw bytes — storing it as **`FixedString(32)`** (hex-encode only at the API edge) would roughly **halve** those → **~0.6–1 TiB physical reclaim.** *Schema + binary change (writes/reads hex today) → a real migration, not a quick win. The single biggest structural opportunity on the box.*

## 3. Cleanup opportunities (ranked)

### ✅ Done now (simple, safe)
- **Dropped `contract_events_daily_old_20260709`** (1 GiB) — a dead rename artifact, superseded by `contract_events_daily` (77 K old rows vs 32 M current).
- **Truncated CH system-log bloat** (~43 GiB): `trace_log`, `query_log`, `processors_profile_log`, `part_log`, metric logs — pure diagnostics. *(TODO: add TTL so they don't re-bloat — a config drop-in.)*
- Confirmed: **no other `_old`/`_bak`/`_tmp`/dated leftover tables**, and **no detached parts** (no orphaned space).

### ⏳ Available after D1 (same proven recompress lever — deferred to avoid stacking CH jobs)
- **Recompress the still-LZ4 columns to ZSTD:** `transactions.tx_hash` + `tx_hash_index` (unique hashes, poorly compressed by LZ4) → ~0.3–0.5 TiB; plus `account_movements`, `ledgers`, `operation_participants`, and `account_id`/`asset` in LEC — marginal. Codecs can be set now (instant); OPTIMIZE after D1.
- **TimescaleDB compression** on the uncompressed PG hypertable chunks (`_hyper_*` vs already-compressed `compress_hyper_*`) — a compression policy → ~0.2–0.4 TiB. Simple-ish (a Timescale `ALTER ... SET (timescaledb.compress)` + policy).

### 🏗️ Big structural levers (real projects, not now)
- **`tx_hash` → `FixedString(32)`** (§2) — ~0.6–1 TiB. Schema + binary migration.
- **MinIO galexie-archive (5.57 TiB) → S3** — the biggest single item; only read during rare backfills (config-ready via `s3_cold_*`). Frees a third of the pool. Needs S3 standup (see `off-site-backup-plan.md`).

### ⚖️ Not recommended
- Trimming pgBackRest further (888 GiB) — it's already self-pruned to one full + diffs and is the **only** backup until off-site exists.

## 4. Net
Immediate simple cleanup recovered ~44 GiB (leftover table + logs). The meaningful headroom is the **after-D1 recompress (~0.3–0.5 TiB)** and, structurally, **`tx_hash` FixedString (~1 TiB)** + the **archive→S3 offload (5.5 TiB)**. None are blocking — Phase D fits in current headroom (bottoms out ~1.5 TiB free) — but they're the roadmap for durable capacity without a 2nd server.
