---
title: Production-readiness — comprehensive remaining-work register
last_verified: 2026-07-22
status: superseded — see v1-launch-plan.md
---

# Remaining work to production state

> **⚠️ SUPERSEDED by [`v1-launch-plan.md`](v1-launch-plan.md)**, which
> absorbed the open items. Rows already done are dropped; what remains is
> detail not restated there. Verify against `main` before acting on a row.

Status: ⬜ to-do · 🏗️ large project · 🟠 needs an operator decision · ⛔ ruled out

## 1. Data pipeline

| # | Item | Notes |
|---|---|---|
| D2 | `intra_ledger_seq` for partitions 39–53 | `scripts/ops/d2-ordinal-reproject.sh` is retired; see [d2-ordinal-reproject.md](d2-ordinal-reproject.md) |
| D3 | `ledger_entries_current` reproject | `deploy/clickhouse/ledger_entries_current_intra_ledger_seq.sql`, windowed, **drop MVs before RENAME** |
| D4 | `projector-replay` → Postgres served tier | `derive_generation` guarded; also closes the sep41 projection gap ⟹ clears `supply_cross_check_divergence` |
| D5 | Census `DELETE` (`op_index=-1 AND tx_hash='' AND change_type='state'`) | ~1.9M rows/partition; only AFTER D2 |
| D6 | `tx_hash`/`tx_hash_index` → ZSTD | ~0.3–0.5 TiB reclaim |

Genesis edge [2 → 287,404] is likely unfillable (a prior ledger-2 walk left 0
rows): accept + document, recover via op-census if ever needed.

## 2. Correctness remainder

- ⬜ F6 ≡ C2-16 — oracle-reconcile window netting; needs a content-level per-update reconcile on a vintage-stable identity.
- ⬜ F8 / F9 / F10 — lower-severity fail-opens from the sweep.
- ⬜ M4 — caller-side supply close-timestamp fix (regression guard landed).

## 3. Explorer performance & product

- ⬜ `/v1/accounts` background snapshot: 214M-row `FINAL` scan, no cache, HTTP 500 under load. Follow the `CoverageCache` pattern, 30-min interval.
- ⬜ `/v1/accounts` honest degradation: the cause is a query timeout, not "projection still backfilling"; return 200 + `flags.degraded`.
- ⬜ `Cache-Control` on `/v1/ledgers`, `/v1/operations`, `/v1/network/throughput` (fall through to `private, no-store`). `/v1/account/*` (singular) is the authed surface; `/v1/accounts` is public.
- ⬜ `api.stellarindex.io` behind Cloudflare — without it every `s-maxage` is inert.
- ⬜ ETag / `If-None-Match` — none in `internal/`; cheap 304s on polling endpoints.
- ⬜ Instrument the Redis (T2) cache layer: no `cache_ops_total`, so the miss-rate alert is blind to it.
- 🏗️ Materialize `ledger_entries_current` aggregates (`classic_supply_current`, `account_wealth_snapshot`) via the aggregator's rollup sweep; partitioning it is low priority once they land.
- ⬜ Prewarm warms `limit=199`; the explorer sends 25/100/500.
- ⬜ `/assets/native` advertises 5,553 markets but lists 100.

## 4. Capacity — R1 software levers only

⛔ **R1 is NOT hardware-upgradeable.** Fixed 4× 7.68 TB NVMe, no 5th drive, no
raidz expansion. Never propose a drive upgrade.

| Item | Reclaim | Notes |
|---|---|---|
| `galexie-archive` → cold S3, then trim local | ~5.5 TiB | `s3_cold_bucket_archive`; reads fall through transparently |
| `tx_hash` → `FixedString(32)` | ~0.6–1 TiB | 64-char hex today, barely compresses; schema + binary migration |
| TimescaleDB compression policies | ~0.2–0.4 TiB | 19 eligible hypertables without a policy; staged in `scripts/ops/add-missing-compression-policies.sql`; run post-D4 |
| CH `system.*_log` TTL | ~40 GiB recurring | applies at the next coordinated CH restart |

`max_*_size_to_drop` stays at the 50 GB default (ansible-pinned); planned big
drops use the force flag: [clickhouse-destructive-ddl.md](clickhouse-destructive-ddl.md).

## 5. Horizontal scale — R2 / R3 / R4

Scale = new boxes; R1 is retired when near-full via bootstrap-from-verified-snapshot.
Topology and HA blockers: [ha-plan.md](../architecture/ha-plan.md).

- ⬜ HA latent blockers before the first HA deploy: F-001 Patroni REST binds 127.0.0.1 vs `ansible_host`; F-002/F-003 HA metrics unscraped; F-004 etcd plaintext, no auth; F-006 keepalived multicast; F-009; F-010.
- 🏗️🟠 ClickHouse HA is missing from `ha-plan.md` §3; CH is the biggest store and a SPOF.

## 6. Ops hardening

- ⬜ F4 real DR restore drill.
- ⬜🟠 F4 pgbackrest retention tune.
- ⬜ API auth / rate-limit review before public launch.
- ⬜ Dependency advisory cadence (newly published advisories break CI without warning).
- 🟠 Operator decisions not recorded elsewhere: peg-set thresholds; served-tier retention / serve-window policy; HA as a v1 requirement or accepted-risk fast-follow.
