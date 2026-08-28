---
title: Runbook — node-root-disk-warning
last_verified: 2026-08-28
status: current
severity: P2
---

# Runbook — `stellarindex_node_root_disk_warning`

## At a glance

| Field | Value |
| ----- | ----- |
| Alert | `stellarindex_node_root_disk_warning` |
| Severity | P2 (`severity: ticket`) |
| Detected by | `configs/prometheus/rules.r1/storage.yml` (loaded on r1 from `/etc/prometheus/rules.r1/*.yml` per `configs/prometheus/prometheus.r1.yml`); `deploy/monitoring/rules/storage.yml` is the multi-host source copy (identical rule). |
| Typical MTTR | 30–60 min |
| Impact | The host's 49 G root filesystem is < 20 % free. No customer impact yet, but the **P1** `stellarindex_node_root_disk_full` fires at < 10 % (for 1 m) — and its cascade (Redis MISCONF stop-writes → `/v1/price` 404s, incident 2026-05-10) is why root matters. Headroom is NOT predictable from this alert alone: the 2026-06-11 ClickHouse log-wedge filled root at ~3.8 GB/min (healthy → full in ~5 min). The **P1** `stellarindex_node_root_disk_filling_fast` (predict_linear: root reaches 0 within 30 min AND < 50 % free) may fire before or alongside this warning; if it does, follow `node-root-disk-filling-fast.md` first. Note "page" tier on r1 currently means Discord `#stellarindex-pages` only — no PagerDuty is wired (see `deploy/monitoring/README.md`), so nobody is automatically woken. |

Note: at HEAD the rule's `runbook_url` annotation points at
`redis-write-blocked-disk-full.md` (the 2026-05-10 incident procedure), not
this file; `alerts-catalog.md` links here. If you arrived via the alert link,
you are in the right place now.

## Symptoms

- `(node_filesystem_avail_bytes{mountpoint="/"} / node_filesystem_size_bytes{mountpoint="/"}) * 100 < 20` for ≥ 10 min.
- Trend in the Prometheus graph UI (`http://localhost:9090/graph` on r1, via SSH tunnel) shows a steady downward slope over recent days. There is no Grafana server on r1 (only the Grafana APT repo, used to install promtail).

Measure the real headroom instead of guessing:

```sh
curl -s 'http://localhost:9090/api/v1/query' \
  --data-urlencode 'query=node_filesystem_avail_bytes{mountpoint="/"}/node_filesystem_size_bytes{mountpoint="/"}'
curl -s 'http://localhost:9090/api/v1/query' \
  --data-urlencode 'query=predict_linear(node_filesystem_avail_bytes{mountpoint="/"}[1h], 3600*12)'   # projected avail in 12 h
```

## Quick diagnosis (≤ 5 min)

Same as `node-root-disk-full.md` § "Quick diagnosis" — what's filling the disk?

```sh
df -h / && df -x zfs -h            # root is the ~49G device; everything else should be ZFS
sudo du -xsh /var/log/* /tmp /var/tmp /var/cache/* /var/lib/* 2>/dev/null | sort -rh | head -20   # -x stays on the root FS
journalctl --disk-usage            # expect ≤ 500M (SystemMaxUse cap)
findmnt -n -o TARGET,FSTYPE /var/lib/postgresql /var/lib/clickhouse /var/lib/loki /var/lib/prometheus /var/lib/minio /var/lib/galexie /var/lib/pgbackrest /var/lib/stellarindex
```

`du -x` matters: `/var/lib/*` holds eight ZFS datasets sized in TB
(`configs/ansible/roles/archival-node/defaults/main.yml` `zfs_datasets`);
without `-x` the scan runs for a very long time and reports non-root
consumers as the top entries. Every path in the `findmnt` line must report
`zfs` — an empty or `ext4` result means the dataset is not mounted and that
service is writing to root.

## Mitigation (≤ 30 min)

This is a warning, not an emergency. Plan the cleanup, don't rush:

- [ ] Step 1 — identify the dominant consumer per the diagnosis above.
- [ ] Step 2 — apply the appropriate cleanup:
  - **Logs** → confirm the `15-log-discipline.yml` guards are in place and re-apply the archival-node role with `--tags log-discipline` if any drifted. "Current" means:
    - `/etc/logrotate.d/rsyslog`: `su root adm`, `maxsize 100M`, `rotate 7`, `delaycompress`;
    - `/etc/rsyslog.d/10-suppress-noisy-units.conf`: stop-filters for `loki` + `clickhouse-server` unit output;
    - `/etc/systemd/journald.conf.d/00-cap.conf`: `SystemMaxUse=500M`;
    - `/etc/clickhouse-server/config.d/zzz-logpath.xml`: ClickHouse logs under `/var/lib/clickhouse/logs` (ZFS), NOT `/var/log/clickhouse-server`.
  - **Known regression candidates** → ClickHouse logs reappearing under `/var/log/clickhouse-server` (2026-06-11 log-wedge loop; kill sequence in `node-root-disk-filling-fast.md`), and Prometheus TSDB / Loki chunks appearing on root (`/var/lib/prometheus` and `/var/lib/loki` are ZFS datasets since 2026-06-30 / 2026-06-11 — if `findmnt` shows them non-zfs, the dataset failed to mount).
  - **Galexie / MinIO** → galexie writes ledger meta through an S3 datastore (`galexie.toml.j2`, `type = "S3"`) into local MinIO at `/var/lib/minio`; `/var/lib/galexie` is the galexie user's home/report dir. Both are ZFS datasets (ADR-0016 only says "galexie-archive: local MinIO"). If either shows fstype ≠ `zfs` in `findmnt`, the dataset failed to mount and data landed on root: stop the writer (`systemctl stop galexie` / `systemctl stop minio`), move the data aside, re-apply the archival-node role `--tags zfs` (`03-zfs.yml`) to remount, then move the data back and restart.
  - **Postgres logs** → `/var/log/postgresql/postgresql-<ver>-main.log` lives on root (`postgresql.conf.j2`: `logging_collector = on`, `log_min_duration_statement = 1000` ms). If it dominates, **raise** `log_min_duration_statement` (e.g. `5000`, or `-1` to disable) in `configs/ansible/roles/archival-node/templates/postgresql.conf.j2` and re-apply the role (editing the live file drifts back on the next apply), then `SELECT pg_reload_conf();`. Confirm `/etc/logrotate.d/postgresql-common` (Debian package default — not managed by the role) is rotating the file. The Postgres data volume `/var/lib/postgresql` is its own ZFS dataset on r1 — vacuuming chunks never frees root; see `db-disk-full.md` for that volume.
- [ ] Step 3 — schedule a follow-up review in 24 h to confirm the trend reversed.
- [ ] Verification: `node_filesystem_avail_bytes{mountpoint="/"} / node_filesystem_size_bytes{mountpoint="/"} > 0.40` (40 % free) sustained for 1 hour. This is an operator target, stricter than the alert's own resolution at 20 %.

## Root cause analysis

If this fires more than once a quarter, the disk-usage trend has a leak. Capture for a planning ticket:
- 30-day trend of `node_filesystem_avail_bytes{mountpoint="/"}` from the Prometheus graph UI or `promtool query range`.
- Per-directory growth rate via two `du -xsh /var/*` snapshots 7 days apart.

## Known false-positive patterns

- **One-time large captures**: manual debug captures and one-shot operator log dumps can take 5–10 GB transiently (historical example: the 2026-05-10 `/var/log/wasm-history-*.stderr` captures, 2.2 GB — no repo mechanism produces these today, so their presence is itself a finding). If the trigger is identifiable and the data is needed, leave it; otherwise clean up.

## Related

- `node-root-disk-filling-fast.md` — the **P1** predict_linear alert that fires first on a fast fill.
- `node-root-disk-full.md` — the **P1** static threshold that fires next if you don't act.
- `redis-write-blocked-disk-full.md` — the incident procedure the rule's `runbook_url` currently links to (Redis MISCONF cascade).
- `db-disk-full.md` — sibling for the postgres data volume (`stellarindex_timescale_disk_full` / `_warning`; separate ZFS dataset per `configs/ansible/roles/archival-node/defaults/main.yml` `zfs_datasets`).
- ADR-0008 — HA topology + DR posture.

## Changelog

- 2026-08-28 — re-verified against HEAD: rule file path (rules.r1), P1 sibling names, no-PagerDuty/no-Grafana reality, `du -x` + `findmnt` ZFS-mount diagnosis, corrected galexie/MinIO and postgres-log mitigations (log_min_duration_statement direction was inverted), added regression candidates.
- 2026-05-12 — initial draft (audit-2026-05-12 F-1237 closure).
