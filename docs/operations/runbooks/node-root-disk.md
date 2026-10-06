---
title: Runbook — node root disk
last_verified: 2026-10-06
status: living
severity: P1
---

# Runbook — node root disk alerts

Three alerts on the host's ~49 G root filesystem (everything else on r1 is a ZFS dataset on the multi-TB `data` pool). Rules in `deploy/monitoring/rules/storage.yml` (multi-host source copy) and `configs/prometheus/rules.r1/storage.yml` (the single-host overlay r1 actually loads from `/etc/prometheus/rules.r1/*.yml` per `configs/prometheus/prometheus.r1.yml`; identical rules).

Why root matters: if it fills, Redis MISCONF blocks every cache write (`/v1/price` 404s), Postgres can't write its log and crashes, journald corrupts. Incident class: 2026-05-10 SEV-2 (`internal/incidents/data/2026-05-10-redis-writes-blocked-disk-full.md`), 2026-05-13, 2026-06-11 ClickHouse log-channel wedge (`internal/incidents/data/2026-06-11-clickhouse-log-channel-wedge-root-full.md`, root filled at ~3.8 GB/min, healthy to full in ~5 min), 2026-08-05 recurrence to 81 % (rsyslog duplicate of the API access log, see `15-log-discipline.yml`).

The static thresholds are too slow for a log-flood, hence the trend alert. Order of firing on a fast fill: filling_fast, then warning/full. If `filling_fast` fires, follow its section first.

"Page" tier on r1 currently means Discord `#stellarindex-pages` only; no PagerDuty is wired (see `deploy/monitoring/README.md`), so nobody is automatically woken.

At HEAD the `runbook_url` for `node_root_disk_full` and `node_root_disk_warning` in both rule files still points at `cache.md#stellarindex_redis_writes_blocked` (the 2026-05-10 incident procedure), so those pages do NOT link here; `docs/operations/alerts-catalog.md` does. If you arrived via the alert link, you are in the right place now.

## At a glance

| Alert | Severity | Trips | MTTR |
| ----- | -------- | ----- | ---- |
| [`stellarindex_node_root_disk_filling_fast`](#stellarindex_node_root_disk_filling_fast) | P1 (page) | predict_linear: root reaches 0 within 30 min (10-min linear fit) AND < 50 % free | 5-20 min |
| [`stellarindex_node_root_disk_full`](#stellarindex_node_root_disk_full) | P1 (page) | root < 10 % free for >= 1 min | 15-60 min |
| [`stellarindex_node_root_disk_warning`](#stellarindex_node_root_disk_warning) | P2 (`severity: ticket`) | root < 20 % free for >= 10 min | 30-60 min |

## stellarindex_node_root_disk_filling_fast

P1 page. Root (/) is trending to 0 bytes within 30 min on a 10-minute linear fit. The static `node_root_disk_full` (<10 %) page is correct but too slow for a log-flood: the 2026-06-11 ClickHouse log-channel wedge filled root at **~3.8 GB/min**. This alert fires on the *trend*, buying the 20+ minutes the static one can't.

Symptoms:

- Root free-space graph is a straight line pointing at zero.
- `journalctl -f` shows one unit repeating at very high rate (2026-06-11 signature: `clickhouse-server` emitting `Cannot log message in OwnAsyncSplitChannel` / Poco rotate stacks).

Diagnosis (60 seconds); the last command names the flooding unit directly:

```sh
df -h /
du -xs /var/log/* /tmp /var/cache 2>/dev/null | sort -rh | head -8
journalctl --since "-5min" --no-pager | awk '{print $5}' | sort | uniq -c | sort -rn | head -5
```

Remediation:

1. **If the flooder is clickhouse-server** (the known wedge): freeing space does NOT unwedge the log channel; **restart CH**: `systemctl restart clickhouse-server`. Then free space (step 3).
2. **Any other flooder**: stop or restart the unit; its journald output is rate-limited but check `/var/log/syslog` growth. If the unit is not covered by `/etc/rsyslog.d/10-suppress-noisy-units.conf`, add a `stop` rule there (and to ansible role 15-log-discipline.yml).
3. **Free space fast**: `journalctl --vacuum-size=200M`; `rm /var/log/syslog.1` (already-rotated copy); truncate the live offender file if needed: `: > /var/log/<offender>`.
4. Verify Redis + Postgres recovered: `redis-cli ping`, `systemctl is-active postgresql` (see [redis-write-blocked-disk-full](cache.md#stellarindex_redis_writes_blocked)).

Prevention state (2026-07-03):

- CH logs live on ZFS (`config.d/zzz-logpath.xml`); the primary 2026-06-11 writer can't touch root.
- journald capped at 500M (`journald.conf.d/00-cap.conf`).
- rsyslog drops loki + clickhouse-server unit output from syslog (`10-suppress-noisy-units.conf`, applied 2026-07-03; forensics showed it was NEVER live on r1, only codified in ansible role 15-log-discipline.yml, which does not auto-run against r1; the 2026-06-11 postmortem recorded codified-as-applied).
- Margin item closed 2026-09-29: the root swap file (`/swap_f1209`) was resized 16G -> 4G (a separate 4G md0 swap partition remains), returning 12G of the 49G root; root usage fell 70% -> 40%.

## stellarindex_node_root_disk_full

P1 page. The host's root filesystem is < 10 % free. Cascading failures can follow within **minutes**, not hours: Redis BGSAVE blocks (every cache write returns MISCONF) -> `/v1/price` 404s on every rewritten/triangulated/stablecoin-proxy pair; **Postgres crashes** (its log lives on `/var/log/postgresql`, root FS, and `postgresql@15-main` will not restart until root is freed, 2026-06-11); postgres WAL stalls; systemd-journald corrupts.

Symptoms:

- `(node_filesystem_avail_bytes{mountpoint="/"} / node_filesystem_size_bytes{mountpoint="/"}) * 100 < 10` for >= 1 min.
- Root free-space graph is a straight line to zero; `stellarindex_node_root_disk_filling_fast` usually fires first.
- Customer-side: `/v1/price` 404s on rewritten pairs; aggregator log shows repeating `WARN` lines about Redis Set MISCONF errors. Companion P1s: `stellarindex_redis_writes_blocked`, `stellarindex_aggregator_cache_write_errors`.
- Synthetic: `cmd/stellarindex-sla-probe` (15-min timer, `configs/healthchecks/stellarindex-sla-probe.timer`) `/v1/price` sample fails -> `stellarindex_sla_probe_*` alerts. (Whether the public status page surfaces this is not verifiable from the repo.)

Quick diagnosis (<= 5 min):

```sh
# What's filling the disk? -x = stay on the root FS.
# /var/lib/{clickhouse,postgresql,galexie,minio,loki,prometheus,pgbackrest,stellarindex}
# are ZFS datasets on the multi-TB `data` pool — never du them without -x.
df -h /
sudo du -xsh /var/log/* /tmp /var/cache 2>/dev/null | sort -rh | head -15

# The two biggest known root consumers that du above will NOT explain:
ls -lh /swap_f1209; swapon --show                    # 4 G swap file on the 49 G root (16 G until 2026-09-29)
zfs list -o name,mountpoint,mounted data/prometheus data/loki data/clickhouse data/postgres
# an UNMOUNTED dataset silently lands that data (e.g. ~13 G prometheus TSDB) back on root

# Is it the journal?
journalctl --disk-usage

# Is it logs that haven't rotated?
ls -lh /var/log/syslog* /var/log/postgresql/*.log 2>/dev/null

# Who is writing? (names the flooding unit)
journalctl --since '-5min' --no-pager | awk '{print $5}' | sort | uniq -c | sort -rn | head -5
```

If the flooder is `clickhouse-server` (2026-06-11 signature: `Cannot log message in OwnAsyncSplitChannel`), freeing space does NOT unwedge it; `sudo systemctl restart clickhouse-server` first (see the filling_fast section).

Key signals:

- **Multi-GB syslog**: one of: stellarindex-* API access log duplicated into syslog (~2.5 GB/day; guard = `/etc/rsyslog.d/30-stellarindex-journald-only.conf`, 2026-08-05); loki / clickhouse-server flood (guard = `/etc/rsyslog.d/10-suppress-noisy-units.conf`); the `/etc/logrotate.d/rsyslog` override silently skipped because it lacks `su root adm` ("insecure permissions"). Note logrotate.timer is daily, so `maxsize` cannot cap intra-day growth.
- **3 GB+ journal**: `SystemMaxUse=500M` cap missing (`/etc/systemd/journald.conf.d/00-cap.conf`).
- **`/var/log/stellarindex/*.log`**: operator one-shot job logs (logrotate 500M/weekly via `/etc/logrotate.d/stellarindex`); ad-hoc walk outputs land in `/tmp`. Heavy jobs should run under `/usr/local/sbin/run-heavy-job.sh`, which has a root-disk watchdog.
- **postgres logs**: the repo template (`postgresql.conf.j2`) still sets `log_min_duration_statement=1000`, `log_connections=on`, `log_disconnections=on`, while r1 was hand-set to `-1`/`none`/`off` on 2026-06-11. Check the live value: `sudo -u postgres psql -Atc "show log_min_duration_statement; show log_statement; show log_connections;"`; an ansible apply reverts it.

Mitigation (<= 15 min):

```sh
# 1. Free immediate space (vacuum the journal first — fast win)
sudo journalctl --vacuum-size=200M

# 2. Truncate any rotated-but-uncompressed syslog
sudo truncate -s 0 /var/log/syslog.1
sudo rm -f /var/log/syslog.[2-9]*

# 3. Postgres log — ONLY if PG is down or the file is > 1 GB
sudo truncate -s 0 /var/log/postgresql/postgresql-15-main.log

# 4. Confirm Redis can BGSAVE again (Debian redis-server, 127.0.0.1:6379, no auth)
redis-cli BGSAVE
# Wait ~5 s then:
redis-cli INFO persistence | grep rdb_last_bgsave_status
# expect: rdb_last_bgsave_status:ok

# 5. Did Postgres / ClickHouse survive?
systemctl is-active postgresql@15-main clickhouse-server redis-server stellarindex-api stellarindex-aggregator stellarindex-indexer
sudo systemctl start postgresql@15-main        # if it crashed on the full root
journalctl -u stellarindex-indexer --since -5min | grep -c 'pool may be wedged'   # indexer PG pool
```

- [ ] Step 1: execute the recovery sequence above to drop usage below 80 %.
- [ ] Step 2: confirm the customer-visible recovery: `curl http://localhost:3000/v1/price?asset=native&quote=fiat:USD` returns 200 with `flags.stale=false`.
- [ ] Step 3: find which guard regressed: `curl -s 'http://localhost:9090/api/v1/query?query=stellarindex_config_assertion_ok' | jq '.data.result[] | {a:.metric.assertion, v:.value[1]}'` (hourly `config-assertions.timer`; assertions `rsyslog_ch_suppress`, `rsyslog_loki_suppress`, `journald_cap`, `ch_logs_on_zfs`, `syslog_maxsize`). Then re-apply ONLY the relevant tags: `ansible-playbook ... --tags logrotate,journald,rsyslog --check --diff` before a real run. Do NOT apply all of `15-log-discipline.yml` mid-incident; its handlers restart `clickhouse-server` and `redis`. Ansible does not auto-run against r1; 2026-06-11 rules were codified-but-never-applied until 2026-07-03.
- [ ] Step 4: update the status page if customer-visible time exceeded 5 min (per SEV playbook).
- [ ] Verification: `node_filesystem_avail_bytes{mountpoint="/"} / node_filesystem_size_bytes{mountpoint="/"} > 0.30` (30% free); `stellarindex_node_root_disk_filling_fast` not firing; `stellarindex_config_assertion_ok == 1` for the five assertions above.

Root cause (for postmortem):

- The full output of `sudo du -xsh /var/log/* /tmp /var/cache` and `swapon --show` at the moment the alert fired.
- The flooding unit from the `journalctl | awk` histogram.
- The state of `/etc/logrotate.d/rsyslog` (incl. `su root adm` + `maxsize`), `/etc/systemd/journald.conf.d/00-cap.conf`, `/etc/rsyslog.d/10-suppress-noisy-units.conf`, `/etc/rsyslog.d/30-stellarindex-journald-only.conf`, `/etc/clickhouse-server/config.d/zzz-logpath.xml`, and the `stellarindex_config_assertion_ok` series over the preceding day.
- Live PG logging settings vs the repo template.
- The aggregator log around the moment Redis stopped accepting writes.

### If `pg_wal` is what filled it (2026-09-16)

Root carries Postgres' WAL, and the path hides it: `pg_wal` in the data directory is a **symlink** out to `/pgwal/…` on root, while the data directory itself sits on the multi-terabyte pool. Reading the data directory's free space and concluding WAL has room is the mistake that caused this, and the volume has to be measured through the link:

```bash
readlink -f /var/lib/postgresql/15/main/pg_wal      # -> /pgwal/15-main/pg_wal
df -h "$(readlink -f /var/lib/postgresql/15/main/pg_wal)"
du -sh "$(readlink -f /var/lib/postgresql/15/main/pg_wal)"
grep max_wal_size /etc/postgresql/15/main/postgresql.conf
```

Lowering `max_wal_size` is a `SIGHUP` (`systemctl reload postgresql@15-main`) and Postgres trims the directory over the next checkpoints; on 2026-09-16 that returned 7.7 GB within minutes of the restart. Fix it in ansible too, or the next apply pushes the value straight back; `05-postgres.yml` now refuses an apply whose `max_wal_size` does not fit the volume the symlink resolves to.

**Never delete anything inside `pg_wal` by hand.** Postgres owns that directory and removing a segment it still needs is unrecoverable. Check the archive backlog instead; zero `.ready` files means every segment reached the repository and nothing is waiting:

```bash
ls "$(readlink -f /var/lib/postgresql/15/main/pg_wal)"/archive_status | grep -c ready
```

**If Postgres has already failed**, `could not write to file "pg_wal/xlogtemp.N": No space left on device` during startup is this failure: it crashed, then could not complete crash recovery because recovery creates new segments. Free space first, then start it and watch recovery finish, then clear the units that failed behind it with `systemctl reset-failed`. Full account in [the 2026-09-16 post-mortem](../postmortems/2026-09-16-r1-pg-wal-fills-root.md).

False positives: none known. Headroom can be under 5 min in a log-flood (3.8 GB/min on 2026-06-11), and the 49 G root carries a 4 G swap file (`/swap_f1209`, resized from 16 G on 2026-09-29). Fire = act immediately.

## stellarindex_node_root_disk_warning

P2 ticket. The host's 49 G root filesystem is < 20 % free. No customer impact yet, but the **P1** `stellarindex_node_root_disk_full` fires at < 10 % (for 1 m), and its cascade (Redis MISCONF stop-writes -> `/v1/price` 404s, incident 2026-05-10) is why root matters. Headroom is NOT predictable from this alert alone: the 2026-06-11 ClickHouse log-wedge filled root at ~3.8 GB/min (healthy -> full in ~5 min). The **P1** `stellarindex_node_root_disk_filling_fast` may fire before or alongside this warning; if it does, follow its section first.

Symptoms:

- `(node_filesystem_avail_bytes{mountpoint="/"} / node_filesystem_size_bytes{mountpoint="/"}) * 100 < 20` for >= 10 min.
- Trend in the Prometheus graph UI (`http://localhost:9090/graph` on r1, via SSH tunnel) shows a steady downward slope over recent days. There is no Grafana server on r1 (only the Grafana APT repo, used to install promtail).

Measure the real headroom instead of guessing:

```sh
curl -s 'http://localhost:9090/api/v1/query' \
  --data-urlencode 'query=node_filesystem_avail_bytes{mountpoint="/"}/node_filesystem_size_bytes{mountpoint="/"}'
curl -s 'http://localhost:9090/api/v1/query' \
  --data-urlencode 'query=predict_linear(node_filesystem_avail_bytes{mountpoint="/"}[1h], 3600*12)'   # projected avail in 12 h
```

Quick diagnosis (<= 5 min): same as the `node_root_disk_full` quick diagnosis above (what's filling the disk?), plus:

```sh
df -h / && df -x zfs -h            # root is the ~49G device; everything else should be ZFS
sudo du -xsh /var/log/* /tmp /var/tmp /var/cache/* /var/lib/* 2>/dev/null | sort -rh | head -20   # -x stays on the root FS
journalctl --disk-usage            # expect ≤ 500M (SystemMaxUse cap)
findmnt -n -o TARGET,FSTYPE /var/lib/postgresql /var/lib/clickhouse /var/lib/loki /var/lib/prometheus /var/lib/minio /var/lib/galexie /var/lib/pgbackrest /var/lib/stellarindex
```

`du -x` matters: `/var/lib/*` holds eight ZFS datasets sized in TB (`configs/ansible/roles/archival-node/defaults/main.yml` `zfs_datasets`); without `-x` the scan runs for a very long time and reports non-root consumers as the top entries. Every path in the `findmnt` line must report `zfs`; an empty or `ext4` result means the dataset is not mounted and that service is writing to root.

Mitigation (<= 30 min). This is a warning, not an emergency; plan the cleanup, don't rush:

- [ ] Step 1: identify the dominant consumer per the diagnosis above.
- [ ] Step 2: apply the appropriate cleanup:
  - **Logs**: confirm the `15-log-discipline.yml` guards are in place and re-apply the archival-node role with `--tags log-discipline` if any drifted. "Current" means:
    - `/etc/logrotate.d/rsyslog`: `su root adm`, `maxsize 100M`, `rotate 7`, `delaycompress`;
    - `/etc/rsyslog.d/10-suppress-noisy-units.conf`: stop-filters for `loki` + `clickhouse-server` unit output;
    - `/etc/systemd/journald.conf.d/00-cap.conf`: `SystemMaxUse=500M`;
    - `/etc/clickhouse-server/config.d/zzz-logpath.xml`: ClickHouse logs under `/var/lib/clickhouse/logs` (ZFS), NOT `/var/log/clickhouse-server`.
  - **Known regression candidates**: ClickHouse logs reappearing under `/var/log/clickhouse-server` (2026-06-11 log-wedge loop; kill sequence in the filling_fast section), and Prometheus TSDB / Loki chunks appearing on root (`/var/lib/prometheus` and `/var/lib/loki` are ZFS datasets since 2026-06-30 / 2026-06-11; if `findmnt` shows them non-zfs, the dataset failed to mount).
  - **Galexie / MinIO**: galexie writes ledger meta through an S3 datastore (`galexie.toml.j2`, `type = "S3"`) into local MinIO at `/var/lib/minio`; `/var/lib/galexie` is the galexie user's home/report dir. Both are ZFS datasets (ADR-0016 only says "galexie-archive: local MinIO"). If either shows fstype ≠ `zfs` in `findmnt`, the dataset failed to mount: **before** re-applying `--tags zfs`, run `zpool list -H data`; if it exits non-zero, the pool is not imported (this is what caused the failed mount) and `03-zfs.yml`'s create task will try to recreate it. Run `zpool import -d /dev/disk/by-id` first and confirm `data` is not listed as importable-but-foreign before re-applying; if it is the same pool, `zpool import data` to bring it back rather than letting the role recreate it (`zpool create -f` would silently overwrite an unimported pool). Once `zpool list -H data` exits 0, stop the writer (`systemctl stop galexie` / `systemctl stop minio`), move the data aside, re-apply the archival-node role `--tags zfs` (`03-zfs.yml`) to remount, then move the data back and restart.
  - **Postgres logs**: `/var/log/postgresql/postgresql-<ver>-main.log` lives on root (`postgresql.conf.j2`: `logging_collector = on`, `log_min_duration_statement = 1000` ms). If it dominates, **raise** `log_min_duration_statement` (e.g. `5000`, or `-1` to disable) in `configs/ansible/roles/archival-node/templates/postgresql.conf.j2` and re-apply the role (editing the live file drifts back on the next apply), then `SELECT pg_reload_conf();`. The size cap is `/etc/stellarindex-pg-logrotate.conf` (`maxsize 500M`, role-managed), run hourly by `pg-logrotate.timer`; confirm it with `systemctl list-timers pg-logrotate.timer`; the stock `/etc/logrotate.d/postgresql-common` has no size cap. The Postgres data volume `/var/lib/postgresql` is its own ZFS dataset on r1, so vacuuming chunks never frees root; see `db-disk-full.md` for that volume.
- [ ] Step 3: schedule a follow-up review in 24 h to confirm the trend reversed.
- [ ] Verification: `node_filesystem_avail_bytes{mountpoint="/"} / node_filesystem_size_bytes{mountpoint="/"} > 0.40` (40 % free) sustained for 1 hour. This is an operator target, stricter than the alert's own resolution at 20 %.

Root cause: if this fires more than once a quarter, the disk-usage trend has a leak. Capture for a planning ticket: the 30-day trend of `node_filesystem_avail_bytes{mountpoint="/"}` from the Prometheus graph UI or `promtool query range`; per-directory growth rate via two `du -xsh /var/*` snapshots 7 days apart.

False positive: **one-time large captures**. Manual debug captures and one-shot operator log dumps can take 5-10 GB transiently (historical example: the 2026-05-10 `/var/log/wasm-history-*.stderr` captures, 2.2 GB). WASM-audit walks now write under `/var/log/wasm-audit/` and are deleted once recorded (`docs/operations/wasm-audits/README.md` section 2); anything left there with no walk running is a missed cleanup, and loose `/var/log/wasm-history-*` files are a finding. If the trigger is identifiable and the data is needed, leave it; otherwise clean up.

## Related

- [redis-write-blocked-disk-full](cache.md#stellarindex_redis_writes_blocked): the downstream cascade when these alerts were missed; the May-10 incident's primary remediation, and the `runbook_url` the full and warning rules currently link to.
- `db-disk-full.md`: sibling for the postgres data volume (`stellarindex_timescale_disk_full` / `_warning`; separate ZFS dataset per `configs/ansible/roles/archival-node/defaults/main.yml` `zfs_datasets`).
- `docs/operations/r1-ansible-drift-2026-07-03.md`: why hand-applied guards and the ansible role disagree.
- ADR-0008: HA topology + DR posture (single-host R1 today; fewer fail-safes than R2/R3 will have).
- 2026-05-10 and 2026-06-11 incident postmortems under `internal/incidents/data/` (paths above).
