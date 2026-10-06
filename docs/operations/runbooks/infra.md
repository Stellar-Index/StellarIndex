---
title: Runbook — infra
last_verified: 2026-10-06
status: living
severity: P1
---

# Runbook — host, disk, backup and worker alerts (infra family)

Alerts live in `configs/prometheus/rules.r1/infra.yml` and `storage.yml` (the files r1 loads from `/etc/prometheus/rules.r1/*.yml`; group `stellarindex.infra`, trend alerts in `stellarindex.infra_pool_trend`; the nvme wear/spare/media alerts and the four backup alerts are in group `stellarindex.storage`, `storage.yml`). `deploy/monitoring/rules/{infra,storage}.yml` are the multi-host twins; exprs, `for:` and severities are identical (comments differ only). Section headings are the alert names.

## At a glance

- [`stellarindex_host_down`](#stellarindex_host_down)
- [`stellarindex_host_cpu_high`](#stellarindex_host_cpu_high)
- [`stellarindex_host_memory_high`](#stellarindex_host_memory_high)
- [`stellarindex_zfs_pool_degraded`](#stellarindex_zfs_pool_degraded)
- [`stellarindex_zfs_pool_low_space`](#stellarindex_zfs_pool_low_space)
- [`stellarindex_zfs_pool_critical_space`](#stellarindex_zfs_pool_critical_space)
- [`stellarindex_zfs_pool_fill_85pct_within_30d`](#stellarindex_zfs_pool_fill_85pct_within_30d)
- [`stellarindex_zfs_pool_fill_90pct_within_7d`](#stellarindex_zfs_pool_fill_90pct_within_7d)
- [`stellarindex_nvme_smart_warn`](#stellarindex_nvme_smart_warn)
- [`stellarindex_nvme_critical_warning`](#stellarindex_nvme_critical_warning)
- [`stellarindex_nvme_wear_high`](#stellarindex_nvme_wear_high)
- [`stellarindex_nvme_spare_low`](#stellarindex_nvme_spare_low)
- [`stellarindex_nvme_media_errors`](#stellarindex_nvme_media_errors)
- [`stellarindex_nvme_thermal_throttle`](#stellarindex_nvme_thermal_throttle)
- [`stellarindex_md_array_degraded`](#stellarindex_md_array_degraded)
- [`stellarindex_filesystem_readonly`](#stellarindex_filesystem_readonly)
- [`stellarindex_systemd_unit_failed`](#stellarindex_systemd_unit_failed)
- [`stellarindex_textfile_scrape_error`](#stellarindex_textfile_scrape_error)
- [`stellarindex_keepalived_textfile_stale`](#stellarindex_keepalived_textfile_stale)
- [`stellarindex_worker_panicked`](#stellarindex_worker_panicked)
- [`stellarindex_timescale_backup_none_24h`](#stellarindex_timescale_backup_none_24h)
- [`stellarindex_timescale_backup_failed`](#stellarindex_timescale_backup_failed)
- [`stellarindex_pgbackrest_backup_metrics_absent`](#stellarindex_pgbackrest_backup_metrics_absent)
- [`stellarindex_pgbackrest_backup_unit_failed`](#stellarindex_pgbackrest_backup_unit_failed)
- [`stellarindex_wal_archive_stale`](#stellarindex_wal_archive_stale)
- [`minio-metrics-403`](#minio-metrics-403)
- [`node-root-disk`](#node-root-disk)
- [`process-mappings`](#process-mappings)

## Shared context

- r1 is a single host: Hetzner EX63 (FSN1), root `ssh root@136.243.90.96`. No replica, no HAProxy/Patroni/Sentinel/PgBouncer, no stellar-core validator (galexie's captive core is the only one). Hetzner Robot (https://robot.hetzner.com, r1 = EX63 #2982698; testnet/futurenet host = Server Auction #3057275) gives KVM console / Rescue / Reset; no customer IPMI. Robot login is held by the maintainer (`docs/operations/maintainer-workflow.md`). Hardware swaps go through a Hetzner support ticket.
- Storage: OS/boot disks are mdadm RAID1 (`md0` swap, `md1` `/`; layout in `docs/operations/r1-deployment-state.md` §Disk layout). On r1, `nvme0n1`/`nvme1n1` carry `md0`, `md1` and a ZFS member (`p4`), so one failed OS drive also degrades the `data` pool. Data is the ZFS pool `data` on 4 NVMe, **raidz1 (single parity)**: one drive failure is the whole tolerance, so DEGRADED means zero redundancy. Never promise two-failure tolerance (the role default is raidz2 but r1's inventory pins `zfs_data_pool_type: "raidz1"`; `scripts/ci/lint-docs.sh` §18 lints this).
- node_exporter is the Debian unit `prometheus-node-exporter` (job label `node_exporter`, `localhost:9100`). The old `node_exporter.service` is deliberately stopped+disabled and shows `inactive (dead)` on a healthy host; never `systemctl start node_exporter` except as rollback after `systemctl stop prometheus-node-exporter` (`configs/ansible/roles/archival-node/tasks/10-observability.yml`).
- Heavy ops one-shots must run under `/usr/local/sbin/run-heavy-job.sh` (transient scope: batch CPUWeight=25 / IOWeight=25, `MemoryMax=20G`, `MemorySwapMax=0`); galexie carries elevated weight and `MemoryLow=16G`. A `heavy-*.scope` dominating `systemd-cgtop` is expected. A heavy binary run raw (outside a scope) is the real fault: stop it and re-run under the wrapper (an unwrapped re-derive once wedged galexie's captive core for 11 h).
- Memory policy: strict overcommit (`vm.overcommit_memory=2`, `vm.overcommit_ratio=80`), 16 G swap at `vm.swappiness=1`. Failure mode is ENOMEM ("Cannot allocate memory") in the next allocator, before the OOM-killer.
- Textfile-collector metrics (`nvme_*`, `keepalived.prom`, `pgbackrest_backup.prom`, ...) are produced by timers/scripts; `scripts/ci/textfile-producers.manifest` lists each file's producer; the textfile dir is `/var/lib/node_exporter/textfile_collector/`.
- `systemctl --failed` empty and `zpool status -x` clean are the generic all-clear after any of these.

## stellarindex_host_down

- Trips: `up{job="node_exporter"} == 0 OR absent_over_time(up{job="node_exporter"}[5m]) == 1`, `for: 2m`, `severity: ticket`.
- Meaning: on r1 Prometheus and Alertmanager run on the box (`configs/prometheus/prometheus.r1.yml` → `localhost:9093`), so this can only fire while the host is alive. Being paged by it means the exporter or the scrape path is broken, not the box. `stellarindex_prometheus_scrape_failing` (`rules.r1/meta.yml`, `meta.md#stellarindex_prometheus_scrape_failing`) fires alongside; expected.
- A genuine r1 outage surfaces only through out-of-band Healthchecks.io checks going red: `stellarindex-heartbeat@{indexer,aggregator,api}.timer` (60 s, `configs/healthchecks/stellarindex-heartbeat@.timer`), `stellarindex_deadmansswitch` heartbeat (Alertmanager → Healthchecks.io every 60 s, `meta.md#stellarindex_deadmansswitch`), `node-healthcheck.timer` (5 min, `tasks/13-healthcheck.yml`), `stellarindex-smoke.timer` (5 min), `stellarindex-sla-probe.timer` (15 min). Each is silently disabled if its URL in `/etc/default/{node-healthcheck,stellarindex-healthchecks}` is empty; if none fired in a real outage, check that afterwards.

Diagnose:

```sh
ping -c 3 <r1-ip>                         # IP in configs/ansible/inventory/r1.yml (gitignored)
ssh -o ConnectTimeout=10 root@r1 uptime   # fail2ban guards sshd (tasks/12-hardening.yml): don't hammer
systemctl status prometheus-node-exporter
journalctl -u prometheus-node-exporter -n 50
curl -s localhost:9100/metrics | head -1
ss -ltnp | grep :9100                     # expect exactly ONE listener
```

Causes: exporter died/OOM-killed; port 9100 collision with the legacy `node_exporter.service`; reboot (`uptime` < 5 min); network isolation (from Robot KVM, ping the gateway); hardware (PSU/NVMe/board/DIMM; then check `## stellarindex_nvme_smart_warn`, `## stellarindex_zfs_pool_degraded`); kernel panic/hang (Robot Reset is the only way back).

Fix:
1. Exporter only: `systemctl restart prometheus-node-exporter`, `curl -s localhost:9100/metrics | head -1`, wait one 15 s scrape for `up` = 1.
2. Host down: r1 has no failover; API, indexer, aggregator, galexie, ClickHouse, Postgres, Redis, MinIO, Prometheus and Alertmanager are all down and alerting is dark. Treat as SEV-1 manually: post in Discord #stellarindex-pages, update the status page (`sev-status-page-update.md`), Robot Reset → KVM. If nftables locked you out: `bootstrap-archival-node.md` §"Firewall locked us out".
3. After return: `zpool status -x` shows `data` ONLINE (else zfs-degraded section) and `zfs list -o name,mountpoint,mounted` shows every `data/*` mounted; `systemctl --failed` empty; `systemctl is-active clickhouse-server postgresql@15-main redis-server galexie minio caddy cap67-movements stellarindex-indexer stellarindex-aggregator stellarindex-api prometheus-node-exporter prometheus` and `ss -ltnp | grep :9093` (Alertmanager). Caddy (:80/:443 → loopback API :3000, `tasks/19-caddy.yml`) returns with the host.
4. Then service runbooks: `api.md#stellarindex_api_down`, `ingestion.md#stellarindex_ingestion_all_sources_stopped`, `archive.md#stellarindex_galexie_catchup_refused`, `clickhouse.md#stellarindex_ingestion_ch_live_sink_drops`. galexie needs ~9 min cold captive-core catchup before the indexer resumes: do not restart it repeatedly; `ingestion.md#stellarindex_ingestion_lag_high` / `ingestion.md#stellarindex_ingestion_source_stopped` clear on their own.
5. Textfile-driven alerts (`sla-probe.md#stellarindex_sla_probe_stale`, `supply.md#stellarindex_supply_snapshot_stale`, `archive.md#stellarindex_archive_completeness_stale`, `binary-version-skew.md`, `stellar-node.md#stellar-stack-version-lag`) may fire until the writing timers' first post-boot run (`OnBootSec`: heartbeat 30 s, smoke 2 min, node-healthcheck 2 min, sla-probe 3 min; daily `OnCalendar` ones at their time). Don't chase them for ~15 min.
6. Verify: `up{job="node_exporter"}` = 1, scrape_failing clears, deadmansswitch resumed, Healthchecks.io green.

RCA: Robot KVM / status page and Hetzner network status for FSN1; `journalctl -b -1`; `smartctl` / `nvme smart-log` on all four NVMes; `dmesg | grep -i oom`.

False positives: node_exporter OOM-killed on a starved box (real problem is memory, see memory section); Prometheus itself sick: if `up == 0` for node_exporter AND the other localhost jobs (`stellarindex-api`/`-indexer`/`-aggregator`, `caddy`, `galexie`) simultaneously, suspect Prometheus (restart, `/` full `infra.md#stellarindex_node_root_disk_full`, `prometheus-tsdb-corruption.md`); legacy unit looking dead is correct.

## stellarindex_host_cpu_high

- Trips: `100 - (avg by (instance) (rate(node_cpu_seconds_total{mode="idle"}[5m])) * 100) > 90`, `for: 10m`, `severity: informational`. Per-instance average over all cores: a single pegged core on a many-core host does not trip it (use a `max by (cpu)` variant for that).
- Meaning: not customer-visible yet; usually precedes `api.md#stellarindex_api_latency_p95_high`. Burst crons (hourly aggregator rollup) are absorbed by `for:`.

```sh
ssh root@136.243.90.96 'top -b -n1 -o %CPU | head -20'
ssh root@136.243.90.96 'systemd-cgtop --order=cpu --iterations=2'
ssh root@136.243.90.96 'mpstat 1 5'     # user vs system vs iowait vs softirq; %steal is structurally 0 on this dedicated box
```

Causes: one pegged process (hot path, unbounded goroutines, bad SQL plan); galexie captive-core catchup (CPU-bound; resolves in 30–120 min; it has no `/info` endpoint, so end-state is fresh objects in `galexie-live`; `stellar-node.md#stellarindex_stellar_core_ledger_age` / `stellar-node.md#stellarindex_stellar_rpc_lag` are inert on r1); Postgres plan regression (`pg_stat_statements` high `mean_exec_time`); pgBackRest `--process-max=4` or Timescale compression (CPU-heavy on purpose); an unwrapped heavy one-shot (shared context). `pg_repack` and `pg_hint_plan` are not installed.

Fix: identify consumer; legitimate load → scale up, bug → incident; catchup → wait; plan regression → `runuser -u postgres -- psql -d stellarindex -c 'ANALYZE <table>'`, then rewrite the query; backup/compression running for hours → lower `--process-max`. Verify CPU < 70 % sustained and alert clears. Related: `postgres.md#stellarindex_timescale_connections_saturated`, `ingestion.md#stellarindex_ingestion_all_sources_stopped`.

## stellarindex_host_memory_high

- Trips: `(node_memory_MemTotal_bytes - node_memory_MemAvailable_bytes) / node_memory_MemTotal_bytes * 100 > 90`, `for: 10m`, `severity: informational`.
- Meaning: allocation-failure risk (strict overcommit, see context). Any sustained swap in/out (`vmstat 1` si/so) is itself a red flag at swappiness 1; `archive.md#stellarindex_host_swap_activity` covers that half (`rate(node_vmstat_pswpout[10m]) > 100`). Follow-ons: host-cpu-high, then service alerts.

```sh
ssh root@136.243.90.96 'ps auxww --sort=-%mem | head -10'
ssh root@136.243.90.96 'systemd-cgtop --order=memory --iterations=2'
ssh root@136.243.90.96 'free -h; cat /proc/meminfo | head -30'
ssh root@136.243.90.96 'journalctl --since -2h | grep -i "cannot allocate memory" | tail'
ssh root@136.243.90.96 'dmesg -T | grep -i "out of memory\|killed process" | tail'
ssh root@136.243.90.96 'systemctl status "heavy-*.scope" --no-pager'   # fenced job consumer, or killed at its 20G wall
```

Causes and fixes:
1. Postgres `work_mem` × `max_connections` (many backends at hundreds of MB): lower `work_mem` / cap `max_connections` via the archival-node role's postgres vars, re-apply, restart `postgresql@15-main`. Single primary, no replica/PgBouncer: a brief served-tier outage, coordinate per the SEV playbook.
2. Go heap growth (RSS climbs monotonically): pprof heap dump, then `systemctl restart stellarindex-<binary>`. Indexer resumes from its persisted cursor (no data loss); API restart is a blip behind Caddy. File an incident.
3. Page cache: large `buff/cache`, low `available`; benign (reclaimable) unless apps get ENOMEM.
4. ZFS ARC: capped at 32 GiB (floor 8 GiB) via `/etc/modprobe.d/zfs.conf` (role vars `zfs_arc_max_bytes` / `zfs_arc_min_bytes`), read only at module load: a new cap needs a reboot or a write to `/sys/module/zfs/parameters/zfs_arc_max`. Check `arcstat` / `/proc/spl/kstat/zfs/arcstats`; change the cap in the role, not by hand.
5. Genuine undersize: scale up or move workloads.

Verify `available` > 20 % and no new ENOMEM/OOM lines for 1 h. False positives: ARC counted as used on older kernels (6.x fixed); a freshly started process warming caches. Related: `postgres.md#stellarindex_timescale_primary_down` (Postgres OOM-kill path).

## stellarindex_zfs_pool_degraded

- Trips: `node_zfs_zpool_state{state=~"degraded|faulted|unavail|suspended"} > 0`, `for: 60s`, `severity: page` (SEV-1). Lowercase states, pool in the `zpool` label.
- Meaning: raidz1 with a faulted/offline drive has ZERO redundancy; a second failure or an unrecoverable read error during resilver loses the pool. Reads/writes still serve, which is not margin. Often follows `nvme_smart_warn`.

```sh
ssh root@136.243.90.96 'zpool status -v data'
ssh root@136.243.90.96 'ls -l /dev/disk/by-id/ | grep nvme1n1'
ssh root@136.243.90.96 'nvme list'          # serial -> slot for the Hetzner ticket
ssh root@136.243.90.96 'zpool status data | grep -A5 resilver'
```

Causes: drive hardware failure; controller/slot/cable dropping a good drive; power event.

Fix:
1. Escalate to SEV-1 now; everything after is a race against the resilver.
2. Quiesce heavy writers: `systemctl stop pgbackrest-backup.timer` for the window, no scrubs, empty `run-heavy-job.sh` queue.
3. Open a Hetzner ticket with the drive serial for the swap; then `zpool replace data <old-drive-id> <new-drive-id>`; watch ETA in `zpool status`; stay quiesced until done.
4. Second drive fails during resilver: pool lost; no failover exists. DR path is the triage tree in `docs/operations/archival-node-bringup.md` (rebuild box, restore Postgres from pgBackRest, re-mirror galexie data from SDF / `aws-public-blockchain` per `docs/adr/0016-per-region-storage-strategy.md`).
5. Verify: `zpool status data` ONLINE, no errors; `node_zfs_zpool_state{zpool="data",state="online"} == 1`.

RCA: keep `smartctl` logs of the failed drive (warranty); install date (`docs/operations/r1-deployment-state.md`, Hetzner order records); did nvme_smart_warn / nvme_thermal_throttle fire earlier; SMART warnings on the other drives. False positive: planned replacement (DEGRADED → resilvering → HEALTHY); silence during the window. Related: zfs-pool-full sections (capacity side of the same pool), `postgres.md#stellarindex_timescale_disk_full`.

## stellarindex_zfs_pool_low_space

- Trips (`for: 15m`, `severity: ticket`): pool free fraction `< 0.07`, with
  ```
  min by (instance) (node_filesystem_avail_bytes{fstype="zfs"})
    /
  (
    sum by (instance) (
      node_filesystem_size_bytes{fstype="zfs"}
        - node_filesystem_avail_bytes{fstype="zfs"}
    )
    + min by (instance) (node_filesystem_avail_bytes{fstype="zfs"})
  ) < 0.07
  ```
  Same expression with `< 0.035` and `for: 5m`, `severity: page` is `stellarindex_zfs_pool_critical_space`. The ratios equal ~1.3 TB / ~650 GB on r1's ~16.8 TiB pool.
- Why this proxy: node_exporter's ZFS collector exports no zpool size/alloc/free, only `node_zfs_zpool_state` and dataset IO counters. Every dataset reports `size = its used + shared pool avail`, so pool_free = `min(avail)`, pool_used = `sum(size - avail)`. A per-dataset `avail/size` ratio is NOT equivalent (fails open when data is spread over several datasets). Single-pool assumption: a host with two zpools needs a `device=~`/`mountpoint=~` selector.
- Meaning: ZFS is copy-on-write, degrades sharply past ~90 % full and can stall writes: ClickHouse ingest, MinIO archive and Postgres all halt (ENOSPC, rising write latency). The lake grows forever by design (no TTL), so a sustained breach is a capacity-planning trigger (tiering / R2), not just an incident.

```sh
ssh root@r1 'zpool list -o name,size,alloc,free,cap,health data'
ssh root@r1 'zfs list -o name,used,avail -s used | tail -20'
ssh root@r1 'zfs list -t snapshot -o name,used -s used | tail -20'
```

cap >= 90 % confirms. Top consumers: ClickHouse lake, MinIO (galexie LCM), pgBackRest repo1 (on-pool, ~2.4 TB: a DR anti-pattern and the biggest single reclaim if moved off-box), Postgres.

Fix (prefer reversible): stale snapshots (`zfs list -t snapshot`; check `zfs holds <snap>`, dry-run `zfs destroy -nv <snap>`, then destroy); move pgBackRest off-box; confirm the Timescale compression job (hypertable job 1034) is running, force-compress recent chunks if lagging. Verify `node_filesystem_avail_bytes{fstype="zfs"}` is back above threshold (clears ~1 min after next scrape). RCA: capture `zpool list`, `zfs list -o space`, 30-day `node_filesystem_avail_bytes` trend. False positive: a large re-derive/backfill transiently dips free space (merges compact it back); the `for:` windows absorb brief dips, sustained is real.

## stellarindex_zfs_pool_critical_space

Same expr as `stellarindex_zfs_pool_low_space` with threshold `< 0.035`, `for: 5m`, `severity: page`. Reclaim NOW: diagnosis, fixes and verification are in that section; stalled writes are imminent.

## stellarindex_zfs_pool_fill_85pct_within_30d

- Trips (`for: 1h`, `severity: ticket`, group `stellarindex.infra_pool_trend`):
  ```
  predict_linear(
    (min by (instance) (min_over_time(node_filesystem_avail_bytes{fstype="zfs"}[1d])))[14d:1h],
    30 * 86400
  )
    /
  (
    sum by (instance) (
      node_filesystem_size_bytes{fstype="zfs"}
        - node_filesystem_avail_bytes{fstype="zfs"}
    )
    + min by (instance) (node_filesystem_avail_bytes{fstype="zfs"})
  ) < 0.15
  ```
- Meaning: a linear fit over 14 days of daily free-space troughs projects under 15 % free within 30 days (or troughs are already below it). Plan reclamation; nothing is stalling yet. Capacity proxy as in low_space.
- Fix: as in `## stellarindex_zfs_pool_low_space`. A large one-off write steepens the fit until it ages out; if the pool has compacted back the projection recovers as new daily troughs arrive; silence the trend alert rather than retune it.

## stellarindex_zfs_pool_fill_90pct_within_7d

Same expr as `stellarindex_zfs_pool_fill_85pct_within_30d` with horizon `7 * 86400` and threshold `< 0.10`, `for: 30m`, `severity: page`. Find what is filling the pool today (`zfs list -o name,used -s used`, compare with yesterday) and reclaim before the floors fire; fixes in `## stellarindex_zfs_pool_low_space`. Same trend false-positive note as above.

## stellarindex_nvme_smart_warn

NVMe health family: five alerts, one decision (replace a drive, and how soon). `nvme_*` series come from the packaged nvme collector, written to the scraped textfile dir (`/var/lib/node_exporter/textfile_collector`) by the archival-node role's systemd drop-in (`10-observability.yml`). If `nvme_percentage_used_ratio`, `nvme_available_spare_ratio` or `nvme_media_errors_total` go missing, check that redirect first (a wrong dir makes the series generated every 5 min and discarded, so no alert can fire).

| Alert | Meaning | Urgency |
| --- | --- | --- |
| `stellarindex_nvme_critical_warning` | controller SMART critical_warning bitmask nonzero | P1 |
| `stellarindex_nvme_smart_warn` | controller logged new errors | P2 |
| `stellarindex_nvme_wear_high` | > 80 % rated write endurance used | P3, procurement trigger |
| `stellarindex_nvme_spare_low` | < 20 % reserve blocks | P1, moves shortly before failure |
| `stellarindex_nvme_media_errors` | new uncorrected media errors in 24 h | P2 |

Read wear_high and spare_low together: `percentage_used` is a predicted figure and can sit > 80 % for a long time; falling `available_spare` is the drive running out of blocks to remap. Wear high + spare full = planning problem; spare low = incident. Because `data` is raidz1, one failing drive already means zero redundancy and r1's NVMe is not replaceable on demand, so these alerts buy lead time: start the procurement conversation at wear_high.

- Trips: `increase(nvme_num_err_log_entries_total[1h]) > 0`, `for: 5m`, `severity: ticket`. The controller's error-information log also records command timeouts and transport errors, so it moves before `nvme_media_errors_total`.
- Symptoms: dmesg `blk_update_request: I/O error` or NVMe equivalent; SMART may not yet flag failure.

Diagnose (shared by all NVMe alerts):

```sh
ssh <host> 'dmesg -T | grep -iE "i/o error|nvme|media error" | tail'
ssh <host> 'smartctl -a /dev/nvme0n1'    # Media/Data Integrity Errors, Power On Hours, Percentage Used, Critical Warning
ssh <host> 'zpool status -v'             # checksum errors on this drive's pool?
```

Causes: end-of-life wear (Percentage Used > 80 %, replace on schedule); one drive flaking while siblings are fine (drive, occasionally backplane/cable/slot); firmware bug (check vendor advisory); spurious kernel event (SMART clean and ZFS checksum errors zero: upgrade kernel when feasible, low priority).

Fix: read SMART + ZFS status; if ZFS already shows checksum errors/scrub/resilver it is escalating, follow `## stellarindex_zfs_pool_degraded`; schedule replacement (`zpool offline`, swap, `zpool replace`, wait for resilver); a clean reboot BEFORE the swap is preferable when traffic can be drained (stop the relevant `stellarindex-*` units first); resilver is online-safe, but stressed drives sometimes fail harder during it. Verify: new drive resilvered, `zpool status` ONLINE, no new IO errors for 24 h. False positives: a single transient IO error at boot (no recurrence within 1 h: close, keep on a watch list); scrub-detected-and-repaired sectors (informational unless growing).

## stellarindex_nvme_critical_warning

`nvme_critical_warning > 0`, `for: 5m`, `severity: page`. nvme-cli's `critical_warning` verbatim: the controller's composite health flag (temperature threshold, spare capacity, reliability degraded, media read-only, backup device failure). Strongest signal in this family since it comes straight from the drive. `smartctl -a /dev/nvmeXn1` decodes which bit(s) are set; diagnosis and fix as in `## stellarindex_nvme_smart_warn`.

## stellarindex_nvme_wear_high

`nvme_percentage_used_ratio > 0.80`, `for: 1h`, `severity: ticket` (`rules.r1/storage.yml`). Rated endurance consumed on `{{ $labels.device }}`. Not urgent at 80 % by design: replacing a drive here is a planned operation, a procurement trigger, not an incident. Read with spare_low; diagnosis and fix as in `## stellarindex_nvme_smart_warn`.

## stellarindex_nvme_spare_low

`nvme_available_spare_ratio < 0.20`, `for: 30m`, `severity: page` (`rules.r1/storage.yml`). The drive is consuming its reserve block pool: late-stage wear that moves shortly before failure. Treat as an incident; diagnosis and fix as in `## stellarindex_nvme_smart_warn`, start replacement (`## stellarindex_zfs_pool_degraded` procedure when it fails).

## stellarindex_nvme_media_errors

`increase(nvme_media_errors_total[24h]) > 0`, `for: 5m`, `severity: ticket` (`rules.r1/storage.yml`). Uncorrected media errors on a ZFS member. ZFS surfaces corruption at scrub but the drive reports first. Diagnosis and fix as in `## stellarindex_nvme_smart_warn`.

## stellarindex_nvme_thermal_throttle

- Trips: `nvme_temperature_celsius > 70`, `for: 5m`, `severity: page`. Per-device series, so the alert names the drive. Fired-state coverage: `deploy/monitoring/rule-tests/infra_test.yml`.
- Meaning: over the thermal limit NVMe IO throttles to ~50 % of rated: Postgres WAL flush, captive-core catchup and aggregator writes slow, `api.md#stellarindex_api_latency_p95_high` may follow; sustained heat also accelerates wear (see nvme_wear_high / spare_low / media_errors, same textfile source, longer lead time).

```sh
ssh root@136.243.90.96 'for d in /dev/nvme?n1; do echo -n "$d: "; smartctl -A "$d" | grep -i temperature; done'
ssh root@136.243.90.96 'sensors | grep -E "Composite|fan"'         # all drives (chassis) or one (drive)?
```

Causes: chassis airflow (clogged filter, failed fan; several drives at once); ambient DC temperature (all drives climb together); single drive (loose heatsink, poor slot); heavy work (scrub, resilver, backup, compaction; expected, throttle does its job).

Fix: all vs one drive; reduce load (pause pgBackRest, cancel scrubs); Hetzner support ticket for remote hands (fans/filters); ambient cause: escalate to Hetzner, nothing to do at 3 AM. Verify < 65 °C sustained 15 min and write throughput at baseline. False positives: brief spikes in scrub/resilver; sensor glitch (one drive 85 °C beside 45 °C neighbours; cross-check `smartctl -x`). Related: `postgres.md#stellarindex_timescale_disk_full`, `timescale.md#stellarindex_timescale_compression_lag`.

## stellarindex_md_array_degraded

- Trips: `node_md_disks{state="failed"} > 0` (per `device`, `md0`/`md1`), `for: 5m`, `severity: page`.
- Meaning: an OS-disk RAID1 member failed; one failure is tolerated, redundancy is lost now, no data loss yet. A second failure before replacement loses the array (for `md1`, the host). Kernel: `md/raid1:mdX: Disk failure on <dev>, disabling device.`; `/proc/mdstat` shows `[_U]`/`[U_]` or `(F)`.

```sh
ssh <host> 'cat /proc/mdstat'
ssh <host> 'mdadm --detail /dev/md0 /dev/md1'
ssh <host> 'dmesg -T | grep -iE "md/raid1|ata.*error|nvme.*error" | tail -30'
```

Fix: identify the failed member from `mdadm --detail`; if the drive is failing outright, schedule replacement (a failing NVMe often also trips `## stellarindex_nvme_smart_warn`); after replacement (GPT layout): copy the partition table from the surviving partner (`sgdisk -R=/dev/<new> /dev/<surviving>`, then `sgdisk -G /dev/<new>`), `mdadm --manage /dev/md0 --add /dev/<new>p2` and `/dev/md1 --add /dev/<new>p3`, and `zpool replace data <old-p4> /dev/<new>p4` for the ZFS member; confirm `cat /proc/mdstat` shows a `resync` percentage. Verify `mdadm --detail /dev/mdX` reports `State : clean` with both members `active sync`. A transient hot-plug drop can re-add itself: confirm with `mdadm --detail` but do not wait out the page on that alone. Layout: `docs/operations/r1-deployment-state.md` §Disk layout. Same redundancy-loss class as `## stellarindex_zfs_pool_degraded` on the data pool.

## stellarindex_filesystem_readonly

- Trips: `node_filesystem_readonly{fstype!~"tmpfs|squashfs|overlay|nsfs|ramfs"} == 1`, `for: 5m`, `severity: page`.
- Meaning: the kernel remounted a filesystem read-only, almost always on an unrecoverable block I/O error. Writes fail outright while free space still reports normally, so the disk-full alerts (`stellarindex_node_root_disk_full`, `stellarindex_timescale_disk_full`) stay silent. DB volume: Postgres write errors, `stellarindex_timescale_primary_down` follows; root: logs and `/tmp` fail. Kernel log: `EXT4-fs error ... remounting filesystem read-only` (or XFS/ZFS equivalent).

```sh
ssh <host> 'mount | grep " ro,"'
ssh <host> 'dmesg -T | grep -iE "remount-ro|EXT4-fs error|XFS.*Corruption|I/O error" | tail -30'
ssh <host> 'smartctl -a /dev/<underlying-device>'
```

Causes: unrecoverable disk I/O error (pairs with `## stellarindex_nvme_smart_warn` / `## stellarindex_md_array_degraded`); detected metadata corruption (needs `fsck` before rw); ZFS pool fault presenting as a read-only dataset (`## stellarindex_zfs_pool_degraded`).

Fix: first confirm the device is not failing (if it is, replace hardware first: remounting rw onto a failing disk repeats the event); if sound, `fsck` offline/unmounted then remount read-write; DB volume: confirm Postgres survived or needs a restart/crash-recovery pass (`postgres.md#stellarindex_timescale_disk_full` WAL considerations). Verify `mount` no longer shows `ro`, a test write succeeds, alert clears within 5 min of `node_filesystem_readonly` = 0. False positive: a legitimately read-only mount outside the fstype exclusion (CD image, deliberate `ro` snapshot): add the `mountpoint` to the exclusion rather than treating each as an incident.

## stellarindex_systemd_unit_failed

- Trips: `max_over_time(node_systemd_unit_state{state="failed",name!~"verify-archive-tier-a\\.service|verify-archive-tier-b\\.service|ch-supply\\.service|pgbackrest-backup\\.service|ch-schema-snapshot\\.service|verify-served-values\\.service"}[10m]) == 1`, `for: 25m`, `severity: ticket`. Producer: node_exporter systemd collector (`node_systemd_unit_state`).
- Meaning: any systemd unit `failed` for 15 m+, or failing every run while its timer restarts it within 10 m (`max_over_time[10m]` bridges the `activating` dips; `for:` grows by the same 10 m so a single failure must still persist 15 m+). Catch-all: everything is covered unless it has a better dedicated alert (exclusions in `scripts/ci/unit-failed-dedicated.baseline`: `verify-archive.md#stellarindex_verify_archive_unit_failed`; `## stellarindex_pgbackrest_backup_unit_failed`; `divergence.md` is where stale `account_directory` eventually shows).
- Key idea: most of these oneshots write data something else reads and trusts, so a failure surfaces as a consumer serving stale data confidently, possibly days later. Ask "what has been reading its output since it stopped?" `directory-sync` is the sole writer of `account_directory` (consulted by the scam-pricing gate on every aggregated serve; `Type=oneshot`, no `OnFailure`, no metric).

```sh
systemctl status <unit>
journalctl -u <unit> --since -24h --no-pager | tail -50
systemctl list-timers <unit>.timer            # is the timer even enabled?
```

| Unit | Writes | Who trusts it | Staleness symptom |
| --- | --- | --- | --- |
| `directory-sync` | `account_directory` | scam-pricing gate, every serve | a flagged issuer keeps getting a published price |
| `holders-rollup` / `census-rollup` | rollup tables | holder counts / census endpoints | figures frozen at last good run |
| `issuer-flags` | issuer flag set | asset payloads | stale verification badges |
| `sep1-refresh` | SEP-1 metadata | asset detail | stale home domains |
| `cap67-movements` | classic movement rows | supply Algorithm 2 | supply divergence |
| `asset-registry-backfill` | `classic_assets` + `issuers` (holdings source) | `/v1/assets`, asset search, SEP-1 fetch queue, `/v1/rwa/assets` | a held-but-never-traded classic asset stays absent from all of them (the old steady state, not a new failure); check `SELECT count(*) FILTER (WHERE last_holding_at IS NOT NULL) FROM classic_assets` moved; a run that stopped early is not a failure, it printed a `RESUME:` line |

Fix: fix the cause, then `systemctl start <unit>`; do not just `reset-failed` (clears state without doing the work and silences the alert while data stays stale). Confirm it wrote something (table `max(synced_at)` / row count moved, not just exit 0). Upstream failure that will not resolve (third-party directory non-200): say so on the ticket; the consumer serving stale data is the reportable fact. When NOT to act: a single failure already restarted successfully by its timer; a unit re-run every few minutes is rarely `failed` when you look, so read `journalctl -u <unit>` for the last runs' exit status. Adding an exclusion: only for a unit with a dedicated alert with better triage, never to quiet a noisy one; add to `scripts/ci/unit-failed-dedicated.baseline` naming the owning alert (`scripts/ci/lint-unit-failed-baseline-test.sh` fails if no rule references the entry).

## stellarindex_textfile_scrape_error

- Trips: `node_textfile_scrape_error > 0`, `for: 15m`, `severity: ticket`.
- Meaning: `node_exporter --collector.textfile.directory=/var/lib/node_exporter/textfile_collector` re-reads every `*.prom` each scrape; one malformed file (bad `HELP`/`TYPE`, partial write, wrong permissions) is dropped and the collector-wide gauge goes to 1. No per-file label. Every alert sourced from that file goes silent instead of firing (archive-completeness, sla-probe, nvme, process-mappings, zfs-snapshots, version-skew...). Writers use atomic tmp-file + `mv`, so this is a genuinely broken file that will fail every scrape, not a race. Tell: textfile-based alerts quiet without their condition clearing.

```sh
journalctl -u prometheus-node-exporter --since -1h | grep -i textfile     # names the file and parse error
curl -s http://<instance>:9100/metrics | grep node_textfile_scrape_error
ls -la /var/lib/node_exporter/textfile_collector/                         # 0-byte / half-written file is the usual culprit
```

Fix: identify the file from the log; stale partial write (writer unit died mid-run): remove it, the next run regenerates it; writer emitting bad syntax (regression): fix the writer and check its output with `scripts/ci/lint_textfile_exposition.py` (CI-time static lint; does not catch runtime disk-full/permissions/mid-write). Owning unit per file: `scripts/ci/textfile-producers.manifest`. Verify gauge back to 0 next scrape and the file's metrics reappear. RCA: crashed mid-write vs bad output every run; how long it was broken (first parse-error line) = how long dependent alerts were blind. No known false positives: a transient mid-write read should not survive a 15-minute `for:`; one that does means a writer racing its own rename. Related: `meta.md#stellarindex_metrics_registry_absent` (same idea one level up, in-process Registry).

## stellarindex_keepalived_textfile_stale

- Trips: `time() - node_textfile_mtime_seconds{file="/var/lib/node_exporter/textfile_collector/keepalived.prom"} > 600`, `for: 5m`, `severity: ticket`.
- Meaning: node_exporter re-serves a textfile's last values forever, so `stellarindex_keepalived_active` / `stellarindex_haproxy_vip_owner` can never reveal the scraper's death; they freeze at last value and a real VIP failover is invisible. Watching the file mtime catches it (same mechanism as `stellarindex_config_assertions_stale`, `config-assertion-failed.md`). Producer: `keepalived-textfile-scraper.timer` (30 s) running `/usr/local/bin/keepalived-textfile-scraper` (haproxy role `configs/ansible/roles/haproxy/tasks/07-monitoring.yml`).

```sh
systemctl status keepalived-textfile-scraper.timer
systemctl status keepalived-textfile-scraper.service
journalctl -u keepalived-textfile-scraper.service -n 50
/usr/local/bin/keepalived-textfile-scraper   # run by hand, inspect the error
```

Causes: `systemctl is-active keepalived` failing (keepalived not installed/running on this host); the `ip -4 a show` interface no longer matching `keepalived_iface`.

## stellarindex_worker_panicked

- Trips: `stellarindex_worker_panics_total > 0`, `for: 0m`, `severity: page`. Raw counter value on purpose: a recovered panic leaves the worker STOPPED until the process restarts, so the alert describes the dead state; `increase(...[10m])` auto-resolved ~10 min after the panic while the worker was still gone. The counter resets on process start, which is the fixing action, so it clears exactly then (same treatment as `stellarindex_decoder_panicked`). Per-worker `worker` label.
- Meaning: a goroutine wrapped in `worker.Recover` (`internal/worker/recover.go`, the only place the counter increments) panicked. The PROCESS is up; that WORKER does not run again until its unit restarts. ~45 background workers across `stellarindex-api`, `-indexer`, `-aggregator`; nothing downstream fires until the symptom (prewarm worker → cold-path latency; snapshot/refresh worker → freshness alert; reaper → a bounded auth table growing) appears, hence the page. Journal line: `background worker panicked — worker STOPPED, process still running` with `worker=<name>`, `panic=<value>` and stack.

```sh
ssh root@136.243.90.96 'journalctl -u stellarindex-api -u stellarindex-indexer -u stellarindex-aggregator --since "-30m" --no-pager | grep -A3 "background worker panicked"'
ssh root@136.243.90.96 'curl -s "http://localhost:9090/api/v1/query?query=stellarindex_worker_panics_total" | python3 -m json.tool | grep -E "worker|value"'
```

Fix: `Recover` does not restart the worker: `systemctl restart stellarindex-<binary>`. An indexer restart resets the ledgerstream cursor: confirm it advances (`ledger-ingest.md#stellarindex_ingestion_cursor_stuck`). Same worker panics again on the same input = poison input: stop the unit rather than crash-loop it, and escalate. A panic is always a code defect (unvalidated index, nil deref, decoder fed attacker-shaped bytes): file it with the journal stack; a worker that "only panicked once" is off until the next deploy. No benign false positives. Related: `dependency-down.md`.

## Backup (pgBackRest) shared context

Applies to the four backup sections and `stellarindex_wal_archive_stale`.

- Scheduler: `pgbackrest-backup.timer` → `pgbackrest-backup.service` → `/usr/local/bin/pgbackrest-backup.sh` (`User=postgres`), daily `02:00 UTC` + up to 15 min jitter, `Persistent=true`; full Sunday / diff Mon–Sat; **one `--repo=N` backup per repo in `pgbackrest.conf` (repo1 then repo2)** because pgBackRest `backup` is single-repo and writes only repo1 without `--repo`. A repo failure does not skip the next; the unit exits with the first non-zero rc. Per-repo textfile metrics `stellarindex_pgbackrest_backup_{last_success_unix,last_rc,duration_seconds}{repo}` (`last_success_unix` carried forward across failed runs). Installed by `configs/ansible/roles/archival-node/tasks/18-pgbackrest-backup.yml`.
- Repos: `repo1` = `/var/lib/pgbackrest`, ZFS dataset `data/pgbackrest`, same pool as the DB, postgres-owned 0750, `repo1-retention-full=2`. `repo2` = offsite S3 (`pgbackrest.conf.j2`, `repo2-retention-full=1` / `-diff=7`, aes-256-cbc; `docs/operations/off-site-backup-plan.md`). pgBackRest does not use MinIO. The live `pgbackrest.conf` is hand-managed (`pgbackrest_manage_conf` defaults false); confirm on r1 repo1 is still `/var/lib/pgbackrest`.
- Metrics: `pgbackrest_backup_since_last_completion_seconds{stanza!~"all-stanzas.*"}` from woblerr `pgbackrest_exporter` (job `pgbackrest_exporter`, `localhost:9854`, unit `pgbackrest_exporter.service`, `--collect.interval=600`, so up to 10 min lag). The `all-stanzas*` pseudo-stanza is excluded so a per-stanza failure can't be masked. The series is already "seconds since", one series per backup type (full/diff/incr); `min by (stanza)` lets the freshest of any type satisfy the alert.
- RPO: declared 5 min, held by continuous WAL archiving (`archive-push` fans out to every repo; `archive_mode` / `archive_command` / `archive_timeout` rendered by the role's `postgresql.conf.j2` where `pgbackrest_backup_enabled` is true, values pinned by `scripts/ci/ansible-postgres-archive-test.sh`). At 24 h without a backup we are 288× over. r1 has no replica and repo1 shares the primary's pool, so pool loss loses both.
- One daily cycle: a single failed/skipped run pages ~24 h after the previous backup's stop; `_none_24h` (page) fires first, `_failed` (ticket) one hour later.
- Never run pgbackrest as root: it leaves root-owned lock/log/manifest files in the postgres-owned repo and breaks later timer runs.

Quick diagnosis (run on r1, `ssh root@136.243.90.96`):

```sh
sudo -u postgres pgbackrest --stanza=stellarindex info          # last backup stop time, repo size
systemctl list-timers pgbackrest-backup.timer --all
systemctl status pgbackrest-backup.service --no-pager
journalctl -u pgbackrest-backup.service --since '48 hours ago' --no-pager | tail -50
ls -ld /var/lib/pgbackrest; zfs list data/pgbackrest data/postgres; df -h /var/lib/pgbackrest
sudo -u postgres psql -Atc "SELECT archived_count, failed_count, last_archived_time, last_failed_time FROM pg_stat_archiver;"
sudo -u postgres psql -Atc "SELECT now(), pg_is_in_recovery();"  # backup needs primary access
curl -s http://127.0.0.1:9854/metrics | grep pgbackrest_backup_since_last_completion_seconds   # if absent: meta.md#stellarindex_redis_exporter_down
cat /var/lib/node_exporter/textfile_collector/pgbackrest_backup.prom   # which repo failed (rc per repo)
```

Root causes:
1. repo1 pool full (repo1 shares the DB's pool, so a full pool stops WAL archiving first and backups fail after; that is why `stellarindex_wal_archive_stale` is the leading signal) or bad permissions on `/var/lib/pgbackrest`: root-owned strays from a manual root run break the timer run. `find /var/lib/pgbackrest /var/log/pgbackrest /var/spool/pgbackrest -not -user postgres`; free space (zfs-pool-full sections / `postgres.md#stellarindex_timescale_disk_full`) and `chown -R postgres:postgres` the strays. S3 credential / bucket-policy / endpoint failures hit only `repo2`: `stellarindex_pgbackrest_backup_last_rc{repo="2"}` non-zero while `{repo="1"}` is 0, so the 24 h alerts stay green; the signal for a repo2 that keeps failing is `stellarindex_backup_offsite_stale` (`backup-offsite.md#stellarindex_backup_offsite_stale`).
2. Primary resource pressure (no backup slot/buffer; rare, seen in heavy write bursts).
3. WAL archive behind and the `archive-async` spool (`/var/spool/pgbackrest`) full: `pg_stat_archiver` `failed_count` rising; usually the same repo1 space/permission issue.
4. pgBackRest binary vs repo format mismatch after a major upgrade; a repo cipher mismatch after a repo re-create (`docs/operations/pgbackrest-encryption.md`) looks identical.
5. Scheduler did not run: timer disabled/absent (`18-pgbackrest-backup.yml` removes it where `pgbackrest_backup_enabled=false`; a missing timer is a documented past cause of `_none_24h`); host down at 02:00 UTC (`Persistent=true` catches up on boot); unit exit 127 (pgbackrest not installed: the testnet/futurenet shape).

False positives: exporter down makes the freshness alerts blind rather than false-firing (`stellarindex_pgbackrest_exporter_down` in `rules.r1/meta.yml`, `meta.md#stellarindex_redis_exporter_down`); the metric can read stale up to 10 min after it returns; `_metrics_absent` deliberately does not fire while `up == 0` so they never double-page. The first backup after a repo re-create (`pgbackrest-encryption.md`) legitimately resets the age series and resolves on the first completed backup.

Related: `restore-drill.md#stellarindex_restore_drill_stale` (`stellarindex_restore_drill_stale`; monthly first-Saturday 04:00 UTC drill proves repo1 restores), `postgres.md#stellarindex_timescale_primary_down` (Patroni failover; not deployed on r1), `account-erasure.md` (a restore brings erased accounts back; replay erasures before the API serves), `docs/adr/0043-backup-and-restore-strategy.md`, `docs/architecture/ha-plan.md` §3.3 and §8. Promtool coverage: `deploy/monitoring/rule-tests/storage-backup_test.yml`.

## stellarindex_timescale_backup_none_24h

- Trips: `min by (stanza) (pgbackrest_backup_since_last_completion_seconds{stanza!~"all-stanzas.*"}) > 24 * 3600`, `for: 5m`, `severity: page` (SEV-1, RPO breach). `alert_family: timescale_backup_age/{{ $labels.stanza }}`. Rule file `storage.yml`.
- Fix (`stellarindex_timescale_backup_*`):
  1. Run a manual backup to prove the system works (as postgres; `backup` is single-repo, without `--repo` only repo1 is written):
     ```sh
     sudo -u postgres pgbackrest --stanza=stellarindex --repo=1 --type=diff backup
     sudo -u postgres pgbackrest --stanza=stellarindex --repo=2 --type=diff backup
     ```
  2. Manual works: investigate the scheduler: `systemctl list-timers pgbackrest-backup.timer --all`, `systemctl enable --now pgbackrest-backup.timer`, `journalctl -u pgbackrest-backup.service -n 100 --no-pager`.
  3. Manual fails: fix the specific error (space, permissions, version/cipher, primary access).
  4. Declare SEV-1 (RPO breach). r1 has no replica; the only other safety net is continuous WAL archiving into repo1 AND repo2. Confirm `pg_stat_archiver.failed_count` is not rising, `last_archived_time` is recent, the pool is not near full (zfs-pool-full sections). Offsite fallback is repo2: check its own freshness with `pgbackrest info --repo=2` before relying on it.
  5. Once healthy take a full (not differential) backup for a known-good restore point (next timer full is Sunday):
     ```sh
     sudo -u postgres pgbackrest --stanza=stellarindex --repo=1 --type=full backup
     sudo -u postgres pgbackrest --stanza=stellarindex --repo=2 --type=full backup
     ```
     `repo1-retention-full=2` (`repo2-retention-full=1`): an extra full expires the oldest chain; make sure the restore drill (`restore-drill.md#stellarindex_restore_drill_stale`) validated the newest chain before relying on it.
  6. Verify: `info` shows a backup with stop time < 24 h AND `pgbackrest_backup_since_last_completion_seconds` dropped in Prometheus (allow up to 10 min exporter lag) AND both alerts resolved in Alertmanager.
- RCA: backup log from last success through first failure; ZFS/kernel logs for `data` in the window; secret/config/binary upgrade around the failure; RPO math (what would be lost if primary failed).

## stellarindex_timescale_backup_failed

Same expr as `stellarindex_timescale_backup_none_24h` with threshold `> 25 * 3600`, `for: 5m`, `severity: ticket` (same `alert_family`). The ticket follows the page by one hour; diagnosis and fix are in `## stellarindex_timescale_backup_none_24h` and the shared backup context.

## stellarindex_pgbackrest_backup_metrics_absent

- Trips: `up{job="pgbackrest_exporter"} == 1 unless on (instance) pgbackrest_backup_since_last_completion_seconds{stanza!~"all-stanzas.*"}`, `for: 15m` (covers `--collect.interval=600` after a restart), `severity: page`. Per-instance `unless on (instance)` so a healthy host can't mask a blind one; gated on `up == 1` so a down exporter pages once (by exporter_down), not twice.
- Meaning: both `min by (stanza)(...) > N` freshness alerts evaluate to an EMPTY vector when the series is missing and can never fire, so a stopped backup would page nothing. Treat as "backup state unknown", same urgency as `_none_24h`. Do not read the freshness alerts' silence as "backups fine".

```sh
curl -s http://127.0.0.1:9854/metrics | grep -E '^pgbackrest_(exporter_status|stanza_status|backup_since_last_completion_seconds)'
journalctl -u pgbackrest_exporter.service -n 50 --no-pager
sudo -u pgbackrest_exporter pgbackrest --stanza=stellarindex info --output=json   # exactly what the exporter sees; NOT as postgres/root
stat -c '%U:%G %a' /etc/pgbackrest/pgbackrest.conf /var/lib/pgbackrest
command -v pgbackrest || echo "pgbackrest NOT installed"
```

| Shape | Fix |
| --- | --- |
| `pgbackrest` not installed (unit exits 127; testnet/futurenet) | Install pgbackrest and create the stanza, or set `pgbackrest_backup_enabled=false` for the host so exporter and timer are removed together |
| Permission denied on `pgbackrest.conf` / repo (managed conf `postgres:postgres 0640`, repo `0750`; exporter runs as `pgbackrest_exporter`) | Make the conf group-readable by the exporter user (add it to the `postgres` group); verify with the `sudo -u pgbackrest_exporter … info` line |
| Stanza error in `pgbackrest info` | Fix the stanza (`stanza-create` / repo cipher / lock files) as in `## stellarindex_timescale_backup_none_24h` |

Verify: the `curl` shows `pgbackrest_backup_since_last_completion_seconds{stanza="stellarindex",…}` and the alert resolves within ~15 min.

## stellarindex_pgbackrest_backup_unit_failed

- Trips: `node_systemd_unit_state{name="pgbackrest-backup.service",state="failed"} == 1`, `for: 5m`, `severity: ticket`. node_exporter runs with `--collector.systemd`.
- Meaning: the nightly unit exited non-zero (exit 127 when pgbackrest is absent, or any backup error): the early, causal signal, ~24 h before the freshness alerts (or never, if the exporter has no stanza). Excluded from `stellarindex_systemd_unit_failed`. `journalctl -u pgbackrest-backup.service -n 100`; diagnosis and fix as in the shared backup context and `## stellarindex_timescale_backup_none_24h`.

## stellarindex_wal_archive_stale

- Trips: `pg_stat_archiver_last_archive_age > 600`, `for: 10m`, `severity: ticket` (`infra.yml`; reads postgres_exporter's built-in `pg_stat_archiver` collector, not pgbackrest's). Same signal as `postgres.wal_archive_max_age_seconds` on `/diagnostics/backups` (`internal/api/v1/diagnostics_backups.go`). Steady-state max observed over 14 d was 82 s.
- Meaning: continuous WAL archiving holds the 5-min RPO between full backups. A stalled archiver with backups otherwise green is a leading indicator: RPO is already wider than believed though `_none_24h` won't fire until a full backup is also overdue.
- Fix: repo1 shares the DB's pool, so a full pool stops WAL archiving first and backups fail after; check `pg_stat_archiver.failed_count` and repo1 free space first (zfs-pool-full sections); same causes as backup root causes #1 and #3 usually explain both.

## minio-metrics-403

**Runbook — MinIO Prometheus scrape returns 403**

_Source page `infra.md#minio-metrics-403`: status current, severity P1, last verified 2026-09-28._


### At a glance

| Field | Value |
| ----- | ----- |
| Symptom | Prometheus targets API shows `minio` job `down` with `lastError: server returned HTTP status 401 Unauthorized` or `403 Forbidden`. MinIO answers 401 for a missing/empty bearer file and 403 for a token whose service account lacks `admin:Prometheus`; both land here. |
| Severity | P1 (page) — matches `stellarindex_minio_exporter_down`, which is what actually detects this |
| Detected by | `stellarindex_minio_exporter_down` (`deploy/monitoring/rules/meta.yml` + the r1 overlay: `up{job="minio"} == 0 OR absent_over_time(up{job="minio"}[5m]) == 1`, `for: 2m`, **`severity: page`**). Its `runbook_url` points at [meta.md#stellarindex_redis_exporter_down](meta.md#stellarindex_redis_exporter_down) — that is the alert's first-response page; come here for the token-provisioning procedure. |
| Typical MTTR | 10 minutes (provision token + restart Prometheus) |
| Impact | MinIO observability gap: no bucket-usage, replication, or write-latency metrics scraped. Operator can't alert on disk exhaustion of the MinIO data partition until the token is wired. |

### Why this happens

MinIO's `/minio/v2/metrics/cluster` endpoint requires a bearer
token by default. `configs/prometheus/prometheus.r1.yml` already
points Prometheus at the right URL with
`bearer_token_file: /etc/prometheus/minio.token`, but the token
file isn't created automatically — it's an operator-mint step. If
no token has ever been provisioned, every scrape is rejected
(401 with no/empty token, 403 with an under-privileged one) and
the job stays `down`.

Because MinIO is where galexie writes ledger metadata and the
indexer reads it back (ADR-0002), a `down` MinIO target also makes
every alert that depends on MinIO cluster metrics silently blind —
which is why the detecting alert is a page, not a ticket. See
`stellarindex_minio_exporter_down`'s own description in
`deploy/monitoring/rules/meta.yml`.

This is finding F-0045 / task #38 of audit-2026-05-26.

### Provisioning procedure

#### 1. Mint a service account on MinIO

SSH to r1 and run `mc` against the local MinIO server. Replace
the placeholder host alias `local` with whatever
`/root/.mc/config.json` calls it (default `local`).

```sh
ssh root@136.243.90.96
mc admin user svcacct add local "<MINIO_ROOT_USER>" \
  --policy /etc/minio/policies/prometheus-read.json \
  --name "prometheus-metrics-scrape" \
  --description "Bearer-token scrape for /minio/v2/metrics/cluster"
```

`--policy` takes a JSON policy file, not a policy name. The
archival-node role renders that file; on a host without it, create it
(and the named policy) first:

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

The `svcacct add` command prints a `Secret Key` — that's the
bearer token. Copy it; you only see it once.

#### 2. Write the token file

```sh
# On r1, as root.
umask 077
echo "<the-secret-key>" > /etc/prometheus/minio.token
chown prometheus:prometheus /etc/prometheus/minio.token
chmod 0400 /etc/prometheus/minio.token
```

The token file is `0400` owned by `prometheus:prometheus` so
only Prometheus can read it. Confirm:

```sh
ls -l /etc/prometheus/minio.token
# -r-------- 1 prometheus prometheus 40 May 28 09:00 /etc/prometheus/minio.token
```

#### 3. Reload Prometheus

```sh
systemctl reload prometheus
```

Wait one scrape interval (15-30 s) and check the target health:

```sh
curl -sS http://localhost:9090/api/v1/targets \
  | jq '.data.activeTargets[] | select(.labels.job=="minio") | {health, lastError}'
```

Expected:

```json
{
  "health": "up",
  "lastError": ""
}
```

#### 4. Confirm metrics flow

```sh
curl -sS http://localhost:9090/api/v1/query \
  --data-urlencode 'query=minio_cluster_capacity_usable_total_bytes' \
  | jq '.data.result | length'
# Expect: > 0
```

If the result is `0`, MinIO accepted the token (no 403) but isn't
returning metrics — usually means the `prometheus-read` policy
needs an additional permission. Re-check the policy JSON above
against the upstream MinIO docs.

### Failure modes

- **Still 403 after token file written.** The token doesn't
  match the service account that was minted, OR the
  service-account policy doesn't include `admin:Prometheus`. Re-mint
  the svcacct (it's free to do — just keep one alive at a time)
  and try again. Confirm the policy is attached via
  `mc admin user svcacct info local <svcacct-access-key>`.
- **Prometheus permission denied on the token file.** Symptom:
  `lastError: error reading bearer token file ... permission
  denied`. Fix: ownership (`chown prometheus:prometheus`) and
  mode (`0400`).
- **Scrape times out.** MinIO under heavy load can take >5 s to
  emit the metrics page. Bump `scrape_timeout` on the `minio`
  job in `prometheus.r1.yml`; reload Prometheus.
- **Service account revoked / token rotated.** Mint a new one and
  re-run steps 2-3.

### Long-term: Ansible

Codified (INV-0981/INV-1144): Group D of
`configs/ansible/roles/archival-node/tasks/16-prometheus-exporters.yml`
(`--tags exporters`, gated on `run_minio` + `run_observability`) runs
exactly steps 1-3 above and writes the token, but ONLY when
`/etc/prometheus/minio.token` is absent — a svcacct secret is shown
once, so the task can never safely overwrite an existing file. This
procedure stays the fall-back for a first-ever provisioning where the
task's preconditions (MinIO up, the `local` mc alias configured by
`09-minio.yml`) are not yet met, and for the rotation case, which is
always this manual mint-then-delete-then-reapply sequence — see
[credential-rotation.md](../credential-rotation.md#prometheus-bearer-token-regen-inv-0981inv-1144--now-codified).

`scripts/ops/config-assertions.sh`'s `minio_prometheus_token_present`
check (hourly) catches a missing, emptied, or wrong-owner file without
reading it — see the same doc.

### Changelog

- 2026-09-24 — corrected the frontmatter/table `severity` from `P2
  (ticket)` to `P1 (page)`: the alert this runbook documents,
  `stellarindex_minio_exporter_down`, is `severity: page` in both
  `deploy/monitoring/rules/meta.yml` and the alerts catalog, and the
  body already said as much (see the 2026-08-29 entry below and
  [meta.md#stellarindex_redis_exporter_down](meta.md#stellarindex_redis_exporter_down)). The header disagreed with
  its own body and with the sibling `meta.md#stellarindex_redis_exporter_down` runbook.
- 2026-08-29 — re-verified against HEAD (runbook Wave L, #319): "Detected by"
  named operator inspection / a bare `up{job="minio"} == 0` — the real detector
  is the P1 `stellarindex_minio_exporter_down` (page, `for: 2m`, with an
  `absent_over_time` arm), whose `runbook_url` routes to `meta.md#stellarindex_redis_exporter_down`;
  the symptom is 401 **or** 403 depending on whether the token is missing or
  under-privileged; the Ansible-gap section re-confirmed (no task owns
  `/etc/prometheus/minio.token`) and the stale "rendered by ansible" comment in
  `configs/prometheus/prometheus.r1.yml` corrected in the same pass.
- 2026-05-28 — initial draft (F-0045 procedure documentation).


## node-root-disk

**Runbook — node root disk alerts**

_Source page `infra.md#node-root-disk`: status living, severity P1, last verified 2026-10-06._

Three alerts on the host's ~49 G root filesystem (everything else on r1 is a ZFS dataset on the multi-TB `data` pool). Rules in `deploy/monitoring/rules/storage.yml` (multi-host source copy) and `configs/prometheus/rules.r1/storage.yml` (the single-host overlay r1 actually loads from `/etc/prometheus/rules.r1/*.yml` per `configs/prometheus/prometheus.r1.yml`; identical rules).

Why root matters: if it fills, Redis MISCONF blocks every cache write (`/v1/price` 404s), Postgres can't write its log and crashes, journald corrupts. Incident class: 2026-05-10 SEV-2 (`internal/incidents/data/2026-05-10-redis-writes-blocked-disk-full.md`), 2026-05-13, 2026-06-11 ClickHouse log-channel wedge (`internal/incidents/data/2026-06-11-clickhouse-log-channel-wedge-root-full.md`, root filled at ~3.8 GB/min, healthy to full in ~5 min), 2026-08-05 recurrence to 81 % (rsyslog duplicate of the API access log, see `15-log-discipline.yml`).

The static thresholds are too slow for a log-flood, hence the trend alert. Order of firing on a fast fill: filling_fast, then warning/full. If `filling_fast` fires, follow its section first.

"Page" tier on r1 currently means Discord `#stellarindex-pages` only; no PagerDuty is wired (see `deploy/monitoring/README.md`), so nobody is automatically woken.

At HEAD the `runbook_url` for `node_root_disk_full` and `node_root_disk_warning` in both rule files still points at `cache.md#stellarindex_redis_writes_blocked` (the 2026-05-10 incident procedure), so those pages do NOT link here; `docs/operations/alerts-catalog.md` does. If you arrived via the alert link, you are in the right place now.

### At a glance

| Alert | Severity | Trips | MTTR |
| ----- | -------- | ----- | ---- |
| [`stellarindex_node_root_disk_filling_fast`](#stellarindex_node_root_disk_filling_fast) | P1 (page) | predict_linear: root reaches 0 within 30 min (10-min linear fit) AND < 50 % free | 5-20 min |
| [`stellarindex_node_root_disk_full`](#stellarindex_node_root_disk_full) | P1 (page) | root < 10 % free for >= 1 min | 15-60 min |
| [`stellarindex_node_root_disk_warning`](#stellarindex_node_root_disk_warning) | P2 (`severity: ticket`) | root < 20 % free for >= 10 min | 30-60 min |

### stellarindex_node_root_disk_filling_fast

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

### stellarindex_node_root_disk_full

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

#### If `pg_wal` is what filled it (2026-09-16)

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

### stellarindex_node_root_disk_warning

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
  - **Postgres logs**: `/var/log/postgresql/postgresql-<ver>-main.log` lives on root (`postgresql.conf.j2`: `logging_collector = on`, `log_min_duration_statement = 1000` ms). If it dominates, **raise** `log_min_duration_statement` (e.g. `5000`, or `-1` to disable) in `configs/ansible/roles/archival-node/templates/postgresql.conf.j2` and re-apply the role (editing the live file drifts back on the next apply), then `SELECT pg_reload_conf();`. The size cap is `/etc/stellarindex-pg-logrotate.conf` (`maxsize 500M`, role-managed), run hourly by `pg-logrotate.timer`; confirm it with `systemctl list-timers pg-logrotate.timer`; the stock `/etc/logrotate.d/postgresql-common` has no size cap. The Postgres data volume `/var/lib/postgresql` is its own ZFS dataset on r1, so vacuuming chunks never frees root; see `postgres.md#stellarindex_timescale_disk_full` for that volume.
- [ ] Step 3: schedule a follow-up review in 24 h to confirm the trend reversed.
- [ ] Verification: `node_filesystem_avail_bytes{mountpoint="/"} / node_filesystem_size_bytes{mountpoint="/"} > 0.40` (40 % free) sustained for 1 hour. This is an operator target, stricter than the alert's own resolution at 20 %.

Root cause: if this fires more than once a quarter, the disk-usage trend has a leak. Capture for a planning ticket: the 30-day trend of `node_filesystem_avail_bytes{mountpoint="/"}` from the Prometheus graph UI or `promtool query range`; per-directory growth rate via two `du -xsh /var/*` snapshots 7 days apart.

False positive: **one-time large captures**. Manual debug captures and one-shot operator log dumps can take 5-10 GB transiently (historical example: the 2026-05-10 `/var/log/wasm-history-*.stderr` captures, 2.2 GB). WASM-audit walks now write under `/var/log/wasm-audit/` and are deleted once recorded (`docs/operations/wasm-audits/README.md` section 2); anything left there with no walk running is a missed cleanup, and loose `/var/log/wasm-history-*` files are a finding. If the trigger is identifiable and the data is needed, leave it; otherwise clean up.


## process-mappings

**Runbook — `stellarindex_process_mappings_*`**

_Source page `infra.md#process-mappings`: status ratified, severity P1 | P3, last verified 2026-09-10._


### At a glance

| Field | Value |
| ----- | ----- |
| Alerts | `stellarindex_process_mappings_high` (P3, ticket) · `stellarindex_process_mappings_critical` (P1, page) · `stellarindex_process_mappings_exhaustion_projected` (P1, page) · `stellarindex_process_mappings_probe_degraded` (P3, ticket) |
| Severity | P3 → P1 escalation |
| Detected by | Prometheus rules in `deploy/monitoring/rules/memory-mappings.yml` + `configs/prometheus/rules.r1/memory-mappings.yml` (byte-identical copies; group `stellarindex.memory_mappings`). Fired-state coverage: `deploy/monitoring/rule-tests/memory-mappings_test.yml`. |
| Metric source | `node_exporter` textfile_collector reads `/var/lib/node_exporter/textfile_collector/memory_mappings.prom`, refreshed every 5 min by `memory-mappings.timer` → `/usr/local/bin/memory-mappings` (`configs/ansible/roles/archival-node/files/memory-mappings.sh`) |
| Steady-state | ~47,000–50,000 mappings for ClickHouse against `vm.max_map_count` = 1,048,576 — about **4.5 %** of the limit (measured on r1, 2026-09-10, immediately after the restart) |
| Customer impact | None while the ticket is firing. At exhaustion the process cannot `mmap` at all: ClickHouse crashes, and the jobs behind it fail (`holders-rollup.service` did on 2026-09-10) |
| Companions | [host-memory-high](infra.md#stellarindex_host_memory_high), [systemd-unit-failed](infra.md#stellarindex_systemd_unit_failed) |

### Why this exists

On **2026-09-10 13:53 CEST** ClickHouse on r1 exhausted the kernel's
per-process virtual-memory-mapping limit. Its own error text:

```
Allocator: Cannot malloc 63.33 MiB: , errno: 12, strerror: Cannot allocate memory
It looks like that the process is near the limit on number of virtual memory mappings.
Current number of mappings (/proc/self/maps): 1048578.
Limit on number of mappings (/proc/sys/vm/max_map_count): 1048576.
```

It went into a `std::bad_alloc` storm and crashed; systemd auto-restarted
it (restart counter 1) and `holders-rollup.service` failed as collateral —
its first failure ever, against 24 successful cycles in the preceding 48 h.

The limit was **already** raised far above the kernel's 65530 default and
was **already** codified — `/etc/sysctl.d/10-map-count.conf`,
`vm.max_map_count=1048576` (2^20). What did not exist was any signal at
all: no metric, no rule, no reference to `max_map_count` anywhere in the
tree. **A server crash was the first and only signal.**

Note what is NOT known: at ~4.5 % steady state, the excursion to
1,048,578 was roughly **20x**, and its driver has not been established.
Nothing here should be read as a diagnosis. The probe exists so the next
excursion is watched on the way up rather than reconstructed afterwards,
and the first job when one of these fires is to **capture evidence**.

`node_exporter` has no per-process mapping series — its procfs collector
is system-wide — which is why this is a textfile producer.

### Quick diagnosis (≤ 5 min)

```sh
# 0. What the probe currently sees.
ssh r1 'cat /var/lib/node_exporter/textfile_collector/memory_mappings.prom'
#    ..._procs 0            → the probe is watching NOTHING (see below)
#    ..._unreadable 1       → it matched but could not read the map
#    ..._ratio              → the number the alerts compare

# 1. Find the pid the probe reports: the HIGHEST-mapping process off the
#    clickhouse binary. Never `pgrep -f clickhouse` — that self-matches
#    the command line you are typing. r1 runs TWO: a watchdog parent
#    (~97 mappings) and the real server (~47,000).
ssh r1 'for x in /proc/[0-9]*; do e=$(readlink -f $x/exe 2>/dev/null); \
  case "$e" in */clickhouse) echo "$(wc -l < $x/maps) ${x##*/}";; esac; done | sort -rn'

# 2. The live count and the live limit.
ssh r1 'wc -l < /proc/<pid>/maps; cat /proc/sys/vm/max_map_count'

# 3. WHAT the mappings are — the one measurement that narrows the cause.
ssh r1 'awk "{print \$6}" /proc/<pid>/maps | sort | uniq -c | sort -rn | head -20'
#    Mostly file paths under /var/lib/clickhouse → file-backed growth
#      (data parts / marks being mmapped; look at part counts next).
#    Mostly blank (anonymous) → allocator / arena growth.
ssh r1 'awk "{print \$6}" /proc/<pid>/maps | grep -c ^$'   # anonymous count

# 4. If file-backed, the obvious ClickHouse-side quantities to read.
#    These are candidates to MEASURE, not causes to assume.
ssh r1 'clickhouse-client --port 9300 -q "SELECT count() AS parts, sum(rows) FROM system.parts WHERE active"'
ssh r1 'clickhouse-client --port 9300 -q "SELECT * FROM system.asynchronous_metrics WHERE metric LIKE \"%Mmap%\" OR metric LIKE \"%OpenFile%\""'
ssh r1 'clickhouse-client --port 9300 -q "SELECT name, value FROM system.settings WHERE name IN (\"mmap_cache_size\",\"min_bytes_to_use_mmap_io\",\"local_filesystem_read_method\")"'
```

### Mitigation (≤ 15 min)

**Capture before you fix.** A restart clears the mappings and the
explanation with them, and the alert resolves — which is exactly how this
condition stayed invisible until it crashed a server.

```sh
ssh r1 'cp /proc/<pid>/maps /var/tmp/maps-$(date +%s).txt'
ssh r1 'awk "{print \$6}" /proc/<pid>/maps | sort | uniq -c | sort -rn > /var/tmp/maps-summary-$(date +%s).txt'
```

Then, in order:

1. **Buy headroom** (`_critical` / `_exhaustion_projected`). Raising the
   limit is the standard remedy and takes effect immediately; it costs a
   little kernel memory per mapping. It is a stopgap — the driver is not
   established — but a crashed ClickHouse is worse than a larger ceiling.

   ```sh
   ssh r1 'sysctl -w vm.max_map_count=2097152'
   # Persist it, or the next boot silently reverts to 1048576:
   ssh r1 'sed -i s/1048576/2097152/ /etc/sysctl.d/10-map-count.conf && sysctl --system'
   ```

   The metric is published as a **ratio**, so every threshold moves with
   the limit automatically — no rule edit is needed after a change.
   `stellarindex_process_memory_mappings_limit` records what the probe
   read, so the change is visible on the graph.

   **`/etc/sysctl.d/10-map-count.conf` is NOT ansible-managed** — it is
   an out-of-band file on r1, and `vm.max_map_count` appears nowhere in
   this repo (checked 2026-09-10). The role's sysctl surface is the
   `sysctl_tunings` map in
   `configs/ansible/roles/archival-node/defaults/main.yml`, applied to
   `/etc/sysctl.d/90-stellarindex.conf`; sysctl.d applies in lexical
   order and the later file wins, so a value added there would take
   precedence over the hand-written one. Adding it is the right long-term
   move and is deliberately NOT done as part of an incident — an
   emergency `sysctl -w` plus a note is; codify it afterwards, in a
   change that can be reviewed with `--check --diff`.

2. **Restart ClickHouse** only if it is already failing to allocate, and
   only after step 0's capture. `systemctl restart clickhouse-server`.
   Expect the mapping count to return to the ~47,000 band within minutes;
   if it climbs straight back, that is a strong signal and worth its own
   incident.

3. **Watch the rollups behind it.** `holders-rollup.service` failed as
   collateral on 2026-09-10. `systemctl --failed` and the
   `stellarindex_systemd_unit_failed` ticket cover the rest.

#### `probe_degraded` — the probe itself

```sh
ssh r1 'systemctl status memory-mappings.timer memory-mappings.service'
ssh r1 'journalctl -u memory-mappings.service --since -1h'
ssh r1 'bash scripts/ci/memory-mappings-test.sh'   # from a checkout: pins the shipped bytes
```

| Reading | Meaning | Action |
| ------- | ------- | ------ |
| `..._procs 0` | Matched **nothing**. The process is stopped, or its binary moved/was renamed. | The most dangerous state: the mapping gauges are *withheld* rather than published as 0, so every threshold alert above is silent and looks healthy. Confirm ClickHouse is running; if the exe path changed, update `MEMORY_MAPPINGS_WATCH` in `memory-mappings.service.j2`. |
| `..._unreadable 1` | Matched, but `/proc/<pid>/maps` or `vm.max_map_count` could not be read. | The unit must run as `User=root` — `/proc/<pid>/{exe,maps}` are readable only by the owning uid or root, and ClickHouse runs as `clickhouse`. Check the unit was not de-privileged and that `ptrace_scope` was not tightened. |
| unit `failed`, previous `.prom` intact | The probe rendered a line that does not parse as Prometheus exposition and **refused to publish**. | Read the journal — it names the offending line. node_exporter rejects an unparseable `.prom` *whole*, so refusing is deliberate: the stale file ages into this same ticket instead of taking every family in the file off the host (r1 2026-09-10, the timescale case). |
| stamp older than 30 min | The timer stopped firing. | node_exporter re-serves a stale textfile verbatim on every scrape, so the gauges *freeze at their last healthy value* rather than going absent — `..._updated_unix` is the only series that can see this. `systemctl start memory-mappings.timer`. |

### Known false-positive patterns

- **A ClickHouse restart** drops the count to near zero and climbs back
  to the steady band over a few minutes. The trend alert's `for: 5m` plus
  its `ratio > 0.10` floor keep that recovery from paging.
- **A deliberate `vm.max_map_count` change** moves the ratio for every
  process at once. That is the metric working: the thresholds are
  fractions of whatever limit the kernel currently reports.

### Changelog

| Date | Change |
| ---- | ------ |
| 2026-09-10 | Created after the ClickHouse `vm.max_map_count` exhaustion crash on r1. Probe, four alerts and this runbook landed together; the driver of the 20x excursion remains unestablished. |

## Related

- [Alerts catalogue](../alerts-catalog.md)

**`minio-metrics-403`**

- ADR-0002 — self-hosted storage is S3-compatible (MinIO is the
  default).
- F-0045 (audit-2026-05-26) — original finding.
- `configs/prometheus/prometheus.r1.yml` — the `job_name: minio` scrape stanza.
- F-0152 closure — sibling exporters (redis / postgres /
  pgbackrest) now installed; MinIO's token is the last piece,
  closed by the Group D task above (INV-0981/INV-1144).
- [meta.md#stellarindex_redis_exporter_down](meta.md#stellarindex_redis_exporter_down) — where
  `stellarindex_minio_exporter_down` routes; its per-exporter notes
  carry the day-to-day `Authorization: Bearer $(cat
  /etc/prometheus/minio.token)` probe.
- [credential-rotation.md](../credential-rotation.md) — regenerating the
  bearer token after a MinIO **root** rotation.

**`node-root-disk`**

- [redis-write-blocked-disk-full](cache.md#stellarindex_redis_writes_blocked): the downstream cascade when these alerts were missed; the May-10 incident's primary remediation, and the `runbook_url` the full and warning rules currently link to.
- `postgres.md#stellarindex_timescale_disk_full`: sibling for the postgres data volume (`stellarindex_timescale_disk_full` / `_warning`; separate ZFS dataset per `configs/ansible/roles/archival-node/defaults/main.yml` `zfs_datasets`).
- `docs/operations/r1-ansible-drift-2026-07-03.md`: why hand-applied guards and the ansible role disagree.
- ADR-0008: HA topology + DR posture (single-host R1 today; fewer fail-safes than R2/R3 will have).
- 2026-05-10 and 2026-06-11 incident postmortems under `internal/incidents/data/` (paths above).

**`process-mappings`**

- [host-memory-high](infra.md#stellarindex_host_memory_high) — RSS pressure, a different
  resource from mapping count: many small mappings are cheap in resident
  bytes and expensive in map count, so these two can move independently.
- [systemd-unit-failed](infra.md#stellarindex_systemd_unit_failed) — the catch-all that
  picks up `memory-mappings.service` when it refuses to publish.
- [zfs-pool-full](infra.md#stellarindex_zfs_pool_low_space) — the other substrate limit that
  takes ClickHouse down.
- `docs/reference/metrics/README.md` — the metric family's reference entry.
