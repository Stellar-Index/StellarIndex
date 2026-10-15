---
title: Runbook — ClickHouse lake data backup (ADR-0043 §2.4)
last_verified: 2026-09-23
status: draft
severity: P3
---

# Runbook — `stellarindex_ch_lake_backup_stale`

## At a glance

P3. Rules: `deploy/monitoring/rules/storage.yml`, `configs/prometheus/rules.r1/storage.yml`. Producer: `scripts/ops/ch-lake-backup.sh` via `ch-lake-backup.timer` (daily 04:10 UTC). Fires per host when: no successful run in 96 h, no confirmed full for 72 h, or the recorded full is older than `FULL_INTERVAL_DAYS` + 4 d. While it fires, losing the lake means a 1-2 week re-derive from the Galexie archive instead of an hours-long restore. A fresh host fires until its first full (~2 days) completes.

`stellarindex_ch_lake_backup_configured{instance="<host>:9100"}` is `0` (no off-site disk; every run fails on purpose with `no BACKUP_DISK configured`) or `1`. Hosts with no off-site target by design set `ch_lake_backup_enabled: false`.

## Check

```bash
journalctl -u ch-lake-backup.service -n 100 --no-pager
cat /var/lib/stellarindex/ch-lake-backup/chain    # <unix>\t<path>, the full first
clickhouse-client --port 9300 -q "SELECT name, status, error, start_time, end_time,
  formatReadableSize(compressed_size) FROM system.backups ORDER BY start_time DESC LIMIT 5"
clickhouse-client --port 9300 -q "SELECT name, type FROM system.disks"   # si_lake_backup present?
```

An id vanishing from `system.backups` means the server restarted under the run; the `error` column holds the reason.

## Fix

- `configured 0`: set `ch_lake_backup_s3_endpoint` (bucket and prefix, trailing slash) in the host inventory and `ch_lake_backup_s3_key` / `ch_lake_backup_s3_key_secret` in the vault, apply with `--tags backup` (no restart needed), then `systemctl start ch-lake-backup.service`.
- `BACKUP_FAILED` with an S3 error: credential, endpoint or quota. Fix the vars and re-apply; the next run retries.
- `BACKUP_NOT_FOUND`: the chain base is gone from the bucket. State is already reset; the next run is a full.
- Already running: a full takes ~2 days at the 100 MiB/s cap (`ch_lake_backup_max_bandwidth`); wait.
- Exit 75: deferred, a heavy job held the lock for all of `HEAVY_LOCK_WAIT_SECONDS` (default 20 h; the run takes the gap between a chunked driver's chunks). Nothing backed up; the stale alert covers prolonged deferral.
- Exit 2: backup succeeded but cleanup failed. `PRUNE FAILED`: the old chain stays on record and the next full retries; if quota is the issue remove it by hand:
  `clickhouse-disks -C /etc/clickhouse-server/config.xml --disk si_lake_backup --query "remove -r stellar/<chain>"`.
  `SWEEP SKIPPED` / `ORPHAN REMOVAL FAILED`: the orphan sweep (partial uploads of failed fulls, tracked via `started` in the state dir) did not run or complete; the next success retries. Find older strays in the B2 bucket's `r1/` listing. `stellarindex_ch_lake_backup_orphans_removed` counts removals.

## Restore

Each chain is `stellar/<chain>/<stamp>-full` plus `<stamp>-incr` links. Restore from the NEWEST link; every earlier link must still be on the disk.

```bash
# ClickHouse installed and si-lake-backup.xml rendered (same ch_lake_backup_* vars).
clickhouse-disks -C /etc/clickhouse-server/config.xml --disk si_lake_backup --query "list stellar"
clickhouse-client --port 9300 -q "RESTORE DATABASE stellar FROM Disk('si_lake_backup', 'stellar/<chain>/<newest-link>')"
```

Then run `ch-live-catchup` to extend to the tip, and check with `stellarindex-ops verify-lake` / `verify-contiguity` before pointing the projector at it. If no chain survives, use the re-derive in [ch-schema-restore](ch-schema-restore.md). `scripts/ops/ch-lake-backup-roundtrip-test.sh` exercises full, incremental, DROP, RESTORE.

Escalate: if no full has been confirmed in 7 days, page the storage owner.

## Related

- [ADR-0043](../../adr/0043-backup-and-restore-strategy.md) §2.4
- [Off-site backup plan](../off-site-backup-plan.md) §4
