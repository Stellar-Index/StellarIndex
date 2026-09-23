---
title: Runbook — ClickHouse lake data backup (ADR-0043 §2.4)
last_verified: 2026-09-23
status: draft
severity: P3
---

# Runbook — `stellarindex_ch_lake_backup_stale`

## At a glance

| Field | Value |
| ----- | ----- |
| Alert | `stellarindex_ch_lake_backup_stale` (no successful lake backup in 96 h, or none inside 96 h on a host that has a lake — including a host with no backup disk configured; per host, carries `instance`) |
| Severity | P3 (ticket) |
| Detected by | `deploy/monitoring/rules/storage.yml` and `configs/prometheus/rules.r1/storage.yml` |
| Producer | `scripts/ops/ch-lake-backup.sh` via `ch-lake-backup.timer` (daily, 04:10 UTC) |
| Typical MTTR | 15 min to fix a credential; a first full takes ~2 days |
| Impact | No immediate customer impact. While it fires, losing the lake means a re-derive from the Galexie archive (~1–2 weeks) instead of a restore (hours). |

## Symptoms

- The ticket names a host. `stellarindex_ch_lake_backup_configured{instance="<host>:9100"}`
  is `0` (no off-site disk configured) or `1` (configured, but no run has
  succeeded in 96 h).
- `systemctl status ch-lake-backup.service` shows a failed or long-running run.

## Quick diagnosis (≤ 5 min)

```bash
journalctl -u ch-lake-backup.service -n 100 --no-pager
cat /var/lib/stellarindex/ch-lake-backup/chain    # <unix>\t<path>, the full first
clickhouse-client -q "SELECT name, status, error, start_time, end_time,
  formatReadableSize(compressed_size) FROM system.backups ORDER BY start_time DESC LIMIT 5"
clickhouse-client -q "SELECT name, type FROM system.disks"   # si_lake_backup present?
```

## Mitigation (≤ 15 min)

- **`configured 0`** — no target. Set `ch_lake_backup_s3_endpoint` (bucket
  and prefix, trailing slash) in the host inventory and
  `ch_lake_backup_s3_key` / `ch_lake_backup_s3_key_secret` in the vault,
  then apply with `--tags backup`. ClickHouse picks up the new disk without
  a restart. `systemctl start ch-lake-backup.service` starts the first full.
- **`BACKUP_FAILED` with an S3 error** — credential, endpoint or quota.
  Fix the vars and re-apply; the next run retries.
- **`BACKUP_NOT_FOUND`** — the base of the current chain is gone from the
  bucket. The script has already reset its state; the next run is a full.
- **Already running** — a full of the lake runs ~2 days at the default
  100 MiB/s cap (`ch_lake_backup_max_bandwidth`). The script refuses to
  start a second backup beside it; wait for it.
- **Exit 2** — the backup succeeded but an old chain could not be removed
  (logged as `PRUNE FAILED`). It stays on record and the next full retries;
  remove it by hand if the bucket's quota is the issue:
  `clickhouse-disks -C /etc/clickhouse-server/config.xml --disk si_lake_backup --query "remove -r stellar/<chain>"`.

## Restore

Each chain is `stellar/<chain>/<stamp>-full` plus `stellar/<chain>/<stamp>-incr`
links. Restore from the NEWEST link; ClickHouse follows its base backups
itself, so every earlier link of the chain must still be on the disk.

```bash
# On the restoring host: ClickHouse installed and si-lake-backup.xml
# rendered (apply the role with the same ch_lake_backup_* vars).
clickhouse-disks -C /etc/clickhouse-server/config.xml --disk si_lake_backup --query "list stellar"
clickhouse-client -q "RESTORE DATABASE stellar FROM Disk('si_lake_backup', 'stellar/<chain>/<newest-link>')"
```

Then bring the lake from the backup's point in time to the tip with
`ch-live-catchup` (its tip extension backfills `[max(ledger_seq)+1, tip]`
of the restored lake), and check it with `stellarindex-ops verify-lake` /
`verify-contiguity` before the projector is pointed at it. The re-derive in
[ch-schema-restore](ch-schema-restore.md) stays the last resort for when
no chain survives.

`scripts/ops/ch-lake-backup-roundtrip-test.sh` runs full → incremental →
DROP → RESTORE against a real ClickHouse and checks the restored content
hash.

## Root cause analysis

The run writes the backup server-side (`BACKUP ... ASYNC`) and polls
`system.backups`. An id that disappears from `system.backups` means the
server restarted under it. The error column of `system.backups` holds the
server's reason.

## Known false-positive patterns

- A freshly provisioned host fires until its first full completes.

## Related

- [ADR-0043](../../adr/0043-backup-and-restore-strategy.md) §2.4
- [Off-site backup plan](../off-site-backup-plan.md) §4
- [ch-schema-restore](ch-schema-restore.md) — the DDL snapshot and the re-derive path

## Changelog

- 2026-09-23 — created with the backup job.
