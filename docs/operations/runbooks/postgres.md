---
title: Runbook — postgres
last_verified: 2026-10-06
status: living
severity: P1
---

# Runbook — postgres alerts

## At a glance

Postgres/Timescale primary, replica and connection alerts. r1 runs a single unclustered Postgres 15 + TimescaleDB as `postgresql@15-main.service` (`configs/ansible/roles/archival-node/tasks/05-postgres.yml`). There is no Patroni, etcd or replica; the HA design in `docs/architecture/ha-plan.md` is not deployed, so nothing fails over. Recovery is "restart the service" or "restore from pgBackRest". Do not follow promote/failover steps in `dr-activation.md`.

## stellarindex_timescale_primary_down

P1 (page, `for: 30s`). `pg_up == 0` from `postgres_exporter` (`localhost:9187`), rule in `configs/prometheus/rules.r1/storage.yml`. Writes halt everywhere (ingestion, key mint, usage rollups). Redis hot path serves cached prices with `stale_flag=true`; ClickHouse-backed endpoints keep working; Timescale-backed endpoints 5xx and `/v1/readyz` returns 503. Companions: `stellarindex_postgres_ping_failing` in ~2-3 min; `stellarindex_ingestion_cursor_stuck` (ticket) in ~10 min, not a confirmation signal. MTTR: minutes to restart, hours for a pgBackRest restore.

Check:

```sh
# Step 1: /v1/readyz; `checks` is an array of {name, ok, error}. No -f: readyz returns 503 on outage.
curl -sS https://api.stellarindex.io/v1/readyz | jq '.checks[] | select(.name=="postgres")'
# {"name":"postgres","ok":false,"error":"dial tcp ... connection refused"} = real DB outage.

# Step 2: the unit (the CLUSTER unit, NOT the `postgresql` umbrella), exporter, direct psql.
ssh root@r1 'systemctl status postgresql@15-main.service --no-pager'
ssh root@r1 'curl -s localhost:9187/metrics | grep ^pg_up'
ssh root@r1 'sudo -u postgres pg_isready -t 5; sudo -u postgres psql -d stellarindex -c "SELECT pg_is_in_recovery();"'
```

If only the alert fires and unit + psql are healthy it is a scrape problem (P3): check `stellarindex_postgres_exporter_down` ([`meta.md#stellarindex_redis_exporter_down`](meta.md#stellarindex_redis_exporter_down)) and [`meta.md#stellarindex_prometheus_scrape_failing`](meta.md#stellarindex_prometheus_scrape_failing).

Find out why it stopped before restarting:

```sh
ssh root@r1 'df -h /var/lib/postgresql; zpool status -x'
ssh root@r1 'tail -n 100 /var/log/postgresql/postgresql-15-main.log'
ssh root@r1 'dmesg -T | grep -i -E "oom|out of memory|nvme|i/o error" | tail -n 30'
ssh root@r1 'journalctl -u postgresql@15-main.service --since "1 hour ago" --no-pager | tail -n 50'
```

Likely causes:

- Disk full (WAL cannot write): [`postgres.md#stellarindex_timescale_disk_full`](postgres.md#stellarindex_timescale_disk_full) first; restarting onto a full volume crashes again. `stellarindex_timescale_disk_warning` usually fires earlier.
- ZFS pool degraded / NVMe dropped: [`infra.md#stellarindex_zfs_pool_degraded`](infra.md#stellarindex_zfs_pool_degraded); do not restart until the pool is online.
- OOM kill (`shared_buffers` + `work_mem` x connections over RAM): restart is safe; keep `dmesg` for RCA.
- Config drift after an Ansible run (`templates/postgresql.conf.j2`): check recent `playbooks/deploy-binary.yml` runs and [`binary-version-skew.md`](binary-version-skew.md).

Fix:

1. Data dir intact: restart the cluster unit.

   ```sh
   ssh root@r1 'systemctl restart postgresql@15-main.service && sleep 5 && systemctl status postgresql@15-main.service --no-pager'
   ssh root@r1 'sudo -u postgres pg_isready -t 5'
   ```

2. Un-wedge the indexer pool (it can hold dead connections after recovery; the pool retires connections every 30 min, do not wait):

   ```sh
   ssh root@r1 'journalctl -u stellarindex-indexer.service --since "30 min ago" | grep -E "postgres.ping|pool may be wedged"'
   # If `pool may be wedged` appears, or stellarindex_postgres_ping_failing stays firing:
   ssh root@r1 'systemctl restart stellarindex-indexer.service'
   ```

3. Verify: `pg_up 1`, `/v1/readyz` 200 with the postgres check `ok:true`, indexer logs inserting (`ssh root@r1 'journalctl -u stellarindex-indexer.service -f'`), `stellarindex_ingestion_cursor_stuck` clears (~10 min).
4. Data dir unrecoverable (corrupt WAL, lost dataset): the only copy is the pgBackRest repo. Declare the incident ([`sev-status-page-update.md`](sev-status-page-update.md)), then restore per [`infra.md#stellarindex_timescale_backup_none_24h`](infra.md#stellarindex_timescale_backup_none_24h) and [`../drills/restore-drills.md`](../drills/restore-drills.md). RPO is the age of the last successful backup. After return, idempotent upserts re-fill from the archive; check `stellarindex_ingestion_cursor_stuck` and `stellarindex_ingest_gap_detected`.

   ```sh
   ssh root@r1 'sudo -u postgres pgbackrest --stanza=stellarindex info'
   ssh root@r1 'systemctl list-timers pgbackrest-backup.timer restore-drill.timer --no-legend'
   ```

5. Host gone: [`dr-activation.md`](dr-activation.md) (describes the undeployed design; needs single-node correction before literal use).

Escalate: for the postmortem collect the postgres log, `dmesg`, disk/IOPS metrics and recent role changes.

## stellarindex_postgres_ping_failing

P1 (page, `for: 2m`). `rate(stellarindex_postgres_ping_total{outcome="error"}[5m]) > 0`. The indexer (`watchPostgresPing`) probes its pool every 60 s and emits `stellarindex_postgres_ping_total{outcome}` and `stellarindex_postgres_ping_failure_streak`; the pool also retires connections (`SetConnMaxLifetime` 30 min, idle 5 min). One failed probe in the window is enough; `for: 2m` rides through a clean restart. Rules: `configs/prometheus/rules.r1/storage.yml` (template `deploy/monitoring/rules/storage.yml`). Indexer ingest is stalled or about to stall. MTTR ~5 min.

Symptoms: streak climbing past 3; indexer journal `pool may be wedged`; `stellarindex_source_insert_errors_total{kind="trade"}` climbing and/or `stellarindex_ingestion_trade_insert_backpressure` firing (ADR-0041: the sink retries infrastructure faults; a `kind="dropped"` increase means the bounded buffer overflowed, genuine loss).

Check:

```sh
# 1. Is postgres itself up? (use the CLUSTER name, not the umbrella)
ssh root@136.243.90.96 'systemctl status postgresql@15-main.service'

# 2. Can a fresh client connect at all?
ssh root@136.243.90.96 'sudo -u postgres pg_isready -t 5'

# 3. What does the indexer journal say about the ping streak?
ssh root@136.243.90.96 'journalctl -u stellarindex-indexer.service --since "10 min ago" | grep -E "postgres.ping|pool may be wedged"'

# 4. Live metric on r1's prometheus (loopback-bound, query from the host):
ssh root@136.243.90.96 "curl -s http://localhost:9090/api/v1/query \
  --data-urlencode 'query=stellarindex_postgres_ping_failure_streak'" | jq .
```

Likely causes:

- Postgres down: fix it first ([`postgres.md#stellarindex_timescale_primary_down`](postgres.md#stellarindex_timescale_primary_down)).
- Postgres up but pings fail: network blip, firewall reset or auth/DSN misconfig.
- Wedged pool with a healthy DB.
- Restart taking too long for `for: 2m` (investigate separately if this fires on every routine restart).

Fix:

1. Postgres down: fix it, then force the pool refresh (otherwise automatic within 30 min):

   ```sh
   ssh root@136.243.90.96 'systemctl restart stellarindex-indexer.service'
   ```

2. Postgres up: verify the DSN. There is no `/etc/default/stellarindex-indexer` on r1. The password-free DSN is `postgres_dsn` in the TOML; the unit's `EnvironmentFile=` is `/etc/default/stellarindex`, whose `STELLARINDEX_POSTGRES_DSN` (with password) wins:

   ```sh
   ssh root@136.243.90.96 'grep -n postgres_dsn /etc/stellarindex.toml'
   # redacts the password before it reaches your terminal/journal:
   ssh root@136.243.90.96 \
     "sed -n 's/^\(STELLARINDEX_POSTGRES_DSN=postgres:\/\/[^:]*\):[^@]*@/\\1:***@/p' /etc/default/stellarindex"
   ```

3. Fix either via ansible (`--check --diff` first; secrets through the vault helper), never by hand; the next playbook run reverts hand edits.
4. If the pool is wedged but the DB healthy, restart the indexer to drain.
5. Verify the streak resets to 0 and `outcome="ok"` climbs.

Escalate: a partition between indexer and DB looks identical; correlate with `up{job="postgres_exporter"}`. Record the streak length before the alert fired.

## stellarindex_timescale_replica_lag

P2 (ticket, `for: 2m`), `configs/prometheus/rules.r1/storage.yml`. INERT on r1: r1 has no replica, so `pg_replication_lag_seconds` has no series (absence, not health). First consumer will be the r3 async DR replica ([ADR-0016](../../adr/0016-per-region-storage-strategy.md)). Do not act on this against r1 as it stands; below is the playbook for when a standby exists. A sync replica holds back primary writes; an async one grows the DR copy's RPO.

Check (`pg_replication_lag_seconds > 5` for 2 min, or `write_lag` / `replay_lag` > 5 s, or primary backends in `wait_event_type = SyncRep`):

```sh
# Primary's view of all replicas
psql -c "SELECT application_name, state, sync_state,
                pg_wal_lsn_diff(sent_lsn, replay_lsn) AS replay_bytes,
                replay_lag
         FROM pg_stat_replication;"

# Replica's view
ssh <replica-host> 'psql -c "SELECT now() - pg_last_xact_replay_timestamp() AS lag"'

# Replica system load: IO-bound, CPU-bound, network?
ssh <replica-host> 'iostat -x 1 5'
ssh <replica-host> 'top -b -n1 | head -10'

# Is the network link OK?
ssh <replica-host> 'ping -c 10 <primary>'
```

Likely causes:

- Replica IO saturated (slow disk, or serving read traffic).
- Long query on the replica blocking WAL apply (`Startup process` waiting in `pg_stat_activity`): cancel it with `SELECT pg_cancel_backend(...)`.
- Network saturated (resilver, big backfill WAL burst).
- WAL already recycled (`max_wal_senders` / `wal_keep_size`): replica log says `requested WAL segment has already been removed`; rebuild from a fresh base-backup (hours).

Fix:

1. Identify which of the above.
2. Resource: shed read load or scale disk. Query: cancel the offender. Network: wait and find the hog. WAL-recycle: rebuild the replica.
3. Verify `pg_replication_lag_seconds` under 1 s for 15 min.

Escalate: expected during a backfill and on replica restart (silence for the duration); chronic async flap means tune the threshold.

## stellarindex_timescale_connections_saturated

P2 (ticket, `for: 5m`), `configs/prometheus/rules.r1/storage.yml`. `(sum by (instance) (pg_stat_activity_count) / on (instance) pg_settings_max_connections) * 100 > 80`; the `sum by (instance)` is load-bearing (the unaggregated form matches nothing and reads healthy). PgBouncer is NOT deployed: every binary connects directly to `postgresql@15-main.service` with `max_connections = 200` (`postgres_max_connections` in `configs/ansible/roles/archival-node/defaults/main.yml`). At 100 % you get `FATAL: sorry, too many clients already`; API latency climbs and the indexer's `database/sql` pool cannot acquire connections. If [`postgres.md#stellarindex_postgres_ping_failing`](postgres.md#stellarindex_postgres_ping_failing) pages too (indexer log `pool may be wedged`), you are at hard saturation.

Check:

```sh
ssh root@136.243.90.96

# What's the state distribution?
runuser -u postgres -- psql -d stellarindex -c \
  "SELECT state, wait_event_type, count(*)
   FROM pg_stat_activity
   GROUP BY 1, 2
   ORDER BY count DESC;"

# Who's the top holder?
runuser -u postgres -- psql -d stellarindex -c \
  "SELECT application_name, count(*)
   FROM pg_stat_activity
   WHERE state != 'idle'
   GROUP BY application_name
   ORDER BY count DESC;"

# Long-running transactions (common cause of accumulation)
runuser -u postgres -- psql -d stellarindex -c \
  "SELECT pid, application_name, state, wait_event_type,
          now() - xact_start AS xact_age, left(query, 80)
   FROM pg_stat_activity
   WHERE xact_start IS NOT NULL
   ORDER BY xact_age DESC
   LIMIT 10;"
```

Likely causes:

- Long-running transaction leaking connections (`xact_age` > 10 min on several connections from one `application_name`).
- Connection leak in one of our binaries (steady climb from one `application_name`).
- Genuine burst load: capacity planning, not cleanup.
- No `idle_in_transaction_session_timeout`.

Fix:

1. Find the top holder (above). If the firing alert is `lock_table_pressure`, use the next section instead.
2. Long transactions: `SELECT pg_terminate_backend(pid)` on the offenders, then fix the code.
3. Leaking binary: `systemctl restart stellarindex-<name>`; consider `idle_in_transaction_session_timeout` (e.g. `5min`) via the archival-node role.
4. Genuine growth: raise `postgres_max_connections` in the role (each connection costs backend memory), ansible + reapply, never live-only.
5. Verify utilization under 50 % and no `too many clients` in logs.

Escalate: a pgBackRest run (`--process-max`) or a Redis outage (longer hold times) causes brief expected spikes.

### stellarindex_timescale_lock_table_pressure

Ticket, `for: 5m`, `rules.r1/storage.yml`: `sum by (instance)(pg_locks_count) / on (instance) (pg_settings_max_locks_per_transaction x pg_settings_max_connections) > 0.7`. The lock table is a fixed shared-memory arena of `max_locks_per_transaction x max_connections`; exhausting it is SQLSTATE 53200 ("out of shared memory") and dropped INSERTs. Many `trades` chunks (TimescaleDB per-chunk locking) drive it. The `pg_stat_activity` checks above find nothing relevant: it is lock slots, not connections.

```sh
runuser -u postgres -- psql -d stellarindex -c \
  "SELECT (SELECT count(*) FROM pg_locks) AS locks_held,
          current_setting('max_locks_per_transaction')::int
            * current_setting('max_connections')::int AS lock_table_size;"
```

r1 templates `max_locks_per_transaction = 4096` (`postgresql.conf.j2`, `postgres_max_locks_per_transaction` in the archival-node defaults; 819,200 entries at 200 connections). For sustained pressure bump `postgres_max_locks_per_transaction` in the role and reapply (`--check --diff` first); never a live-only `ALTER SYSTEM`. The bump needs a Postgres restart.

## stellarindex_timescale_disk_full

P1 (page, `_disk_full` < 10 % free, `for: 1m`) / P2 (ticket, `_disk_warning` < 20 % free, `for: 10m`). Both watch `node_filesystem_*_bytes{mountpoint="/var/lib/postgresql"}` (`configs/prometheus/rules.r1/storage.yml`). WAL refuses to write when full, so every write halts: act early. `/var/lib/postgresql` is the ZFS dataset `data/postgres` on the shared raidz1 `data` pool, so its free space IS the pool's: this alert is usually the pool-full problem, and `infra.md#stellarindex_zfs_pool_low_space` ([`infra.md#stellarindex_zfs_pool_low_space`](infra.md#stellarindex_zfs_pool_low_space)) fires with it. Cascading: [`ingestion-sink.md#stellarindex_ingestion_insert_errors`](ingestion-sink.md#stellarindex_ingestion_insert_errors), [`ingestion-sink.md#stellarindex_ingestion_trade_insert_backpressure`](ingestion-sink.md#stellarindex_ingestion_trade_insert_backpressure). Postgres log shows `could not extend file ... No space left on device`.

Check:

```sh
ssh root@136.243.90.96

# Capacity breakdown: dataset AND pool view
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

Likely causes:

- Compression policy not running ([`timescale.md#stellarindex_timescale_compression_lag`](timescale.md#stellarindex_timescale_compression_lag), [`timescale.md#stellarindex_timescale_cagg_stale`](timescale.md#stellarindex_timescale_cagg_stale)).
- The shared `data` pool is full (ClickHouse lake, MinIO galexie-archive, pgBackRest repo1, stale ZFS snapshots): pool levers are owned by `infra.md#stellarindex_zfs_pool_low_space`. A running pgBackRest backup adds real space (`repo1-retention-full=2`); check `systemctl list-timers pgbackrest-backup.timer`.
- WAL fill-up: a long transaction or a stale slot in `pg_replication_slots` (r1 has no replica, so any slot is left-over cruft: advance or drop it).
- A large unwindowed load or `pg_repack` (doubles a table briefly). Heavy one-shots must run under `/usr/local/sbin/run-heavy-job.sh`. The Postgres log is on the root fs, not here: that is `infra.md#stellarindex_node_root_disk_full` / `infra.md#stellarindex_node_root_disk_warning`.

Fix:

1. Retention is NOT a lever: raw `trades` are kept forever (ADR-0034) and the served `prices_*` caggs are indefinite. Do NOT adjust a retention interval or `drop_chunks` on data tables. The surviving policies are `api_usage_events` and `usage_daily` (12 months) and `prices_1m` and `price_source_contributions` (90 days, shipped disabled; arming is a deliberate act, not a disk-full response).
2. Create headroom NOW at the pool level, before investigating: follow `infra.md#stellarindex_zfs_pool_low_space` (stale ZFS snapshots, pgBackRest repo pruning, ZSTD recompression of ClickHouse tables). `runuser -u postgres -- psql -d stellarindex -c "CHECKPOINT;"` lets WAL recycle sooner but frees little.
3. There is no scale-the-volume escape hatch: the pool is fixed and r1 is not hardware-upgradeable (ADR-0027). Software relief only.
4. Once green, find why it filled (compression lag, snapshots, backfill, ClickHouse growth) and verify WAL recycles: `SELECT * FROM pg_stat_archiver;`.
5. Verify pool free space trending up, `/var/lib/postgresql` avail > 20 %, no `No space` errors for an hour.

Escalate: this is a P1; if the pool cannot be relieved in minutes, page the database owner.

## stellarindex_pg_lock_convoy

P1 (page, `for: 2m`), `configs/prometheus/rules.r1/storage.yml`. Producer: `timescale-jobs-probe.timer` (60 s, `configs/ansible/roles/archival-node/tasks/10-observability.yml`) publishing `stellarindex_pg_lock_convoy_backends`, `stellarindex_pg_lock_convoy_wait_seconds_max` and `stellarindex_pg_lock_blocked_backends` via the textfile collector. Postgres queues lock requests fairly, so one pending exclusive-lock request blocks every later request for that object, including reads. The predicate counts only backends whose blocker is itself blocked, so a healthy long decompress (holding `AccessExclusiveLock` for hours, with a running blocker) does not fire it. The probe runs its own short `psql` and reads only `pg_stat_activity` and `pg_blocking_pids()` (no lock on `trades`), so it answers when postgres_exporter, which is in the convoy, cannot: most `pg_*` alerts are blind while this fires. Every unit reads `active`; unit state is useless here. Expect alongside: `stellarindex_postgres_exporter_down`, [`stellarindex_aggregator_silent`](aggregator.md#stellarindex_aggregator_silent), `/v1/status` degraded; treat them as consequences. MTTR 5-20 min.

Check (copy the first output into the incident note before cancelling; `pg_stat_activity` is not retained):

```sh
ssh root@136.243.90.96

# 1. The whole chain, blocked backends and who blocks them. This reads
#    shared memory only, so it is answerable from inside the convoy.
runuser -u postgres -- psql -d stellarindex -x -c "
  SELECT a.pid,
         a.wait_event_type,
         pg_blocking_pids(a.pid)                        AS blocked_by,
         now() - a.query_start                          AS waited,
         a.application_name,
         a.client_port,
         left(a.query, 120)                             AS query
    FROM pg_stat_activity a
   WHERE a.wait_event_type = 'Lock'
   ORDER BY a.query_start"

# 2. The HEAD of the chain: the backend that is blocking others and is
#    itself blocked by something that is NOT waiting. This is the one
#    decision to make.
runuser -u postgres -- psql -d stellarindex -x -c "
  SELECT h.pid, now() - h.query_start AS running, left(h.query, 200) AS query
    FROM pg_stat_activity h
   WHERE h.wait_event_type IS DISTINCT FROM 'Lock'
     AND h.pid IN (SELECT unnest(pg_blocking_pids(a.pid))
                     FROM pg_stat_activity a
                    WHERE a.wait_event_type = 'Lock')"
```

`client_port` maps a backend to its OS process (`ss -tnp | grep <port>`) when `application_name` is empty.

Likely causes:

- A long read (aggregator cold-start scan of `trades`, `wait_event = IO/BufFileRead` means it spilled to disk) colliding with a chunk decompress/compress, e.g. a `usd-volume-restamp -chunks` window. The restamp asks under `SET LOCAL lock_timeout` and retries (`internal/storage/timescale/trades_chunks.go`); if a restamp heads the convoy, check the binary predates that change.
- A by-hand `ALTER TABLE` / `CREATE INDEX` / `VACUUM FULL` on a busy relation, or a migration auto-deployed mid-day: run DDL under an explicit `SET lock_timeout` and retry.
- A built-in compression policy compressing a chunk a long reader scans (`trades_compression_policy` uses a 5 s `lock_timeout` and cannot head a convoy).
- An `idle in transaction` session at the head of the chain.

Fix:

1. Get the head of the chain first. Cancelling a backend in the middle frees only its own slot and the convoy re-forms.
2. Head is a long read safe to lose: `SELECT pg_cancel_backend(<pid>);` (drained in under 20 s previously).
3. Head is a decompress/compress that is working: let it finish (`chunks_detailed_size` on that chunk climbing is progress; flat for minutes is wedged). Killing it mid-flight can leave a 160 GB chunk decompressed with compression paused: see [ops-job-stalled](ops-job-stalled.md) and `docs/operations/usd-volume-rederive-2026-08.md`. Head is a DDL: cancel and re-run with `lock_timeout` and a retry.
4. Prefer `pg_cancel_backend` over `pg_terminate_backend` (terminate drops the connection and clients reconnect into the same convoy).
5. Verify both convoy gauges read 0 on the next probe tick and the alert clears in 2 min; confirm `stellarindex_postgres_exporter_down` clears and `up{job="postgres"}` is 1.

Escalate: record what the head was doing, deploys/migrations/restamps in the previous ten minutes, and how long `postgres_exporter` was blind; say so rather than reading `pg_*` silence as health.

## Related

- [`infra.md#stellarindex_timescale_backup_none_24h`](infra.md#stellarindex_timescale_backup_none_24h), [`meta.md#stellarindex_redis_exporter_down`](meta.md#stellarindex_redis_exporter_down), `docs/architecture/ha-plan.md` (design only, not deployed on r1; ADR-0050: no cross-region Postgres replication).
