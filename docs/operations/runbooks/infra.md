---
title: Runbook — infra
last_verified: 2026-10-06
status: living
severity: P1
---

# Runbook — host, disk, backup and worker alerts (infra family)

## At a glance

Alerts live in `configs/prometheus/rules.r1/infra.yml` and `storage.yml`; `deploy/monitoring/rules/{infra,storage}.yml` are the multi-host twins. Section headings are the alert names.

## Shared context

- r1 is a single host: Hetzner EX63 (FSN1), `ssh root@136.243.90.96`. No replica, HAProxy, Patroni, Sentinel, PgBouncer or validator. Hetzner Robot (KVM / Rescue / Reset) is held by the maintainer (`docs/operations/maintainer-workflow.md`).
- Storage: OS disks are mdadm RAID1 (`md0` swap, `md1` `/`; `docs/operations/r1-deployment-state.md`); `nvme0n1`/`nvme1n1` also carry a ZFS member, so one failed OS drive also degrades `data`. Data is ZFS pool `data` on 4 NVMe, **raidz1 (single parity)**: DEGRADED means zero redundancy. Never promise two-failure tolerance (`scripts/ci/lint-docs.sh` section 18).
- node_exporter is the Debian unit `prometheus-node-exporter` (`localhost:9100`); the old `node_exporter.service` is deliberately stopped.
- Heavy one-shots run under `/usr/local/sbin/run-heavy-job.sh` (`heavy-*.scope` is expected; a heavy binary run raw is the fault).
- Memory: strict overcommit (`vm.overcommit_memory=2`), 16 G swap, `vm.swappiness=1`; failure shows as ENOMEM before the OOM-killer. Textfile metrics: `/var/lib/node_exporter/textfile_collector/` (producers: `scripts/ci/textfile-producers.manifest`).
- All-clear: `systemctl --failed` empty and `zpool status -x` clean.

## stellarindex_host_down

Ticket. `up{job="node_exporter"} == 0 OR absent_over_time(up{job="node_exporter"}[5m]) == 1`, `for: 2m`. Prometheus and Alertmanager run on r1, so this fires only while the box is alive (exporter or scrape path broken); alongside it `stellarindex_prometheus_scrape_failing` ([`meta.md#stellarindex_prometheus_scrape_failing`](meta.md#stellarindex_prometheus_scrape_failing)). A real outage shows only via out-of-band Healthchecks.io (`stellarindex-heartbeat@{indexer,aggregator,api}.timer`, [`meta.md#stellarindex_deadmansswitch`](meta.md#stellarindex_deadmansswitch)); each is disabled if its URL in `/etc/default/{node-healthcheck,stellarindex-healthchecks}` is empty.

Check:

```sh
ping -c 3 <r1-ip>                         # IP in configs/ansible/inventory/r1.yml (gitignored)
ssh -o ConnectTimeout=10 root@r1 uptime   # fail2ban guards sshd: don't hammer
systemctl status prometheus-node-exporter
journalctl -u prometheus-node-exporter -n 50
curl -s localhost:9100/metrics | head -1
ss -ltnp | grep :9100                     # expect exactly ONE listener
```

Likely causes: exporter died or OOM-killed; port 9100 collision with the legacy unit; reboot (`uptime` < 5 min); network isolation; hardware/kernel hang (`## stellarindex_nvme_smart_warn`, `## stellarindex_zfs_pool_degraded`); Prometheus itself ([`prometheus-tsdb-corruption.md`](prometheus-tsdb-corruption.md)).

Fix:

1. Exporter only: `systemctl restart prometheus-node-exporter`, `curl -s localhost:9100/metrics | head -1`.
2. Host down: no failover and alerting is dark. Treat as SEV-1 manually: Discord #stellarindex-pages, status page ([`sev-status-page-update.md`](sev-status-page-update.md)), Robot Reset then KVM. Locked out by nftables: `bootstrap-archival-node.md` "Firewall locked us out".
3. After return: `zpool status -x` ONLINE, `zfs list -o name,mountpoint,mounted` all mounted, `systemctl --failed` empty, then `systemctl is-active clickhouse-server postgresql@15-main redis-server galexie minio caddy cap67-movements stellarindex-indexer stellarindex-aggregator stellarindex-api prometheus-node-exporter prometheus` and `ss -ltnp | grep :9093`. galexie needs ~9 min cold catchup: do not restart it repeatedly.
4. Service runbooks: `api.md#stellarindex_api_down`, `ingestion.md#stellarindex_ingestion_all_sources_stopped`. Textfile alerts may fire ~15 min after boot.
5. Verify `up{job="node_exporter"}` = 1.

Escalate: RCA from Robot KVM, `journalctl -b -1`, `nvme smart-log` on all four NVMes.

## stellarindex_host_cpu_high

Informational. `100 - (avg by (instance) (rate(node_cpu_seconds_total{mode="idle"}[5m])) * 100) > 90`, `for: 10m`. Usually precedes `api.md#stellarindex_api_latency_p95_high`.

```sh
ssh root@136.243.90.96 'top -b -n1 -o %CPU | head -20'
ssh root@136.243.90.96 'systemd-cgtop --order=cpu --iterations=2'
ssh root@136.243.90.96 'mpstat 1 5'     # %steal is structurally 0 here
```

Likely causes: a pegged process or bad SQL plan (`pg_stat_statements`); galexie catchup (30-120 min); pgBackRest `--process-max=4` or Timescale compression; an unwrapped heavy one-shot.

Fix: plan regression: `runuser -u postgres -- psql -d stellarindex -c 'ANALYZE <table>'` then rewrite the query; long backup/compression: lower `--process-max`. Verify CPU < 70 %.

## stellarindex_host_memory_high

Informational. `(node_memory_MemTotal_bytes - node_memory_MemAvailable_bytes) / node_memory_MemTotal_bytes * 100 > 90`, `for: 10m`. Allocation-failure risk under strict overcommit; sustained swap is a red flag (`archive.md#stellarindex_host_swap_activity`).

```sh
ssh root@136.243.90.96 'ps auxww --sort=-%mem | head -10'
ssh root@136.243.90.96 'systemd-cgtop --order=memory --iterations=2'
ssh root@136.243.90.96 'free -h; cat /proc/meminfo | head -30'
ssh root@136.243.90.96 'journalctl --since -2h | grep -i "cannot allocate memory" | tail'
ssh root@136.243.90.96 'dmesg -T | grep -i "out of memory\|killed process" | tail'
ssh root@136.243.90.96 'systemctl status "heavy-*.scope" --no-pager'
```

Causes and fixes:

1. Postgres `work_mem` x `max_connections`: lower via the archival-node role vars, re-apply, restart `postgresql@15-main` (brief served-tier outage).
2. Go heap growth: pprof heap dump, then `systemctl restart stellarindex-<binary>`.
3. ZFS ARC: capped at 32 GiB in `/etc/modprobe.d/zfs.conf` (`zfs_arc_max_bytes`); a new cap needs a reboot or a write to `/sys/module/zfs/parameters/zfs_arc_max`.

Verify `available` > 20 % and no ENOMEM/OOM lines for 1 h.

## stellarindex_zfs_pool_degraded

Page (SEV-1). `node_zfs_zpool_state{state=~"degraded|faulted|unavail|suspended"} > 0`, `for: 60s` (lowercase states, `zpool` label). raidz1 with a faulted drive has ZERO redundancy: a second failure or an unrecoverable read error during resilver loses the pool. Often follows `nvme_smart_warn`.

```sh
ssh root@136.243.90.96 'zpool status -v data'
ssh root@136.243.90.96 'ls -l /dev/disk/by-id/ | grep nvme1n1'
ssh root@136.243.90.96 'nvme list'          # serial -> slot for the Hetzner ticket
ssh root@136.243.90.96 'zpool status data | grep -A5 resilver'
```

Likely causes: drive failure; controller/slot/cable dropping a good drive; power event.

Fix:

1. Escalate to SEV-1 now; the rest is a race against the resilver.
2. Quiesce heavy writers: `systemctl stop pgbackrest-backup.timer` for the window, no scrubs, empty `run-heavy-job.sh` queue.
3. Hetzner ticket with the drive serial; then `zpool replace data <old-drive-id> <new-drive-id>`; watch the ETA in `zpool status`; stay quiesced.
4. Second drive fails during resilver: pool lost. DR: triage tree in `docs/operations/archival-node-bringup.md` (restore Postgres from pgBackRest, re-mirror galexie data per `docs/adr/0016-per-region-storage-strategy.md`).
5. Verify `zpool status data` ONLINE, no errors; `node_zfs_zpool_state{zpool="data",state="online"} == 1`.

Escalate: keep `smartctl` logs of the failed drive. A planned replacement passes through DEGRADED: silence it.

## stellarindex_zfs_pool_low_space

Ticket (`for: 15m`): pool free fraction `< 0.07`: `min by (instance)(node_filesystem_avail_bytes{fstype="zfs"}) / (sum by (instance)(node_filesystem_size_bytes{fstype="zfs"} - node_filesystem_avail_bytes{fstype="zfs"}) + min by (instance)(node_filesystem_avail_bytes{fstype="zfs"}))` (node_exporter exports no zpool size; single-pool assumption). `stellarindex_zfs_pool_critical_space` is the same expr at `< 0.035`, `for: 5m`, page.

```sh
ssh root@r1 'zpool list -o name,size,alloc,free,cap,health data'
ssh root@r1 'zfs list -o name,used,avail -s used | tail -20'
ssh root@r1 'zfs list -t snapshot -o name,used -s used | tail -20'
```

`cap` >= 90 % confirms. Top consumers: ClickHouse lake, MinIO (galexie LCM), pgBackRest repo1 (on-pool, ~2.4 TB, the biggest reclaim if moved off-box), Postgres.

Fix (prefer reversible): stale snapshots (`zfs holds <snap>`, dry-run `zfs destroy -nv <snap>`, then destroy; see [`zfs-snapshots.md`](zfs-snapshots.md)); move pgBackRest off-box; confirm the Timescale compression job (hypertable job 1034) runs and force-compress recent chunks if lagging. Verify `node_filesystem_avail_bytes{fstype="zfs"}` back above threshold (~1 min after next scrape).

## stellarindex_zfs_pool_critical_space

Page. Same expr as `stellarindex_zfs_pool_low_space` at `< 0.035`, `for: 5m`. Reclaim NOW: diagnosis and fixes are in that section; stalled writes are imminent.

## stellarindex_zfs_pool_fill_85pct_within_30d

Ticket (`for: 1h`, group `stellarindex.infra_pool_trend`): `predict_linear((min by (instance)(min_over_time(node_filesystem_avail_bytes{fstype="zfs"}[1d])))[14d:1h], 30 * 86400)` divided by the same pool-size denominator as low_space `< 0.15`. Plan reclamation; fix as in `## stellarindex_zfs_pool_low_space`. A one-off large write steepens the fit: silence rather than retune.

## stellarindex_zfs_pool_fill_90pct_within_7d

Page. Same expr as the 30-day alert with horizon `7 * 86400`, threshold `< 0.10`, `for: 30m`. Find what fills the pool today (`zfs list -o name,used -s used`, compare with yesterday) and reclaim before the floors fire; fixes in `## stellarindex_zfs_pool_low_space`.

## stellarindex_nvme_smart_warn

NVMe health family, one decision: replace a drive, and how soon. `nvme_*` series come from the nvme collector writing to `/var/lib/node_exporter/textfile_collector` (`10-observability.yml`); if `nvme_percentage_used_ratio`, `nvme_available_spare_ratio` or `nvme_media_errors_total` go missing, check that redirect first.

| Alert | Fires when | Urgency |
| --- | --- | --- |
| `stellarindex_nvme_critical_warning` | `nvme_critical_warning > 0`, `for: 5m`, page | P1 |
| `stellarindex_nvme_smart_warn` | `increase(nvme_num_err_log_entries_total[1h]) > 0`, `for: 5m`, ticket | P2 |
| `stellarindex_nvme_wear_high` | `nvme_percentage_used_ratio > 0.80`, `for: 1h`, ticket | P3, procurement trigger |
| `stellarindex_nvme_spare_low` | `nvme_available_spare_ratio < 0.20`, `for: 30m`, page | P1 |
| `stellarindex_nvme_media_errors` | `increase(nvme_media_errors_total[24h]) > 0`, `for: 5m`, ticket | P2 |

With raidz1 one failing drive already means zero redundancy and NVMe is not replaceable on demand: start procurement at wear_high. `smartctl -a /dev/nvmeXn1` decodes `critical_warning` bits.

Check (all NVMe alerts):

```sh
ssh <host> 'dmesg -T | grep -iE "i/o error|nvme|media error" | tail'
ssh <host> 'smartctl -a /dev/nvme0n1'    # Media/Data Integrity Errors, Power On Hours, Percentage Used, Critical Warning
ssh <host> 'zpool status -v'             # checksum errors on this drive's pool?
```

Likely causes: end-of-life wear; one flaking drive/slot; firmware bug; spurious kernel event (SMART clean: low priority).

Fix:

1. Read SMART and ZFS status; if ZFS shows checksum errors/scrub/resilver, follow `## stellarindex_zfs_pool_degraded`.
2. Schedule replacement (`zpool offline`, swap, `zpool replace`, wait for resilver); a clean reboot before the swap is preferable (stop the `stellarindex-*` units first).
3. Verify resilver done, `zpool status` ONLINE, no new IO errors for 24 h.

## stellarindex_nvme_critical_warning

Page. Table and fix in `## stellarindex_nvme_smart_warn`.

## stellarindex_nvme_wear_high

Ticket. Planned replacement, not an incident. Fix as in `## stellarindex_nvme_smart_warn`.

## stellarindex_nvme_spare_low

Page. Reserve blocks exhausted: treat as an incident; fix as in `## stellarindex_nvme_smart_warn`.

## stellarindex_nvme_media_errors

Ticket. Uncorrected media errors on a ZFS member. Fix as in `## stellarindex_nvme_smart_warn`.

## stellarindex_nvme_thermal_throttle

Page. `nvme_temperature_celsius > 70`, `for: 5m`, per device; IO throttles to ~50 % and heat accelerates wear.

```sh
ssh root@136.243.90.96 'for d in /dev/nvme?n1; do echo -n "$d: "; smartctl -A "$d" | grep -i temperature; done'
ssh root@136.243.90.96 'sensors | grep -E "Composite|fan"'         # all drives (chassis) or one (drive)?
```

Likely causes: chassis airflow; ambient DC temperature; a single drive (heatsink, slot); heavy work (scrub, resilver, backup).

Fix: reduce load (pause pgBackRest, cancel scrubs); Hetzner ticket for remote hands. Verify < 65 C for 15 min. One drive 85 C beside 45 C neighbours is a sensor glitch: cross-check `smartctl -x`.

## stellarindex_md_array_degraded

Page. `node_md_disks{state="failed"} > 0` per `device` (`md0`/`md1`), `for: 5m`. An OS-disk RAID1 member failed.

```sh
ssh <host> 'cat /proc/mdstat'
ssh <host> 'mdadm --detail /dev/md0 /dev/md1'
ssh <host> 'dmesg -T | grep -iE "md/raid1|ata.*error|nvme.*error" | tail -30'
```

Fix:

1. Identify the failed member from `mdadm --detail` (often also `## stellarindex_nvme_smart_warn`); schedule replacement.
2. After replacement (GPT): `sgdisk -R=/dev/<new> /dev/<surviving>`, `sgdisk -G /dev/<new>`; `mdadm --manage /dev/md0 --add /dev/<new>p2` and `/dev/md1 --add /dev/<new>p3`; `zpool replace data <old-p4> /dev/<new>p4`.
3. Verify `/proc/mdstat` shows `resync`, then `mdadm --detail /dev/mdX` `State : clean`, both members `active sync`.

## stellarindex_filesystem_readonly

Page. `node_filesystem_readonly{fstype!~"tmpfs|squashfs|overlay|nsfs|ramfs"} == 1`, `for: 5m`. The kernel remounted read-only, almost always on an unrecoverable block I/O error; free space looks normal so disk-full alerts stay silent.

```sh
ssh <host> 'mount | grep " ro,"'
ssh <host> 'dmesg -T | grep -iE "remount-ro|EXT4-fs error|XFS.*Corruption|I/O error" | tail -30'
ssh <host> 'smartctl -a /dev/<underlying-device>'
```

Likely causes: disk I/O error (`## stellarindex_nvme_smart_warn`, `## stellarindex_md_array_degraded`); metadata corruption (`fsck`); ZFS pool fault (`## stellarindex_zfs_pool_degraded`).

Fix: if the device is failing, replace hardware; if sound, `fsck` offline then remount rw; on the DB volume confirm Postgres survived. Verify `mount` shows no `ro`.

## stellarindex_systemd_unit_failed

Ticket. `max_over_time(node_systemd_unit_state{state="failed",name!~"verify-archive-tier-a\\.service|verify-archive-tier-b\\.service|ch-supply\\.service|pgbackrest-backup\\.service|ch-schema-snapshot\\.service|verify-served-values\\.service"}[10m]) == 1`, `for: 25m`. Catch-all for units without a dedicated alert (exclusions in `scripts/ci/unit-failed-dedicated.baseline`). Failures show as a consumer serving stale data days later: ask what has been reading the unit's output.

```sh
systemctl status <unit>
journalctl -u <unit> --since -24h --no-pager | tail -50
systemctl list-timers <unit>.timer            # is the timer even enabled?
```

| Unit | Writes | Staleness symptom |
| --- | --- | --- |
| `directory-sync` | `account_directory` | a flagged issuer keeps getting a published price |
| `holders-rollup` / `census-rollup` | rollup tables | holder/census figures frozen |
| `issuer-flags` | issuer flag set | stale verification badges |
| `sep1-refresh` | SEP-1 metadata | stale home domains |
| `cap67-movements` | classic movement rows | supply divergence |
| `asset-registry-backfill` | `classic_assets` + `issuers` | held-but-never-traded assets absent from `/v1/assets`; a run that stopped early printed `RESUME:` and is not a failure |

Fix: fix the cause, then `systemctl start <unit>`; do not just `reset-failed`. Confirm it wrote something (row count / `max(synced_at)` moved). Add an exclusion only for a unit with a better dedicated alert, in `scripts/ci/unit-failed-dedicated.baseline` naming the owning alert.

## stellarindex_textfile_scrape_error

Ticket. `node_textfile_scrape_error > 0`, `for: 15m`. One malformed `*.prom` file is dropped (no per-file label) and every alert sourced from it goes silent.

```sh
journalctl -u prometheus-node-exporter --since -1h | grep -i textfile     # names the file and parse error
curl -s http://<instance>:9100/metrics | grep node_textfile_scrape_error
ls -la /var/lib/node_exporter/textfile_collector/                         # 0-byte / half-written file
```

Fix: partial write: remove the file, the next run regenerates it; bad syntax: fix the writer and check with `scripts/ci/lint_textfile_exposition.py`. Owning unit: `scripts/ci/textfile-producers.manifest`. Verify the gauge returns to 0. 

## stellarindex_keepalived_textfile_stale

Ticket. `time() - node_textfile_mtime_seconds{file="/var/lib/node_exporter/textfile_collector/keepalived.prom"} > 600`, `for: 5m`. A dead scraper freezes `stellarindex_keepalived_active` / `stellarindex_haproxy_vip_owner`. Producer: `keepalived-textfile-scraper.timer` (30 s) running `/usr/local/bin/keepalived-textfile-scraper` (`configs/ansible/roles/haproxy/tasks/07-monitoring.yml`).

```sh
systemctl status keepalived-textfile-scraper.timer
systemctl status keepalived-textfile-scraper.service
journalctl -u keepalived-textfile-scraper.service -n 50
/usr/local/bin/keepalived-textfile-scraper   # run by hand, inspect the error
```

Likely causes: `systemctl is-active keepalived` failing; the `ip -4 a show` interface no longer matching `keepalived_iface`.

## stellarindex_worker_panicked

Page. `stellarindex_worker_panics_total > 0`, `for: 0m`, per `worker` label. A goroutine wrapped in `worker.Recover` (`internal/worker/recover.go`) panicked: the process is up but that worker is STOPPED until restart (the restart resets the counter and clears the alert). Journal: `background worker panicked — worker STOPPED, process still running`.

```sh
ssh root@136.243.90.96 'journalctl -u stellarindex-api -u stellarindex-indexer -u stellarindex-aggregator --since "-30m" --no-pager | grep -A3 "background worker panicked"'
ssh root@136.243.90.96 'curl -s "http://localhost:9090/api/v1/query?query=stellarindex_worker_panics_total" | python3 -m json.tool | grep -E "worker|value"'
```

Fix: `systemctl restart stellarindex-<binary>`; after an indexer restart confirm the cursor advances (`ledger-ingest.md#stellarindex_ingestion_cursor_stuck`). The same worker panicking again is a poison input: stop the unit rather than crash-loop it. A panic is always a code defect: file it with the journal stack.

## Backup (pgBackRest) shared context

- Scheduler: `pgbackrest-backup.timer` runs `pgbackrest-backup.service` (`/usr/local/bin/pgbackrest-backup.sh`, `User=postgres`) daily 02:00 UTC + up to 15 min jitter; full Sunday, diff Mon-Sat; one `--repo=N` backup per repo (repo1 then repo2), exit with the first non-zero rc. Textfile metrics `stellarindex_pgbackrest_backup_{last_success_unix,last_rc,duration_seconds}{repo}`. Installed by `configs/ansible/roles/archival-node/tasks/18-pgbackrest-backup.yml`.
- Repos: `repo1` = `/var/lib/pgbackrest` (ZFS `data/pgbackrest`, same pool as the DB, `repo1-retention-full=2`); `repo2` = offsite S3 (`repo2-retention-full=1`, aes-256-cbc; `docs/operations/off-site-backup-plan.md`). Live `pgbackrest.conf` is hand-managed.
- Metrics: `pgbackrest_backup_since_last_completion_seconds{stanza!~"all-stanzas.*"}` from `pgbackrest_exporter` (job `pgbackrest_exporter`, `localhost:9854`, unit `pgbackrest_exporter.service`, up to 10 min lag).
- RPO 5 min via continuous WAL archiving (`archive-push`; pinned by `scripts/ci/ansible-postgres-archive-test.sh`). Pool loss loses repo1 and the DB together.
- `_none_24h` (page) fires first, `_failed` (ticket) one hour later. Never run pgbackrest as root (root-owned files break later timer runs).

Check (on r1, `ssh root@136.243.90.96`):

```sh
sudo -u postgres pgbackrest --stanza=stellarindex info          # last backup stop time, repo size
systemctl list-timers pgbackrest-backup.timer --all
systemctl status pgbackrest-backup.service --no-pager
journalctl -u pgbackrest-backup.service --since '48 hours ago' --no-pager | tail -50
ls -ld /var/lib/pgbackrest; zfs list data/pgbackrest data/postgres; df -h /var/lib/pgbackrest
sudo -u postgres psql -Atc "SELECT archived_count, failed_count, last_archived_time, last_failed_time FROM pg_stat_archiver;"
sudo -u postgres psql -Atc "SELECT now(), pg_is_in_recovery();"  # backup needs primary access
curl -s http://127.0.0.1:9854/metrics | grep pgbackrest_backup_since_last_completion_seconds
cat /var/lib/node_exporter/textfile_collector/pgbackrest_backup.prom   # which repo failed (rc per repo)
```

Likely causes:

1. repo1 pool full (stops WAL archiving first, so `stellarindex_wal_archive_stale` leads) or root-owned strays: `find /var/lib/pgbackrest /var/log/pgbackrest /var/spool/pgbackrest -not -user postgres`, then `chown -R postgres:postgres`; free space per [`postgres.md#stellarindex_timescale_disk_full`](postgres.md#stellarindex_timescale_disk_full). S3 failures hit only `repo2` (`stellarindex_pgbackrest_backup_last_rc{repo="2"}` non-zero) and surface as `stellarindex_backup_offsite_stale` ([`backup-offsite.md#stellarindex_backup_offsite_stale`](backup-offsite.md#stellarindex_backup_offsite_stale)).
2. `archive-async` spool (`/var/spool/pgbackrest`) full: `pg_stat_archiver` `failed_count` rising.
3. Binary/repo format or cipher mismatch (`docs/operations/pgbackrest-encryption.md`).
4. Scheduler did not run: timer disabled, host down at 02:00 UTC, or exit 127 (pgbackrest absent on testnet/futurenet).

Related: [`restore-drill.md#stellarindex_restore_drill_stale`](restore-drill.md#stellarindex_restore_drill_stale), `account-erasure.md` (a restore brings erased accounts back), `docs/adr/0043-backup-and-restore-strategy.md`, `deploy/monitoring/rule-tests/storage-backup_test.yml`.

## stellarindex_timescale_backup_none_24h

Page (SEV-1, RPO breach). `min by (stanza) (pgbackrest_backup_since_last_completion_seconds{stanza!~"all-stanzas.*"}) > 24 * 3600`, `for: 5m`, `alert_family: timescale_backup_age/{{ $labels.stanza }}`, `storage.yml`.

Fix (`stellarindex_timescale_backup_*`):

1. Run a manual backup as postgres to prove the system works (single-repo; without `--repo` only repo1 is written):

   ```sh
   sudo -u postgres pgbackrest --stanza=stellarindex --repo=1 --type=diff backup
   sudo -u postgres pgbackrest --stanza=stellarindex --repo=2 --type=diff backup
   ```

2. Manual works: check the scheduler: `systemctl list-timers pgbackrest-backup.timer --all`, `systemctl enable --now pgbackrest-backup.timer`, `journalctl -u pgbackrest-backup.service -n 100 --no-pager`. Manual fails: fix that error.
3. Declare SEV-1. Confirm WAL archiving still works (`pg_stat_archiver.failed_count` not rising) and check repo2 with `pgbackrest info --repo=2`.
4. Once healthy take a full (not differential) backup for a known-good restore point (next timer full is Sunday):

   ```sh
   sudo -u postgres pgbackrest --stanza=stellarindex --repo=1 --type=full backup
   sudo -u postgres pgbackrest --stanza=stellarindex --repo=2 --type=full backup
   ```

   An extra full expires the oldest chain; make sure the restore drill validated the newest chain first.
5. Verify `info` shows a backup with stop time < 24 h AND `pgbackrest_backup_since_last_completion_seconds` dropped (allow 10 min exporter lag) AND both alerts resolved.

Escalate: RCA from the backup log (last success through first failure) and ZFS/kernel logs for `data`.

## stellarindex_timescale_backup_failed

Ticket. Same expr as `stellarindex_timescale_backup_none_24h` with `> 25 * 3600`, `for: 5m`, same `alert_family`; follows the page by one hour. Diagnosis and fix are in `## stellarindex_timescale_backup_none_24h` and the shared context.

## stellarindex_pgbackrest_backup_metrics_absent

Page. `up{job="pgbackrest_exporter"} == 1 unless on (instance) pgbackrest_backup_since_last_completion_seconds{stanza!~"all-stanzas.*"}`, `for: 15m`. The freshness alerts are blind on an empty series: treat as "backup state unknown", same urgency as `_none_24h`.

```sh
curl -s http://127.0.0.1:9854/metrics | grep -E '^pgbackrest_(exporter_status|stanza_status|backup_since_last_completion_seconds)'
journalctl -u pgbackrest_exporter.service -n 50 --no-pager
sudo -u pgbackrest_exporter pgbackrest --stanza=stellarindex info --output=json   # exactly what the exporter sees; NOT as postgres/root
stat -c '%U:%G %a' /etc/pgbackrest/pgbackrest.conf /var/lib/pgbackrest
command -v pgbackrest || echo "pgbackrest NOT installed"
```

- `pgbackrest` not installed (exit 127; testnet/futurenet): install it and create the stanza, or set `pgbackrest_backup_enabled=false`.
- Permission denied on conf/repo (conf `postgres:postgres 0640`, repo `0750`, exporter runs as `pgbackrest_exporter`): add that user to the `postgres` group; verify with the `sudo -u pgbackrest_exporter ... info` line.
- Stanza error: fix as in `## stellarindex_timescale_backup_none_24h`.

Verify the `curl` shows `pgbackrest_backup_since_last_completion_seconds{stanza="stellarindex",...}`; the alert resolves within ~15 min.

## stellarindex_pgbackrest_backup_unit_failed

Ticket. `node_systemd_unit_state{name="pgbackrest-backup.service",state="failed"} == 1`, `for: 5m`. The nightly unit exited non-zero (127 when pgbackrest is absent): the early causal signal, ~24 h before the freshness alerts. Excluded from `stellarindex_systemd_unit_failed`. `journalctl -u pgbackrest-backup.service -n 100`; fix as in the shared context and `## stellarindex_timescale_backup_none_24h`.

## stellarindex_wal_archive_stale

Ticket. `pg_stat_archiver_last_archive_age > 600`, `for: 10m` (`infra.yml`; postgres_exporter's `pg_stat_archiver` collector; same signal as `postgres.wal_archive_max_age_seconds` on `/diagnostics/backups`). Continuous WAL archiving holds the 5-min RPO between fulls; a stalled archiver with green backups means RPO is already wider than believed. Fix: repo1 shares the DB's pool, so check `pg_stat_archiver.failed_count` and repo1 free space first; the shared-context causes 1 and 2 usually explain both.

## minio-metrics-403

P1 (page). The `minio` job is `down` with 401 (missing/empty bearer file) or 403 (service account lacks `admin:Prometheus`). Detected by `stellarindex_minio_exporter_down` (`up{job="minio"} == 0 OR absent_over_time(up{job="minio"}[5m]) == 1`, `for: 2m`), whose `runbook_url` is [meta.md#stellarindex_redis_exporter_down](meta.md#stellarindex_redis_exporter_down). `configs/prometheus/prometheus.r1.yml` uses `bearer_token_file: /etc/prometheus/minio.token` (operator-minted). MTTR ~10 min.

Fix:

1. Mint a service account (host alias `local` unless `/root/.mc/config.json` differs). `--policy` takes a JSON file; the archival-node role renders it, otherwise create it first:

   ```sh
   ssh root@136.243.90.96
   mc admin user svcacct add local "<MINIO_ROOT_USER>" \
     --policy /etc/minio/policies/prometheus-read.json \
     --name "prometheus-metrics-scrape" \
     --description "Bearer-token scrape for /minio/v2/metrics/cluster"
   ```

   ```sh
   mkdir -p /etc/minio/policies
   cat > /etc/minio/policies/prometheus-read.json <<'EOF'
   {
     "Version": "2012-10-17",
     "Statement": [
       {
         "Effect": "Allow",
         "Action": ["admin:Prometheus"],
         "Resource": ["arn:aws:s3:::*"]
       }
     ]
   }
   EOF
   mc admin policy create local prometheus-read /etc/minio/policies/prometheus-read.json
   ```

   `svcacct add` prints the `Secret Key` once: that is the bearer token.
2. Write the token file (on r1, as root; `0400` `prometheus:prometheus`):

   ```sh
   umask 077
   echo "<the-secret-key>" > /etc/prometheus/minio.token
   chown prometheus:prometheus /etc/prometheus/minio.token
   chmod 0400 /etc/prometheus/minio.token
   ls -l /etc/prometheus/minio.token
   ```

3. Reload and check the target (wait one 15-30 s scrape); expect `{"health": "up", "lastError": ""}`:

   ```sh
   systemctl reload prometheus
   curl -sS http://localhost:9090/api/v1/targets \
     | jq '.data.activeTargets[] | select(.labels.job=="minio") | {health, lastError}'
   ```

4. Confirm metrics flow (expect > 0; if 0 the token is accepted but the policy needs another permission):

   ```sh
   curl -sS http://localhost:9090/api/v1/query \
     --data-urlencode 'query=minio_cluster_capacity_usable_total_bytes' \
     | jq '.data.result | length'
   ```

Failure modes: still 403 (re-mint the account); token file permission denied (chown/chmod); scrape timeout (raise `scrape_timeout` on the `minio` job in `prometheus.r1.yml`); rotated token (redo steps 2-3).

Codified: Group D of `configs/ansible/roles/archival-node/tasks/16-prometheus-exporters.yml` (`--tags exporters`) runs steps 1-3 only when `/etc/prometheus/minio.token` is absent; rotation: [credential-rotation.md](../credential-rotation.md#prometheus-bearer-token-regen-inv-0981inv-1144--now-codified). `scripts/ops/config-assertions.sh` `minio_prometheus_token_present` (hourly) catches a missing, empty or wrong-owner file.

## node-root-disk

The ~49 G root filesystem; everything else on r1 is a ZFS dataset on `data`. Rules: `configs/prometheus/rules.r1/storage.yml` (twin `deploy/monitoring/rules/storage.yml`). A full root blocks Redis writes (MISCONF, `/v1/price` 404s), crashes Postgres (its log is on root) and corrupts journald. `runbook_url` of `_full` / `_warning` points at [`cache.md#stellarindex_redis_writes_blocked`](cache.md#stellarindex_redis_writes_blocked), the downstream cascade. Incidents: `internal/incidents/data/2026-05-10-redis-writes-blocked-disk-full.md`, `internal/incidents/data/2026-06-11-clickhouse-log-channel-wedge-root-full.md`.

### stellarindex_node_root_disk_filling_fast

Page. Root trends to 0 within 30 min (10-min linear fit) AND < 50 % free.

```sh
df -h /
du -xs /var/log/* /tmp /var/cache 2>/dev/null | sort -rh | head -8
journalctl --since "-5min" --no-pager | awk '{print $5}' | sort | uniq -c | sort -rn | head -5   # the flooding unit
```

Fix:

1. Flooder is clickhouse-server (`Cannot log message in OwnAsyncSplitChannel`): freeing space does NOT unwedge it; `systemctl restart clickhouse-server`.
2. Other flooder: stop it; add a `stop` rule in `/etc/rsyslog.d/10-suppress-noisy-units.conf` and ansible `15-log-discipline.yml`.
3. Free space: `journalctl --vacuum-size=200M`; `rm /var/log/syslog.1`; `: > /var/log/<offender>`.
4. Verify `redis-cli ping`, `systemctl is-active postgresql`.

Guards: ClickHouse logs on ZFS (`config.d/zzz-logpath.xml`), journald cap 500M, rsyslog suppress file above.

### stellarindex_node_root_disk_full

Page. `(node_filesystem_avail_bytes{mountpoint="/"} / node_filesystem_size_bytes{mountpoint="/"}) * 100 < 10` for 1 min.

Check:

```sh
# -x stays on root: the /var/lib/* datasets are multi-TB ZFS
df -h /
sudo du -xsh /var/log/* /tmp /var/cache 2>/dev/null | sort -rh | head -15
ls -lh /swap_f1209; swapon --show
zfs list -o name,mountpoint,mounted data/prometheus data/loki data/clickhouse data/postgres   # unmounted dataset lands its data on root
journalctl --disk-usage
ls -lh /var/log/syslog* /var/log/postgresql/*.log 2>/dev/null
journalctl --since '-5min' --no-pager | awk '{print $5}' | sort | uniq -c | sort -rn | head -5
```

Likely causes: multi-GB syslog (guards `/etc/rsyslog.d/30-stellarindex-journald-only.conf`, `10-suppress-noisy-units.conf`; `/etc/logrotate.d/rsyslog` needs `su root adm`); journal 3 GB+ (`SystemMaxUse=500M` missing in `/etc/systemd/journald.conf.d/00-cap.conf`); `/var/log/stellarindex/*.log` (run heavy jobs via `/usr/local/sbin/run-heavy-job.sh`); Postgres log (`log_min_duration_statement`, `log_connections`; `sudo -u postgres psql -Atc "show log_min_duration_statement; show log_statement; show log_connections;"`).

Fix:

1. Free space and confirm services:

   ```sh
   sudo journalctl --vacuum-size=200M
   sudo truncate -s 0 /var/log/syslog.1
   sudo rm -f /var/log/syslog.[2-9]*
   sudo truncate -s 0 /var/log/postgresql/postgresql-15-main.log   # ONLY if PG down or file > 1 GB
   redis-cli BGSAVE
   redis-cli INFO persistence | grep rdb_last_bgsave_status        # expect ok
   systemctl is-active postgresql@15-main clickhouse-server redis-server stellarindex-api stellarindex-aggregator stellarindex-indexer
   sudo systemctl start postgresql@15-main                         # if it crashed
   journalctl -u stellarindex-indexer --since -5min | grep -c 'pool may be wedged'
   ```

2. Verify `curl http://localhost:3000/v1/price?asset=native&quote=fiat:USD` is 200 with `flags.stale=false`.
3. Find the regressed guard: `curl -s 'http://localhost:9090/api/v1/query?query=stellarindex_config_assertion_ok' | jq '.data.result[] | {a:.metric.assertion, v:.value[1]}'` (`rsyslog_ch_suppress`, `rsyslog_loki_suppress`, `journald_cap`, `ch_logs_on_zfs`, `syslog_maxsize`). Re-apply ONLY `--tags logrotate,journald,rsyslog`, `--check --diff` first (the full `15-log-discipline.yml` handlers restart clickhouse-server and redis).
4. Status page if customer impact > 5 min. Target avail > 0.30.

If `pg_wal` filled it (it is a symlink onto root):

```bash
readlink -f /var/lib/postgresql/15/main/pg_wal      # -> /pgwal/15-main/pg_wal
df -h "$(readlink -f /var/lib/postgresql/15/main/pg_wal)"
du -sh "$(readlink -f /var/lib/postgresql/15/main/pg_wal)"
grep max_wal_size /etc/postgresql/15/main/postgresql.conf
ls "$(readlink -f /var/lib/postgresql/15/main/pg_wal)"/archive_status | grep -c ready
```

Lowering `max_wal_size` is SIGHUP (`systemctl reload postgresql@15-main`); fix in ansible too (`05-postgres.yml`). NEVER delete inside `pg_wal`. If PG failed with `could not write to file "pg_wal/xlogtemp.N": No space left on device`: free space, start it, watch recovery, `systemctl reset-failed` dependants. See [the post-mortem](../postmortems/2026-09-16-r1-pg-wal-fills-root.md).

### stellarindex_node_root_disk_warning

Ticket. Root < 20 % free for 10 min.

```sh
curl -s 'http://localhost:9090/api/v1/query' --data-urlencode 'query=node_filesystem_avail_bytes{mountpoint="/"}/node_filesystem_size_bytes{mountpoint="/"}'
curl -s 'http://localhost:9090/api/v1/query' --data-urlencode 'query=predict_linear(node_filesystem_avail_bytes{mountpoint="/"}[1h], 3600*12)'
df -h / && df -x zfs -h
sudo du -xsh /var/log/* /tmp /var/tmp /var/cache/* /var/lib/* 2>/dev/null | sort -rh | head -20
journalctl --disk-usage            # expect <= 500M
findmnt -n -o TARGET,FSTYPE /var/lib/postgresql /var/lib/clickhouse /var/lib/loki /var/lib/prometheus /var/lib/minio /var/lib/galexie /var/lib/pgbackrest /var/lib/stellarindex
```

Every `findmnt` path must be `zfs`; otherwise that dataset is unmounted and the service writes to root.

Fix:

1. Find the dominant consumer above.
2. Logs: re-apply `--tags log-discipline` if guards drifted (`/etc/logrotate.d/rsyslog` `su root adm` + `maxsize 100M`, `10-suppress-noisy-units.conf`, journald 500M, ClickHouse logs under `/var/lib/clickhouse/logs`).
3. Galexie/MinIO dataset not mounted: run `zpool list -H data` first; if non-zero, `zpool import -d /dev/disk/by-id` then `zpool import data` (never let the role `zpool create -f` overwrite). Then `systemctl stop galexie` / `systemctl stop minio`, move data aside, re-apply `--tags zfs` (`03-zfs.yml`), move back, restart.
4. Postgres log on root: raise `log_min_duration_statement` in `postgresql.conf.j2`, re-apply, `SELECT pg_reload_conf();` (cap: `/etc/stellarindex-pg-logrotate.conf` via `pg-logrotate.timer`). Data volume: [`postgres.md#stellarindex_timescale_disk_full`](postgres.md#stellarindex_timescale_disk_full).
5. Verify > 0.40 free for 1 h.

Escalate: recurring means a leak; capture two `du -xsh /var/*` snapshots 7 days apart. One-time captures under `/var/log/wasm-audit/` are fine.

## process-mappings

Alerts: `stellarindex_process_mappings_high` (P3), `_critical` (P1 page), `_exhaustion_projected` (P1 page), `_probe_degraded` (P3). Rules: `deploy/monitoring/rules/memory-mappings.yml` + `configs/prometheus/rules.r1/memory-mappings.yml` (tests `deploy/monitoring/rule-tests/memory-mappings_test.yml`). `memory-mappings.timer` (5 min) runs `/usr/local/bin/memory-mappings` (`configs/ansible/roles/archival-node/files/memory-mappings.sh`) into `/var/lib/node_exporter/textfile_collector/memory_mappings.prom`. Steady state ~47,000-50,000 mappings vs `vm.max_map_count` 1,048,576; at exhaustion ClickHouse cannot `mmap` (errno 12) and crashes. The cause of past excursions is not established: capture evidence first.

Check:

```sh
# 0. What the probe sees (_procs 0 = watching nothing; _unreadable 1; _ratio = what alerts compare)
ssh r1 'cat /var/lib/node_exporter/textfile_collector/memory_mappings.prom'

# 1. Find the pid: highest-mapping process off the clickhouse binary (never `pgrep -f clickhouse`;
#    r1 runs a watchdog parent ~97 and the server ~47,000)
ssh r1 'for x in /proc/[0-9]*; do e=$(readlink -f $x/exe 2>/dev/null); \
  case "$e" in */clickhouse) echo "$(wc -l < $x/maps) ${x##*/}";; esac; done | sort -rn'

# 2. Live count and limit
ssh r1 'wc -l < /proc/<pid>/maps; cat /proc/sys/vm/max_map_count'

# 3. What the mappings are (file paths under /var/lib/clickhouse = file-backed; blank = anonymous)
ssh r1 'awk "{print \$6}" /proc/<pid>/maps | sort | uniq -c | sort -rn | head -20'
ssh r1 'awk "{print \$6}" /proc/<pid>/maps | grep -c ^$'

# 4. If file-backed, measure part counts and mmap settings
ssh r1 'clickhouse-client --port 9300 -q "SELECT count() AS parts, sum(rows) FROM system.parts WHERE active"'
ssh r1 'clickhouse-client --port 9300 -q "SELECT * FROM system.asynchronous_metrics WHERE metric LIKE \"%Mmap%\" OR metric LIKE \"%OpenFile%\""'
ssh r1 'clickhouse-client --port 9300 -q "SELECT name, value FROM system.settings WHERE name IN (\"mmap_cache_size\",\"min_bytes_to_use_mmap_io\",\"local_filesystem_read_method\")"'
```

Fix. Capture BEFORE you fix: a restart clears the mappings and the explanation.

```sh
ssh r1 'cp /proc/<pid>/maps /var/tmp/maps-$(date +%s).txt'
ssh r1 'awk "{print \$6}" /proc/<pid>/maps | sort | uniq -c | sort -rn > /var/tmp/maps-summary-$(date +%s).txt'
```

1. Buy headroom (`_critical` / `_exhaustion_projected`); a stopgap, but a crashed ClickHouse is worse than a larger ceiling:

   ```sh
   ssh r1 'sysctl -w vm.max_map_count=2097152'
   # Persist it, or the next boot silently reverts to 1048576:
   ssh r1 'sed -i s/1048576/2097152/ /etc/sysctl.d/10-map-count.conf && sysctl --system'
   ```

   The metric is a ratio, so thresholds follow the limit. `/etc/sysctl.d/10-map-count.conf` is NOT ansible-managed (role `sysctl_tunings` in `configs/ansible/roles/archival-node/defaults/main.yml` writes `90-stellarindex.conf`, later file wins); codify afterwards.
2. Restart ClickHouse only if it already fails to allocate, after the capture: `systemctl restart clickhouse-server`; expect ~47,000 within minutes.
3. Watch the rollups behind it: `systemctl --failed` and `stellarindex_systemd_unit_failed`.

`probe_degraded` (the probe itself):

```sh
ssh r1 'systemctl status memory-mappings.timer memory-mappings.service'
ssh r1 'journalctl -u memory-mappings.service --since -1h'
ssh r1 'bash scripts/ci/memory-mappings-test.sh'   # from a checkout: pins the shipped bytes
```

- `..._procs 0`: matched nothing (process stopped or exe moved); gauges are withheld so every threshold alert is silent. Confirm ClickHouse runs; update `MEMORY_MAPPINGS_WATCH` in `memory-mappings.service.j2`.
- `..._unreadable 1`: the unit must run as `User=root`; check `ptrace_scope`.
- Unit `failed`, previous `.prom` intact: the probe refused an unparseable line (journal names it).
- Stamp (`..._updated_unix`) older than 30 min: `systemctl start memory-mappings.timer`.

A ClickHouse restart or a deliberate `vm.max_map_count` change moves the ratio: expected.

## Related

- [Alerts catalogue](../alerts-catalog.md); [`cache.md#stellarindex_redis_writes_blocked`](cache.md#stellarindex_redis_writes_blocked); `postgres.md#stellarindex_timescale_disk_full`; ADR-0008 (HA topology + DR posture); `docs/operations/ansible-drift.md`; `docs/reference/metrics/README.md`.
