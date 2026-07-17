---
title: Runbook — zfs-pool-full
last_verified: 2026-07-17
status: draft
severity: P1
---

# Runbook — `stellarindex_zfs_pool_low_space` / `stellarindex_zfs_pool_critical_space`

## At a glance

| Field | Value |
| ----- | ----- |
| Alert | `stellarindex_zfs_pool_low_space` (P3, ticket) · `stellarindex_zfs_pool_critical_space` (P1, page) |
| Severity | P1 (critical) / P3 (low) |
| Detected by | Prometheus rule in `configs/prometheus/rules.r1/infra.yml` (R1 overlay) |
| Typical MTTR | 15–45 min (reclaim) |
| Impact | The data pool is the single shared store for every writer. Near-full, ZFS (copy-on-write) degrades sharply and can **stall writes** — halting ClickHouse ingest, MinIO archive, and Postgres. |

## Why this exists

The 2026-07-17 live R1 review found the data pool ~90% full with **no**
capacity alert at all (only a redundancy/degraded alert existed). node_exporter's
ZFS collector on this box exports no zpool-level capacity metric, so these
alerts use `node_filesystem_avail_bytes{fstype="zfs"}` — every ZFS dataset
reports the same shared pool-free value, so `min by (instance)` of it is the
pool-free proxy. Thresholds are absolute (box-specific: ~16.8 TiB usable).

## Symptoms

- `stellarindex_zfs_pool_low_space` — under **1.3 TB** free for 15 min. Plan reclamation.
- `stellarindex_zfs_pool_critical_space` — under **650 GB** free for 5 min. Reclaim NOW.
- ClickHouse / MinIO / Postgres write latency climbing; possible `ENOSPC` in service logs.

## Quick diagnosis (≤ 5 min)

```sh
ssh root@r1 'zpool list -o name,size,alloc,free,cap,health data'   # cap% + health
ssh root@r1 'zfs list -o name,used,avail -s used | tail -20'        # top consumers
ssh root@r1 'zfs list -t snapshot -o name,used -s used | tail -20'  # reclaimable snapshots
```

- **cap ≥ 90%** confirms real. Check the biggest consumers: ClickHouse lake,
  MinIO (galexie LCM), pgBackRest (backups on-pool — DR anti-pattern), Postgres.

## Mitigation (≤ 15 min)

Prefer reversible reclamation over destructive deletes.

- [ ] **Stale snapshots** — `zfs list -t snapshot`; verify no holds/clones
      (`zfs holds <snap>`), dry-run `zfs destroy -nv <snap>`, then destroy.
- [ ] **pgBackRest off-box** — ~2.4 TB of backups live on the same pool; moving
      them off is the biggest single reclaim (and fixes the DR anti-pattern).
- [ ] **TimescaleDB compression** — confirm the compression job (hypertable
      job 1034) is running; force-compress recent chunks if lagging.
- [ ] **Verification:** `node_filesystem_avail_bytes{fstype="zfs"}` rises above
      the threshold; the alert clears within ~1 min of the next scrape.

## Root cause analysis

Capture `zpool list`, `zfs list -o space`, and the growth trend
(`node_filesystem_avail_bytes` over 30d) for the postmortem. The lake grows
forever by design (no TTL — correct for a canonical explorer), so a sustained
low-space signal is a **capacity-planning** trigger (tiering / R2), not just an
incident. See the storage section of `docs/audit/audit-2026-07-16/go-live-master-plan.md`.

## Known false-positive patterns

- A large re-derive / backfill (e.g. ADR-0047 Phase 0) transiently dips free
  space, then merges compact it back. The `for:` windows (15 m / 5 m) ride out
  brief dips; a sustained breach is real.

## Related

- Companion runbook: [`zfs-degraded.md`](zfs-degraded.md) — the redundancy side
  (drive failure) of the same pool. Capacity here; parity there.
- Rule: `configs/prometheus/rules.r1/infra.yml` (`stellarindex.infra` group).
- Storage plan: `docs/audit/audit-2026-07-16/go-live-master-plan.md` (§ storage/runway).

## Changelog

- 2026-07-17 — initial draft; added after the live R1 review found a ~90%-full
  pool with no capacity alert.
