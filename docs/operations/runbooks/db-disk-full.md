---
title: Runbook — db-disk-full
last_verified: 2026-08-28
status: current
severity: P1
---

# Runbook — `stellarindex_timescale_disk_full` / `_disk_warning`

## At a glance

| Field | Value |
| ----- | ----- |
| Alerts | `_disk_full` (< 10 % free, `severity: page`, `for: 1m`, SEV-1), `_disk_warning` (< 20 % free, `severity: ticket`, `for: 10m`) |
| Severity | P1 (disk_full) / P2 (disk_warning) |
| Detected by | `configs/prometheus/rules.r1/storage.yml` (group `stellarindex.storage`) — the file r1 actually loads; multi-host twin in `deploy/monitoring/rules/storage.yml`. Both watch `node_filesystem_*_bytes{mountpoint="/var/lib/postgresql"}`. |
| Typical MTTR | 15–90 min |
| Impact | **WAL refuses to write once the disk is full.** Every write across the system halts — indexer insert-errors spike, API's `/v1/price` falls back to slow paths, every service readyz may go red. Act well before full. On r1, `/var/lib/postgresql` is a ZFS dataset (`data/postgres`) on the shared raidz1 `data` pool — its "free space" IS the pool's free space, so this alert is almost always the pool-full problem wearing a Postgres hat. |

## Symptoms

- `/var/lib/postgresql` mount < 10 % (or < 20 % for warning)
  sustained 1 min (or 10 min for warning).
- Postgres logs: `could not extend file ... No space left on device`.
- `stellarindex_zfs_pool_low_space` / `zfs-pool-full.md` firing at
  the same time — expected, because every ZFS dataset on r1 shares
  the `data` pool's free space, so `node_filesystem_avail_bytes` is
  (near-)identical across the postgres, clickhouse, minio, and
  pgbackrest mountpoints. This alert and the pool alert are two
  views of one condition.
- Cascading: `insert-errors.md`, `trade-insert-backpressure.md`.

## Quick diagnosis (≤ 5 min)

```sh
ssh root@136.243.90.96

# Capacity breakdown — dataset AND pool view
df -h /var/lib/postgresql
zfs list -o name,used,avail,mountpoint data data/postgres data/clickhouse data/pgbackrest
zpool list data
du -sh /var/lib/postgresql/*/pg_wal /var/lib/postgresql/*/base /var/lib/postgresql/*/pg_tblspc 2>/dev/null

# Biggest hypertable / chunk
runuser -u postgres -- psql -d stellarindex -c \
  "SELECT hypertable_name, pg_size_pretty(hypertable_size(format('%I.%I', hypertable_schema, hypertable_name)::regclass))
   FROM timescaledb_information.hypertables
   ORDER BY hypertable_size(format('%I.%I', hypertable_schema, hypertable_name)::regclass) DESC;"

# Is a compression-policy misfire leaving cold chunks uncompressed?
runuser -u postgres -- psql -d stellarindex -c \
  "SELECT * FROM timescaledb_information.jobs WHERE job_id IN
   (SELECT job_id FROM timescaledb_information.job_stats WHERE last_run_status != 'Success');"

# pgBackRest repo eating space? (repo1 is LOCAL, on the same pool.)
du -sh /var/lib/pgbackrest
```

## Typical root causes

1. **Compression policy isn't running.** Chunks older than their
   `compress_after` should be compressed; if the job fails silently,
   raw chunks accumulate.
   - Signal: `compression-lag.md` is also firing.
   - Mitigation: `cagg-stale.md` and `compression-lag.md` explain
     the fix paths.

2. **The shared `data` pool is full** — ClickHouse lake growth, the
   MinIO galexie-archive, pgBackRest repo1, or stale ZFS snapshots.
   Because all datasets share the pool, Postgres reports "its" disk
   full even when Postgres grew by nothing.
   - Mitigation: `zfs-pool-full.md` — that runbook owns the
     pool-level relief levers.

3. **WAL fill-up.** A long-running transaction prevents WAL
   recycling; `pg_replication_slots` tells you if a stale slot is
   holding WAL (r1 has no replica, so any slot present is left-over
   cruft, not load-bearing).
   - Mitigation: advance or drop the stuck slot.

4. **Unbounded log growth** in the Postgres stderr log file
   (misconfig writing debug everywhere). Note the log is NOT on this
   filesystem: `/var/log/postgresql/postgresql-15-main.log` lives on
   the **root fs**, which has its own alerts — if logs are the
   problem you'll see `node-root-disk-full.md` /
   `node-root-disk-warning.md`, not this alert.
   - Mitigation: truncate the log + fix the verbosity.

5. **Someone loaded a large dataset** (backfill, import) beyond the
   pool's headroom. Heavy one-shots must run under
   `/usr/local/sbin/run-heavy-job.sh` (CLAUDE.md rule) precisely to
   keep this survivable.

## Mitigation (fastest paths first)

> **Retention is NOT a lever on this database.** Raw `trades` are
> kept forever by design (ADR-0034; migration 0031 removed the old
> 90-day retention), the `prices_*` continuous aggregates are
> indefinite, and the only surviving `add_retention_policy` is on
> `api_usage_events`. Do NOT "adjust the retention interval" and do
> NOT `drop_chunks` on data tables — that destroys served history to
> buy hours of runway. Space relief on r1 is **pool-level**: follow
> `zfs-pool-full.md` (stale ZFS snapshots, pgBackRest repo pruning,
> ZSTD recompression of ClickHouse tables).

- [ ] Step 1 — **create headroom NOW**, at the pool level. Don't
      investigate first. Follow `zfs-pool-full.md`'s relief levers
      (snapshots / pgBackRest repo / recompression). A quick
      `CHECKPOINT;` (`runuser -u postgres -- psql -d stellarindex -c
      "CHECKPOINT;"`) lets WAL recycle sooner but frees little.

- [ ] Step 2 — there is no "scale the volume" escape hatch:
      `/var/lib/postgresql` is a dataset on r1's fixed raidz1 `data`
      pool, and r1 is explicitly **not hardware-upgradeable**
      (ADR-0027; ha-plan §"Headroom levers"). No PVC to expand, no
      5th drive. Software-only relief per `zfs-pool-full.md`.

- [ ] Step 3 — once green again, investigate why the pool filled:
      compression lag, snapshot accumulation, an unwindowed backfill,
      ClickHouse growth ahead of plan.

- [ ] Step 4 — verify WAL is recycling: `SELECT * FROM pg_stat_archiver;`.

- [ ] Verification: pool free trending up and `/var/lib/postgresql`
      avail > 20 %; no `No space` errors in the last hour.

## Root cause analysis

- 30-day growth curve of the `data` pool and of `/var/lib/postgresql`.
- Compression job success rate over the period.
- WAL generation rate — did it jump recently?
- Was there a backfill / migration that bloated the DB?

## Known false-positive patterns

- **Backup-in-progress** — pgBackRest repo1 is **local** at
  `/var/lib/pgbackrest` on the same pool; there is no upload/staging
  step. A running backup adds real load and real space on the shared
  pool (bounded by `repo1-retention-full=2`), so it's not a false
  positive so much as a scheduled contributor — check
  `systemctl list-timers pgbackrest-backup.timer` for overlap.
- **pg_repack** runs rebuild a table's storage; doubles it briefly.

## Related

- `zfs-pool-full.md` — the pool-level view of the same space; on r1
  the two alerts fire together and that runbook owns the relief levers.
- `node-root-disk-full.md` — the ROOT fs (where the Postgres log
  lives) has separate alerts; a log flood shows up there, not here.
- `insert-errors.md` — downstream when writes start failing.
- `compression-lag.md` — the policy that should be shrinking cold data.
- `cagg-stale.md` — separate issue that often correlates.

## Changelog

- 2026-08-28 — re-verified against HEAD. Removed the DANGEROUS
  retention advice ("adjust add_retention_policy" / `drop_chunks`) —
  raw `trades` are kept forever (ADR-0034, migration 0031); the only
  surviving retention policy is `api_usage_events`. Removed the k8s
  PVC-expansion path (no k8s; `/var/lib/postgresql` is a ZFS dataset
  on the fixed raidz1 `data` pool, r1 not hardware-upgradeable per
  ADR-0027) — space relief redirected to `zfs-pool-full.md`.
  pgBackRest reframed as local repo1 on the same pool (no staging
  upload). Commands use r1 shapes (`ssh root@136.243.90.96`,
  `runuser -u postgres -- psql -d stellarindex`); log path corrected
  to `/var/log/postgresql/postgresql-15-main.log` (root fs →
  `node-root-disk-full.md`); replica framing dropped (no replica);
  rule citation → `rules.r1/storage.yml`.
- 2026-04-23 — initial draft. Emphasises "create headroom first,
  investigate second" — at the edge you can't afford to root-cause
  before acting.
