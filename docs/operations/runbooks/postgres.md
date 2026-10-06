---
title: Runbook — postgres
last_verified: 2026-10-06
status: living
severity: P1
---

# Runbook — postgres alerts

Postgres/Timescale primary, replica and connection alerts. Merged from four former pages.

## At a glance

- [`stellarindex_timescale_primary_down`](#stellarindex_timescale_primary_down)
- [`stellarindex_postgres_ping_failing`](#stellarindex_postgres_ping_failing)
- [`stellarindex_timescale_replica_lag`](#stellarindex_timescale_replica_lag)
- [`stellarindex_timescale_connections_saturated`](#stellarindex_timescale_connections_saturated)
- [`stellarindex_timescale_lock_table_pressure`](#lock-table-pressure-stellarindex_timescale_lock_table_pressure)

## stellarindex_timescale_primary_down

_Source page `postgres.md#stellarindex_timescale_primary_down`: status ratified, severity P1, last verified 2026-08-28._

> **R1 DEPLOYMENT REALITY (re-verified 2026-08-28).** R1 runs a
> **single, unclustered Postgres 15 + TimescaleDB** as
> `postgresql@15-main.service`, managed by the Ansible `archival-node`
> role (`configs/ansible/roles/archival-node/tasks/05-postgres.yml`).
> There is **no Patroni, no etcd, no replica** — the HAProxy/Patroni
> design in `docs/architecture/ha-plan.md` §3.3 is a ratified DESIGN
> that no playbook invokes (see that doc's "DEPLOYMENT STATE" banner and
> `configs/ansible/roles/patroni/README.md` F-1266). Nothing fails over
> automatically; recovery is "restart the service" or "restore from
> pgBackRest". The rule comment in
> `configs/prometheus/rules.r1/storage.yml` (F-1329) says the same.
> The pre-2026-08-28 version of this runbook described the undeployed
> cluster — see the appendix at the bottom.

### At a glance

| Field | Value |
| ----- | ----- |
| Alert | `stellarindex_timescale_primary_down` — `configs/prometheus/rules.r1/storage.yml` (group `stellarindex.storage`, `severity: page`, `for: 30s`) |
| Severity | **P1** (SEV-1) |
| Detected by | Prometheus rule `pg_up == 0` from the `postgres_exporter` scrape job (`localhost:9187`, `configs/prometheus/prometheus.r1.yml`). Companions: `stellarindex_postgres_ping_failing` (page, `for: 2m`) within ~2–3 min; `stellarindex_api_price_stale` + `stellarindex_api_error_rate_critical` (`rules.r1/api.yml`) as served data goes stale. |
| Typical MTTR | **No automatic failover on r1.** MTTR is time-to-restart `postgresql@15-main.service` (minutes) when the data dir is intact, or a pgBackRest restore (hours — see [`infra.md#stellarindex_timescale_backup_none_24h`](infra.md#stellarindex_timescale_backup_none_24h) and [`../drills/restore-drills.md`](../drills/restore-drills.md)) when it is not. |
| Impact | Writes halt everywhere (trade ingestion, API-key mint, usage rollups). Reads: the Redis hot path keeps serving cached prices with `stale_flag=true`; ClickHouse-backed lake/explorer endpoints keep working; Timescale-backed endpoints 5xx and `/v1/readyz` returns 503. |

### Symptoms

- Alert `stellarindex_timescale_primary_down` fires — `pg_up == 0` for 30 s
  (`postgres_exporter` could not reach the server).
- `/v1/readyz` returns 503 with the `postgres` check `ok:false` (the
  `postgres` checker is `Critical()` — `cmd/stellarindex-api/main.go`).
- API write endpoints (`POST /v1/account/keys` etc.) error; reads from the
  Redis hot path still work with `stale_flag=true`.
- `stellarindex_postgres_ping_failing` (page) follows within ~2–3 min — the
  indexer's 60 s pool-ping probe starts logging errors.
- `stellarindex_ingestion_cursor_stuck` (**ticket**, `increase(...[5m]) == 0`
  with `for: 5m`, `rules.r1/ingestion.yml`) follows ~10 min later. It cannot
  fire within 60 s — do not wait for it as a confirmation signal.

### Quick diagnosis (≤ 5 min)

**Step 1 — start with `/v1/readyz`.** It's the fastest signal
that distinguishes "API hitting a real DB problem" from
"Prometheus scrape blip". `checks` is an **array** of
`{name, ok, error}` (`internal/api/v1/server.go` `checkResult`):

```sh
curl -sS https://api.stellarindex.io/v1/readyz | jq '.checks[] | select(.name=="postgres")'
# Expect: {"name":"postgres","ok":true} when DB is reachable.
# {"name":"postgres","ok":false,"error":"dial tcp ... connection refused"} → real DB outage.
# (drop -f: on outage readyz returns 503 and -f would hide the body)
```

The 2026-04 SEV-1 tabletop drill found this ordering shaved
~1 min off detection vs the older "metric → readyz" path
(see [drill log](../drills/README.md#drill-log)
— a pre-launch tabletop that assumed a Patroni replica; historical only).

**Step 2 — confirm on r1 (unit, exporter signal, direct psql):**

```sh
# The cluster unit, NOT the `postgresql` umbrella (F-0154).
ssh root@r1 'systemctl status postgresql@15-main.service --no-pager'

# What postgres_exporter sees (this is the alert's source signal).
ssh root@r1 'curl -s localhost:9187/metrics | grep ^pg_up'
# pg_up 1 = exporter reaches the server; pg_up 0 = it cannot.

# Directly poke Postgres over the local socket (Postgres is loopback-only on r1).
ssh root@r1 'sudo -u postgres pg_isready -t 5; sudo -u postgres psql -d stellarindex -c "SELECT pg_is_in_recovery();"'
# Any error = Postgres really down.
```

If all three confirm Postgres is down → real incident, proceed to mitigation.
If only the Prometheus alert fires and `systemctl status` + `psql` say
healthy → a monitoring-side issue: check `stellarindex_postgres_exporter_down`
(`rules.r1/meta.yml`, [`meta.md#stellarindex_redis_exporter_down`](meta.md#stellarindex_redis_exporter_down)) and
`stellarindex_prometheus_scrape_failing` ([`meta.md#stellarindex_prometheus_scrape_failing`](meta.md#stellarindex_prometheus_scrape_failing)).
Treat as P3 scrape failure, not P1.

### Mitigation (≤ 15 min)

#### A. Find out why it stopped before restarting it

```sh
ssh root@r1 'df -h /var/lib/postgresql; zpool status -x'
ssh root@r1 'tail -n 100 /var/log/postgresql/postgresql-15-main.log'
ssh root@r1 'dmesg -T | grep -i -E "oom|out of memory|nvme|i/o error" | tail -n 30'
ssh root@r1 'journalctl -u postgresql@15-main.service --since "1 hour ago" --no-pager | tail -n 50'
```

- Disk full → follow [`db-disk-full.md`](db-disk-full.md) first; a restart
  onto a full volume will just crash again.
- ZFS pool degraded / NVMe dropped → [`infra.md#stellarindex_zfs_pool_degraded`](infra.md#stellarindex_zfs_pool_degraded);
  do not restart until the pool is online.
- OOM-killed → restart (B) is safe; capture `dmesg` for RCA.

#### B. Restart the cluster unit

```sh
# The CLUSTER unit, not the `postgresql` umbrella (F-0154 / postgres.md#stellarindex_postgres_ping_failing).
ssh root@r1 'systemctl restart postgresql@15-main.service && sleep 5 && systemctl status postgresql@15-main.service --no-pager'
ssh root@r1 'sudo -u postgres pg_isready -t 5'
```

Verify recovery:

- [ ] `pg_up 1` at `localhost:9187/metrics`; alert clears on the next
      30 s evaluation.
- [ ] `/v1/readyz` → 200 with `{"name":"postgres","ok":true}`.
- [ ] Indexer logs resume inserting trades
      (`ssh root@r1 'journalctl -u stellarindex-indexer.service -f'`).

#### C. Un-wedge the indexer pool after Postgres comes back

F-0151 (2026-05-26): the indexer's `*sql.DB` pool held dead connections
for ~14 h after `postgresql@15-main` recovered. The pool now retires
connections every 30 min, but do not wait for that:

```sh
ssh root@r1 'journalctl -u stellarindex-indexer.service --since "30 min ago" | grep -E "postgres.ping|pool may be wedged"'
# If `pool may be wedged` appears, or stellarindex_postgres_ping_failing stays firing:
ssh root@r1 'systemctl restart stellarindex-indexer.service'
```

See [`postgres.md#stellarindex_postgres_ping_failing`](postgres.md#stellarindex_postgres_ping_failing). Then confirm
`stellarindex_ingestion_cursor_stuck` clears — it needs the cursor to
advance for a full 5 m window, so allow ~10 min.

#### D. Data directory unrecoverable → restore from pgBackRest

If Postgres will not start (corrupt WAL, lost dataset) the only copy of
the Timescale data is the pgBackRest repo:

```sh
ssh root@r1 'sudo -u postgres pgbackrest --stanza=stellarindex info'
ssh root@r1 'systemctl list-timers pgbackrest-backup.timer restore-drill.timer --no-legend'
```

Declare the incident on the status page ([`sev-status-page-update.md`](sev-status-page-update.md))
— writes are unavailable for the duration. Restore per
[`infra.md#stellarindex_timescale_backup_none_24h`](infra.md#stellarindex_timescale_backup_none_24h) and the rehearsed procedure in
[`../drills/restore-drills.md`](../drills/restore-drills.md)
(`scripts/ops/restore-drill.sh`). RPO is the age of the last successful
backup, not seconds. On return, the indexer's idempotent upserts
(`ON CONFLICT DO NOTHING`) re-fill the gap from the archive; check
`stellarindex_ingestion_cursor_stuck` and `stellarindex_ingest_gap_detected`.

#### Restore in place vs. fail over

There is no failover decision to make on r1 today: there is no standby
or replica to promote, and ADR-0050 builds no cross-region Postgres
replication (R2/R3 re-ingest independently). Recovery is always in place:

1. Data directory intact → restart the cluster unit (B, then C).
2. Data directory unrecoverable → pgBackRest restore (D) onto r1.
3. r1 itself gone → host-loss path (E).

Do not follow the promote/failover steps in `dr-activation.md` or the
appendix below; they describe the undeployed design. Revisit this bar
when the Phase-1 Patroni playbook lands (patroni README F-1266) and a
replica exists to promote.

#### E. Complete host loss

Refer to [`runbooks/dr-activation.md`](dr-activation.md). Out-of-scope for
this runbook. **Caveat:** `dr-activation.md` is itself `last_verified
2026-05-03` and describes the same undeployed Patroni/replica design;
it needs the same single-node correction before it can be followed
literally.

### Root cause analysis

Gather for the postmortem:

- `journalctl -u postgresql@15-main.service --since "1 hour ago"` on r1.
- Postgres logs: `/var/log/postgresql/postgresql-15-main.log` from around the
  event (`log_directory`/`log_filename` in
  `configs/ansible/roles/archival-node/templates/postgresql.conf.j2`).
- `dmesg -T` around the event (OOM, NVMe).
- Grafana screenshot of the Postgres panels.
- Disk-space + IOPS metrics — was it full, was it OOMKilled?
- Recent deploys: anything touching the Ansible `archival-node` role
  (`tasks/05-postgres.yml`, `templates/postgresql.conf.j2`) in the last 24 h?
  Check `stellarindex_binary_version_skew` /
  `stellarindex_binary_version_probe_degraded`
  (`rules.r1/binary-version-skew.yml`, [`binary-version-skew.md`](binary-version-skew.md))
  and recent `playbooks/deploy-binary.yml` runs.

Common root causes observed in similar systems:
1. **Disk full** — WAL couldn't write; Postgres halted writes. Catches: `stellarindex_timescale_disk_warning` (`storage.yml`) fires BEFORE this one usually. If it didn't, tune thresholds. This is the actual 2026-05-26/27 cascade.
2. **OOMKill** — Postgres process killed by the kernel. Check dmesg. Often means `shared_buffers` + `work_mem` × active_connections exceeded host RAM.
3. **Kernel / ZFS issue** — NVMe drive dropped, ZFS pool degraded. Catches: `stellarindex_zfs_pool_degraded` (`infra.yml`) fires first.
4. **Runaway query** — a long-running SELECT blocked WAL recycling. pg_stat_activity shows it.
5. **Config drift** — a hand edit to `/etc/postgresql/15/main/*.conf` that the next Ansible run reverted (or vice versa) and the service failed to start on reload.

### Known false-positive patterns

- **postgres_exporter restart / scrape gap**: `pg_up` is emitted by the
  exporter, so an exporter restart or a `postgres_exporter` scrape failure
  can look like a DB outage. Confirm via
  `systemctl status postgresql@15-main.service` and
  `stellarindex_postgres_exporter_down` before declaring.
- **Network partition between Prometheus and the exporter**: both run on r1
  today, so this is unlikely; still confirm via direct `psql` before declaring.

### Appendix — undeployed HA design (do not follow on r1)

Kept only so the intent is not lost when the Patroni playbook lands.
Everything here is **not runnable on r1 today**: no `patronictl`/`etcdctl`
binaries, no `patroni`/`etcd` units, no `db-*.internal` hostnames.

- Patroni scope would be `patroni_cluster_name` (`stellarindex-r1` in the
  role README's example inventory — not `stellarindex`); commands would be
  `patronictl -c /etc/patroni/patroni.yml list` / `... failover stellarindex-r1 --candidate <host>`.
- etcd would be a 3-node cluster (Patroni role preflight asserts exactly 3
  `postgres_cluster` hosts; quorum 2/3), keys under `/service/<scope>/`.
- Automatic sync-replica promotion in 30–60 s; async-replica promotion
  accepts ≤ 5 s RPO.

### Changelog

- 2026-10-02 — set the restore-in-place vs. failover bar: no replica
  exists, so recovery is always in place (restart, else pgBackRest
  restore) until the Patroni playbook lands.

- 2026-08-28 — rewritten for the single-node r1 reality (no Patroni/etcd/
  replicas; `postgresql@15-main.service` restart + pgBackRest restore paths);
  alert expr is `pg_up == 0` from `postgres_exporter` (F-1329); readyz
  `checks` is an array; companion alerts are `stellarindex_postgres_ping_failing`
  then `stellarindex_ingestion_cursor_stuck` (ticket, ~10 min); dropped the
  `deploy/timescale-statefulset.yaml` and R3-async-replica references
  (ADR-0050). Patroni text moved to the appendix.
- 2026-04-22 — initial draft. the maintainer.

## stellarindex_postgres_ping_failing

_Source page `postgres.md#stellarindex_postgres_ping_failing`: status ratified, severity P1, last verified 2026-08-29._

### At a glance

| Field | Value |
| ----- | ----- |
| Alert | `stellarindex_postgres_ping_failing` |
| Severity | P1 (page) |
| Detected by | `configs/prometheus/rules.r1/storage.yml` (the overlay r1 actually loads from `/etc/prometheus/rules.r1/*.yml`); multi-host template: `deploy/monitoring/rules/storage.yml`. Both trees carry the same expr. |
| Typical MTTR | 5 min |
| Impact | Indexer ingest is stalled or about to stall; live ledger writes failing every 60 s probe. |

### Why this exists

During the disk-full SEV cascade of 2026-05-26-27, a postgres
outage brought
`postgresql@15-main.service` down for ~10 h. Postgres recovered
when disk was freed, but the indexer's `*sql.DB` connection pool
held stale conns and silently failed writes for an additional ~3 h
until a manual `systemctl restart stellarindex-indexer`. Total
ledger gap: ~14 h.

The code fix shipped alongside this runbook:

1. `internal/storage/timescale/store.go` now sets
   `SetConnMaxLifetime(30 min)` + `SetConnMaxIdleTime(5 min)` —
   automatic pool refresh, bounds the cascade-gap to the lifetime
   interval.
2. `cmd/stellarindex-indexer/main.go::watchPostgresPing` probes the
   pool every 60 s and emits
   `stellarindex_postgres_ping_total{outcome="ok"|"error"}` plus the
   live streak gauge `stellarindex_postgres_ping_failure_streak`.

This alert fires on ANY nonzero ping-error rate sustained for
2 min (`rate(...{outcome="error"}[5m]) > 0`) — the live signal that
the safety-net hasn't refreshed yet AND something past the conn
layer is broken.

The original threshold was `> 0.5/s`, which was **unreachable**:
the probe ticks once per 60 s, so a permanently-wedged pool tops
out at 1/60 ≈ 0.0167/s — 30× below the trip point. The page could
never fire in its own target scenario. Corrected to `> 0` in both
rule trees on 2026-08-04; at the 60 s cadence a single failed probe
inside the 5 m window is already enough, and `for: 2m` rides through
a clean Postgres restart.

### Symptoms

- `rate(stellarindex_postgres_ping_total{outcome="error"}[5m]) > 0` for 2+ min.
- `stellarindex_postgres_ping_failure_streak` climbing past 3.
- Indexer journal: `pool may be wedged` log line.
- Downstream: `stellarindex_source_insert_errors_total{kind="trade"}`
  climbing, and/or `stellarindex_ingestion_trade_insert_backpressure`
  firing (post-ADR-0041 the sink RETRIES infrastructure faults rather
  than dropping, so backpressure is the earlier signal and a
  `kind="dropped"` increase means the bounded buffer overflowed —
  genuine loss). `stellarindex_trade_inserts_total` carries only
  `source` + `usd_volume_populated` labels; it has no `outcome`
  label to read here.

### Quick diagnosis (≤ 5 min)

```sh
# 1. Is postgres itself up? (F-0154 — use the CLUSTER name, not the umbrella.)
ssh root@136.243.90.96 'systemctl status postgresql@15-main.service'

# 2. Can a fresh client connect at all?
ssh root@136.243.90.96 'sudo -u postgres pg_isready -t 5'

# 3. What does the indexer journal say about the ping streak?
ssh root@136.243.90.96 'journalctl -u stellarindex-indexer.service --since "10 min ago" | grep -E "postgres.ping|pool may be wedged"'

# 4. Live metric on r1's prometheus (loopback-bound — query from the host):
ssh root@136.243.90.96 "curl -s http://localhost:9090/api/v1/query \
  --data-urlencode 'query=stellarindex_postgres_ping_failure_streak'" | jq .
```

### Mitigation (≤ 15 min)

If postgres@15-main is DOWN:

- [ ] Fix postgres first (see `db-disk-full.md` / `postgres.md#stellarindex_timescale_primary_down` depending on cause).
- [ ] The pool will refresh automatically within
      `PoolConnMaxLifetime` (30 min) once postgres is reachable,
      but you can force it now with a restart:
      `ssh root@136.243.90.96 'systemctl restart stellarindex-indexer.service'`
- [ ] Verification: ping streak resets to 0 + `outcome="ok"` rate climbs back.

If postgres is UP but ping still fails:

- [ ] Likely a network blip, firewall reset, or auth misconfig.
- [ ] Verify the DSN. There is no `/etc/default/stellarindex-indexer`
      on r1. The password-free DSN lives in the TOML as
      `postgres_dsn`; the unit's `EnvironmentFile=` is
      `/etc/default/stellarindex`, which carries the WITH-password
      override `STELLARINDEX_POSTGRES_DSN` (env wins):
      ```sh
      ssh root@136.243.90.96 'grep -n postgres_dsn /etc/stellarindex.toml'
      # redacts the password before it reaches your terminal/journal:
      ssh root@136.243.90.96 \
        "sed -n 's/^\(STELLARINDEX_POSTGRES_DSN=postgres:\/\/[^:]*\):[^@]*@/\\1:***@/p' /etc/default/stellarindex"
      ```
      Fix either one via ansible (`--check --diff` first, secrets via
      `ansible-vault edit inventory/r1.secrets.yml`), never by hand —
      a hand edit is reverted by the next playbook run.
- [ ] If pool wedged but DB healthy, restart the indexer to drain.

### Root cause analysis

- The 14 h cascade gap on 2026-05-26-27 root caused this whole
  resilience seam. Future post-mortems should record:
  - the streak length before the alert fired,
  - whether the lifetime safety-net or the manual restart refreshed
    the pool first.

### Known false-positive patterns

- A postgres restart will briefly fail pings — but
  `for: 2m` should ride through any clean restart. If you see this
  alert on every routine restart, the restart is taking too long
  (investigate separately).
- A network partition between indexer + DB will look identical to
  a pool problem from this alert's perspective; correlate with
  `up{job="postgres_exporter"}` (the scrape job landed 2026-05-27,
  F-0152 — see `meta.md#stellarindex_redis_exporter_down`).

### Changelog

- 2026-05-27 — initial draft alongside the F-0151 resilience fix.
- 2026-08-29 — re-verified against HEAD (runbook re-verification
  wave K). Threshold corrected to the shipped `> 0` (the documented
  0.5/s was unreachable by 30× at the 60 s probe cadence, so the
  page could never fire); the downstream signal `trade_inserts_total{outcome="error"}`
  does not exist (no `outcome` label) and was replaced with
  `source_insert_errors_total` + the ADR-0041 backpressure alert;
  DSN lookup repointed at `/etc/stellarindex.toml` +
  `/etc/default/stellarindex`; F-0152 landed; host shapes → r1's IP.

## stellarindex_timescale_replica_lag

_Source page `postgres.md#stellarindex_timescale_replica_lag`: status current, severity P2, last verified 2026-08-28._

> **R1 DEPLOYMENT REALITY (re-verified 2026-08-28).** R1 runs a
> **single, unclustered Postgres with NO replica** — no standby, no
> streaming replication, no sync quorum. `pg_replication_lag_seconds`
> therefore has **no series**, and this alert is **INERT on r1
> today**: it cannot fire, and nothing it describes exists yet. The
> planned first consumer is the **r3 async DR replica**
> ([ADR-0016](../../adr/0016-per-region-storage-strategy.md)); the
> "sync replica holding back primary writes" scenario belongs to the
> undeployed HA topology (`docs/architecture/ha-plan.md`). The
> procedures below are kept as the playbook for **when a standby
> exists** — do not act on them against r1 as it stands.

### At a glance

| Field | Value |
| ----- | ----- |
| Alert | `stellarindex_timescale_replica_lag` — **inert on r1, see banner** |
| Severity | P2 (`severity: ticket`) |
| Detected by | `configs/prometheus/rules.r1/storage.yml` (group `stellarindex.storage`, `severity: ticket`, `for: 2m`); multi-host twin in `deploy/monitoring/rules/storage.yml`. |
| Typical MTTR | 15–60 min |
| Impact | Depends on the replica's role. If it's a **sync** replica (undeployed HA topology — not r1), writes on the primary are held back until it catches up → indexer + aggregator insert-rate drops, cursor alerts may follow. If it's an async replica (the planned r3 DR shape), the impact is weaker — the DR copy's RPO grows; reads served from it (none planned) would see stale data. |

### Symptoms

- `pg_replication_lag_seconds > 5` sustained 2 min. Note this metric
  only exists **on a standby** — a primary with no replica (r1
  today) exports nothing, which is absence, not health.
- `pg_stat_replication` on the primary shows a `write_lag` /
  `replay_lag` value > 5 s.
- If sync replica (future topology): primary has transactions in
  `wait_event_type = SyncRep`.

### Quick diagnosis (≤ 5 min)

```sh
# Primary's view of all replicas
psql -c "SELECT application_name, state, sync_state,
                pg_wal_lsn_diff(sent_lsn, replay_lsn) AS replay_bytes,
                replay_lag
         FROM pg_stat_replication;"

# Replica's view
ssh <replica-host> 'psql -c "SELECT now() - pg_last_xact_replay_timestamp() AS lag"'

# Replica system load — IO-bound, CPU-bound, network?
ssh <replica-host> 'iostat -x 1 5'
ssh <replica-host> 'top -b -n1 | head -10'

# Is the network link OK?
ssh <replica-host> 'ping -c 10 <primary>'
```

### Typical root causes

1. **Replica IO saturated** — can't replay WAL as fast as primary
   generates it. Usually means the replica's disk is slower than
   the primary's, or it's busy serving read traffic.
   - Mitigation: reduce read load on the replica; improve disk.

2. **Long-running query on the replica** blocking WAL apply
   (`hot_standby_feedback` or conflict). Read queries on the
   replica can indefinitely postpone WAL application.
   - Signal: `pg_stat_activity` on the replica shows `Startup
     process` waiting on a query.
   - Mitigation: cancel the offending query with
     `SELECT pg_cancel_backend(...)`.

3. **Network bandwidth saturated.** Happens during resilvering of
   the primary host's storage, or during a big backfill that
   produces WAL faster than the link can stream.

4. **Replica process wedged** — rare but possible, especially on
   older Postgres versions. Usually requires replica restart.

5. **max_wal_senders / wal_keep_size misconfig**. Primary has
   already recycled WAL the replica needed; replica can't catch
   up incrementally and needs a base-backup.
   - Signal: replica log says `requested WAL segment has already
     been removed`.
   - Mitigation: rebuild the replica from a fresh base-backup.

### Mitigation

- [ ] Step 1 — identify whether it's resource, query, network, or
      WAL-recycle (above).
- [ ] Step 2 — for resource: shed read load / scale disk.
- [ ] Step 3 — for query block: cancel the offender.
- [ ] Step 4 — for network: monitor for recovery; investigate
      whatever's hogging bandwidth.
- [ ] Step 5 — for WAL-recycle: rebuild the replica from
      base-backup — this is a longer procedure (hours).
- [ ] Verification: `pg_replication_lag_seconds` back under 1 s
      sustained 15 min.

### Root cause analysis

- Primary + replica system metrics across the window.
- `pg_stat_replication` timeline — when did the lag start
  growing?
- Was there a deploy, schema migration, or backfill around the
  event? Those produce WAL bursts.

### Known false-positive patterns

- **Burst write during a backfill** — lag rises during and
  subsides after. If expected, silence for the duration.
- **Replica restart** — briefly out of sync on startup until it
  catches up.
- **Asynchronous replica** during peak traffic — some async lag
  is expected; the 5 s alert threshold should cover normal cases
  but tune if you get chronic flap.

### Changelog

- 2026-08-28 — re-verified against HEAD. Added the R1 DEPLOYMENT
  REALITY banner: no replica exists on r1, `pg_replication_lag_seconds`
  has no series, alert inert; planned consumer is the r3 async DR
  replica (ADR-0016). Symptoms re-scoped (metric exists only on a
  standby); sync-replica impact marked as the undeployed HA topology;
  the `postgres.md#stellarindex_timescale_primary_down` "precursor to failover" claim
  re-scoped to the future topology (no automatic failover on r1).
  Procedures retained as the playbook for when a standby exists.
  Rule citation → `rules.r1/storage.yml`.
- 2026-04-23 — initial draft.

## stellarindex_timescale_connections_saturated

_Source page `postgres.md#stellarindex_timescale_connections_saturated`: status current, severity P2, last verified 2026-08-28._

> **R1 DEPLOYMENT REALITY (re-verified 2026-08-28).** PgBouncer is
> **NOT deployed** on r1 — the only trace in the repo is a
> future-tense comment in the archival-node role's `pg_hba.conf.j2`.
> Every binary connects **directly** to Postgres
> (`postgresql@15-main.service`, `max_connections = 200` —
> `configs/ansible/roles/archival-node/defaults/main.yml`
> `postgres_max_connections`). There is no pooler to queue behind, no
> `SHOW POOLS`, no `pool_size` to bump: at 100 % you get
> `FATAL: sorry, too many clients already`, full stop. The rule's own
> description ("PgBouncer should be smoothing this") is itself
> aspirational.

### At a glance

| Field | Value |
| ----- | ----- |
| Alert | `stellarindex_timescale_connections_saturated` |
| Severity | P2 (`severity: ticket`) |
| Detected by | `configs/prometheus/rules.r1/storage.yml` (group `stellarindex.storage`, `severity: ticket`, `for: 5m`) — the file r1 actually loads; multi-host twin in `deploy/monitoring/rules/storage.yml`. Metric source: `postgres_exporter` (`localhost:9187`). |
| Typical MTTR | 15–60 min |
| Impact | > 80 % of `max_connections` (200 on r1) in use. New connections start failing with `sorry, too many clients` as we approach 100 %. API latency rises; the indexer's `database/sql` pool fails to acquire connections. |

### Symptoms

- `(sum by (instance) (pg_stat_activity_count) / on (instance) pg_settings_max_connections) * 100 > 80`
  for ≥ 5 min. `pg_stat_activity_count` is per connection state
  (`active`, `idle`, …), so summing by instance first is load-bearing:
  the unaggregated form leaves several series per instance facing one
  `pg_settings_max_connections` series, the division matches nothing,
  and pasting it during a suspected saturation event returns no data —
  reading as healthy when it may not be.
- API latency climbs (connection acquisition becomes the bottleneck).
- Postgres log shows `FATAL: sorry, too many clients already` if
  we've hit 100 %.
- Companion signal: the indexer's `watchPostgresPing` goroutine
  (`cmd/stellarindex-indexer/main.go`) pings its pool every 60 s;
  when the DB stops answering, `stellarindex_postgres_ping_failing`
  **pages** (`postgres.md#stellarindex_postgres_ping_failing`) and the indexer journal
  logs the `pool may be wedged` line. If that page is firing too,
  you're at (or past) hard saturation, not just the 80 % ticket.

### Quick diagnosis (≤ 5 min)

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

### Typical root causes

1. **Long-running transaction leaking connections.** A handler
   opens a transaction, does work, but never commits or rolls
   back because of an unhandled error path.
   - Signal: `xact_age` > 10 min on several connections from one
     `application_name`.
   - Mitigation: kill the offending pids:
     `SELECT pg_terminate_backend(pid)`; fix the code.

2. **Connection leak in one of our binaries** — goroutines that
   never release their `*sql.Conn` (the indexer's pool is Go's
   `database/sql` `*sql.DB`, not pgx). Usually shows as a steadily
   climbing count from one `application_name`.
   - Mitigation: restart the leaking binary
     (`systemctl restart stellarindex-<name>`); fix the leak with
     pprof / `sql.DBStats` introspection.

3. **Burst load** — a marketing campaign, a viral asset, a big
   external caller. Legitimate; the fix is capacity planning, not
   cleanup.

4. **Idle-in-transaction timeout not configured.** Without a
   timeout, a broken client's leaked transactions accumulate
   forever.
   - Mitigation: consider `idle_in_transaction_session_timeout`
     (e.g. `5min`) at the role or db level — via the archival-node
     ansible role, not a live-only edit.

### Lock-table pressure (`stellarindex_timescale_lock_table_pressure`)

The **other** alert that routes to this runbook:
`stellarindex_timescale_lock_table_pressure`
(`rules.r1/storage.yml`, `severity: ticket`, `for: 5m`) fires when
`sum by (instance)(pg_locks_count) / on (instance)
(pg_settings_max_locks_per_transaction × pg_settings_max_connections)
> 0.7`. The `sum` is load-bearing: postgres_exporter emits
`pg_locks_count` once per (datname, mode), so the pre-#301
unqualified division matched no series and this alert was silently
dead from the day the exporter landed. The lock table is a fixed
shared-memory arena sized `max_locks_per_transaction ×
max_connections`; exhausting it is SQLSTATE 53200
("out of shared memory") and dropped INSERTs — the 2026-05-06 SEV-3
class, which recurred 2026-05-15 when `trades` grew to ~2,738 chunks
and TimescaleDB's per-chunk locking thrashed the arena. Everything
above about `pg_stat_activity` finds **nothing relevant** for this
alert — it's lock slots, not connections.

Diagnosis:

```sh
runuser -u postgres -- psql -d stellarindex -c \
  "SELECT (SELECT count(*) FROM pg_locks) AS locks_held,
          current_setting('max_locks_per_transaction')::int
            * current_setting('max_connections')::int AS lock_table_size;"
```

r1 templates `max_locks_per_transaction = 4096`
(`postgresql.conf.j2`, `postgres_max_locks_per_transaction` in the
archival-node defaults — 819,200 entries with `max_connections=200`).
The fix for sustained pressure is bumping
`postgres_max_locks_per_transaction` in the archival-node ansible
role and reapplying (`--check --diff` first) — **never** a live-only
`ALTER SYSTEM` edit, which the next ansible run reverts (AGENTS.md
"codify every host change"). The bump needs a Postgres restart.

### Mitigation

- [ ] Step 1 — identify the top holder (above). If the firing alert
      is `_lock_table_pressure`, jump to that section instead.
- [ ] Step 2 — if long-running xact: terminate the offenders with
      `pg_terminate_backend`, then fix the code.
- [ ] Step 3 — if a binary is leaking: restart it; consider
      `idle_in_transaction_session_timeout` as a backstop (via
      ansible).
- [ ] Step 4 — if genuine growth: raise `postgres_max_connections`
      in the archival-node role deliberately (each connection costs
      backend memory) — again via ansible + reapply, never live-only.
- [ ] Verification: utilization drops under 50 % sustained; no
      more `too many clients` errors in logs.

### Root cause analysis

- Which application name was the offender?
- Timeline of connection growth — steady climb (leak) or spike
  (burst)?
- Correlated API metrics — did request rate rise in lockstep?

### Known false-positive patterns

- **Backup process** briefly opens many connections (pgBackRest
  `--process-max`). Expected; narrow to backup window.
- **Brief spike during an outage of a downstream service**
  (Redis) — API handlers take longer, so connection hold-time
  rises, so utilization spikes. Resolves when the downstream
  recovers.

### Changelog

- 2026-08-29 — corrected the quoted `_lock_table_pressure` expr: it
  now sums `pg_locks_count` over the exporter's per-(datname, mode)
  series and joins `on (instance)`. The previous unqualified
  division could never match real postgres_exporter output, so the
  alert had been inert since F-0152 put the exporter on r1 (#301).
- 2026-08-28 — re-verified against HEAD. Added the R1 DEPLOYMENT
  REALITY banner: PgBouncer is not deployed (future-tense comment in
  `pg_hba.conf.j2` only); all PgBouncer diagnosis/mitigation
  (`SHOW POOLS` on :6432, `pool_size` bumps) replaced with
  direct-Postgres guidance (terminate offenders,
  `idle_in_transaction_session_timeout`, restart the leaking binary).
  Added the lock-table-pressure section
  (`stellarindex_timescale_lock_table_pressure` routes here; fix =
  `postgres_max_locks_per_transaction` bump in the ansible role, not
  a live edit). Indexer pool corrected to `database/sql` (not pgx),
  with `watchPostgresPing` / `stellarindex_postgres_ping_failing` as
  the companion signal. Rule citation → `rules.r1/storage.yml`;
  replica cross-ref dropped (no replica).
- 2026-04-23 — initial draft.

## Related

**stellarindex_timescale_primary_down**

- ADR-0006 (TimescaleDB) — storage choice.
- HA plan §3.3 — Patroni topology (**design only — NOT deployed on r1**, see
  `docs/architecture/ha-plan.md` §2.1).
- ADR-0050 / `docs/architecture/ha-plan.md` §1 — no cross-region
  Postgres replication; the older `multi-region-topology.md` §5 is superseded.
- [`postgres.md#stellarindex_postgres_ping_failing`](postgres.md#stellarindex_postgres_ping_failing),
  [`db-disk-full.md`](db-disk-full.md), [`infra.md#stellarindex_timescale_backup_none_24h`](infra.md#stellarindex_timescale_backup_none_24h),
  [`meta.md#stellarindex_redis_exporter_down`](meta.md#stellarindex_redis_exporter_down).
- Postmortems: `docs/operations/postmortems/` (none yet; first one goes here).

**stellarindex_postgres_ping_failing**

- `internal/storage/timescale/store.go` — `configurePool` +
  `PingContext` (the implementation).
- `cmd/stellarindex-indexer/main.go::watchPostgresPing` —
  the probe goroutine.
- `db-disk-full.md` — the most common upstream cause.
- `postgres.md#stellarindex_timescale_primary_down` — adjacent alert; this one fires
  when the DB is reachable from prometheus but not from the
  indexer.

**stellarindex_timescale_replica_lag**

- `postgres.md#stellarindex_timescale_primary_down` — that runbook now states there is
  **no automatic failover on r1**; in the future clustered topology,
  replica lag would be the precursor signal to a failover event, but
  today primary-down means restart-or-restore, and this alert plays
  no part.
- `db-disk-full.md` — no disk → no WAL → infinite lag.
- `postgres.md#stellarindex_timescale_connections_saturated` — if the (future) replica pool is
  saturated.
- [ADR-0016](../../adr/0016-per-region-storage-strategy.md) — the r3
  async DR replica that will be this alert's first real consumer.

**stellarindex_timescale_connections_saturated**

- `api.md#stellarindex_api_latency_p95_high` — upstream symptom.
- `postgres.md#stellarindex_postgres_ping_failing` — the indexer-side page that fires when
  saturation becomes unavailability.
- `db-disk-full.md` — writes blocked → xacts can't commit →
  connections hold.
